package auth

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"relay/pkg/relay"
)

// The browser flow is exercised against a real loopback listener and a real
// token endpoint, so the wire shape — the authorization URL, the callback, and
// the PKCE exchange — is what the tests actually check (RFC 6749 §4.1,
// RFC 7636). No test binds a fixed port or waits on real time.

const browserSuccessBody = ` {"access_token":"access-token-secret","token_type":"bearer","scope":"read","expires_in":3600,"refresh_token":"refresh-token-secret"} `

type browserTokenStub struct {
	server *httptest.Server
	mu     sync.Mutex
	forms  []url.Values
	status int
	body   string
}

func newBrowserTokenStub(t *testing.T) *browserTokenStub {
	t.Helper()
	stub := &browserTokenStub{status: http.StatusOK, body: browserSuccessBody}
	stub.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = request.ParseForm()
		stub.mu.Lock()
		stub.forms = append(stub.forms, request.PostForm)
		status, body := stub.status, stub.body
		stub.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(body))
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *browserTokenStub) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.forms)
}

func (s *browserTokenStub) lastForm() url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.forms) == 0 {
		return nil
	}
	return s.forms[len(s.forms)-1]
}

func browserSpec(tokenEndpoint string) BrowserFlowSpec {
	return BrowserFlowSpec{
		Tool:                  "demo",
		AuthorizationEndpoint: "https://auth.example.test/authorize",
		TokenEndpoint:         tokenEndpoint,
		ClientID:              "client-123",
		Scopes:                []string{"read"},
	}
}

// beginBrowserFlow injects a listener on an ephemeral port and returns it so a
// test can assert it was released.
func beginBrowserFlow(t *testing.T, spec BrowserFlowSpec) (*BrowserFlow, net.Listener) {
	t.Helper()
	var bound net.Listener
	client := NewBrowserClient(BrowserOptions{
		Listener: func(BrowserFlowSpec) (net.Listener, error) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			bound = listener
			return listener, err
		},
	})
	flow, failure := client.Begin(context.Background(), spec)
	if failure != nil {
		t.Fatalf("Begin failed: %v", failure)
	}
	t.Cleanup(func() { _ = flow.Close() })
	return flow, bound
}

type browserWaitResult struct {
	token   *Token
	failure *relay.Error
}

func waitAsync(flow *BrowserFlow) <-chan browserWaitResult {
	results := make(chan browserWaitResult, 1)
	go func() {
		token, failure := flow.Wait(context.Background())
		results <- browserWaitResult{token: token, failure: failure}
	}()
	return results
}

func browserGET(t *testing.T, rawURL string, wantStatus int) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(rawURL)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != wantStatus {
		t.Fatalf("GET %s = %d, want %d", rawURL, response.StatusCode, wantStatus)
	}
}

func stateFromURL(t *testing.T, rawURL string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("the authorization URL is not a URL: %v", err)
	}
	state := parsed.Query().Get("state")
	if state == "" {
		t.Fatal("the authorization URL has no state")
	}
	return state
}

func TestBrowserURLCarriesStateAndS256Challenge(t *testing.T) {
	spec := browserSpec("https://example.test/token")
	spec.AuthorizationEndpoint = "https://auth.example.test/authorize?prompt=consent"
	spec.Scopes = []string{"read", "write"}
	flow, _ := beginBrowserFlow(t, spec)

	raw := flow.URL()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("URL is not a URL: %v", err)
	}
	query := parsed.Query()
	if query.Get("response_type") != "code" {
		t.Fatalf("response_type = %q, want code", query.Get("response_type"))
	}
	if query.Get("client_id") != "client-123" {
		t.Fatalf("client_id = %q, want client-123", query.Get("client_id"))
	}
	if query.Get("redirect_uri") != flow.RedirectURI() {
		t.Fatalf("redirect_uri = %q, want the bound redirect URI %q", query.Get("redirect_uri"), flow.RedirectURI())
	}
	if query.Get("scope") != "read write" {
		t.Fatalf("scope = %q, want %q", query.Get("scope"), "read write")
	}
	if query.Get("state") != flow.state || flow.state == "" {
		t.Fatal("the authorization URL did not carry the minted state")
	}
	if query.Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", query.Get("code_challenge_method"))
	}
	if query.Get("code_challenge") != codeChallenge(flow.verifier) {
		t.Fatal("the challenge is not the S256 transform of the verifier")
	}
	if strings.Contains(raw, flow.verifier) {
		t.Fatal("the authorization URL leaked the PKCE verifier")
	}
	if query.Get("prompt") != "consent" {
		t.Fatalf("prompt = %q, want the endpoint's existing parameter preserved", query.Get("prompt"))
	}
}

