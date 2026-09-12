package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"relay/internal/keychain"
	"relay/pkg/relay"
)

// The envelope is the backward-compatibility boundary: a pasted token must
// decode to ok=false and be used verbatim, and an envelope must round-trip
// without losing the refresh token a background refresh needs (spec §21, §59).

func TestStoredTokenEnvelopeRoundTrips(t *testing.T) {
	original := StoredToken{
		Version:       StoredTokenVersion,
		AccessToken:   "access-token",
		RefreshToken:  "refresh-token",
		TokenType:     "bearer",
		Scope:         "repo read:user",
		ExpiresAt:     "2026-09-11T18:04:05Z",
		TokenEndpoint: "https://example.test/token",
		ClientID:      "client-123",
		Flow:          FlowBrowser,
	}

	encoded, failure := EncodeStoredToken(original)
	if failure != nil {
		t.Fatalf("EncodeStoredToken failed: %v", failure)
	}

	// The version key is what identifies the envelope on the way back in.
	var raw map[string]any
	if err := json.Unmarshal([]byte(encoded), &raw); err != nil {
		t.Fatalf("the envelope is not JSON: %v", err)
	}
	if raw["relay_oauth2"] != float64(StoredTokenVersion) {
		t.Fatalf("relay_oauth2 = %v, want %d", raw["relay_oauth2"], StoredTokenVersion)
	}
	if _, present := raw["access_token"]; !present {
		t.Fatal("the envelope has no access_token key")
	}

	decoded, ok := DecodeStoredToken(encoded)
	if !ok {
		t.Fatal("DecodeStoredToken rejected the envelope it just produced")
	}
	if decoded != original {
		t.Fatalf("round trip = %+v, want %+v", decoded, original)
	}
	if decoded.AccessValue() != "access-token" {
		t.Fatalf("AccessValue = %q, want the access token", decoded.AccessValue())
	}
}

func TestEncodeStoredTokenRefusesAnEmptyAccessToken(t *testing.T) {
	if _, failure := EncodeStoredToken(StoredToken{RefreshToken: "refresh"}); failure == nil || failure.Code != relay.CodeInvalidInput {
		t.Fatalf("EncodeStoredToken(no access token) = %v, want INVALID_INPUT", failure)
	}
}

func TestDecodeStoredTokenRejectsNonEnvelopes(t *testing.T) {
	wellFormed, failure := EncodeStoredToken(StoredToken{AccessToken: "a", RefreshToken: "r"})
	if failure != nil {
		t.Fatalf("EncodeStoredToken failed: %v", failure)
	}

	cases := []struct {
		name  string
		value string
	}{
		{"pasted plaintext token", "ghp_aPastedTokenValue"},
		{"empty", ""},
		{"json with the wrong version", ` {"relay_oauth2":2,"access_token":"a"} `},
		{"envelope missing access_token", ` {"relay_oauth2":1,"refresh_token":"r"} `},
		{"json that is not an object", ` ["relay_oauth2"] `},
		{"object without the version key", ` {"access_token":"a"} `},
		{"truncated json", ` {"relay_oauth2":1 `},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, ok := DecodeStoredToken(testCase.value); ok {
				t.Fatalf("DecodeStoredToken(%q) = ok, want not-an-envelope", testCase.value)
			}
		})
	}

	// Sanity check the fixture actually is an envelope, so the test above
	// cannot pass by rejecting everything.
	if _, ok := DecodeStoredToken(wellFormed); !ok {
		t.Fatal("a well-formed envelope was rejected")
	}
}

