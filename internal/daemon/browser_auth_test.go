package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"relay/internal/auth"
	"relay/internal/ipc"
	"relay/internal/keychain"
	"relay/pkg/relay"
)

// The daemon owns the browser authorization-code login: it binds the loopback
// listener, holds the PKCE verifier and the state, performs the code exchange,
// and writes the token envelope to the Keychain. These tests drive the flow end
// to end against httptest authorization and token servers and assert that the
// CLI-facing replies carry only the URL and the bound redirect, never a secret
// (spec §21, §22, §54).

const browserSuccessToken = `{"access_token":"browser-access-secret","token_type":"bearer",` +
	`"scope":"read write","refresh_token":"browser-refresh-secret","expires_in":3600}`

// browserAuthManifestTemplate opts a tool into the browser grant. The endpoints
// are the httptest server the test started (spec §21).
const browserAuthManifestTemplate = `apiVersion: relay/v1
kind: Tool
metadata:
  name: demo
  version: 1.0.0
runtime:
  name: relay
  apiVersion: v1
protocol:
  type: rest
  baseUrl: https://example.test
auth:
  type: oauth2
  provider: example
  authorizationEndpoint: %s
  tokenEndpoint: %s
  clientId: client-123
  scopes:
    - read
    - write
tools:
  - name: ping
    description: ping the example service
    input:
      type: object
    request:
      method: GET
      path: /ping
`

// browserAuthServer is a minimal OAuth2 authorization server: /authorize is
// where the human's browser would go, /token exchanges the code. Only /token is
// counted, because the state-mismatch test proves it is never reached.
type browserAuthServer struct {
	server *httptest.Server

	mu         sync.Mutex
	tokenBody  string
	tokenCalls int
}

func newBrowserAuthServer(t *testing.T) *browserAuthServer {
	t.Helper()
	fake := &browserAuthServer{tokenBody: browserSuccessToken}
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = writer.Write([]byte("authorize"))
	})
	mux.HandleFunc("/token", func(writer http.ResponseWriter, request *http.Request) {
		_ = request.ParseForm()
		fake.mu.Lock()
		fake.tokenCalls++
		body := fake.tokenBody
		fake.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(body))
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *browserAuthServer) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenCalls
}

// installBrowserFlow points the daemon's browser client at the real (ephemeral
// loopback) client and clears any pending logins, so each test starts from a
// clean flow table. The cleanup closes any flow a test left pending, so no
// listener leaks between tests.
func installBrowserFlow(t *testing.T) {
	t.Helper()
	previous := browserFlowClient
	browserFlowClient = func() *auth.BrowserClient {
		return auth.NewBrowserClient(auth.BrowserOptions{})
	}
	browserLoginsMu.Lock()
	browserLogins = map[string]*pendingBrowserLogin{}
	browserLoginsMu.Unlock()
	t.Cleanup(func() {
		browserFlowClient = previous
		browserLoginsMu.Lock()
		for handle, record := range browserLogins {
			_ = record.flow.Close()
			delete(browserLogins, handle)
		}
		browserLoginsMu.Unlock()
	})
}

// newBrowserHarness builds a daemon whose tool declares the browser flow
// against the given authorization server.
func newBrowserHarness(t *testing.T, server *browserAuthServer) (*Daemon, *memKeychain) {
	t.Helper()
	installBrowserFlow(t)
	kc := newMemKeychain()
	manifestYAML := fmt.Sprintf(browserAuthManifestTemplate,
		server.server.URL+"/authorize", server.server.URL+"/token")
	return newDaemonHarness(t, manifestYAML, &fakeExecutor{}, kc), kc
}

// callbackState reads the state the authorization URL carries, so the test can
// hand it back on the loopback callback exactly as a browser would.
func callbackState(t *testing.T, authorizationURL string) string {
	t.Helper()
	parsed, err := url.Parse(authorizationURL)
	if err != nil {
		t.Fatalf("authorization URL is not a URL: %v", err)
	}
	state := parsed.Query().Get("state")
	if state == "" {
		t.Fatal("the authorization URL carried no state")
	}
	return state
}

// waitForBrowser runs authBrowserWait off the test goroutine, because Wait
// serves the loopback listener and only returns once the callback arrives.
func waitForBrowser(t *testing.T, d *Daemon, handle string) <-chan ipc.AuthBrowserWaitResponse {
	t.Helper()
	result := make(chan ipc.AuthBrowserWaitResponse, 1)
	go func() {
		result <- d.authBrowserWait(context.Background(), ipc.AuthBrowserWaitRequest{
			Type: ipc.FrameAuthBrowserWait,
			Flow: handle,
		})
	}()
	return result
}

