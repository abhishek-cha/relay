// Package relay holds Relay's public, language-neutral contracts: the stable
// wire types shared by the CLI, the daemon, and the MCP adapter, plus the
// structured error model that both surfaces report (spec §26).
//
// Keep this package free of runtime dependencies. It is the part of Relay that
// other languages and tools are expected to speak, so it must stay stable.
package relay

// Code is a stable, machine-readable error code. Agents branch on these values,
// so once a code ships it must not change meaning.
type Code string

// Error codes from spec §26.
const (
	CodeInvalidInput        Code = "INVALID_INPUT"
	CodeToolNotFound        Code = "TOOL_NOT_FOUND"
	CodeOperationNotFound   Code = "OPERATION_NOT_FOUND"
	CodeAuthRequired        Code = "AUTH_REQUIRED"
	CodeAuthFailed          Code = "AUTH_FAILED"
	CodePermissionDenied    Code = "PERMISSION_DENIED"
	CodeNetworkError        Code = "NETWORK_ERROR"
	CodeTimeout             Code = "TIMEOUT"
	CodeRateLimited         Code = "RATE_LIMITED"
	CodeRemoteError         Code = "REMOTE_ERROR"
	CodeProtocolError       Code = "PROTOCOL_ERROR"
	CodeRuntimeIncompatible Code = "RUNTIME_INCOMPATIBLE"
)

// Error is the single error shape returned over IPC, printed by the CLI, and
// surfaced through MCP. Predictable errors are what let an agent recover, so
// every failure path must produce one of these rather than a bare string.
type Error struct {
	Code      Code           `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// NewError builds a structured error, defaulting Retryable from the code.
func NewError(code Code, message string) *Error {
	return &Error{Code: code, Message: message, Retryable: defaultRetryable[code]}
}

// WithDetails attaches non-sensitive context (never credentials or payloads).
func (e *Error) WithDetails(details map[string]any) *Error {
	e.Details = details
	return e
}

var defaultRetryable = map[Code]bool{
	CodeNetworkError: true,
	CodeTimeout:      true,
	CodeRateLimited:  true,
}