func TestPastedTokenIsUsedVerbatim(t *testing.T) {
	const pasted = "ghp_aPastedTokenValue"
	if got := encodeSecret("oauth2", pasted); got != pasted {
		t.Fatalf("encodeSecret(oauth2, pasted) = %q, want the pasted value verbatim", got)
	}
	if got := encodeSecret("bearer", pasted); got != pasted {
		t.Fatalf("encodeSecret(bearer, pasted) = %q, want the pasted value verbatim", got)
	}

	store := newFakeStore()
	if err := store.Set(keychain.Service("demo"), keychain.AccountDefault, pasted); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	credential, failure := New(store).Resolve(docWithAuth("oauth2", "example"), "demo")
	if failure != nil {
		t.Fatalf("Resolve failed: %v", failure)
	}
	if credential.Secret != pasted {
		t.Fatalf("credential.Secret = %q, want the pasted token", credential.Secret)
	}
}

func TestEnvelopeAccessTokenIsUsedOnTheWire(t *testing.T) {
	encoded, failure := EncodeStoredToken(StoredToken{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
	})
	if failure != nil {
		t.Fatalf("EncodeStoredToken failed: %v", failure)
	}
	store := newFakeStore()
	if err := store.Set(keychain.Service("demo"), keychain.AccountDefault, encoded); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	credential, failure := New(store).Resolve(docWithAuth("oauth2", "example"), "demo")
	if failure != nil {
		t.Fatalf("Resolve failed: %v", failure)
	}
	if credential.Secret != "access-token" {
		t.Fatalf("credential.Secret = %q, want the envelope's access token", credential.Secret)
	}
}

func TestResolverStoredReadsTheEnvelope(t *testing.T) {
	encoded, failure := EncodeStoredToken(StoredToken{AccessToken: "access-token", RefreshToken: "refresh-token", Flow: FlowDevice})
	if failure != nil {
		t.Fatalf("EncodeStoredToken failed: %v", failure)
	}
	store := newFakeStore()
	if err := store.Set(keychain.Service("demo"), keychain.AccountDefault, encoded); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	resolver := New(store)

	stored, ok, failure := resolver.Stored("demo")
	if failure != nil || !ok {
		t.Fatalf("Stored(envelope) = (%+v, %v, %v), want an envelope", stored, ok, failure)
	}
	if stored.RefreshToken != "refresh-token" || stored.Flow != FlowDevice {
		t.Fatalf("stored = %+v, want the decoded envelope", stored)
	}

	// A pasted token is not an envelope: ok false, no error.
	_ = store.Set(keychain.Service("pasted"), keychain.AccountDefault, "ghp_pasted")
	if _, ok, failure := resolver.Stored("pasted"); ok || failure != nil {
		t.Fatalf("Stored(pasted) = (%v, %v), want (false, nil)", ok, failure)
	}
	// Nothing stored: ok false, no error.
	if _, ok, failure := resolver.Stored("absent"); ok || failure != nil {
		t.Fatalf("Stored(absent) = (%v, %v), want (false, nil)", ok, failure)
	}
}

func TestResolverStoredScrubsReadErrors(t *testing.T) {
	const secret = "top-secret-value"
	store := &leakyGetStore{err: errors.New("keychain exploded holding " + secret), value: secret}
	_, ok, failure := New(store).Stored("demo")
	if ok || failure == nil || failure.Code != relay.CodeAuthFailed {
		t.Fatalf("Stored(store error) = (%v, %v), want (false, AUTH_FAILED)", ok, failure)
	}
	if strings.Contains(failure.Message, secret) {
		t.Fatalf("message leaked the secret: %q", failure.Message)
	}
}