// awaitBrowserWait waits for authBrowserWait with a guard, so a bug that never
// completes the flow fails the test instead of hanging it.
func awaitBrowserWait(t *testing.T, result <-chan ipc.AuthBrowserWaitResponse) ipc.AuthBrowserWaitResponse {
	t.Helper()
	select {
	case wait := <-result:
		return wait
	case <-time.After(10 * time.Second):
		t.Fatal("authBrowserWait did not return")
		return ipc.AuthBrowserWaitResponse{}
	}
}

func TestAuthBrowserLoginStoresAnEnvelopeWithoutLeakingIt(t *testing.T) {
	server := newBrowserAuthServer(t)
	d, kc := newBrowserHarness(t, server)

	start := d.authBrowserStart(context.Background(), ipc.AuthBrowserStartRequest{
		Type: ipc.FrameAuthBrowserStart,
		Tool: "demo",
	})
	if !start.Success {
		t.Fatalf("authBrowserStart failed: %v", start.Error)
	}
	if start.Flow == "" || start.URL == "" || start.RedirectURI == "" {
		t.Fatalf("start = %+v, want a handle, a URL, and the bound redirect", start)
	}
	parsed, err := url.Parse(start.URL)
	if err != nil || parsed.Query().Get("code_challenge") == "" {
		t.Fatalf("authorization URL = %q, want a PKCE challenge", start.URL)
	}
	if parsed.Query().Get("code_verifier") != "" {
		t.Fatal("the authorization URL carried the PKCE verifier")
	}
	startJSON, _ := json.Marshal(start)
	for _, secret := range []string{"browser-access-secret", "browser-refresh-secret", "code_verifier"} {
		if strings.Contains(string(startJSON), secret) {
			t.Fatalf("start reply leaked %q: %s", secret, startJSON)
		}
	}

	state := callbackState(t, start.URL)
	pending := waitForBrowser(t, d, start.Flow)

	// Drive the daemon's own loopback callback exactly as the browser would.
	callback := start.RedirectURI + "?code=authorization-code-secret&state=" + url.QueryEscape(state)
	response, err := http.Get(callback)
	if err != nil {
		t.Fatalf("callback GET: %v", err)
	}
	_ = response.Body.Close()

	wait := awaitBrowserWait(t, pending)
	if !wait.Success || !wait.Stored {
		t.Fatalf("authBrowserWait failed: %v", wait.Error)
	}
	waitJSON, _ := json.Marshal(wait)
	for _, secret := range []string{"browser-access-secret", "browser-refresh-secret", "authorization-code-secret", "code_verifier"} {
		if strings.Contains(string(waitJSON), secret) {
			t.Fatalf("wait reply leaked %q: %s", secret, waitJSON)
		}
	}
	if calls := server.calls(); calls != 1 {
		t.Fatalf("token endpoint called %d times, want 1", calls)
	}

	stored, err := kc.Get(keychain.Service("demo"), keychain.AccountDefault)
	if err != nil {
		t.Fatalf("stored credential read: %v", err)
	}
	envelope, ok := auth.DecodeStoredToken(stored)
	if !ok {
		t.Fatalf("the browser login stored a bare token, not an envelope: %q", stored)
	}
	if envelope.Flow != auth.FlowBrowser {
		t.Fatalf("envelope flow = %q, want %q", envelope.Flow, auth.FlowBrowser)
	}
	if envelope.AccessToken != "browser-access-secret" {
		t.Fatalf("envelope access token = %q, want the exchanged token", envelope.AccessToken)
	}
	if envelope.RefreshToken != "browser-refresh-secret" {
		t.Fatalf("the browser login dropped the refresh token: %q", envelope.RefreshToken)
	}

	// The handle is single-use: the same login cannot be finished twice.
	second := d.authBrowserWait(context.Background(), ipc.AuthBrowserWaitRequest{Flow: start.Flow})
	if second.Success || second.Error == nil || second.Error.Code != relay.CodeAuthFailed {
		t.Fatalf("second wait = %+v, want AUTH_FAILED", second)
	}
}

