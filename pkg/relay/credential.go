package relay

// IPC frame kinds for credential management (spec §21, §22, §40).
//
// Credentials are daemon-owned and live in the Keychain. The CLI sends these
// frames; the daemon reads and writes the store and never returns a secret over
// IPC, so the CLI never has to handle one (spec §22).
const (
	FrameAuthSet    = "auth_set"
	FrameAuthClear  = "auth_clear"
	FrameAuthStatus = "auth_status"
)

// AuthSetRequest stores a credential for a tool.
//
// Type is the IPC frame kind ("auth_set"), matching every other request frame.
// AuthType is the credential's declared type; it is advisory because the tool's
// own manifest is authoritative for what that tool needs (spec §3), and the
// daemon rejects a contradiction rather than trusting the caller.
type AuthSetRequest struct {
	Type     string `json:"type"`               // always FrameAuthSet
	Tool     string `json:"tool"`               // tool name
	AuthType string `json:"authType,omitempty"` // api_key | bearer | basic | oauth2 | client_credentials
	Secret   string `json:"secret,omitempty"`   // the secret value; never echoed back
}

// AuthClearRequest removes a tool's stored credential.
type AuthClearRequest struct {
	Type string `json:"type"` // always FrameAuthClear
	Tool string `json:"tool"`
}

// AuthStatusRequest asks whether a credential exists and its declared type.
type AuthStatusRequest struct {
	Type string `json:"type"` // always FrameAuthStatus
	Tool string `json:"tool"`
}

// AuthStatusResponse reports the presence and declared type of a credential.
//
// The summary fields (AuthType, Provider, LoginKind, ExpiresAt, Scope,
// Refreshable) are non-secret metadata about the stored credential: what kind of
// login produced it, when it expires, which scopes it holds, and whether a
// refresh token is stored. They let the CLI describe the credential without ever
// seeing it. The value itself is never included (spec §22): the reply is built
// from a presence check, the tool's manifest, and stored non-secret metadata, so
// the secret is never read out even to be redacted.
type AuthStatusResponse struct {
	Success  bool   `json:"success"`
	Error    *Error `json:"error,omitempty"`
	Tool     string `json:"tool"`
	Stored   bool   `json:"stored"`             // true when a secret is in the Keychain
	AuthType string `json:"authType,omitempty"` // declared auth type (api_key, bearer, ...)
	Provider string `json:"provider,omitempty"` // declared provider (e.g. github)

	// The following are additive, non-secret summaries of the stored credential.
	// All are omitempty so a legacy reply keeps exactly its old wire shape and no
	// frame ever carries the credential value itself (spec §22).
	LoginKind   string `json:"loginKind,omitempty"`   // e.g. "oauth2 browser", "oauth2 device", "pasted token"; empty when unknown
	ExpiresAt   string `json:"expiresAt,omitempty"`   // RFC3339 UTC; empty when the server declared no expiry
	Scope       string `json:"scope,omitempty"`       // space-delimited scopes the credential was granted
	Refreshable bool   `json:"refreshable,omitempty"` // true when a refresh token is stored
}