func TestNewStoredTokenConvertsExpiryAndKeepsContext(t *testing.T) {
	before := time.Now()
	stored := NewStoredToken(Token{AccessToken: "a", RefreshToken: "r", ExpiresIn: 90 * time.Second},
		FlowBrowser, "https://example.test/token", "client-123")
	after := time.Now()

	if stored.Version != StoredTokenVersion {
		t.Fatalf("Version = %d, want %d", stored.Version, StoredTokenVersion)
	}
	if stored.Flow != FlowBrowser || stored.TokenEndpoint != "https://example.test/token" || stored.ClientID != "client-123" {
		t.Fatalf("context = %+v, want the supplied flow, endpoint, and client id", stored)
	}
	expiresAt, err := time.Parse(time.RFC3339, stored.ExpiresAt)
	if err != nil {
		t.Fatalf("ExpiresAt = %q, want RFC3339: %v", stored.ExpiresAt, err)
	}
	low := before.Add(90 * time.Second).Truncate(time.Second)
	if expiresAt.Before(low) || expiresAt.After(after.Add(90*time.Second)) {
		t.Fatalf("ExpiresAt = %v, want now+90s", expiresAt)
	}

	unknown := NewStoredToken(Token{AccessToken: "a"}, FlowDevice, "e", "c")
	if unknown.ExpiresAt != "" {
		t.Fatalf("ExpiresAt = %q, want empty when the server declared no expiry", unknown.ExpiresAt)
	}
}

func TestStoredTokenExpiring(t *testing.T) {
	now := time.Date(2026, 9, 11, 18, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		expiresAt string
		skew      time.Duration
		want      bool
	}{
		{"unknown expiry never expires", "", time.Minute, false},
		{"future expiry is live", now.Add(time.Hour).Format(time.RFC3339), time.Minute, false},
		{"past expiry has expired", now.Add(-time.Hour).Format(time.RFC3339), time.Minute, true},
		{"inside the skew window", now.Add(30 * time.Second).Format(time.RFC3339), time.Minute, true},
		{"unreadable expiry is treated as unknown", "not-a-time", time.Minute, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			token := StoredToken{ExpiresAt: testCase.expiresAt}
			if got := token.Expiring(now, testCase.skew); got != testCase.want {
				t.Fatalf("Expiring(%q, %v) = %v, want %v", testCase.expiresAt, testCase.skew, got, testCase.want)
			}
		})
	}
}

func TestStoredTokenDescribeIsNonSecretAndHonest(t *testing.T) {
	token := StoredToken{
		AccessToken:  "access-token-secret",
		RefreshToken: "refresh-token-secret",
		Scope:        "repo read:user",
		ExpiresAt:    "2026-09-11T18:04:05Z",
		Flow:         FlowBrowser,
	}
	got := token.Describe()
	want := "oauth2 browser grant, expires 2026-09-11T18:04:05Z, scope repo read:user, refreshable"
	if got != want {
		t.Fatalf("Describe = %q, want %q", got, want)
	}
	for _, secret := range []string{token.AccessToken, token.RefreshToken} {
		if strings.Contains(got, secret) {
			t.Fatalf("Describe leaked %q: %q", secret, got)
		}
	}

	unknown := StoredToken{AccessToken: "a", Flow: FlowDevice}.Describe()
	if !strings.Contains(unknown, "expiry unknown") {
		t.Fatalf("Describe = %q, want it to say the expiry is unknown", unknown)
	}
	if strings.Contains(unknown, "0001-01-01") {
		t.Fatalf("Describe = %q, must not print a zero date for an unknown expiry", unknown)
	}
	if strings.Contains(unknown, "refreshable") {
		t.Fatalf("Describe = %q, must not claim refreshability without a refresh token", unknown)
	}
}

// refreshStub is a token endpoint that records what the client sent and replies
// with a fixed status and body.
type refreshStub struct {
	server *httptest.Server
	mu     sync.Mutex
	forms  []url.Values
	status int
	body   string
}

func newRefreshStub(t *testing.T, status int, body string) *refreshStub {
	t.Helper()
	stub := &refreshStub{status: status, body: body}
	stub.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = request.ParseForm()
		stub.mu.Lock()
		stub.forms = append(stub.forms, request.PostForm)
		stub.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(stub.status)
		_, _ = writer.Write([]byte(stub.body))
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *refreshStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.forms)
}

func (s *refreshStub) lastForm() url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.forms) == 0 {
		return nil
	}
	return s.forms[len(s.forms)-1]
}

