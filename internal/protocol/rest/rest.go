package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"relay/internal/protocol"
	"relay/pkg/relay"
)

// maxResponseBodyBytes is the hard cap on how much response body we will
// buffer. Anything larger is rejected to guard against hostile backends
// (spec §40).
const maxResponseBodyBytes = 10 << 20 // 10 MiB

// maxRetryAttempts is the default upper bound for automatic retries.
const maxRetryAttempts = 3

// defaultRetryBase is the initial backoff duration between retries.
const defaultRetryBase = 500 * time.Millisecond

// maxRetryAfter caps Retry-After durations to avoid blocking indefinitely.
const maxRetryAfter = 30 * time.Second

// Sleeper is a function that pauses execution for the given duration.
// Tests inject a no-op sleeper to avoid real sleeps.
type Sleeper func(d time.Duration)

// Executor is a REST protocol executor that satisfies protocol.Executor.
type Executor struct {
	// Client is the HTTP client used for all requests. If nil, a default
	// client with a 30-second timeout is used.
	Client *http.Client

	// Sleeper is called between retry attempts. If nil, a real time.Sleep
	// is used. Tests should inject a no-op.
	Sleeper Sleeper

	// MaxAttempts is the maximum number of attempts (including the
	// initial request) for retryable requests. Defaults to
	// maxRetryAttempts (3) when zero.
	MaxAttempts int
}

// New returns an Executor with sensible defaults.
func New() *Executor {
	return &Executor{
		Client: &http.Client{Timeout: 30 * time.Second},
		Sleeper: func(d time.Duration) {
			time.Sleep(d)
		},
		MaxAttempts: maxRetryAttempts,
	}
}

// idempotentMethods are the HTTP methods safe to retry automatically.
// POST is never retried because a retried POST can duplicate a side effect.
var idempotentMethods = map[string]bool{
	"GET":    true,
	"HEAD":   true,
	"PUT":    true,
	"DELETE": true,
}

// placeholderRe matches {name} placeholders in path and query templates.
var placeholderRe = regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)