func TestBrowserWaitExchangesTheCode(t *testing.T) {
	stub := newBrowserTokenStub(t)
	flow, _ := beginBrowserFlow(t, browserSpec(stub.server.URL))
	results := waitAsync(flow)
	state := stateFromURL(t, flow.URL())

	callback := flow.RedirectURI() + "?state=" + url.QueryEscape(state) + "&code=auth-code-secret"
	browserGET(t, callback, http.StatusOK)

	result := <-results
	if result.failure != nil {
		t.Fatalf("Wait failed: %v", result.failure)
	}
	if result.token.AccessToken != "access-token-secret" || result.token.RefreshToken != "refresh-token-secret" {
		t.Fatalf("token = %+v, want the server's access and refresh tokens", result.token)
	}
	if result.token.ExpiresIn != time.Hour {
		t.Fatalf("ExpiresIn = %v, want 1h", result.token.ExpiresIn)
	}

	if calls := stub.calls(); calls != 1 {
		t.Fatalf("token endpoint called %d times, want 1", calls)
	}
	form := stub.lastForm()
	if form.Get("grant_type") != "authorization_code" {
		t.Fatalf("grant_type = %q, want authorization_code", form.Get("grant_type"))
	}
	if form.Get("code") != "auth-code-secret" {
		t.Fatal("the authorization code was not sent to the token endpoint")
	}
	if form.Get("redirect_uri") != flow.RedirectURI() {
		t.Fatalf("redirect_uri = %q, want the bound redirect URI", form.Get("redirect_uri"))
	}
	if form.Get("client_id") != "client-123" {
		t.Fatalf("client_id = %q, want client-123", form.Get("client_id"))
	}
	if form.Get("code_verifier") != flow.verifier {
		t.Fatal("the PKCE verifier was not sent to the token endpoint")
	}

	if err := flow.Close(); err != nil {
		t.Fatalf("Close after Wait = %v, want nil", err)
	}
	if err := flow.Close(); err != nil {
		t.Fatalf("second Close after Wait = %v, want nil", err)
	}
}

func TestBrowserWrongStateIsRejectedWithoutExchanging(t *testing.T) {
	stub := newBrowserTokenStub(t)
	flow, _ := beginBrowserFlow(t, browserSpec(stub.server.URL))
	results := waitAsync(flow)

	browserGET(t, flow.RedirectURI()+"?state=not-the-state&code=leaked", http.StatusBadRequest)

	result := <-results
	if result.failure == nil || result.failure.Code != relay.CodeAuthFailed {
		t.Fatalf("Wait = %v, want AUTH_FAILED for a mismatched state", result.failure)
	}
	if calls := stub.calls(); calls != 0 {
		t.Fatalf("token endpoint called %d times, want 0 — nothing may be exchanged", calls)
	}
}

func TestBrowserServerErrorSurfacesStructurally(t *testing.T) {
	stub := newBrowserTokenStub(t)
	flow, _ := beginBrowserFlow(t, browserSpec(stub.server.URL))
	results := waitAsync(flow)
	state := stateFromURL(t, flow.URL())

	callback := flow.RedirectURI() + "?state=" + url.QueryEscape(state) + "&error=access_denied&error_description=user+denied"
	browserGET(t, callback, http.StatusBadRequest)

	result := <-results
	if result.failure == nil || result.failure.Code != relay.CodeAuthFailed {
		t.Fatalf("Wait = %v, want AUTH_FAILED for a server error", result.failure)
	}
	if !strings.Contains(result.failure.Message, "access_denied") {
		t.Fatalf("message = %q, want the symbolic OAuth2 error code", result.failure.Message)
	}
	if strings.Contains(result.failure.Message, "state=") {
		t.Fatalf("message = %q, must not echo the query string", result.failure.Message)
	}
	if calls := stub.calls(); calls != 0 {
		t.Fatalf("token endpoint called %d times, want 0", calls)
	}
}

