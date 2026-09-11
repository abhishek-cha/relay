package ipc

import "relay/pkg/relay"

// IPC frames for the OAuth2 device authorization grant (RFC 8628) — spec §21,
// §54.
//
// The flow is deliberately two frames. auth_device_start asks the daemon to
// begin an authorization and answers with only what a human must see: the user
// code and the verification URI. The device code itself stays in the daemon —
// possession of it authorizes the token request (RFC 8628 §5.2), so it is a
// secret and never crosses IPC (spec §22). The CLI prints the code and calls
// auth_device_wait with the opaque flow handle; the daemon polls the
// authorization server, writes the resulting token to the Keychain, and reports
// success or failure. No frame carries the token in either direction, so the
// CLI never has one to mishandle.
//
// The frame kinds live here beside the transport, following the skill frame:
// the daemon is the only speaker, while pkg/relay stays the public contract
// spoken by tool binaries.
const (
	FrameAuthDeviceStart = "auth_device_start"
	FrameAuthDeviceWait  = "auth_device_wait"
)

// AuthDeviceStartRequest asks the daemon to begin a device authorization for a
// registered tool.
type AuthDeviceStartRequest struct {
	Type string `json:"type"` // always FrameAuthDeviceStart
	Tool string `json:"tool"`
}

// AuthDeviceStartResponse carries the human-facing half of the authorization:
// the code to type and where to type it. Flow is an opaque, single-use handle
// the CLI passes back to FrameAuthDeviceWait; it is not derived from the device
// code and grants nothing on its own.
//
// The reply never includes the device code, the token, or any other secret
// (spec §22, §54).
type AuthDeviceStartResponse struct {
	Success                 bool         `json:"success"`
	Error                   *relay.Error `json:"error,omitempty"`
	Tool                    string       `json:"tool,omitempty"`
	Flow                    string       `json:"flow,omitempty"`
	UserCode                string       `json:"userCode,omitempty"`
	VerificationURI         string       `json:"verificationUri,omitempty"`
	VerificationURIComplete string       `json:"verificationUriComplete,omitempty"`
	ExpiresIn               int          `json:"expiresIn,omitempty"` // seconds the code stays valid
	Interval                int          `json:"interval,omitempty"`  // seconds the server asks to be polled at
}

// AuthDeviceWaitRequest asks the daemon to finish a pending authorization. The
// flow handle is the one from AuthDeviceStartResponse.
type AuthDeviceWaitRequest struct {
	Type string `json:"type"` // always FrameAuthDeviceWait
	Flow string `json:"flow"`
}

// AuthDeviceWaitResponse reports that the credential was stored. It carries no
// token: the daemon wrote it to the Keychain, which is the only place it lives
// (spec §22).
type AuthDeviceWaitResponse struct {
	Success bool         `json:"success"`
	Error   *relay.Error `json:"error,omitempty"`
	Tool    string       `json:"tool,omitempty"`
	Stored  bool         `json:"stored,omitempty"`
}
