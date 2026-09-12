package ipc

import "relay/pkg/relay"

// IPC frames for the OAuth2 browser authorization-code login with PKCE —
// spec §21, §22, §54.
//
// The flow is deliberately two frames, matching the device grant's shape and its
// reasoning. auth_browser_start asks the daemon to begin an authorization and
// answers with only what a human must see: the authorization URL and the exact
// loopback redirect that was bound. The authorization URL is safe to display:
// the PKCE challenge and the state are public by design, because they travel
// through the browser, and neither is useful without the verifier, which stays
// in the daemon along with any token (spec §22). The CLI opens the URL; the
// human approves in a browser; the loopback listener the daemon owns receives
// the authorization code. The CLI then calls auth_browser_wait with the opaque
// flow handle, and the daemon completes the exchange, writes the token to the
// Keychain, and reports success or failure. No frame carries the code, the PKCE
// verifier, or a token in either direction, so the CLI never has one to
// mishandle.
//
// The frame kinds live here beside the transport, following the device and
// session frames: the daemon is the only speaker, while pkg/relay stays the
// public contract spoken by tool binaries.
const (
	FrameAuthBrowserStart = "auth_browser_start"
	FrameAuthBrowserWait  = "auth_browser_wait"
)

// AuthBrowserStartRequest asks the daemon to begin a browser authorization-code
// login for a registered tool.
type AuthBrowserStartRequest struct {
	Type string `json:"type"` // always FrameAuthBrowserStart
	Tool string `json:"tool"`
}

// AuthBrowserStartResponse carries the human-facing half of the authorization:
// the URL to open and the loopback redirect the daemon bound.
//
// The authorization URL is safe to display. The PKCE challenge and the state
// are public by design — they travel through the browser — and neither is
// useful without the verifier, which lives only in the daemon (spec §22). Flow
// is an opaque, single-use handle the CLI passes back to FrameAuthBrowserWait;
// it is not derived from anything secret and grants nothing on its own. The
// reply never includes the PKCE verifier, the authorization code, or the token.
type AuthBrowserStartResponse struct {
	Success     bool         `json:"success"`
	Error       *relay.Error `json:"error,omitempty"`
	Tool        string       `json:"tool,omitempty"`
	Flow        string       `json:"flow,omitempty"`        // opaque single-use handle; grants nothing on its own
	URL         string       `json:"url,omitempty"`         // the authorization URL
	RedirectURI string       `json:"redirectUri,omitempty"` // the exact loopback redirect that was bound
}

// AuthBrowserWaitRequest asks the daemon to finish a pending browser
// authorization. The flow handle is the one from AuthBrowserStartResponse.
type AuthBrowserWaitRequest struct {
	Type string `json:"type"` // always FrameAuthBrowserWait
	Flow string `json:"flow"`
}

// AuthBrowserWaitResponse reports that the credential was stored. It carries no
// token: the daemon wrote it to the Keychain, which is the only place it lives
// (spec §22).
type AuthBrowserWaitResponse struct {
	Success bool         `json:"success"`
	Error   *relay.Error `json:"error,omitempty"`
	Tool    string       `json:"tool,omitempty"`
	Stored  bool         `json:"stored,omitempty"`
}