func TestBrowserCallbackPathMustMatchExactly(t *testing.T) {
	stub := newBrowserTokenStub(t)
	spec := browserSpec(stub.server.URL)
	spec.RedirectURI = "http://127.0.0.1/relay/callback"
	flow, _ := beginBrowserFlow(t, spec)
	if flow.callbackPath != "/relay/callback" {
		t.Fatalf("callback path = %q, want the declared path", flow.callbackPath)
	}
	results := waitAsync(flow)
	state := stateFromURL(t, flow.URL())
	base := strings.TrimSuffix(flow.RedirectURI(), "/relay/callback")

	// A browser may ask for other paths first; tolerate them, require the path.
	browserGET(t, base+"/favicon.ico", http.StatusNotFound)
	browserGET(t, base+"/relay/callback?state="+url.QueryEscape(state)+"&code=the-code", http.StatusOK)

	result := <-results
	if result.failure != nil {
		t.Fatalf("Wait failed: %v", result.failure)
	}
	if calls := stub.calls(); calls != 1 {
		t.Fatalf("token endpoint called %d times, want 1", calls)
	}
}

func TestBrowserCloseReleasesTheListener(t *testing.T) {
	flow, bound := beginBrowserFlow(t, browserSpec("https://example.test/token"))

	if err := flow.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	address := bound.Addr().String()
	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err == nil {
		_ = connection.Close()
		t.Fatalf("the listener at %s was still accepting after Close", address)
	}
	if err := flow.Close(); err != nil {
		t.Fatalf("second Close = %v, want a no-op", err)
	}
}

