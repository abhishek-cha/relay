package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"relay/pkg/relay"
)

// The device grant is exercised against a real HTTP server so the wire shape —
// form encoding, the RFC 8628 polling loop, and the error codes — is what the
// tests actually check (RFC 8628 §3).

// fakeClock drives the polling loop without waiting: Sleep advances the same
// clock Now reads, so expiry happens the way it would in production, just
// faster.
type fakeClock struct {
	mu    sync.Mutex
	at    time.Time
	slept []time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{at: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) Sleep(_ context.Context, wait time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, wait)
	c.at = c.at.Add(wait)
	return nil
}

func (c *fakeClock) sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

// fakeOAuthServer is a minimal authorization server. It records what the client
// sent, so a test can assert on the wire shape rather than on internals.
type fakeOAuthServer struct {
	server *httptest.Server

	mu           sync.Mutex
	deviceCalls  int
	deviceForm   url.Values
	deviceStatus int
	deviceBody   string
	tokenForms   []url.Values
	tokenStatus  []int
	tokenBodies  []string
}

const defaultDeviceBody = `{"device_code":"dev-secret-code","user_code":"ABCD-1234",` +
	`"verification_uri":"https://example.test/activate",` +
	`"verification_uri_complete":"https://example.test/activate?user_code=ABCD-1234",` +
	`"expires_in":900,"interval":5}`

const successTokenBody = `{"access_token":"access-token-secret","token_type":"bearer",` +
	`"scope":"read write","expires_in":3600,"refresh_token":"refresh-token-secret"}`

type fakeServerOption func(*fakeOAuthServer)

func newFakeOAuthServer(t *testing.T, options ...fakeServerOption) *fakeOAuthServer {
	t.Helper()
	server := &fakeOAuthServer{deviceStatus: http.StatusOK, deviceBody: defaultDeviceBody}
	for _, option := range options {
		option(server)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/device", func(writer http.ResponseWriter, request *http.Request) {
		server.mu.Lock()
		server.deviceCalls++
		_ = request.ParseForm()
		server.deviceForm = request.PostForm
		status, body := server.deviceStatus, server.deviceBody
		server.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(body))
	})
	mux.HandleFunc("/token", func(writer http.ResponseWriter, request *http.Request) {
		server.mu.Lock()
		_ = request.ParseForm()
		server.tokenForms = append(server.tokenForms, request.PostForm)
		index := len(server.tokenForms) - 1
		if index >= len(server.tokenStatus) {
			index = len(server.tokenStatus) - 1
		}
		if index < 0 {
			index = 0
		}
		status := http.StatusOK
		if len(server.tokenStatus) > 0 {
			status = server.tokenStatus[index]
		}
		body := successTokenBody
		if len(server.tokenBodies) > 0 {
			body = server.tokenBodies[index]
		}
		server.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(body))
	})
	server.server = httptest.NewServer(mux)
	t.Cleanup(server.server.Close)
	return server
}

func withDeviceBody(body string) fakeServerOption {
	return func(server *fakeOAuthServer) { server.deviceBody = body }
}

func withDeviceStatus(status int) fakeServerOption {
	return func(server *fakeOAuthServer) { server.deviceStatus = status }
}

func withTokenReplies(statuses []int, bodies []string) fakeServerOption {
	return func(server *fakeOAuthServer) {
		server.tokenStatus = statuses
		server.tokenBodies = bodies
	}
}

func (f *fakeOAuthServer) spec() DeviceFlowSpec {
	return DeviceFlowSpec{
		Tool:                        "demo",
		DeviceAuthorizationEndpoint: f.server.URL + "/device",
		TokenEndpoint:               f.server.URL + "/token",
		ClientID:                    "client-123",
		Scopes:                      []string{"read", "write"},
	}
}

func (f *fakeOAuthServer) tokenCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tokenForms)
}

func (f *fakeOAuthServer) lastTokenForm() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tokenForms) == 0 {
		return nil
	}
	return f.tokenForms[len(f.tokenForms)-1]
}

