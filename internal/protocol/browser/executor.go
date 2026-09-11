package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"relay/internal/browser"
	"relay/internal/protocol"
	"relay/pkg/relay"
)

// maxResponseBodyBytes is the hard cap on a response body, matching the daemon's
// IPC frame limit, the MCP frame limit, and the local executor's output limit
// (8 MiB). A larger body is refused rather than buffered (spec §40).
const maxResponseBodyBytes = 8 << 20

// Executor runs browser operations: HTTP requests sent with a tool's stored
// session. It satisfies protocol.Executor and is safe for concurrent use; the
// underlying session store serializes access to each tool's jar.
type Executor struct {
	sessions *browser.Sessions
}

// New returns an Executor over the given session layer.
func New(sessions *browser.Sessions) *Executor {
	return &Executor{sessions: sessions}
}

// Login runs the tool's declared login flow. The daemon calls it for the
// `session_login` IPC frame. It is a thin forward so the daemon can hold the
// one object it registered as the `browser` executor.
func (e *Executor) Login(ctx context.Context, tool string, manifestYAML []byte) *relay.Error {
	return e.sessions.Login(ctx, tool, manifestYAML)
}

// Clear removes the tool's stored session. The daemon calls it for the
// `session_clear` IPC frame.
func (e *Executor) Clear(tool string) *relay.Error {
	return e.sessions.Clear(tool)
}

// Execute runs one resolved browser operation against a backend using the
// tool's session.
//
// The request shape is the REST shape — method, path, query, headers, body, with
// `{placeholder}` substitution from the operation input — so only the transport
// beneath it differs (spec §19, §23). The daemon's credential is ignored: a
// browser tool authenticates with its session, and a stored secret is used by
// the login flow, never attached to an operation.
func (e *Executor) Execute(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	spec := req.Spec
	method := strings.ToUpper(spec.Method)
	if method == "" {
		method = http.MethodGet
	}

	resolvedPath, err := resolvePath(spec.Path, req.Input)
	if err != nil {
		return protocol.Response{}, err
	}

	baseURL := spec.BaseURL
	if baseURL == "" {
		baseURL = spec.Endpoint
	}
	if baseURL == "" {
		return protocol.Response{}, relay.NewError(
			relay.CodeInvalidInput,
			"BaseURL or Endpoint must be set in the spec").
			WithDetails(map[string]any{"method": method})
	}

	resolvedURL, err := url.Parse(baseURL + resolvedPath)
	if err != nil {
		return protocol.Response{}, relay.NewError(
			relay.CodeInvalidInput,
			"failed to parse resolved URL: "+err.Error()).
			WithDetails(map[string]any{"method": method})
	}

	consumed := consumedInputs(spec, req.Input)
	query := resolvedURL.Query()
	for name, template := range spec.Query {
		resolved, missing := resolveOptional(template, req.Input)
		if missing {
			continue
		}
		query.Set(name, resolved)
	}
	resolvedURL.RawQuery = query.Encode()

	headers := make(http.Header)
	for name, template := range spec.Headers {
		resolved, missing := resolveOptional(template, req.Input)
		if missing {
			continue
		}
		headers.Set(name, resolved)
	}

	var bodyBytes []byte
	if methodHasBody(method) {
		bodyBytes, err = buildBody(req, consumed)
		if err != nil {
			return protocol.Response{}, err
		}
		if bodyBytes != nil {
			if _, ok := headers["Content-Type"]; !ok {
				headers.Set("Content-Type", "application/json")
			}
		}
	}

	session, err := e.sessions.Store().Jar(req.Tool)
	if err != nil {
		return protocol.Response{}, relay.NewError(relay.CodeRemoteError, err.Error())
	}
	if !session.HasCookies() {
		return protocol.Response{}, relay.NewError(
			relay.CodeAuthRequired,
			fmt.Sprintf("%s has no browser session; run 'relay session login %s'", req.Tool, req.Tool)).
			WithDetails(map[string]any{"protocol": "browser", "operation": req.Operation})
	}

	var bodyReader io.Reader
	if bodyBytes != nil {
		bodyReader = strings.NewReader(string(bodyBytes))
	}
	request, err := http.NewRequestWithContext(ctx, method, resolvedURL.String(), bodyReader)
	if err != nil {
		return protocol.Response{}, relay.NewError(
			relay.CodeInvalidInput,
			"failed to build the request: "+err.Error()).
			WithDetails(map[string]any{"method": method, "url": resolvedURL.String()})
	}
	request.Header = headers

	response, err := e.sessions.HTTPClient(session).Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return protocol.Response{}, relay.NewError(
				relay.CodeTimeout,
				"context deadline exceeded or cancelled").
				WithDetails(map[string]any{"method": method, "url": resolvedURL.String()})
		}
		return protocol.Response{}, relay.NewError(
			relay.CodeNetworkError,
			"transport error: "+err.Error()).
			WithDetails(map[string]any{"method": method, "url": resolvedURL.String()})
	}
	defer response.Body.Close()

	mapped, failure := readAndMapResponse(response, method, resolvedURL.String())
	// A response may rotate the session cookie; persist it even when the status
	// is an error, since a service commonly re-issues a session alongside a 4xx.
	// Persistence is best-effort: failing an operation because the session file
	// could not be written would turn a disk problem into a service failure.
	if saveErr := session.Save(); saveErr != nil {
		_ = saveErr
	}
	if failure != nil {
		return protocol.Response{}, failure
	}
	return mapped, nil
}

