package graphql

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"relay/internal/protocol"
	"relay/pkg/relay"
)

// maxResponseBodyBytes caps how much response body we buffer, matching the REST
// executor's guard against a hostile backend (spec §40).
const maxResponseBodyBytes = 10 << 20 // 10 MiB

// defaultTimeout bounds a single GraphQL call when the caller's context carries
// no earlier deadline. The daemon supplies its own deadline; this is a backstop.
const defaultTimeout = 30 * time.Second

// variableRefPattern matches a GraphQL variable reference or declaration such
// as $id. Variables are the only $name tokens a document contains, so this is
// enough to know which inputs a query needs (spec §44).
var variableRefPattern = regexp.MustCompile("\\$([a-zA-Z_][a-zA-Z0-9_]*)")

// Executor is a GraphQL protocol executor that satisfies protocol.Executor.
//
// It holds no per-request state, and its http.Client is documented as safe for
// concurrent use, so one Executor safely serves concurrent invocations.
type Executor struct {
	// Client is the HTTP client used for every request. If nil, a default
	// client with a 30-second timeout is used.
	Client *http.Client
}

// New returns an Executor with sensible defaults.
func New() *Executor {
	return &Executor{Client: &http.Client{Timeout: defaultTimeout}}
}

// Execute posts one GraphQL document to the endpoint.
func (e *Executor) Execute(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	spec := req.Spec

	endpoint := spec.Endpoint
	if endpoint == "" {
		endpoint = spec.BaseURL
	}
	if endpoint == "" {
		return protocol.Response{}, relay.NewError(
			relay.CodeInvalidInput,
			"Endpoint must be set for a graphql operation",
		)
	}
	if spec.Document == "" {
		return protocol.Response{}, relay.NewError(
			relay.CodeInvalidInput,
			"Document must be set for a graphql operation",
		).WithDetails(map[string]any{"endpoint": endpoint})
	}

	payload := map[string]any{"query": spec.Document}
	if variables := buildVariables(spec.Variables, spec.Document, req.Input); len(variables) > 0 {
		payload["variables"] = variables
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return protocol.Response{}, relay.NewError(
			relay.CodeInvalidInput,
			"failed to encode graphql request: "+err.Error(),
		)
	}

	headers := make(http.Header)
	for name, value := range spec.Headers {
		headers.Set(name, value)
	}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	if req.Credential != nil {
		applyCredential(headers, req.Credential)
	}

	client := e.Client
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return protocol.Response{}, relay.NewError(
			relay.CodeInvalidInput,
			"failed to build graphql request: "+err.Error(),
		).WithDetails(map[string]any{"endpoint": endpoint})
	}
	httpRequest.Header = headers

	httpResponse, err := client.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return protocol.Response{}, relay.NewError(
				relay.CodeTimeout,
				"context deadline exceeded or cancelled",
			).WithDetails(map[string]any{"endpoint": endpoint})
		}
		return protocol.Response{}, relay.NewError(
			relay.CodeNetworkError,
			"transport error: "+err.Error(),
		).WithDetails(map[string]any{"endpoint": endpoint})
	}
	defer httpResponse.Body.Close()

	return readAndMapResponse(httpResponse, endpoint)
}

// buildVariables resolves the variables a document uses from the operation
// input. A mapping entry renames a variable onto an input property; with no
// entry, the variable name is the property name. Only variables the document
// actually references are sent, so the server never sees an unused variable
// (which GraphQL rejects).
func buildVariables(mapping map[string]string, document string, input map[string]any) map[string]any {
	variables := make(map[string]any)
	for _, name := range variableRefs(document) {
		property := name
		if mapped, ok := mapping[name]; ok && mapped != "" {
			property = mapped
		}
		value, ok := input[property]
		if !ok {
			continue
		}
		variables[name] = value
	}
	return variables
}