func (f *fakeOAuthServer) startedForm() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deviceForm
}

func newTestClient(clock *fakeClock) *DeviceClient {
	return NewDeviceClient(DeviceOptions{Now: clock.Now, Sleep: clock.Sleep})
}

func TestDeviceStartSendsTheDocumentedRequest(t *testing.T) {
	server := newFakeOAuthServer(t)
	authorization, failure := newTestClient(newFakeClock()).Start(context.Background(), server.spec())
	if failure != nil {
		t.Fatalf("Start failed: %v", failure)
	}

	form := server.startedForm()
	if form.Get("client_id") != "client-123" {
		t.Fatalf("client_id = %q, want client-123", form.Get("client_id"))
	}
	if form.Get("scope") != "read write" {
		t.Fatalf("scope = %q, want \"read write\"", form.Get("scope"))
	}
	if authorization.UserCode != "ABCD-1234" || authorization.VerificationURI != "https://example.test/activate" {
		t.Fatalf("authorization = %+v, want the server's user code and verification URI", authorization)
	}
	if authorization.VerificationURIComplete == "" {
		t.Fatal("verification_uri_complete was dropped")
	}
	if authorization.ExpiresIn != 900*time.Second || authorization.Interval != 5*time.Second {
		t.Fatalf("expiry/interval = %v/%v, want 900s/5s", authorization.ExpiresIn, authorization.Interval)
	}
	if authorization.DeviceCode != "dev-secret-code" {
		t.Fatal("the device code was not parsed; the daemon needs it to poll")
	}
}

func TestDeviceStartRefusesAnIncompleteManifestSpec(t *testing.T) {
	_, failure := newTestClient(newFakeClock()).Start(context.Background(), DeviceFlowSpec{
		Tool:                        "demo",
		DeviceAuthorizationEndpoint: "https://example.test/device",
		ClientID:                    "client-123",
	})
	if failure == nil || failure.Code != relay.CodeInvalidInput {
		t.Fatalf("Start = %v, want INVALID_INPUT for a spec with no token endpoint", failure)
	}
	if !strings.Contains(failure.Message, "auth.tokenEndpoint") {
		t.Fatalf("message = %q, want it to name the missing field", failure.Message)
	}
}

func TestDeviceStartServerFailureIsRemote(t *testing.T) {
	server := newFakeOAuthServer(t, withDeviceStatus(http.StatusBadGateway),
		withDeviceBody(`{"error":"server_error"}`))
	_, failure := newTestClient(newFakeClock()).Start(context.Background(), server.spec())
	if failure == nil || failure.Code != relay.CodeRemoteError {
		t.Fatalf("Start = %v, want REMOTE_ERROR for HTTP 502", failure)
	}
}

// startAndAuthorize runs the first leg, so each Wait test only has to describe
// the token endpoint's behaviour.
func startAndAuthorize(t *testing.T, server *fakeOAuthServer) (*DeviceClient, DeviceFlowSpec, DeviceAuthorization) {
	t.Helper()
	client := newTestClient(newFakeClock())
	authorization, failure := client.Start(context.Background(), server.spec())
	if failure != nil {
		t.Fatalf("Start failed: %v", failure)
	}
	return client, server.spec(), authorization
}

func TestDeviceWaitHappyPathReturnsTheToken(t *testing.T) {
	server := newFakeOAuthServer(t)
	client, spec, authorization := startAndAuthorize(t, server)

	token, failure := client.Wait(context.Background(), spec, authorization)
	if failure != nil {
		t.Fatalf("Wait failed: %v", failure)
	}
	if token.AccessToken != "access-token-secret" || token.TokenType != "bearer" {
		t.Fatalf("token = %+v, want the server's access token", token)
	}
	if token.RefreshToken != "refresh-token-secret" || token.ExpiresIn != 3600*time.Second {
		t.Fatalf("token = %+v, want the refresh token and 3600s expiry", token)
	}
	form := server.lastTokenForm()
	if form.Get("grant_type") != DeviceGrantType {
		t.Fatalf("grant_type = %q, want %q", form.Get("grant_type"), DeviceGrantType)
	}
	if form.Get("device_code") != "dev-secret-code" {
		t.Fatal("the device code was not sent to the token endpoint")
	}
	if form.Get("client_id") != "client-123" {
		t.Fatalf("client_id = %q, want client-123", form.Get("client_id"))
	}
}