// Execute runs a resolved REST operation against a backend.
func (e *Executor) Execute(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	spec := req.Spec
	method := strings.ToUpper(spec.Method)
	if method == "" {
		method = "GET"
	}

	// Resolve path.
	resolvedPath, err := resolvePath(spec.Path, req.Input)
	if err != nil {
		return protocol.Response{}, err
	}

	// Build URL. BaseURL takes precedence; Endpoint is the fallback.
	baseURL := spec.BaseURL
	if baseURL == "" {
		baseURL = spec.Endpoint
	}
	if baseURL == "" {
		return protocol.Response{}, relay.NewError(
			relay.CodeInvalidInput,
			"BaseURL or Endpoint must be set in the spec",
		).WithDetails(map[string]any{
			"method": method,
		})
	}

	resolvedURL, err := url.Parse(baseURL + resolvedPath)
	if err != nil {
		return protocol.Response{}, relay.NewError(
			relay.CodeInvalidInput,
			"failed to parse resolved URL: "+err.Error(),
		).WithDetails(map[string]any{
			"method": method,
		})
	}

	// Track which input keys have been consumed by path, query, or headers.
	consumedInputs := make(map[string]bool)
	for _, key := range placeholderRe.FindAllStringSubmatch(spec.Path, -1) {
		consumedInputs[key[1]] = true
	}

	// Resolve query parameters. A template naming a missing input simply
	// omits that parameter (all query params are treated as optional).
	q := resolvedURL.Query()
	for paramName, template := range spec.Query {
		resolved, missing := resolveOptional(template, req.Input)
		if missing {
			continue
		}
		q.Set(paramName, resolved)
		for _, key := range placeholderRe.FindAllStringSubmatch(template, -1) {
			consumedInputs[key[1]] = true
		}
	}
	resolvedURL.RawQuery = q.Encode()

	// Resolve headers.
	headers := make(http.Header)
	for name, template := range spec.Headers {
		resolved, missing := resolveOptional(template, req.Input)
		if missing {
			continue
		}
		headers.Set(name, resolved)
		for _, key := range placeholderRe.FindAllStringSubmatch(template, -1) {
			consumedInputs[key[1]] = true
		}
	}

	// Build body for methods that accept one.
	var bodyBytes []byte
	if methodHasBody(method) {
		bodyBytes, err = buildBody(req, consumedInputs)
		if err != nil {
			return protocol.Response{}, err
		}
		if bodyBytes != nil {
			if _, ok := headers["Content-Type"]; !ok {
				headers.Set("Content-Type", "application/json")
			}
		}
	}

	// Apply credential when present.
	if req.Credential != nil {
		applyCredential(headers, req.Credential)
	}

	// Execute with retries for idempotent methods on 429/5xx.
	maxAttempts := e.effectiveMaxAttempts()
	for attempt := 0; attempt < maxAttempts; attempt++ {
		resp, err := e.doRequest(ctx, method, resolvedURL.String(), headers, bodyBytes)
		if err != nil {
			if ctx.Err() != nil {
				return protocol.Response{}, relay.NewError(
					relay.CodeTimeout,
					"context deadline exceeded or cancelled",
				).WithDetails(map[string]any{
					"method": method,
					"url":    resolvedURL.String(),
				})
			}
			return protocol.Response{}, relay.NewError(
				relay.CodeNetworkError,
				"transport error: "+err.Error(),
			).WithDetails(map[string]any{
				"method": method,
				"url":    resolvedURL.String(),
			})
		}

		// Retry logic: only for idempotent methods on 429/5xx, and only
		// when we have remaining attempts.
		if isRetryableStatus(resp.StatusCode, method) && attempt < maxAttempts-1 {
			wait := retryAfterOrBackoff(resp, attempt)
			resp.Body.Close()
			e.sleep(wait)
			continue
		}

		defer resp.Body.Close()
		return readAndMapResponse(resp, method, resolvedURL.String())
	}

	// Should not be reached — the last iteration always returns.
	return protocol.Response{}, relay.NewError(
		relay.CodeRemoteError,
		"exhausted retries",
	).WithDetails(map[string]any{
		"method": method,
		"url":    resolvedURL.String(),
	})
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func (e *Executor) effectiveMaxAttempts() int {
	if e.MaxAttempts > 0 {
		return e.MaxAttempts
	}
	return maxRetryAttempts
}

func (e *Executor) sleep(d time.Duration) {
	if e.Sleeper != nil {
		e.Sleeper(d)
	} else {
		time.Sleep(d)
	}
}

func (e *Executor) doRequest(ctx context.Context, method, rawURL string, headers http.Header, bodyBytes []byte) (*http.Response, error) {
	client := e.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	var bodyReader io.Reader
	if bodyBytes != nil {
		bodyReader = strings.NewReader(string(bodyBytes))
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header = headers
	return client.Do(req)
}

// ---------------------------------------------------------------------------
// Path / template resolution
// ---------------------------------------------------------------------------

// resolvePath resolves {placeholders} in the path and URL-escapes each
// substituted value. Returns an INVALID_INPUT error if a required input
// is missing.
func resolvePath(template string, input map[string]any) (string, error) {
	matches := placeholderRe.FindAllStringSubmatch(template, -1)
	if len(matches) == 0 {
		return template, nil
	}

	result := template
	for _, match := range matches {
		fullMatch := match[0]
		key := match[1]
		val, ok := input[key]
		if !ok {
			return "", relay.NewError(
				relay.CodeInvalidInput,
				fmt.Sprintf("missing required input %q for path placeholder", key),
			)
		}
		strVal := fmt.Sprintf("%v", val)
		result = strings.ReplaceAll(result, fullMatch, url.PathEscape(strVal))
	}
	return result, nil
}

// resolveOptional resolves {placeholders} in a template string. If any
// placeholder names an input that was not supplied, missing is returned as
// true and the caller should skip the parameter entirely.
func resolveOptional(template string, input map[string]any) (string, bool) {
	matches := placeholderRe.FindAllStringSubmatch(template, -1)
	if len(matches) == 0 {
		return template, false
	}
	result := template
	for _, match := range matches {
		fullMatch := match[0]
		key := match[1]
		val, ok := input[key]
		if !ok {
			return "", true
		}
		strVal := fmt.Sprintf("%v", val)
		result = strings.ReplaceAll(result, fullMatch, strVal)
	}
	return result, false
}

// ---------------------------------------------------------------------------
// Body construction
// ---------------------------------------------------------------------------

func methodHasBody(method string) bool {
	switch method {
	case "POST", "PUT", "PATCH", "DELETE":
		return true
	default:
		return false
	}
}

// buildBody constructs the request body. If Spec.Body is non-nil, string
// values within it are template-resolved. Otherwise, unconsumed input keys
// are sent as a JSON object. Returns nil when there is nothing to send.
func buildBody(req protocol.Request, consumedInputs map[string]bool) ([]byte, error) {
	if req.Spec.Body != nil {
		return resolveBodyPlaceholders(req.Spec.Body, req.Input)
	}

	// Unconsumed input keys become the JSON body.
	unconsumed := make(map[string]any)
	for k, v := range req.Input {
		if !consumedInputs[k] {
			unconsumed[k] = v
		}
	}
	if len(unconsumed) == 0 {
		return nil, nil
	}
	return json.Marshal(unconsumed)
}

// resolveBodyPlaceholders walks a JSON-serialisable value and resolves
// {placeholder} tokens found in any string leaf.
func resolveBodyPlaceholders(body any, input map[string]any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, relay.NewError(relay.CodeInvalidInput, "failed to marshal Spec.Body")
	}

	resolved := placeholderRe.ReplaceAllStringFunc(string(raw), func(match string) string {
		key := match[1 : len(match)-1]
		val, ok := input[key]
		if !ok {
			return match // leave unresolved — the literal appears in JSON
		}
		// Re-escape for JSON embedding.
		b, _ := json.Marshal(fmt.Sprintf("%v", val))
		// b includes surrounding quotes; strip them for raw insertion.
		s := string(b)
		if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
			return s[1 : len(s)-1]
		}
		return s
	})

	var check any
	if err := json.Unmarshal([]byte(resolved), &check); err != nil {
		return nil, relay.NewError(
			relay.CodeInvalidInput,
			"resolved Spec.Body is not valid JSON after placeholder substitution",
		)
	}
	return []byte(resolved), nil
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

func applyCredential(headers http.Header, cred *protocol.Credential) {
	headerName := cred.Header
	if headerName == "" {
		headerName = "Authorization"
	}
	value := cred.Secret
	if cred.Scheme != "" {
		value = cred.Scheme + " " + cred.Secret
	}
	headers.Set(headerName, value)
}

// ---------------------------------------------------------------------------
// Response handling
// ---------------------------------------------------------------------------

func readAndMapResponse(resp *http.Response, method, resolvedURL string) (protocol.Response, error) {
	limitedReader := io.LimitReader(resp.Body, maxResponseBodyBytes+1)
	bodyBytes, err := io.ReadAll(limitedReader)
	if err != nil {
		return protocol.Response{}, relay.NewError(
			relay.CodeNetworkError,
			"failed to read response body",
		).WithDetails(map[string]any{
			"httpStatus": resp.StatusCode,
			"method":     method,
			"url":        resolvedURL,
		})
	}
	if int64(len(bodyBytes)) > maxResponseBodyBytes {
		return protocol.Response{}, relay.NewError(
			relay.CodeRemoteError,
			"response body exceeds maximum allowed size",
		).WithDetails(map[string]any{
			"httpStatus": resp.StatusCode,
			"method":     method,
			"url":        resolvedURL,
		})
	}

	// Copy response headers.
	responseHeaders := make(map[string][]string)
	for name, values := range resp.Header {
		responseHeaders[name] = values
	}

	result := protocol.Response{
		Status:  resp.StatusCode,
		Headers: responseHeaders,
	}

	// Parse JSON when Content-Type indicates JSON.
	if isJSONContentType(resp.Header.Get("Content-Type")) {
		var jsonBody any
		if err := json.Unmarshal(bodyBytes, &jsonBody); err == nil {
			result.Body = jsonBody
			return result, mapHTTPStatus(resp.StatusCode, method, resolvedURL)
		}
	}

	// Non-JSON or malformed JSON: return raw string.
	result.Body = string(bodyBytes)
	return result, mapHTTPStatus(resp.StatusCode, method, resolvedURL)
}

func isJSONContentType(ct string) bool {
	ct = strings.TrimSpace(ct)
	return strings.HasPrefix(ct, "application/json") || strings.HasPrefix(ct, "text/json")
}

// ---------------------------------------------------------------------------
// Error mapping
// ---------------------------------------------------------------------------

func mapHTTPStatus(statusCode int, method, resolvedURL string) error {
	if statusCode >= 200 && statusCode < 300 {
		return nil
	}

	details := map[string]any{
		"httpStatus": statusCode,
		"method":     method,
		"url":        resolvedURL,
	}

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

// ---------------------------------------------------------------------------
// Retry helpers
// ---------------------------------------------------------------------------

func isRetryableStatus(statusCode int, method string) bool {
	if !idempotentMethods[method] {
		return false
	}
	return statusCode == http.StatusTooManyRequests || statusCode >= 500
}

func retryAfterOrBackoff(resp *http.Response, attempt int) time.Duration {
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.ParseFloat(ra, 64); err == nil {
			d := time.Duration(secs * float64(time.Second))
			if d > maxRetryAfter {
				d = maxRetryAfter
			}
			if d > 0 {
				return d
			}
		}
		// HTTP-date parsing not needed for MVP; fall through to backoff.
	}
	return exponentialBackoff(attempt)
}

func exponentialBackoff(attempt int) time.Duration {
	d := time.Duration(float64(defaultRetryBase) * math.Pow(2, float64(attempt)))
	if d > maxRetryAfter {
		d = maxRetryAfter
	}
	return d
}
