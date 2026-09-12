package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"relay/internal/auth"
	"relay/internal/ipc"
	"relay/pkg/relay"
)

// ─── OAuth2 browser authorization-code login with PKCE — spec §21, §22, §23, §54
//
// The daemon drives this flow for the same reason it drives the device grant:
// it owns credentials (spec §22). The CLI asks for an authorization and gets
// back only what a human must see — the URL to open and the loopback redirect
// the daemon bound. The PKCE verifier, the authorization code, and any token
// stay in this process, so the CLI never holds one to store or print even by
// accident.
//
// The manifest is machine truth for the endpoints (spec §3, §16), so they are
// read from the installed binary exactly as the device flow reads them. When a
// manifest omits auth.redirectURI, the auth client binds an ephemeral loopback
// port and reports the redirect it actually bound; the daemon never picks a port.

// pendingBrowserLogin is one in-flight browser authorization, held between the
// start and wait frames. It owns the loopback listener and the one-shot secrets
// (the PKCE verifier and the state) that must not cross IPC (spec §22).
//
// expires bounds the login in wall-clock time and is checked by the sweep below.
type pendingBrowserLogin struct {
	tool    string
	spec    auth.BrowserFlowSpec
	flow    *auth.BrowserFlow
	expires time.Time
}

// browserLogins holds authorizations no credential has been issued for yet. The
// daemon owns exactly one Relay home in one process (spec §3.3), so process-wide
// state is the honest scope for a login that has nothing on disk to live in.
var (
	browserLoginsMu sync.Mutex
	browserLogins   = map[string]*pendingBrowserLogin{}
)

// browserLoginLifetime bounds how long an unfinished browser login may pin its
// loopback listener. The browser flow has no server-declared expires_in, unlike
// the device grant, so the daemon supplies the deadline: a human who walks away
// from a half-finished login must not pin a listener for the daemon's lifetime.
const browserLoginLifetime = 10 * time.Minute

// browserFlowClient builds the authorization-code client. It is a seam: tests
// replace it so the loopback listener and the clock can be driven directly.
var browserFlowClient = func() *auth.BrowserClient {
	return auth.NewBrowserClient(auth.BrowserOptions{})
}

// authBrowserStart begins a browser authorization and replies with the
// human-facing half of it: the URL to open and the loopback redirect that was
// bound (spec §21, §54).
//
// The URL is safe to display: it carries the PKCE challenge and the state,
// which are public by design because they travel through the browser, and
// neither is useful without the verifier, which stays in this process along
// with any token. A tool whose manifest declares oauth2 without a browser
// authorization endpoint is answered with a structured "fall back" failure,
// which is what keeps every existing manifest working unchanged (spec §59).
func (d *Daemon) authBrowserStart(ctx context.Context, request ipc.AuthBrowserStartRequest) ipc.AuthBrowserStartResponse {
	if strings.TrimSpace(request.Tool) == "" {
		return failedBrowserStart(relay.NewError(relay.CodeInvalidInput, "no tool was named"))
	}
	installation, doc, failure := d.authTarget(ctx, request.Tool)
	if failure != nil {
		return failedBrowserStart(failure)
	}
	declared, ok := auth.Declared(doc)
	if !ok || auth.Canonical(declared.Type) != "oauth2" {
		return failedBrowserStart(relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("tool %q does not declare oauth2 auth; there is no browser login for it", request.Tool)))
	}

	// The manifest is machine truth for the endpoints (spec §3, §21). The
	// additive browser fields are read from the manifest the binary serves, the
	// same source readManifest trusts, rather than from a cached copy.
	raw, failure := d.run(ctx, installation.Path, "--manifest")
	if failure != nil {
		return failedBrowserStart(failure)
	}
	spec, browserFlow, failure := auth.BrowserFlowSpecFromManifest(raw)
	if failure != nil {
		return failedBrowserStart(failure)
	}
	if !browserFlow {
		// Backward compatible by design: oauth2 with no authorization endpoint
		// keeps meaning "a pasted token" or the device grant, so the CLI falls
		// back to those paths when it sees this marker.
		return failedBrowserStart(relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("tool %q declares oauth2 without a browser authorization endpoint", request.Tool)).
			WithDetails(map[string]any{"browserFlow": false}))
	}
	spec.Tool = installation.Name

	// Begin binds the loopback listener before minting state and the PKCE
	// challenge, so the redirect URI the reply carries names the port that
	// actually got bound.
	flow, failure := browserFlowClient().Begin(ctx, spec)
	if failure != nil {
		return failedBrowserStart(failure)
	}
	handle := newBrowserFlowID()
	browserLoginsMu.Lock()
	sweepBrowserLoginsLocked(d.now())
	browserLogins[handle] = &pendingBrowserLogin{
		tool:    installation.Name,
		spec:    spec,
		flow:    flow,
		expires: d.now().Add(browserLoginLifetime),
	}
	browserLoginsMu.Unlock()

	return ipc.AuthBrowserStartResponse{
		Success:     true,
		Tool:        installation.Name,
		Flow:        handle,
		URL:         flow.URL(),
		RedirectURI: flow.RedirectURI(),
	}
}

