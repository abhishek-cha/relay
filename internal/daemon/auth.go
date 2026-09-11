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
	"relay/internal/manifest"
	"relay/pkg/relay"
)

// authTarget resolves a tool name to its registry record and manifest.
//
// Both the status and set verbs need the declared auth type, and the manifest —
// not the registry — is machine truth for it (spec §3, §16), so it is read from
// the installed binary exactly as execution does.
func (d *Daemon) authTarget(ctx context.Context, name string) (relay.Installation, *manifest.Document, *relay.Error) {
	installation, err := d.store.Get(name)
	if err != nil {
		return relay.Installation{}, nil, unknownTool(err, name)
	}
	doc, failure := d.readManifest(ctx, installation.Path)
	if failure != nil {
		return relay.Installation{}, nil, failure
	}
	return installation, doc, nil
}

// authSet stores a credential for a registered tool (spec §21, §22).
//
// The manifest decides what the tool needs; a request that names a different
// auth type is rejected rather than silently stored under the wrong assumption.
// The secret goes straight into the Keychain and is never logged or returned.
func (d *Daemon) authSet(ctx context.Context, request relay.AuthSetRequest) relay.MutationResponse {
	if request.Tool == "" {
		return failedMutation(relay.NewError(relay.CodeInvalidInput, "no tool was named"))
	}
	installation, doc, failure := d.authTarget(ctx, request.Tool)
	if failure != nil {
		return failedMutation(failure)
	}
	declared, ok := auth.Declared(doc)
	if !ok {
		return failedMutation(relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("tool %q declares no auth; there is no credential to store", request.Tool)))
	}
	if request.AuthType != "" && auth.Canonical(request.AuthType) != auth.Canonical(declared.Type) {
		return failedMutation(relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("tool %q declares auth type %q, not %q",
				request.Tool, declared.Type, request.AuthType)))
	}
	if failure := d.resolver.Set(installation.Name, request.Secret); failure != nil {
		return failedMutation(failure)
	}
	return relay.MutationResponse{
		Success: true,
		Tool:    installation.Name,
		Message: fmt.Sprintf("stored %s credential for %s", declared.Type, installation.Name),
	}
}

// authClear removes a tool's stored credential. Clearing is idempotent: a tool
// with no stored credential is reported as cleared, because that is the state
// the caller asked for.
func (d *Daemon) authClear(request relay.AuthClearRequest) relay.MutationResponse {
	if request.Tool == "" {
		return failedMutation(relay.NewError(relay.CodeInvalidInput, "no tool was named"))
	}
	installation, err := d.store.Get(request.Tool)
	if err != nil {
		return failedMutation(unknownTool(err, request.Tool))
	}
	if failure := d.resolver.Clear(installation.Name); failure != nil {
		return failedMutation(failure)
	}
	return relay.MutationResponse{
		Success: true,
		Tool:    installation.Name,
		Message: fmt.Sprintf("cleared the credential for %s", installation.Name),
	}
}

// authStatus reports whether a credential is stored and what the tool declares.
//
// It reports presence as a boolean and the declared type from the manifest; the
// secret is never read out, let alone returned, so there is nothing to leak
// (spec §22).
func (d *Daemon) authStatus(ctx context.Context, request relay.AuthStatusRequest) relay.AuthStatusResponse {
	if request.Tool == "" {
		return failedAuthStatus(relay.NewError(relay.CodeInvalidInput, "no tool was named"))
	}
	installation, doc, failure := d.authTarget(ctx, request.Tool)
	if failure != nil {
		return failedAuthStatus(failure)
	}
	stored, failure := d.resolver.Present(installation.Name)
	if failure != nil {
		return failedAuthStatus(failure)
	}
	response := relay.AuthStatusResponse{
		Success: true,
		Tool:    installation.Name,
		Stored:  stored,
	}
	if declared, ok := auth.Declared(doc); ok {
		response.AuthType = declared.Type
		response.Provider = declared.Provider
	}
	return response
}

// failedAuthStatus mirrors the other failed* constructors for the auth status
// reply, which has its own response type.
func failedAuthStatus(err *relay.Error) relay.AuthStatusResponse {
	return relay.AuthStatusResponse{Success: false, Error: err}
}

// ─── OAuth2 device authorization grant (RFC 8628) — spec §21, §54 ───────────
//
// The daemon drives this flow because it owns credentials (spec §22). The CLI
// asks for an authorization, shows the user code, and waits; the device code
// and the token never leave this process, so the CLI has nothing to store or
// print even by accident.

// pendingDeviceLogin is one in-flight authorization, held between the start and
// wait frames. The device code is a secret — possession of it authorizes the
// token request (RFC 8628 §5.2) — which is exactly why it is held here rather
// than handed to the CLI.
//
// expires is wall-clock bookkeeping for the sweep below. The deadline the flow
// actually enforces is the authorization's own expires_in, applied by the
// polling client, so a stalled login cannot outlive the code's validity.
type pendingDeviceLogin struct {
	tool          string
	spec          auth.DeviceFlowSpec
	authorization auth.DeviceAuthorization
	expires       time.Time
}

// deviceLogins holds authorizations no credential has been issued for yet. The
// daemon owns exactly one Relay home in one process (spec §3.3), so
// process-wide state is the honest scope for a login that has nothing on disk
// to live in.
var (
	deviceLoginsMu sync.Mutex
	deviceLogins   = map[string]*pendingDeviceLogin{}
)

