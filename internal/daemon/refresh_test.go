package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"relay/internal/auth"
	"relay/internal/ipc"
	"relay/internal/keychain"
	"relay/pkg/relay"
)

// The daemon owns credential refresh: it reads the stored envelope, performs the
// token-endpoint exchange, and writes the rotated envelope back to the Keychain.
// These tests drive authRefresh against httptest token servers and assert that a
// skip reason or an error never carries a token value, and that one failing tool
// does not stop an --all sweep (spec §21, §22, §54).

const refreshRotatedToken = `{"access_token":"rotated-access-secret","token_type":"bearer",` +
	`"scope":"read write","refresh_token":"rotated-refresh-secret","expires_in":3600}`

// refreshCarryForwardToken has no refresh_token, so the daemon must keep the
// previous one rather than losing the ability to refresh again.
const refreshCarryForwardToken = `{"access_token":"rotated-access-secret","token_type":"bearer","expires_in":3600}`

// refreshManifestTemplate opts a tool into an OAuth2 flow with a token endpoint
// the refresh can reach. The authorization endpoint is never used by a refresh;
// it only has to be present to select the browser spec (spec §21).
const refreshManifestTemplate = `apiVersion: relay/v1
kind: Tool
metadata:
  name: %s
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
  authorizationEndpoint: https://example.test/authorize
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

// refreshClock is a fixed wall clock, so a rotated token's computed expiry is
// asserted exactly instead of against the test's real clock.
var refreshClock = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// refreshAuthServer is a minimal token endpoint. Its reply and status are
// set by the test, and every request is counted.
type refreshAuthServer struct {
	server *httptest.Server

	mu     sync.Mutex
	status int
	body   string
	calls  int
}

func newRefreshAuthServer(t *testing.T) *refreshAuthServer {
	t.Helper()
	fake := &refreshAuthServer{status: http.StatusOK, body: refreshRotatedToken}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(writer http.ResponseWriter, request *http.Request) {
		_ = request.ParseForm()
		fake.mu.Lock()
		fake.calls++
		status, body := fake.status, fake.body
		fake.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(body))
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *refreshAuthServer) reply(status int, body string) *refreshAuthServer {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
	return f
}

func (f *refreshAuthServer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// installRefreshClient substitutes the refresh client seam with a client that
// uses a fixed clock, so a rotated expiry is deterministic. The HTTP leg still
// runs against the httptest server the manifest names.
func installRefreshClient(t *testing.T) {
	t.Helper()
	previous := refreshClient
	refreshClient = func() *auth.RefreshClient {
		return auth.NewRefreshClient(auth.RefreshOptions{Now: func() time.Time { return refreshClock }})
	}
	t.Cleanup(func() { refreshClient = previous })
}

// newRefreshHarness builds a daemon whose "demo" tool refreshes against the
// given token server.
func newRefreshHarness(t *testing.T, server *refreshAuthServer) (*Daemon, *memKeychain) {
	t.Helper()
	installRefreshClient(t)
	kc := newMemKeychain()
	manifestYAML := fmt.Sprintf(refreshManifestTemplate, "demo", server.server.URL+"/token")
	return newDaemonHarness(t, manifestYAML, &fakeExecutor{}, kc), kc
}

// seedTool registers a second tool in an already-built daemon, so a sweep test
// can watch one failing tool not stop the others.
func seedTool(t *testing.T, d *Daemon, name, manifestYAML string) {
	t.Helper()
	if err := d.store.Put(relay.Installation{
		Name:    name,
		Version: "1.0.0",
		Path:    writeToolScript(t, manifestYAML),
		Runtime: "relay/v1",
	}); err != nil {
		t.Fatalf("seed tool %s: %v", name, err)
	}
}

func seedEnvelope(t *testing.T, kc *memKeychain, tool string, token auth.StoredToken) {
	t.Helper()
	encoded, failure := auth.EncodeStoredToken(token)
	if failure != nil {
		t.Fatalf("encode envelope: %v", failure)
	}
	if err := kc.Set(keychain.Service(tool), keychain.AccountDefault, encoded); err != nil {
		t.Fatalf("seed keychain: %v", err)
	}
}

func refreshEnvelope(expiresAt string) auth.StoredToken {
	return auth.StoredToken{
		Version:       auth.StoredTokenVersion,
		AccessToken:   "access-token-secret",
		RefreshToken:  "refresh-token-secret",
		TokenType:     "bearer",
		Scope:         "read write",
		ExpiresAt:     expiresAt,
		TokenEndpoint: "https://example.test/token",
		ClientID:      "client-123",
		Flow:          auth.FlowBrowser,
	}
}

func pastExpiry() string   { return time.Now().Add(-time.Minute).UTC().Format(time.RFC3339) }
func futureExpiry() string { return time.Now().Add(time.Hour).UTC().Format(time.RFC3339) }

func onlyResult(t *testing.T, response ipc.AuthRefreshResponse) ipc.AuthRefreshResult {
	t.Helper()
	if len(response.Results) != 1 {
		t.Fatalf("results = %d, want exactly one", len(response.Results))
	}
	return response.Results[0]
}

func storedEnvelope(t *testing.T, kc *memKeychain, tool string) auth.StoredToken {
	t.Helper()
	stored, err := kc.Get(keychain.Service(tool), keychain.AccountDefault)
	if err != nil {
		t.Fatalf("stored credential read: %v", err)
	}
	envelope, ok := auth.DecodeStoredToken(stored)
	if !ok {
		t.Fatalf("stored value is not an envelope: %q", stored)
	}
	return envelope
}

// assertNoSecret fails if any part of a reply carries a token value. It is the
// cross-cutting assertion for the whole refresh surface: skip reasons, error
// messages, and result fields are all covered by one marshal (spec §22).
func assertNoSecret(t *testing.T, label string, payload any, secrets ...string) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s: %v", label, err)
	}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%s leaked %q: %s", label, secret, raw)
		}
	}
}

func TestAuthRefreshRequiresAToolOrAll(t *testing.T) {
	server := newRefreshAuthServer(t)
	d, _ := newRefreshHarness(t, server)

	response := d.authRefresh(context.Background(), ipc.AuthRefreshRequest{})
	if response.Success || response.Error == nil || response.Error.Code != relay.CodeInvalidInput {
		t.Fatalf("refresh = %+v, want INVALID_INPUT", response)
	}
	if server.callCount() != 0 {
		t.Fatal("the token endpoint was called with no tool named")
	}
}

func TestAuthRefreshRejectsAnUnknownTool(t *testing.T) {
	server := newRefreshAuthServer(t)
	d, _ := newRefreshHarness(t, server)

	response := d.authRefresh(context.Background(), ipc.AuthRefreshRequest{Tool: "ghost"})
	if response.Success || response.Error == nil || response.Error.Code != relay.CodeToolNotFound {
		t.Fatalf("refresh = %+v, want TOOL_NOT_FOUND", response)
	}
}

func TestAuthRefreshSkipsAFreshToken(t *testing.T) {
	server := newRefreshAuthServer(t)
	d, kc := newRefreshHarness(t, server)
	seedEnvelope(t, kc, "demo", refreshEnvelope(futureExpiry()))

	response := d.authRefresh(context.Background(), ipc.AuthRefreshRequest{Tool: "demo"})
	if !response.Success {
		t.Fatalf("refresh failed: %v", response.Error)
	}
	result := onlyResult(t, response)
	if result.Refreshed || result.Error != nil {
		t.Fatalf("result = %+v, want a skip with no error", result)
	}
	if !strings.Contains(result.Skipped, "fresh") {
		t.Fatalf("skip reason = %q, want it to explain the token is fresh", result.Skipped)
	}
	if server.callCount() != 0 {
		t.Fatal("a fresh token was refreshed without --force")
	}
	assertNoSecret(t, "fresh skip reply", response, "access-token-secret", "refresh-token-secret")
}

func TestAuthRefreshRefreshesAnExpiringToken(t *testing.T) {
	server := newRefreshAuthServer(t)
	d, kc := newRefreshHarness(t, server)
	seedEnvelope(t, kc, "demo", refreshEnvelope(pastExpiry()))

	response := d.authRefresh(context.Background(), ipc.AuthRefreshRequest{Tool: "demo"})
	if !response.Success {
		t.Fatalf("refresh failed: %v", response.Error)
	}
	result := onlyResult(t, response)
	if !result.Refreshed || result.Error != nil {
		t.Fatalf("result = %+v, want a refresh", result)
	}
	if result.ExpiresAt != "2026-01-02T04:04:05Z" {
		t.Fatalf("result expiry = %q, want the recomputed RFC3339 expiry", result.ExpiresAt)
	}
	if server.callCount() != 1 {
		t.Fatalf("token endpoint called %d times, want 1", server.callCount())
	}
	stored := storedEnvelope(t, kc, "demo")
	if stored.AccessToken != "rotated-access-secret" {
		t.Fatalf("stored access token = %q, want the rotated one", stored.AccessToken)
	}
	if stored.RefreshToken != "rotated-refresh-secret" {
		t.Fatalf("stored refresh token = %q, want the rotated one", stored.RefreshToken)
	}
	assertNoSecret(t, "refresh reply", response, "access-token-secret", "refresh-token-secret")
}

func TestAuthRefreshForceRotatesAFreshToken(t *testing.T) {
	server := newRefreshAuthServer(t)
	d, kc := newRefreshHarness(t, server)
	seedEnvelope(t, kc, "demo", refreshEnvelope(futureExpiry()))

	response := d.authRefresh(context.Background(), ipc.AuthRefreshRequest{Tool: "demo", Force: true})
	if !response.Success {
		t.Fatalf("refresh failed: %v", response.Error)
	}
	result := onlyResult(t, response)
	if !result.Refreshed || result.Error != nil {
		t.Fatalf("result = %+v, want --force to rotate a fresh token", result)
	}
	if server.callCount() != 1 {
		t.Fatalf("token endpoint called %d times, want 1", server.callCount())
	}
}

func TestAuthRefreshCarriesTheRefreshTokenForward(t *testing.T) {
	server := newRefreshAuthServer(t).reply(http.StatusOK, refreshCarryForwardToken)
	d, kc := newRefreshHarness(t, server)
	seedEnvelope(t, kc, "demo", refreshEnvelope(pastExpiry()))

	response := d.authRefresh(context.Background(), ipc.AuthRefreshRequest{Tool: "demo"})
	if !response.Success {
		t.Fatalf("refresh failed: %v", response.Error)
	}
	if result := onlyResult(t, response); !result.Refreshed || result.Error != nil {
		t.Fatalf("result = %+v, want a refresh", result)
	}
	stored := storedEnvelope(t, kc, "demo")
	if stored.RefreshToken != "refresh-token-secret" {
		t.Fatalf("stored refresh token = %q, want the previous one carried forward", stored.RefreshToken)
	}
	if stored.AccessToken != "rotated-access-secret" {
		t.Fatalf("stored access token = %q, want the rotated one", stored.AccessToken)
	}
}

func TestAuthRefreshInvalidGrantRequiresLogin(t *testing.T) {
	server := newRefreshAuthServer(t).reply(http.StatusBadRequest,
		`{"error":"invalid_grant","error_description":"refresh token refresh-token-secret and access-token-secret are dead"}`)
	d, kc := newRefreshHarness(t, server)
	seedEnvelope(t, kc, "demo", refreshEnvelope(pastExpiry()))

	response := d.authRefresh(context.Background(), ipc.AuthRefreshRequest{Tool: "demo"})
	if !response.Success {
		t.Fatalf("a per-tool failure must not fail the request: %v", response.Error)
	}
	result := onlyResult(t, response)
	if result.Error == nil || result.Error.Code != relay.CodeAuthRequired {
		t.Fatalf("result = %+v, want AUTH_REQUIRED", result)
	}
	assertNoSecret(t, "invalid_grant reply", response, "access-token-secret", "refresh-token-secret")
}

func TestAuthRefreshSkipsAPastedToken(t *testing.T) {
	server := newRefreshAuthServer(t)
	d, kc := newRefreshHarness(t, server)
	if err := kc.Set(keychain.Service("demo"), keychain.AccountDefault, "pasted-token-secret"); err != nil {
		t.Fatalf("seed keychain: %v", err)
	}

	response := d.authRefresh(context.Background(), ipc.AuthRefreshRequest{Tool: "demo"})
	if !response.Success {
		t.Fatalf("refresh failed: %v", response.Error)
	}
	result := onlyResult(t, response)
	if result.Refreshed || result.Error != nil || result.Skipped == "" {
		t.Fatalf("result = %+v, want a skip with a reason and no error", result)
	}
	if server.callCount() != 0 {
		t.Fatal("a pasted token was sent to the token endpoint")
	}
	assertNoSecret(t, "pasted skip reply", response, "pasted-token-secret")
}

func TestAuthRefreshSkipsAToolWithNoOAuth2Flow(t *testing.T) {
	installRefreshClient(t)
	d := newDaemonHarness(t, plainOAuth2Manifest, &fakeExecutor{}, newMemKeychain())

	response := d.authRefresh(context.Background(), ipc.AuthRefreshRequest{Tool: "demo"})
	if !response.Success {
		t.Fatalf("refresh failed: %v", response.Error)
	}
	result := onlyResult(t, response)
	if result.Refreshed || result.Error != nil || result.Skipped == "" {
		t.Fatalf("result = %+v, want a designed no-op skip", result)
	}
}

func TestAuthRefreshAllContinuesPastAFailingTool(t *testing.T) {
	good := newRefreshAuthServer(t)
	bad := newRefreshAuthServer(t).reply(http.StatusInternalServerError, `{"error":"server_error"}`)
	d, kc := newRefreshHarness(t, good)
	seedTool(t, d, "zulu", fmt.Sprintf(refreshManifestTemplate, "zulu", bad.server.URL+"/token"))
	seedEnvelope(t, kc, "demo", refreshEnvelope(pastExpiry()))
	seedEnvelope(t, kc, "zulu", refreshEnvelope(pastExpiry()))

	response := d.authRefresh(context.Background(), ipc.AuthRefreshRequest{Type: ipc.FrameAuthRefresh, All: true})
	if !response.Success {
		t.Fatalf("authRefresh --all failed: %v", response.Error)
	}
	if len(response.Results) != 2 {
		t.Fatalf("results = %d, want one per tool", len(response.Results))
	}
	byTool := map[string]ipc.AuthRefreshResult{}
	for _, result := range response.Results {
		byTool[result.Tool] = result
	}
	if !byTool["demo"].Refreshed || byTool["demo"].Error != nil {
		t.Fatalf("demo result = %+v, want a refresh", byTool["demo"])
	}
	if byTool["zulu"].Error == nil {
		t.Fatalf("zulu result = %+v, want a per-tool error", byTool["zulu"])
	}
	if good.callCount() != 1 {
		t.Fatalf("the good token endpoint was called %d times, want 1", good.callCount())
	}
	if bad.callCount() != 1 {
		t.Fatalf("the failing token endpoint was called %d times, want 1", bad.callCount())
	}
	assertNoSecret(t, "refresh --all reply", response, "access-token-secret", "refresh-token-secret")
}