func TestAuthBrowserLoginRejectsAWrongStateWithoutExchanging(t *testing.T) {
	server := newBrowserAuthServer(t)
	d, kc := newBrowserHarness(t, server)

	start := d.authBrowserStart(context.Background(), ipc.AuthBrowserStartRequest{Tool: "demo"})
	if !start.Success {
		t.Fatalf("authBrowserStart failed: %v", start.Error)
	}
	pending := waitForBrowser(t, d, start.Flow)

	callback := start.RedirectURI + "?code=authorization-code-secret&state=not-our-state"
	response, err := http.Get(callback)
	if err != nil {
		t.Fatalf("callback GET: %v", err)
	}
	_ = response.Body.Close()

	wait := awaitBrowserWait(t, pending)
	if wait.Success || wait.Error == nil || wait.Error.Code != relay.CodeAuthFailed {
		t.Fatalf("wait = %+v, want AUTH_FAILED for a mismatched state", wait)
	}
	if calls := server.calls(); calls != 0 {
		t.Fatalf("token endpoint called %d times, want 0 after a state mismatch", calls)
	}
	if _, err := kc.Get(keychain.Service("demo"), keychain.AccountDefault); err == nil {
		t.Fatal("a credential was stored after a state mismatch")
	}
}

func TestAuthBrowserWaitRejectsAnUnknownHandle(t *testing.T) {
	server := newBrowserAuthServer(t)
	d, _ := newBrowserHarness(t, server)

	wait := d.authBrowserWait(context.Background(), ipc.AuthBrowserWaitRequest{Flow: "not-a-real-flow"})
	if wait.Success || wait.Error == nil || wait.Error.Code != relay.CodeAuthFailed {
		t.Fatalf("wait = %+v, want AUTH_FAILED for an unknown handle", wait)
	}
}

func TestAuthBrowserWaitRequiresAHandle(t *testing.T) {
	server := newBrowserAuthServer(t)
	d, _ := newBrowserHarness(t, server)

	wait := d.authBrowserWait(context.Background(), ipc.AuthBrowserWaitRequest{})
	if wait.Success || wait.Error == nil || wait.Error.Code != relay.CodeInvalidInput {
		t.Fatalf("wait = %+v, want INVALID_INPUT for a missing handle", wait)
	}
}

func TestAuthBrowserStartRequiresATool(t *testing.T) {
	installBrowserFlow(t)
	d := newDaemonHarness(t, plainOAuth2Manifest, &fakeExecutor{}, newMemKeychain())

	start := d.authBrowserStart(context.Background(), ipc.AuthBrowserStartRequest{})
	if start.Success || start.Error == nil || start.Error.Code != relay.CodeInvalidInput {
		t.Fatalf("start = %+v, want INVALID_INPUT for a missing tool", start)
	}
}

func TestAuthBrowserStartRejectsUnknownTool(t *testing.T) {
	installBrowserFlow(t)
	d := newDaemonHarness(t, plainOAuth2Manifest, &fakeExecutor{}, newMemKeychain())

	start := d.authBrowserStart(context.Background(), ipc.AuthBrowserStartRequest{Tool: "ghost"})
	if start.Success || start.Error == nil || start.Error.Code != relay.CodeToolNotFound {
		t.Fatalf("start = %+v, want TOOL_NOT_FOUND", start)
	}
}

func TestAuthBrowserStartRejectsANonOAuth2Tool(t *testing.T) {
	installBrowserFlow(t)
	d := newDaemonHarness(t, authManifest, &fakeExecutor{}, newMemKeychain())

	start := d.authBrowserStart(context.Background(), ipc.AuthBrowserStartRequest{Tool: "demo"})
	if start.Success || start.Error == nil || start.Error.Code != relay.CodeInvalidInput {
		t.Fatalf("start = %+v, want INVALID_INPUT for a non-oauth2 tool", start)
	}
}

func TestAuthBrowserStartKeepsPlainOAuth2OnTheManualPath(t *testing.T) {
	// oauth2 with no authorization endpoint is the pre-existing pasted-token
	// path; the caller must be able to tell them apart (spec §59).
	installBrowserFlow(t)
	d := newDaemonHarness(t, plainOAuth2Manifest, &fakeExecutor{}, newMemKeychain())

	start := d.authBrowserStart(context.Background(), ipc.AuthBrowserStartRequest{Tool: "demo"})
	if start.Success || start.Error == nil || start.Error.Code != relay.CodeInvalidInput {
		t.Fatalf("start = %+v, want INVALID_INPUT with a fallback marker", start)
	}
	if marker, ok := start.Error.Details["browserFlow"]; !ok || marker != false {
		t.Fatalf("details[browserFlow] = %v, want false so the CLI falls back", start.Error.Details["browserFlow"])
	}
	if start.Flow != "" {
		t.Fatal("a flow handle was minted for the manual path")
	}
}