func refreshFixture() StoredToken {
	return StoredToken{
		Version:       StoredTokenVersion,
		AccessToken:   "old-access-token",
		RefreshToken:  "old-refresh-token",
		TokenType:     "bearer",
		Scope:         "read",
		ExpiresAt:     "2026-09-11T17:00:00Z",
		TokenEndpoint: "https://example.test/token",
		ClientID:      "client-123",
		Flow:          FlowBrowser,
	}
}

func TestRefreshSendsTheDocumentedRequest(t *testing.T) {
	stub := newRefreshStub(t, http.StatusOK,
		` {"access_token":"fresh","token_type":"bearer","expires_in":3600,"refresh_token":"rotated"} `)
	now := time.Date(2026, 9, 11, 18, 0, 0, 0, time.UTC)
	client := NewRefreshClient(RefreshOptions{Now: func() time.Time { return now }})
	spec := RefreshSpec{Tool: "demo", TokenEndpoint: stub.server.URL, ClientID: "client-123"}

	refreshed, failure := client.Refresh(context.Background(), spec, refreshFixture())
	if failure != nil {
		t.Fatalf("Refresh failed: %v", failure)
	}
	if stub.callCount() != 1 {
		t.Fatalf("token endpoint called %d times, want 1", stub.callCount())
	}
	form := stub.lastForm()
	if form.Get("grant_type") != "refresh_token" {
		t.Fatalf("grant_type = %q, want refresh_token", form.Get("grant_type"))
	}
	if form.Get("refresh_token") != "old-refresh-token" {
		t.Fatal("the previous refresh token was not sent")
	}
	if form.Get("client_id") != "client-123" {
		t.Fatalf("client_id = %q, want client-123", form.Get("client_id"))
	}
	if form.Get("scope") != "read" {
		t.Fatalf("scope = %q, want the previously granted scope", form.Get("scope"))
	}
	if refreshed.AccessToken != "fresh" {
		t.Fatalf("access token = %q, want the server's new one", refreshed.AccessToken)
	}
	if refreshed.Flow != FlowBrowser || refreshed.TokenEndpoint != spec.TokenEndpoint || refreshed.ClientID != spec.ClientID {
		t.Fatalf("context = %+v, want the flow and spec context carried forward", refreshed)
	}
}

func TestRefreshEnvelopeOutcomes(t *testing.T) {
	now := time.Date(2026, 9, 11, 18, 0, 0, 0, time.UTC)
	wantExpiry := now.Add(time.Hour).UTC().Format(time.RFC3339)

	cases := []struct {
		name        string
		status      int
		body        string
		wantRefresh string
		wantExpiry  string
		wantType    string
		wantScope   string
		wantCode    relay.Code
	}{
		{
			name:        "rotation carried forward when the server omits a token",
			status:      http.StatusOK,
			body:        ` {"access_token":"fresh","token_type":"bearer","expires_in":3600} `,
			wantRefresh: "old-refresh-token",
			wantExpiry:  wantExpiry,
			wantType:    "bearer",
			wantScope:   "read",
		},
		{
			name:        "a rotated token replaces the old one",
			status:      http.StatusOK,
			body:        ` {"access_token":"fresh","expires_in":3600,"refresh_token":"new-refresh"} `,
			wantRefresh: "new-refresh",
			wantExpiry:  wantExpiry,
			wantType:    "bearer",
			wantScope:   "read",
		},
		{
			name:        "expiry unknown when the server omits expires_in",
			status:      http.StatusOK,
			body:        ` {"access_token":"fresh"} `,
			wantRefresh: "old-refresh-token",
			wantExpiry:  "",
			wantType:    "bearer",
			wantScope:   "read",
		},
		{
			name:     "invalid_grant requires a new login",
			status:   http.StatusBadRequest,
			body:     ` {"error":"invalid_grant"} `,
			wantCode: relay.CodeAuthRequired,
		},
		{
			name:     "other oauth2 errors fail auth",
			status:   http.StatusBadRequest,
			body:     ` {"error":"invalid_client"} `,
			wantCode: relay.CodeAuthFailed,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stub := newRefreshStub(t, testCase.status, testCase.body)
			client := NewRefreshClient(RefreshOptions{Now: func() time.Time { return now }})
			spec := RefreshSpec{Tool: "demo", TokenEndpoint: stub.server.URL, ClientID: "client-123"}

			refreshed, failure := client.Refresh(context.Background(), spec, refreshFixture())
			if testCase.wantCode != "" {
				if failure == nil || failure.Code != testCase.wantCode {
					t.Fatalf("Refresh = %v, want code %s", failure, testCase.wantCode)
				}
				return
			}
			if failure != nil {
				t.Fatalf("Refresh failed: %v", failure)
			}
			if refreshed.RefreshToken != testCase.wantRefresh {
				t.Fatalf("refresh token = %q, want %q", refreshed.RefreshToken, testCase.wantRefresh)
			}
			if refreshed.ExpiresAt != testCase.wantExpiry {
				t.Fatalf("expires_at = %q, want %q", refreshed.ExpiresAt, testCase.wantExpiry)
			}
			if refreshed.TokenType != testCase.wantType || refreshed.Scope != testCase.wantScope {
				t.Fatalf("token_type/scope = %q/%q, want %q/%q",
					refreshed.TokenType, refreshed.Scope, testCase.wantType, testCase.wantScope)
			}
		})
	}
}

