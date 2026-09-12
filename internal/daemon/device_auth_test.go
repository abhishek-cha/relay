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

// The daemon owns the device login: it holds the device code, polls the
// authorization server, and writes the token to the Keychain. These tests drive
// the flow end to end against an httptest authorization server and assert that
// the CLI-facing replies carry only the user code and never a secret
// (spec §21, §22, §54).

const daemonDeviceBody = `{"device_code":"dev-secret-code","user_code":"ABCD-1234",` +
	`"verification_uri":"https://example.test/activate",` +
	`"verification_uri_complete":"https://example.test/activate?user_code=ABCD-1234",` +
	`"expires_in":900,"interval":5}`

const daemonSuccessToken = `{"access_token":"access-token-secret","token_type":"bearer",` +
	`"scope":"read write","expires_in":3600}`

// deviceAuthServer is a minimal RFC 8628 authorization server. Token replies
// are replayed in order, with the last reply repeating, so a test can describe
// a polling sequence without counting calls.
type deviceAuthServer struct {
	server *httptest.Server

	mu          sync.Mutex
	deviceBody  string
	tokenStatus []int
	tokenBodies []string
	tokenCalls  int
	forms       []url.Values
}

func newDeviceAuthServer(t *testing.T) *deviceAuthServer {
	t.Helper()
	fake := &deviceAuthServer{deviceBody: daemonDeviceBody}
	mux := http.NewServeMux()
	mux.HandleFunc("/device", func(writer http.ResponseWriter, request *http.Request) {
		fake.mu.Lock()
		body := fake.deviceBody
		fake.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(body))
	})
	mux.HandleFunc("/token", func(writer http.ResponseWriter, request *http.Request) {
		_ = request.ParseForm()
		fake.mu.Lock()
		fake.tokenCalls++
		fake.forms = append(fake.forms, request.PostForm)
		status := http.StatusOK
		body := daemonSuccessToken
		if len(fake.tokenStatus) > 0 {
			index := fake.tokenCalls - 1
			if index >= len(fake.tokenStatus) {
				index = len(fake.tokenStatus) - 1
			}
			status = fake.tokenStatus[index]
			if len(fake.tokenBodies) > 0 {
				if index >= len(fake.tokenBodies) {
					index = len(fake.tokenBodies) - 1
				}
				body = fake.tokenBodies[index]
			}
		}
		fake.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(body))
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *deviceAuthServer) withTokenReplies(statuses []int, bodies []string) *deviceAuthServer {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenStatus = statuses
	f.tokenBodies = bodies
	return f
}

func (f *deviceAuthServer) withDeviceBody(body string) *deviceAuthServer {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deviceBody = body
	return f
}

func (f *deviceAuthServer) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenCalls
}

// deviceAuthManifest opts a tool into the device grant. The endpoints are the
// httptest server the test started (spec §21).
const deviceAuthManifest = `apiVersion: relay/v1
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
  deviceAuthorizationEndpoint: %s
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

// plainOAuth2Manifest is the pre-existing shape: a token a human pastes in.
const plainOAuth2Manifest = `apiVersion: relay/v1
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
  provider: github
tools:
  - name: ping
    description: ping the example service
    input:
      type: object
    request:
      method: GET
      path: /ping
`

// partialDeviceManifest starts declaring a device flow but omits tokenEndpoint.
const partialDeviceManifest = `apiVersion: relay/v1
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
  deviceAuthorizationEndpoint: https://example.test/oauth/device
  clientId: client-123
tools:
  - name: ping
    description: ping the example service
    input:
      type: object
    request:
      method: GET
      path: /ping
