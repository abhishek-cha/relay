package ipc

import "relay/pkg/relay"

// IPC frames for a browser tool's session (spec §23).
//
// A browser tool needs a logged-in session rather than a static token. The
// daemon owns the session — the cookies live under the Relay home and never
// cross IPC — so the CLI only asks the daemon to run the manifest's login flow
// or to forget the session. No frame carries a cookie, a token, or any other
// secret in either direction (spec §22): a login frame names a tool, and the
// daemon reads the credential from the Keychain and writes the resulting
// cookies to its own store.
//
// The frames live here beside the transport, following the skill and device
// frames: the daemon is the only speaker, while pkg/relay stays the public
// contract spoken by tool binaries.
const (
	// FrameSessionLogin asks the daemon to run a registered tool's declared
	// browser login and persist the session.
	FrameSessionLogin = "session_login"
	// FrameSessionClear asks the daemon to forget a registered tool's session.
	FrameSessionClear = "session_clear"
)

// SessionLoginRequest asks the daemon to run a tool's login flow. It names the
// tool and nothing else: the username and password live in the Keychain, and
// the login shape lives in the manifest the daemon reads (spec §3, §16, §22).
type SessionLoginRequest struct {
	Type string `json:"type"` // always FrameSessionLogin
	Tool string `json:"tool"`
}

// SessionLoginResponse reports that a session was established. It carries no
// cookie: the daemon stored it, and that store is the only place it lives
// (spec §22).
type SessionLoginResponse struct {
	Success  bool         `json:"success"`
	Error    *relay.Error `json:"error,omitempty"`
	Tool     string       `json:"tool,omitempty"`
	LoggedIn bool         `json:"loggedIn,omitempty"`
}

// SessionClearRequest asks the daemon to forget a tool's session.
type SessionClearRequest struct {
	Type string `json:"type"` // always FrameSessionClear
	Tool string `json:"tool"`
}

// SessionClearResponse reports that the session was cleared. Clearing is
// idempotent, so a tool with no session is reported as cleared.
type SessionClearResponse struct {
	Success bool         `json:"success"`
	Error   *relay.Error `json:"error,omitempty"`
	Tool    string       `json:"tool,omitempty"`
	Cleared bool         `json:"cleared,omitempty"`
}