func TestRefreshInvalidGrantNeverEchoesTheToken(t *testing.T) {
	// A hostile or careless server reflects the refresh token in its
	// description; the scrubbing must still keep it out of the error.
	stub := newRefreshStub(t, http.StatusBadRequest,
		` {"error":"invalid_grant","error_description":"refresh old-refresh-token rejected"} `)
	client := NewRefreshClient(RefreshOptions{Now: time.Now})
	spec := RefreshSpec{Tool: "demo", TokenEndpoint: stub.server.URL, ClientID: "client-123"}

	_, failure := client.Refresh(context.Background(), spec, refreshFixture())
	if failure == nil || failure.Code != relay.CodeAuthRequired {
		t.Fatalf("Refresh = %v, want AUTH_REQUIRED", failure)
	}
	if !strings.Contains(failure.Message, "relay auth login") {
		t.Fatalf("message = %q, want it to tell the human to log in", failure.Message)
	}
	if strings.Contains(failure.Message, "old-refresh-token") {
		t.Fatalf("message leaked the refresh token: %q", failure.Message)
	}
	if !strings.Contains(failure.Message, "[REDACTED]") {
		t.Fatalf("message = %q, want the reflected token redacted", failure.Message)
	}
}

func TestRefreshRequiresARefreshToken(t *testing.T) {
	client := NewRefreshClient(RefreshOptions{})
	spec := RefreshSpec{Tool: "demo", TokenEndpoint: "https://example.test/token", ClientID: "client-123"}
	previous := refreshFixture()
	previous.RefreshToken = ""

	_, failure := client.Refresh(context.Background(), spec, previous)
	if failure == nil || failure.Code != relay.CodeAuthRequired {
		t.Fatalf("Refresh = %v, want AUTH_REQUIRED with no refresh token", failure)
	}
}

func TestRefreshRejectsAnIncompleteSpec(t *testing.T) {
	client := NewRefreshClient(RefreshOptions{})
	_, failure := client.Refresh(context.Background(), RefreshSpec{Tool: "demo", ClientID: "client-123"}, refreshFixture())
	if failure == nil || failure.Code != relay.CodeInvalidInput {
		t.Fatalf("Refresh = %v, want INVALID_INPUT", failure)
	}
	if !strings.Contains(failure.Message, "auth.tokenEndpoint") {
		t.Fatalf("message = %q, want it to name the missing field", failure.Message)
	}
}