`

// deviceClock advances virtual time so the polling interval and expiry are
// exercised without the test actually waiting.
type deviceClock struct {
	mu sync.Mutex
	at time.Time
}

func newDeviceClock() *deviceClock { return &deviceClock{at: time.Unix(1_700_000_000, 0)} }

func (c *deviceClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *deviceClock) Sleep(_ context.Context, wait time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(wait)
	return nil
}

// installDeviceFlow points the daemon's polling client at the fake clock and
// clears any pending logins, so each test starts from a clean flow table.
func installDeviceFlow(t *testing.T, clock *deviceClock) {
	t.Helper()
	previous := deviceFlowClient
	deviceFlowClient = func() *auth.DeviceClient {
		return auth.NewDeviceClient(auth.DeviceOptions{Now: clock.Now, Sleep: clock.Sleep})
	}
	deviceLoginsMu.Lock()
	deviceLogins = map[string]*pendingDeviceLogin{}
	deviceLoginsMu.Unlock()
	t.Cleanup(func() {
		deviceFlowClient = previous
		deviceLoginsMu.Lock()
		deviceLogins = map[string]*pendingDeviceLogin{}
		deviceLoginsMu.Unlock()
	})
}

// newDeviceHarness builds a daemon whose tool declares the device flow against
// the given authorization server.
func newDeviceHarness(t *testing.T, server *deviceAuthServer, clock *deviceClock) (*Daemon, *memKeychain) {
	t.Helper()
	installDeviceFlow(t, clock)
	kc := newMemKeychain()
	manifestYAML := fmt.Sprintf(deviceAuthManifest, server.server.URL+"/device", server.server.URL+"/token")
	return newDaemonHarness(t, manifestYAML, &fakeExecutor{}, kc), kc
}

func startDeviceLogin(t *testing.T, d *Daemon) ipc.AuthDeviceStartResponse {
	t.Helper()
	start := d.authDeviceStart(context.Background(), ipc.AuthDeviceStartRequest{
		Type: ipc.FrameAuthDeviceStart,
		Tool: "demo",
	})
	if !start.Success {
		t.Fatalf("authDeviceStart failed: %v", start.Error)
	}
	return start
}

func TestAuthDeviceLoginStoresTheTokenWithoutLeakingIt(t *testing.T) {
	server := newDeviceAuthServer(t)
	d, kc := newDeviceHarness(t, server, newDeviceClock())

	start := startDeviceLogin(t, d)
	if start.UserCode != "ABCD-1234" || start.VerificationURI != "https://example.test/activate" {
		t.Fatalf("start = %+v, want the user code and verification URI", start)
	}
	if start.Flow == "" {
		t.Fatal("start returned no flow handle")
	}
	if start.ExpiresIn != 900 || start.Interval != 5 {
		t.Fatalf("start expiry/interval = %d/%d, want 900/5", start.ExpiresIn, start.Interval)
	}
	// The device code authorizes the token request, so it must not cross IPC.
	startJSON, _ := json.Marshal(start)
	if strings.Contains(string(startJSON), "dev-secret-code") {
		t.Fatalf("start reply leaked the device code: %s", startJSON)
	}

	wait := d.authDeviceWait(context.Background(), ipc.AuthDeviceWaitRequest{
		Type: ipc.FrameAuthDeviceWait,
		Flow: start.Flow,
	})
	if !wait.Success || !wait.Stored {
		t.Fatalf("authDeviceWait failed: %v", wait.Error)
	}
	waitJSON, _ := json.Marshal(wait)
	for _, secret := range []string{"access-token-secret", "dev-secret-code"} {
		if strings.Contains(string(waitJSON), secret) {
			t.Fatalf("wait reply leaked %q: %s", secret, waitJSON)
		}
	}

	// The token landed in the injected Keychain and nowhere else.
	stored, err := kc.Get(keychain.Service("demo"), keychain.AccountDefault)
	if err != nil {
		t.Fatalf("reading the stored credential: %v", err)
	}
	// It is stored as the versioned envelope: the envelope is what carries the
	// refresh token and the endpoint context a later refresh needs, so a bare
	// access token is the regression this guards against.
	envelope, ok := auth.DecodeStoredToken(stored)
	if !ok || envelope.AccessToken != "access-token-secret" || envelope.Flow != auth.FlowDevice {
		t.Fatalf("stored credential is not a device-flow envelope carrying the access token: %q", stored)
	}
}

func TestAuthDeviceLoginPollsThroughAuthorizationPending(t *testing.T) {
	server := newDeviceAuthServer(t).withTokenReplies(
		[]int{http.StatusOK, http.StatusOK},
		[]string{`{"error":"authorization_pending"}`, daemonSuccessToken},
	)
	d, kc := newDeviceHarness(t, server, newDeviceClock())

	start := startDeviceLogin(t, d)
	wait := d.authDeviceWait(context.Background(), ipc.AuthDeviceWaitRequest{Flow: start.Flow})
	if !wait.Success || !wait.Stored {
		t.Fatalf("authDeviceWait failed: %v", wait.Error)
	}
	if calls := server.calls(); calls != 2 {
		t.Fatalf("token endpoint polled %d times, want 2", calls)
	}
	stored, err := kc.Get(keychain.Service("demo"), keychain.AccountDefault)
	if err != nil {
		t.Fatalf("reading the stored credential: %v", err)
	}
	envelope, ok := auth.DecodeStoredToken(stored)
	if !ok || envelope.AccessToken != "access-token-secret" {
		t.Fatalf("stored credential is not an envelope carrying the access token: %q", stored)
	}
}

func TestAuthDeviceLoginFailedTokenRequestStoresNothing(t *testing.T) {
	server := newDeviceAuthServer(t).withTokenReplies(
		[]int{http.StatusBadRequest},
		[]string{`{"error":"invalid_client","error_description":"device_code dev-secret-code was rejected"}`},
	)
	d, kc := newDeviceHarness(t, server, newDeviceClock())

	start := startDeviceLogin(t, d)
	wait := d.authDeviceWait(context.Background(), ipc.AuthDeviceWaitRequest{Flow: start.Flow})
	if wait.Success || wait.Error == nil {
		t.Fatal("authDeviceWait succeeded, want a failure")
	}
	if wait.Error.Code != relay.CodeAuthFailed {
		t.Fatalf("code = %s, want AUTH_FAILED", wait.Error.Code)
	}
	if strings.Contains(wait.Error.Message, "dev-secret-code") {
		t.Fatalf("error leaked the device code: %q", wait.Error.Message)
	}
	if _, err := kc.Get(keychain.Service("demo"), keychain.AccountDefault); err == nil {
		t.Fatal("a credential was stored after a failed token request")
	}
}

func TestAuthDeviceLoginExpiryStoresNothing(t *testing.T) {
	server := newDeviceAuthServer(t).
		withDeviceBody(`{"device_code":"dev-secret-code","user_code":"ABCD-1234",`+
			`"verification_uri":"https://example.test/activate","expires_in":12,"interval":5}`).
		withTokenReplies([]int{http.StatusOK}, []string{`{"error":"authorization_pending"}`})
	d, kc := newDeviceHarness(t, server, newDeviceClock())

	start := startDeviceLogin(t, d)
	wait := d.authDeviceWait(context.Background(), ipc.AuthDeviceWaitRequest{Flow: start.Flow})
	if wait.Success || wait.Error == nil || wait.Error.Code != relay.CodeAuthFailed {
		t.Fatalf("wait = %+v, want AUTH_FAILED after the code expired", wait)
	}
	if _, err := kc.Get(keychain.Service("demo"), keychain.AccountDefault); err == nil {
		t.Fatal("a credential was stored after the login expired")
	}
}

func TestAuthDeviceLoginFlowHandleIsSingleUse(t *testing.T) {
	server := newDeviceAuthServer(t)
	d, _ := newDeviceHarness(t, server, newDeviceClock())

	start := startDeviceLogin(t, d)
	if wait := d.authDeviceWait(context.Background(), ipc.AuthDeviceWaitRequest{Flow: start.Flow}); !wait.Success {
		t.Fatalf("first authDeviceWait failed: %v", wait.Error)
	}
	second := d.authDeviceWait(context.Background(), ipc.AuthDeviceWaitRequest{Flow: start.Flow})
	if second.Success || second.Error == nil {
		t.Fatal("a flow handle was replayable, want a failure")
	}
	if second.Error.Code != relay.CodeAuthFailed {
		t.Fatalf("code = %s, want AUTH_FAILED", second.Error.Code)
	}
}

func TestAuthDeviceLoginUnknownFlowIsRejected(t *testing.T) {
	server := newDeviceAuthServer(t)
	d, _ := newDeviceHarness(t, server, newDeviceClock())

	wait := d.authDeviceWait(context.Background(), ipc.AuthDeviceWaitRequest{Flow: "not-a-real-flow"})
	if wait.Success || wait.Error == nil || wait.Error.Code != relay.CodeAuthFailed {
		t.Fatalf("wait = %+v, want AUTH_FAILED for an unknown handle", wait)
	}
}

func TestAuthDeviceStartRejectsUnknownTool(t *testing.T) {
	server := newDeviceAuthServer(t)
	d, _ := newDeviceHarness(t, server, newDeviceClock())

	start := d.authDeviceStart(context.Background(), ipc.AuthDeviceStartRequest{Tool: "ghost"})
	if start.Success || start.Error == nil || start.Error.Code != relay.CodeToolNotFound {
		t.Fatalf("start = %+v, want TOOL_NOT_FOUND", start)
	}
}

func TestAuthDeviceStartRejectsAToolWithNoAuth(t *testing.T) {
	installDeviceFlow(t, newDeviceClock())
	d := newDaemonHarness(t, noAuthManifest, &fakeExecutor{}, newMemKeychain())

	start := d.authDeviceStart(context.Background(), ipc.AuthDeviceStartRequest{Tool: "demo"})
	if start.Success || start.Error == nil || start.Error.Code != relay.CodeInvalidInput {
		t.Fatalf("start = %+v, want INVALID_INPUT for a tool with no auth", start)
	}
}

func TestAuthDeviceStartKeepsPlainOAuth2OnTheManualPath(t *testing.T) {
	// oauth2 with no device endpoints is the pre-existing pasted-token path;
	// the caller must be able to tell them apart (spec §59).
	installDeviceFlow(t, newDeviceClock())
	d := newDaemonHarness(t, plainOAuth2Manifest, &fakeExecutor{}, newMemKeychain())

	start := d.authDeviceStart(context.Background(), ipc.AuthDeviceStartRequest{Tool: "demo"})
	if start.Success || start.Error == nil || start.Error.Code != relay.CodeInvalidInput {
		t.Fatalf("start = %+v, want INVALID_INPUT with a manual-path marker", start)
	}
	if marker, ok := start.Error.Details["deviceFlow"]; !ok || marker != false {
		t.Fatalf("details[deviceFlow] = %v, want false so the CLI falls back", start.Error.Details["deviceFlow"])
	}
	if start.Flow != "" {
		t.Fatal("a flow handle was minted for the manual path")
	}
}

func TestAuthDeviceStartRefusesAPartialManifest(t *testing.T) {
	installDeviceFlow(t, newDeviceClock())
	d := newDaemonHarness(t, partialDeviceManifest, &fakeExecutor{}, newMemKeychain())

	start := d.authDeviceStart(context.Background(), ipc.AuthDeviceStartRequest{Tool: "demo"})
	if start.Success || start.Error == nil || start.Error.Code != relay.CodeInvalidInput {
		t.Fatalf("start = %+v, want INVALID_INPUT for a half-written device flow", start)
	}
	if !strings.Contains(start.Error.Message, "auth.tokenEndpoint") {
		t.Fatalf("message = %q, want it to name the missing field", start.Error.Message)
	}
}