// authBrowserWait serves the loopback callback, exchanges the authorization
// code, then stores the token in the Keychain and reports only that it is stored
// (spec §22).
//
// The flow handle is single-use: it is removed before the callback is served,
// so two callers cannot finish the same authorization, and a handle that is
// lost is restarted rather than replayed.
func (d *Daemon) authBrowserWait(ctx context.Context, request ipc.AuthBrowserWaitRequest) ipc.AuthBrowserWaitResponse {
	if strings.TrimSpace(request.Flow) == "" {
		return failedBrowserWait(relay.NewError(relay.CodeInvalidInput, "no browser login was named"))
	}
	browserLoginsMu.Lock()
	sweepBrowserLoginsLocked(d.now())
	record, ok := browserLogins[request.Flow]
	delete(browserLogins, request.Flow)
	browserLoginsMu.Unlock()
	if !ok {
		return failedBrowserWait(relay.NewError(relay.CodeAuthFailed,
			"no browser login is pending for that handle; start the login again"))
	}
	// Release the loopback listener on every path, including a failed Wait, so
	// an abandoned or rejected login never leaks the port it bound.
	defer func() { _ = record.flow.Close() }()

	token, failure := record.flow.Wait(ctx)
	if failure != nil {
		return failedBrowserWait(failure)
	}
	// The token goes straight to the Keychain; it is never logged, echoed, or
	// included in a reply (spec §22, §54). A token that came with a refresh
	// token is written as the envelope, which is what carries the refresh token,
	// expiry, and endpoint context a later background refresh needs.
	encoded, failure := encodeLoginCredential(*token, auth.FlowBrowser,
		record.spec.TokenEndpoint, record.spec.ClientID)
	if failure != nil {
		return failedBrowserWait(failure)
	}
	if failure := d.resolver.Set(record.tool, encoded); failure != nil {
		return failedBrowserWait(failure)
	}
	return ipc.AuthBrowserWaitResponse{Success: true, Tool: record.tool, Stored: true}
}

// sweepBrowserLoginsLocked drops logins that can no longer complete, so a CLI
// killed mid-login does not leave listeners bound for the daemon's lifetime.
// Every dropped entry has its flow closed, or the listener leaks. The caller
// must hold browserLoginsMu.
func sweepBrowserLoginsLocked(now time.Time) {
	for handle, record := range browserLogins {
		if now.After(record.expires) {
			_ = record.flow.Close()
			delete(browserLogins, handle)
		}
	}
}

// newBrowserFlowID mints an opaque handle for one pending authorization. It is
// not derived from the authorization URL, the state, or the verifier, and
// carries no authority; it only spares the CLI from ever holding one of those.
func newBrowserFlowID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("browser-login-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}

// failedBrowserStart and failedBrowserWait keep the reply constructors in one
// place, mirroring failedDeviceStart and failedDeviceWait.
func failedBrowserStart(err *relay.Error) ipc.AuthBrowserStartResponse {
	return ipc.AuthBrowserStartResponse{Success: false, Error: err}
}

func failedBrowserWait(err *relay.Error) ipc.AuthBrowserWaitResponse {
	return ipc.AuthBrowserWaitResponse{Success: false, Error: err}
}