// daemonRefreshToken is the device-grant reply for the regression that motivates
// the whole envelope change: a token that comes with a refresh token.
const daemonRefreshToken = `{"access_token":"access-token-secret","token_type":"bearer",` +
	`"scope":"read write","refresh_token":"refresh-token-secret","expires_in":3600}`

func TestAuthDeviceLoginStoresTheRefreshEnvelope(t *testing.T) {
	server := newDeviceAuthServer(t).withTokenReplies([]int{http.StatusOK}, []string{daemonRefreshToken})
	d, kc := newDeviceHarness(t, server, newDeviceClock())

	start := startDeviceLogin(t, d)
	wait := d.authDeviceWait(context.Background(), ipc.AuthDeviceWaitRequest{Flow: start.Flow})
	if !wait.Success || !wait.Stored {
		t.Fatalf("authDeviceWait failed: %v", wait.Error)
	}

	stored, err := kc.Get(keychain.Service("demo"), keychain.AccountDefault)
	if err != nil {
		t.Fatalf("stored credential read: %v", err)
	}
	envelope, ok := auth.DecodeStoredToken(stored)
	if !ok {
		t.Fatalf("the device login stored a bare token, not an envelope: %q", stored)
	}
	if envelope.Flow != auth.FlowDevice {
		t.Fatalf("envelope flow = %q, want %q", envelope.Flow, auth.FlowDevice)
	}
	if envelope.AccessToken != "access-token-secret" {
		t.Fatalf("envelope access token = %q, want the issued token", envelope.AccessToken)
	}
	if envelope.RefreshToken != "refresh-token-secret" {
		t.Fatalf("the device login dropped the refresh token: %q", envelope.RefreshToken)
	}
}

func TestAuthStatusSummarizesTheBrowserEnvelope(t *testing.T) {
	kc := newMemKeychain()
	envelope := auth.NewStoredToken(auth.Token{
		AccessToken:  "status-access-secret",
		RefreshToken: "status-refresh-secret",
		TokenType:    "bearer",
		Scope:        "read write",
		ExpiresIn:    time.Hour,
	}, auth.FlowBrowser, "https://example.test/token", "client-123")
	encoded, failure := auth.EncodeStoredToken(envelope)
	if failure != nil {
		t.Fatalf("encode envelope: %v", failure)
	}
	if err := kc.Set(keychain.Service("demo"), keychain.AccountDefault, encoded); err != nil {
		t.Fatalf("seed keychain: %v", err)
	}
	d := newDaemonHarness(t, authManifest, &fakeExecutor{}, kc)

	status := d.authStatus(context.Background(), relay.AuthStatusRequest{Tool: "demo"})
	if !status.Success || !status.Stored {
		t.Fatalf("status = %+v, want a stored credential", status)
	}
	if status.LoginKind != "oauth2 browser" {
		t.Fatalf("login kind = %q, want %q", status.LoginKind, "oauth2 browser")
	}
	if !status.Refreshable {
		t.Fatal("status did not report the envelope as refreshable")
	}
	if status.Scope != "read write" {
		t.Fatalf("scope = %q, want read write", status.Scope)
	}
	if status.ExpiresAt == "" {
		t.Fatal("status reported no expiry for an envelope that has one")
	}
	raw, _ := json.Marshal(status)
	for _, secret := range []string{"status-access-secret", "status-refresh-secret"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("status reply leaked %q: %s", secret, raw)
		}
	}
}

func TestAuthStatusReportsAPastedToken(t *testing.T) {
	kc := newMemKeychain()
	if err := kc.Set(keychain.Service("demo"), keychain.AccountDefault, "pasted-token-secret"); err != nil {
		t.Fatalf("seed keychain: %v", err)
	}
	d := newDaemonHarness(t, authManifest, &fakeExecutor{}, kc)

	status := d.authStatus(context.Background(), relay.AuthStatusRequest{Tool: "demo"})
	if !status.Success || !status.Stored {
		t.Fatalf("status = %+v, want a stored credential", status)
	}
	if status.LoginKind != "pasted token" {
		t.Fatalf("login kind = %q, want %q", status.LoginKind, "pasted token")
	}
	if status.Refreshable || status.ExpiresAt != "" || status.Scope != "" {
		t.Fatalf("status = %+v, want no envelope metadata for a pasted token", status)
	}
}