// consumedInputs marks the input keys the path and header templates already
// consumed, so they are not also sent in the JSON body.
func consumedInputs(spec protocol.Spec, input map[string]any) map[string]bool {
	consumed := map[string]bool{}
	for _, match := range placeholderRe.FindAllStringSubmatch(spec.Path, -1) {
		consumed[match[1]] = true
	}
	for _, template := range spec.Query {
		for _, match := range placeholderRe.FindAllStringSubmatch(template, -1) {
			consumed[match[1]] = true
		}
	}
	for _, template := range spec.Headers {
		for _, match := range placeholderRe.FindAllStringSubmatch(template, -1) {
			consumed[match[1]] = true
		}
	}
	return consumed
}

// readAndMapResponse reads a capped body and maps the status onto the shared
// taxonomy. A 401 means the stored session was refused, which is AUTH_REQUIRED:
// the remedy is to log in again (spec §23, §26).
func readAndMapResponse(response *http.Response, method, resolvedURL string) (protocol.Response, *relay.Error) {
	bodyBytes, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBodyBytes+1))
	if err != nil {
		return protocol.Response{}, relay.NewError(
			relay.CodeNetworkError,
			"failed to read response body").
			WithDetails(map[string]any{"httpStatus": response.StatusCode, "method": method, "url": resolvedURL})
	}
	if int64(len(bodyBytes)) > maxResponseBodyBytes {
		return protocol.Response{}, relay.NewError(
			relay.CodeRemoteError,
			"response body exceeds maximum allowed size").
			WithDetails(map[string]any{"httpStatus": response.StatusCode, "method": method, "url": resolvedURL})
	}

	headers := make(map[string][]string, len(response.Header))
	for name, values := range response.Header {
		headers[name] = values
	}
	result := protocol.Response{Status: response.StatusCode, Headers: headers}

	if isJSONContentType(response.Header.Get("Content-Type")) {
		var parsed any
		if json.Unmarshal(bodyBytes, &parsed) == nil {
			result.Body = parsed
			return result, mapStatus(response.StatusCode, method, resolvedURL)
		}
	}
	result.Body = string(bodyBytes)
	return result, mapStatus(response.StatusCode, method, resolvedURL)
}

// mapStatus maps a non-2xx status onto the structured error model.
func mapStatus(status int, method, resolvedURL string) *relay.Error {
	if status >= 200 && status < 300 {
		return nil
	}
	details := map[string]any{"httpStatus": status, "method": method, "url": resolvedURL}
	switch {
	case status == http.StatusUnauthorized:
		return relay.NewError(relay.CodeAuthRequired, "authentication required").WithDetails(details)
	case status == http.StatusForbidden:
		return relay.NewError(relay.CodePermissionDenied, "permission denied").WithDetails(details)
	case status == http.StatusTooManyRequests:
		return relay.NewError(relay.CodeRateLimited, "rate limited").WithDetails(details)
	case status >= 500:
		return relay.NewError(relay.CodeRemoteError, fmt.Sprintf("remote error: HTTP %d", status)).WithDetails(details)
	default:
		return relay.NewError(relay.CodeRemoteError, fmt.Sprintf("HTTP %d", status)).WithDetails(details)
	}
}