func TestDeviceWaitPollsThroughAuthorizationPending(t *testing.T) {
	server := newFakeOAuthServer(t, withTokenReplies(
		[]int{http.StatusOK, http.StatusOK},
		[]string{`{"error":"authorization_pending"}`, successTokenBody},
	))
	client, spec, authorization := startAndAuthorize(t, server)

	if _, failure := client.Wait(context.Background(), spec, authorization); failure != nil {
		t.Fatalf("Wait failed: %v", failure)
	}
	if calls := server.tokenCallCount(); calls != 2 {
		t.Fatalf("token endpoint polled %d times, want 2", calls)
	}
}

func TestDeviceWaitSlowDownBacksOffByFiveSeconds(t *testing.T) {
	server := newFakeOAuthServer(t, withTokenReplies(
		[]int{http.StatusOK, http.StatusOK, http.StatusOK},
		[]string{`{"error":"slow_down"}`, `{"error":"slow_down"}`, successTokenBody},
	))
	clock := newFakeClock()
	client := newTestClient(clock)
	authorization, failure := client.Start(context.Background(), server.spec())
	if failure != nil {
		t.Fatalf("Start failed: %v", failure)
	}
	spec := server.spec()

	if _, failure := client.Wait(context.Background(), spec, authorization); failure != nil {
		t.Fatalf("Wait failed: %v", failure)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 15 * time.Second}
	got := clock.sleeps()
	if len(got) != len(want) {
		t.Fatalf("waited %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("waited %v, want %v", got, want)
		}
	}
}

func TestDeviceWaitExpiresWhenTheCodeTimesOut(t *testing.T) {
	server := newFakeOAuthServer(t,
		withDeviceBody(`{"device_code":"dev-secret-code","user_code":"ABCD-1234",`+
			`"verification_uri":"https://example.test/activate","expires_in":12,"interval":5}`),
		withTokenReplies([]int{http.StatusOK}, []string{`{"error":"authorization_pending"}`}),
	)
	client, spec, authorization := startAndAuthorize(t, server)

	_, failure := client.Wait(context.Background(), spec, authorization)
	if failure == nil || failure.Code != relay.CodeAuthFailed {
		t.Fatalf("Wait = %v, want AUTH_FAILED once the code expired", failure)
	}
	if !strings.Contains(failure.Message, "expired") {
		t.Fatalf("message = %q, want it to say the login expired", failure.Message)
	}
	if strings.Contains(failure.Message, "dev-secret-code") {
		t.Fatal("the expiry error leaked the device code")
	}
}

func TestDeviceWaitTreatsExpiredTokenAsExpiry(t *testing.T) {
	server := newFakeOAuthServer(t, withTokenReplies(
		[]int{http.StatusOK}, []string{`{"error":"expired_token"}`},
	))
	client, spec, authorization := startAndAuthorize(t, server)

	_, failure := client.Wait(context.Background(), spec, authorization)
	if failure == nil || failure.Code != relay.CodeAuthFailed || !strings.Contains(failure.Message, "expired") {
		t.Fatalf("Wait = %v, want an AUTH_FAILED expiry", failure)
	}
}

func TestDeviceWaitReportsAccessDenied(t *testing.T) {
	server := newFakeOAuthServer(t, withTokenReplies(
		[]int{http.StatusOK}, []string{`{"error":"access_denied"}`},
	))
	client, spec, authorization := startAndAuthorize(t, server)

	_, failure := client.Wait(context.Background(), spec, authorization)
	if failure == nil || failure.Code != relay.CodeAuthFailed || !strings.Contains(failure.Message, "denied") {
		t.Fatalf("Wait = %v, want an AUTH_FAILED denial", failure)
	}
}