// variableRefs returns the distinct variable names a document references, in
// first-seen order.
func variableRefs(document string) []string {
	seen := make(map[string]bool)
	var names []string
	for _, match := range variableRefPattern.FindAllStringSubmatch(document, -1) {
		name := match[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// applyCredential attaches the daemon-injected secret, exactly as the REST
// executor does so both protocols authenticate identically (spec §22).
func applyCredential(headers http.Header, credential *protocol.Credential) {
	headerName := credential.Header
	if headerName == "" {
		headerName = "Authorization"
	}
	value := credential.Secret
	if credential.Scheme != "" {
		value = credential.Scheme + " " + credential.Secret
	}
	headers.Set(headerName, value)
}

// readAndMapResponse normalizes a GraphQL HTTP response and maps failures onto
// the shared error taxonomy (spec §26).
func readAndMapResponse(response *http.Response, endpoint string) (protocol.Response, error) {
	limitedReader := io.LimitReader(response.Body, maxResponseBodyBytes+1)
	bodyBytes, err := io.ReadAll(limitedReader)
	if err != nil {
		return protocol.Response{}, relay.NewError(
			relay.CodeNetworkError,
			"failed to read response body",
		).WithDetails(map[string]any{"httpStatus": response.StatusCode, "endpoint": endpoint})
	}
	if int64(len(bodyBytes)) > maxResponseBodyBytes {
		return protocol.Response{}, relay.NewError(
			relay.CodeRemoteError,
			"response body exceeds maximum allowed size",
		).WithDetails(map[string]any{"httpStatus": response.StatusCode, "endpoint": endpoint})
	}

	result := protocol.Response{
		Status:  response.StatusCode,
		Headers: copyHeaders(response.Header),
	}

	// A non-2xx status is a transport-level failure; the status decides the
	// code and any body is only diagnostic.
	if statusError := mapHTTPStatus(response.StatusCode, endpoint); statusError != nil {
		return result, statusError
	}

	var envelope map[string]any
	if !isJSONContentType(response.Header.Get("Content-Type")) || json.Unmarshal(bodyBytes, &envelope) != nil {
		return result, relay.NewError(
			relay.CodeProtocolError,
			"graphql endpoint did not return a JSON object",
		).WithDetails(map[string]any{"httpStatus": response.StatusCode, "endpoint": endpoint})
	}

	// A GraphQL errors array is a real failure even on HTTP 200, and we refuse
	// to return whatever data accompanied it. Silent partial data is worse than
	// an error an agent can act on.
	if messages := graphQLErrorMessages(envelope); len(messages) > 0 {
		return result, relay.NewError(
			relay.CodeRemoteError,
			fmt.Sprintf("graphql response reported %d error(s)", len(messages)),
		).WithDetails(map[string]any{"endpoint": endpoint, "graphqlErrors": messages})
	}

	if data, ok := envelope["data"]; ok {
		result.Body = data
		return result, nil
	}
	result.Body = envelope
	return result, nil
}

// graphQLErrorMessages extracts the human-readable messages from an errors
// member. It returns nil when the member is absent or an empty array.
func graphQLErrorMessages(envelope map[string]any) []string {
	raw, ok := envelope["errors"]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	messages := make([]string, 0, len(list))
	for _, item := range list {
		if object, ok := item.(map[string]any); ok {
			if message, ok := object["message"].(string); ok {
				messages = append(messages, message)
			}
		}
	}
	if len(messages) == 0 {
		// An errors array with no message is still an error; report the count.
		return []string{fmt.Sprintf("%d error(s) with no message", len(list))}
	}
	return messages
}

// mapHTTPStatus mirrors the REST executor's status mapping so both protocols
// surface the same codes (spec §26). A 401 becomes AUTH_REQUIRED; the daemon
// upgrades that to AUTH_FAILED when it injected a credential.
func mapHTTPStatus(statusCode int, endpoint string) error {
	if statusCode >= 200 && statusCode < 300 {
		return nil
	}

	details := map[string]any{"httpStatus": statusCode, "endpoint": endpoint}
	switch {
	case statusCode == http.StatusUnauthorized:
		return relay.NewError(relay.CodeAuthRequired, "authentication required").WithDetails(details)
	case statusCode == http.StatusForbidden:
		return relay.NewError(relay.CodePermissionDenied, "permission denied").WithDetails(details)
	case statusCode == http.StatusTooManyRequests:
		return relay.NewError(relay.CodeRateLimited, "rate limited").WithDetails(details)
	case statusCode >= 500:
		return relay.NewError(relay.CodeRemoteError, fmt.Sprintf("remote error: HTTP %d", statusCode)).WithDetails(details)
	default:
		return relay.NewError(relay.CodeRemoteError, fmt.Sprintf("HTTP %d", statusCode)).WithDetails(details)
	}
}

func copyHeaders(source http.Header) map[string][]string {
	headers := make(map[string][]string, len(source))
	for name, values := range source {
		headers[name] = values
	}
	return headers
}

func isJSONContentType(contentType string) bool {
	trimmed := strings.TrimSpace(contentType)
	return strings.HasPrefix(trimmed, "application/json") || strings.HasPrefix(trimmed, "text/json")
}