func TestBrowserWaitHonoursTheCallerContext(t *testing.T) {
	flow, _ := beginBrowserFlow(t, browserSpec("https://example.test/token"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, failure := flow.Wait(ctx); failure == nil || failure.Code != relay.CodeTimeout {
		t.Fatalf("Wait = %v, want TIMEOUT after cancellation", failure)
	}
}

func TestBrowserBeginValidatesTheSpec(t *testing.T) {
	client := NewBrowserClient(BrowserOptions{})
	base := browserSpec("https://example.test/token")

	missing := []struct {
		name   string
		mutate func(*BrowserFlowSpec)
		field  string
	}{
		{"no authorization endpoint", func(s *BrowserFlowSpec) { s.AuthorizationEndpoint = "" }, "auth.authorizationEndpoint"},
		{"no token endpoint", func(s *BrowserFlowSpec) { s.TokenEndpoint = "" }, "auth.tokenEndpoint"},
		{"no client id", func(s *BrowserFlowSpec) { s.ClientID = "" }, "auth.clientId"},
	}
	for _, testCase := range missing {
		t.Run(testCase.name, func(t *testing.T) {
			spec := base
			testCase.mutate(&spec)
			_, failure := client.Begin(context.Background(), spec)
			if failure == nil || failure.Code != relay.CodeInvalidInput {
				t.Fatalf("Begin = %v, want INVALID_INPUT", failure)
			}
			if !strings.Contains(failure.Message, testCase.field) {
				t.Fatalf("message = %q, want it to name %s", failure.Message, testCase.field)
			}
		})
	}

	badRedirects := []string{
		"https://127.0.0.1/callback",
		"http://evil.example.test/callback",
		"http://127.0.0.1/callback?x=1",
		"http://127.0.0.1/callback#fragment",
		"http://127.0.0.1",
		"http://localhost",
	}
	for _, redirect := range badRedirects {
		t.Run("redirect "+redirect, func(t *testing.T) {
			spec := base
			spec.RedirectURI = redirect
			_, failure := client.Begin(context.Background(), spec)
			if failure == nil || failure.Code != relay.CodeInvalidInput {
				t.Fatalf("Begin = %v, want INVALID_INPUT for %s", failure, redirect)
			}
			if strings.Contains(failure.Message, redirect) {
				t.Fatalf("message = %q, must name the field not the value", failure.Message)
			}
		})
	}
}

// browserManifest opts into the browser grant: oauth2 with the authorization
// endpoint that selects it, plus the token endpoint and client id (spec §21).
const browserManifest = `apiVersion: relay/v1
kind: Tool
metadata:
  name: demo
  version: 1.0.0
auth:
  type: oauth2
  provider: example
  authorizationEndpoint: https://example.test/oauth/authorize
  tokenEndpoint: https://example.test/oauth/token
  clientId: client-123
  redirectURI: http://127.0.0.1:53682/callback
  scopes:
    - read
    - write
`

func TestBrowserFlowSpecFromManifestReadsTheAdditiveFields(t *testing.T) {
	spec, ok, failure := BrowserFlowSpecFromManifest([]byte(browserManifest))
	if failure != nil {
		t.Fatalf("BrowserFlowSpecFromManifest failed: %v", failure)
	}
	if !ok {
		t.Fatal("the manifest declares a browser flow but was not recognised")
	}
	if spec.AuthorizationEndpoint != "https://example.test/oauth/authorize" ||
		spec.TokenEndpoint != "https://example.test/oauth/token" ||
		spec.ClientID != "client-123" ||
		spec.RedirectURI != "http://127.0.0.1:53682/callback" {
		t.Fatalf("spec = %+v, want the manifest's endpoints, client id, and redirect", spec)
	}
	if len(spec.Scopes) != 2 || spec.Scopes[0] != "read" || spec.Scopes[1] != "write" {
		t.Fatalf("scopes = %v, want [read write]", spec.Scopes)
	}
}

func TestBrowserFlowSpecFromManifestStaysManualWithoutAnAuthorizationEndpoint(t *testing.T) {
	// Plain oauth2 and a device declaration both lack the authorization
	// endpoint; neither is a browser flow, and neither is an error.
	manifests := []string{
		`auth:
  type: oauth2
  provider: github
`,
		`auth:
  type: oauth2
  deviceAuthorizationEndpoint: https://example.test/device
  tokenEndpoint: https://example.test/token
  clientId: client-123
`,
	}
	for _, manifest := range manifests {
		if _, ok, failure := BrowserFlowSpecFromManifest([]byte(manifest)); ok || failure != nil {
			t.Fatalf("BrowserFlowSpecFromManifest = (%v, %v), want (false, nil)", ok, failure)
		}
	}
}

func TestBrowserFlowSpecFromManifestRefusesAPartialDeclaration(t *testing.T) {
	manifest := `auth:
  type: oauth2
  provider: example
  authorizationEndpoint: https://example.test/authorize
  clientId: client-123
`
	_, ok, failure := BrowserFlowSpecFromManifest([]byte(manifest))
	if ok || failure == nil || failure.Code != relay.CodeInvalidInput {
		t.Fatalf("partial declaration = (%v, %v), want INVALID_INPUT", ok, failure)
	}
	if !strings.Contains(failure.Message, "auth.tokenEndpoint") {
		t.Fatalf("message = %q, want it to name the missing field", failure.Message)
	}
}

func TestBrowserFlowSpecFromManifestDoesNotEchoAMalformedManifest(t *testing.T) {
	manifest := "auth:\n\ttype: oauth2\n\tclientId: super-secret-client\n"
	_, ok, failure := BrowserFlowSpecFromManifest([]byte(manifest))
	if ok || failure == nil || failure.Code != relay.CodeRuntimeIncompatible {
		t.Fatalf("malformed manifest = (%v, %v), want RUNTIME_INCOMPATIBLE", ok, failure)
	}
	if strings.Contains(failure.Message, "super-secret-client") {
		t.Fatalf("message leaked the manifest body: %q", failure.Message)
	}
}

func TestDeviceReaderDoesNotMisreadABrowserManifest(t *testing.T) {
	// A browser manifest also declares tokenEndpoint and clientId, which used
	// to make the device reader report a broken device flow. It must instead
	// return ok=false with no error (spec §21, §59).
	spec, ok, failure := DeviceFlowSpecFromManifest([]byte(browserManifest))
	if failure != nil || ok {
		t.Fatalf("DeviceFlowSpecFromManifest(browser manifest) = (%+v, %v, %v), want (false, nil)", spec, ok, failure)
	}
}