func TestDeviceWaitFailedTokenRequestIsAuthFailed(t *testing.T) {
	server := newFakeOAuthServer(t, withTokenReplies(
		[]int{http.StatusBadRequest},
		[]string{`{"error":"invalid_client","error_description":"client_id is unknown"}`},
	))
	client, spec, authorization := startAndAuthorize(t, server)

	_, failure := client.Wait(context.Background(), spec, authorization)
	if failure == nil || failure.Code != relay.CodeAuthFailed {
		t.Fatalf("Wait = %v, want AUTH_FAILED for a rejected token request", failure)
	}
	if !strings.Contains(failure.Message, "invalid_client") {
		t.Fatalf("message = %q, want the server's OAuth2 error code", failure.Message)
	}
}

func TestDeviceWaitScrubsTheDeviceCodeFromRemoteText(t *testing.T) {
	server := newFakeOAuthServer(t, withTokenReplies(
		[]int{http.StatusBadRequest},
		[]string{`{"error":"invalid_grant","error_description":"device_code dev-secret-code was rejected"}`},
	))
	client, spec, authorization := startAndAuthorize(t, server)

	_, failure := client.Wait(context.Background(), spec, authorization)
	if failure == nil {
		t.Fatal("Wait succeeded, want a failure")
	}
	if strings.Contains(failure.Message, "dev-secret-code") {
		t.Fatalf("message leaked the device code: %q", failure.Message)
	}
	if !strings.Contains(failure.Message, "[REDACTED]") {
		t.Fatalf("message = %q, want the echoed device code redacted", failure.Message)
	}
}

func TestDeviceWaitTransportFailureIsNetworkError(t *testing.T) {
	server := newFakeOAuthServer(t)
	client, spec, authorization := startAndAuthorize(t, server)
	spec.TokenEndpoint = "http://127.0.0.1:1/token"

	_, failure := client.Wait(context.Background(), spec, authorization)
	if failure == nil || failure.Code != relay.CodeNetworkError {
		t.Fatalf("Wait = %v, want NETWORK_ERROR", failure)
	}
}