// deviceFlowClient builds the RFC 8628 polling client. It is a seam: tests
// replace it so the polling clock can be driven instead of waited on.
var deviceFlowClient = func() *auth.DeviceClient {
	return auth.NewDeviceClient(auth.DeviceOptions{})
}

// authDeviceStart begins a device authorization and replies with the user-facing
// half of it: the code to type and where to type it (spec §21, §54).
//
// The device code goes into the pending map, never into the reply. A tool whose
// manifest declares oauth2 without device endpoints is answered with a
// structured "this is the pasted-token path" failure, which is what keeps
// existing manifests working unchanged.
func (d *Daemon) authDeviceStart(ctx context.Context, request ipc.AuthDeviceStartRequest) ipc.AuthDeviceStartResponse {
	if strings.TrimSpace(request.Tool) == "" {
		return failedDeviceStart(relay.NewError(relay.CodeInvalidInput, "no tool was named"))
	}
	installation, doc, failure := d.authTarget(ctx, request.Tool)
	if failure != nil {
		return failedDeviceStart(failure)
	}
	declared, ok := auth.Declared(doc)
	if !ok || auth.Canonical(declared.Type) != "oauth2" {
		return failedDeviceStart(relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("tool %q does not declare oauth2 auth; there is no device login for it", request.Tool)))
	}

	// The manifest is machine truth for the endpoints (spec §3, §21). The
	// additive device fields are read from the manifest the binary serves, which
	// is the same source readManifest trusts, rather than from a cached copy.
	raw, failure := d.run(ctx, installation.Path, "--manifest")
	if failure != nil {
		return failedDeviceStart(failure)
	}
	spec, deviceFlow, failure := auth.DeviceFlowSpecFromManifest(raw)
	if failure != nil {
		return failedDeviceStart(failure)
	}
	if !deviceFlow {
		// Backward compatible by design: oauth2 with no device endpoints keeps
		// meaning "a human pastes a token", so the caller falls back to that.
		return failedDeviceStart(relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("tool %q declares oauth2 with a pasted token, not a device flow", request.Tool)).
			WithDetails(map[string]any{"deviceFlow": false}))
	}
	spec.Tool = installation.Name

	authorization, failure := deviceFlowClient().Start(ctx, spec)
	if failure != nil {
		return failedDeviceStart(failure)
	}
	flow := newDeviceFlowID()
	deviceLoginsMu.Lock()
	sweepDeviceLoginsLocked(time.Now())
	deviceLogins[flow] = &pendingDeviceLogin{
		tool:          installation.Name,
		spec:          spec,
		authorization: authorization,
		expires:       time.Now().Add(authorization.ExpiresIn),
	}
	deviceLoginsMu.Unlock()

	return ipc.AuthDeviceStartResponse{
		Success:                 true,
		Tool:                    installation.Name,
		Flow:                    flow,
		UserCode:                authorization.UserCode,
		VerificationURI:         authorization.VerificationURI,
		VerificationURIComplete: authorization.VerificationURIComplete,
		ExpiresIn:               int(authorization.ExpiresIn / time.Second),
		Interval:                int(authorization.Interval / time.Second),
	}
}

// authDeviceWait polls until the user finishes authorizing, then stores the
// token in the Keychain and reports only that it is stored (spec §22).
//
// The flow handle is single-use: it is removed before the first poll, so two
// callers cannot poll the same authorization, and a handle that is lost is
// restarted rather than replayed.
func (d *Daemon) authDeviceWait(ctx context.Context, request ipc.AuthDeviceWaitRequest) ipc.AuthDeviceWaitResponse {
	if strings.TrimSpace(request.Flow) == "" {
		return failedDeviceWait(relay.NewError(relay.CodeInvalidInput, "no device login was named"))
	}
	deviceLoginsMu.Lock()
	sweepDeviceLoginsLocked(time.Now())
	record, ok := deviceLogins[request.Flow]
	delete(deviceLogins, request.Flow)
	deviceLoginsMu.Unlock()
	if !ok {
		return failedDeviceWait(relay.NewError(relay.CodeAuthFailed,
			"no device login is pending for that handle; start the login again"))
	}

	token, failure := deviceFlowClient().Wait(ctx, record.spec, record.authorization)
	if failure != nil {
		return failedDeviceWait(failure)
	}
	// The token goes straight to the Keychain; it is never logged, echoed, or
	// included in a reply (spec §22, §54).
	if failure := d.resolver.Set(record.tool, token.AccessToken); failure != nil {
		return failedDeviceWait(failure)
	}
	return ipc.AuthDeviceWaitResponse{Success: true, Tool: record.tool, Stored: true}
}

// sweepDeviceLoginsLocked drops authorizations that can no longer complete, so
// a CLI killed mid-login does not leave the map growing for the daemon's
// lifetime. The caller must hold deviceLoginsMu.
func sweepDeviceLoginsLocked(now time.Time) {
	for flow, record := range deviceLogins {
		if now.After(record.expires) {
			delete(deviceLogins, flow)
		}
	}
}

// newDeviceFlowID mints an opaque handle for one pending authorization. It is
// not derived from the device code and carries no authority; it only spares the
// CLI from ever holding the code.
func newDeviceFlowID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("device-login-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}

// failedDeviceStart and failedDeviceWait keep the reply constructors in one
// place, mirroring failedAuthStatus.
func failedDeviceStart(err *relay.Error) ipc.AuthDeviceStartResponse {
	return ipc.AuthDeviceStartResponse{Success: false, Error: err}
}

func failedDeviceWait(err *relay.Error) ipc.AuthDeviceWaitResponse {
	return ipc.AuthDeviceWaitResponse{Success: false, Error: err}
}