func TestDeviceWaitHonoursTheCallerContext(t *testing.T) {
	server := newFakeOAuthServer(t)
	client, spec, authorization := startAndAuthorize(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// The fake clock's Sleep ignores the context, so use the real one to
	// observe cancellation.
	client.sleep = sleepContext
	_, failure := client.Wait(ctx, spec, authorization)
	if failure == nil || failure.Code != relay.CodeTimeout {
		t.Fatalf("Wait = %v, want TIMEOUT after cancellation", failure)
	}
}

func TestDeviceWaitRefusesAnEmptyDeviceCode(t *testing.T) {
	client := newTestClient(newFakeClock())
	_, failure := client.Wait(context.Background(), DeviceFlowSpec{Tool: "demo"}, DeviceAuthorization{})
	if failure == nil || failure.Code != relay.CodeInvalidInput {
		t.Fatalf("Wait = %v, want INVALID_INPUT", failure)
	}
}

// deviceManifest is a manifest that opts into the device grant: oauth2 with the
// additive endpoints, client id, and scopes (spec §21, §54).
const deviceManifest = `apiVersion: relay/v1
kind: Tool
metadata:
  name: demo
  version: 1.0.0
auth:
  type: oauth2
  provider: example
  deviceAuthorizationEndpoint: https://example.test/oauth/device
  tokenEndpoint: https://example.test/oauth/token
  clientId: client-123
  scopes:
    - read
    - write
`

func TestDeviceFlowSpecFromManifestReadsTheAdditiveFields(t *testing.T) {
	spec, ok, failure := DeviceFlowSpecFromManifest([]byte(deviceManifest))
	if failure != nil {
		t.Fatalf("DeviceFlowSpecFromManifest failed: %v", failure)
	}
	if !ok {
		t.Fatal("the manifest declares a device flow but was not recognised")
	}
	if spec.DeviceAuthorizationEndpoint != "https://example.test/oauth/device" ||
		spec.TokenEndpoint != "https://example.test/oauth/token" ||
		spec.ClientID != "client-123" {
		t.Fatalf("spec = %+v, want the manifest's endpoints and client id", spec)
	}
	if len(spec.Scopes) != 2 || spec.Scopes[0] != "read" || spec.Scopes[1] != "write" {
		t.Fatalf("scopes = %v, want [read write]", spec.Scopes)
	}
}

func TestDeviceFlowSpecFromManifestAcceptsSpaceDelimitedScopes(t *testing.T) {
	manifest := `auth:
  type: oauth2
  deviceAuthorizationEndpoint: https://example.test/device
  tokenEndpoint: https://example.test/token
  clientId: client-123
  scopes: "read write"
`
	spec, ok, failure := DeviceFlowSpecFromManifest([]byte(manifest))
	if failure != nil || !ok {
		t.Fatalf("DeviceFlowSpecFromManifest = (%+v, %v, %v), want a device flow", spec, ok, failure)
	}
	if len(spec.Scopes) != 2 || spec.Scopes[0] != "read" || spec.Scopes[1] != "write" {
		t.Fatalf("scopes = %v, want [read write]", spec.Scopes)
	}
}

func TestDeviceFlowSpecFromManifestKeepsPlainOAuth2Manual(t *testing.T) {
	// The pre-existing shape: an oauth2 provider with no endpoints means a
	// token a human pastes in, and must keep working unchanged (spec §59).
	manifest := `auth:
  type: oauth2
  provider: github
`
	_, ok, failure := DeviceFlowSpecFromManifest([]byte(manifest))
	if failure != nil {
		t.Fatalf("plain oauth2 was rejected: %v", failure)
	}
	if ok {
		t.Fatal("plain oauth2 was read as a device flow; it must stay the manual path")
	}
}

func TestDeviceFlowSpecFromManifestIgnoresOtherAuthTypes(t *testing.T) {
	manifest := `auth:
  type: bearer
  provider: example
`
	_, ok, failure := DeviceFlowSpecFromManifest([]byte(manifest))
	if failure != nil || ok {
		t.Fatalf("bearer auth = (%v, %v), want no device flow and no error", ok, failure)
	}
}

func TestDeviceFlowSpecFromManifestRefusesAPartialDeclaration(t *testing.T) {
	// Missing tokenEndpoint: loudly wrong, not a silent fall back to pasting.
	manifest := `auth:
  type: oauth2
  provider: example
  deviceAuthorizationEndpoint: https://example.test/device
  clientId: client-123
`
	_, ok, failure := DeviceFlowSpecFromManifest([]byte(manifest))
	if ok || failure == nil || failure.Code != relay.CodeInvalidInput {
		t.Fatalf("partial declaration = (%v, %v), want INVALID_INPUT", ok, failure)
	}
	if !strings.Contains(failure.Message, "auth.tokenEndpoint") {
		t.Fatalf("message = %q, want it to name the missing field", failure.Message)
	}
}

func TestDeviceFlowSpecFromManifestDoesNotEchoAMalformedManifest(t *testing.T) {
	// A tab cannot start a YAML token, so the document fails to parse. The body
	// must not come back in the diagnostic (spec §22).
	manifest := "auth:\n\ttype: oauth2\n\tclientId: super-secret-client\n"
	_, ok, failure := DeviceFlowSpecFromManifest([]byte(manifest))
	if ok || failure == nil || failure.Code != relay.CodeRuntimeIncompatible {
		t.Fatalf("malformed manifest = (%v, %v), want RUNTIME_INCOMPATIBLE", ok, failure)
	}
	if strings.Contains(failure.Message, "super-secret-client") {
		t.Fatalf("message leaked the manifest body: %q", failure.Message)
	}
}
