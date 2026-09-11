package auth

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"relay/internal/keychain"
	"relay/internal/manifest"
	"relay/pkg/relay"
)

// fakeStore is an in-memory keychain.Store for tests, plus knobs to force
// failures without a real Keychain.
type fakeStore struct {
	values map[string]string
	getErr error
	setErr error
	delErr error
}

func newFakeStore() *fakeStore { return &fakeStore{values: map[string]string{}} }

func fakeKey(service, account string) string { return service + "\x00" + account }

func (f *fakeStore) Get(service, account string) (string, error) {
	if f.getErr != nil {
		return "", f.getErr
	}
	value, ok := f.values[fakeKey(service, account)]
	if !ok {
		return "", keychain.ErrNotFound
	}
	return value, nil
}

func (f *fakeStore) Set(service, account, secret string) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.values[fakeKey(service, account)] = secret
	return nil
}

func (f *fakeStore) Delete(service, account string) error {
	if f.delErr != nil {
		return f.delErr
	}
	delete(f.values, fakeKey(service, account))
	return nil
}

func docWithAuth(authType, provider string) *manifest.Document {
	return &manifest.Document{Auth: &manifest.Auth{Type: authType, Provider: provider}}
}

func TestResolveMapsEachTypeToItsPresentation(t *testing.T) {
	basic := base64.StdEncoding.EncodeToString([]byte("user:pass"))
	client := base64.StdEncoding.EncodeToString([]byte("client-id:client-secret"))

	cases := []struct {
		name       string
		authType   string
		secret     string
		wantHeader string
		wantScheme string
		wantSecret string
	}{
		{"api key", "api_key", "key-123", "X-API-Key", "", "key-123"},
		{"bearer", "bearer", "token-abc", "Authorization", "Bearer", "token-abc"},
		{"bearer token alias", "bearer_token", "token-abc", "Authorization", "Bearer", "token-abc"},
		{"basic", "basic", "user:pass", "Authorization", "Basic", basic},
		{"client credentials", "client_credentials", "client-id:client-secret", "Authorization", "Basic", client},
		{"oauth2", "oauth2", "access-token", "Authorization", "Bearer", "access-token"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			if err := store.Set(keychain.Service("demo"), keychain.AccountDefault, tc.secret); err != nil {
				t.Fatalf("seed store: %v", err)
			}

			credential, failure := New(store).Resolve(docWithAuth(tc.authType, "example"), "demo")
			if failure != nil {
				t.Fatalf("Resolve returned %v, want success", failure)
			}
			if credential == nil {
				t.Fatal("Resolve returned a nil credential for a stored secret")
			}
			if credential.Header != tc.wantHeader {
				t.Errorf("Header = %q, want %q", credential.Header, tc.wantHeader)
			}
			if credential.Scheme != tc.wantScheme {
				t.Errorf("Scheme = %q, want %q", credential.Scheme, tc.wantScheme)
			}
			if credential.Secret != tc.wantSecret {
				t.Errorf("Secret = %q, want %q", credential.Secret, tc.wantSecret)
			}
		})
	}
}

func TestResolveNoAuthIsUnaffected(t *testing.T) {
	resolver := New(newFakeStore())
	for _, doc := range []*manifest.Document{
		nil,
		{},
		{Auth: &manifest.Auth{Type: ""}},
	} {
		credential, failure := resolver.Resolve(doc, "demo")
		if failure != nil {
			t.Fatalf("Resolve(no auth) returned %v, want no error", failure)
		}
		if credential != nil {
			t.Fatalf("Resolve(no auth) returned a credential, want nil")
		}
	}
}

func TestResolveMissingCredentialRequiresAuth(t *testing.T) {
	_, failure := New(newFakeStore()).Resolve(docWithAuth("bearer", "example"), "demo")
	if failure == nil {
		t.Fatal("Resolve with no stored credential succeeded, want AUTH_REQUIRED")
	}
	if failure.Code != relay.CodeAuthRequired {
		t.Fatalf("code = %s, want %s", failure.Code, relay.CodeAuthRequired)
	}
	if failure.Details["authType"] != "bearer" {
		t.Fatalf("details = %v, want authType bearer", failure.Details)
	}
}

func TestResolveUnknownTypeFailsAuth(t *testing.T) {
	store := newFakeStore()
	_ = store.Set(keychain.Service("demo"), keychain.AccountDefault, "secret")
	_, failure := New(store).Resolve(docWithAuth("mtls", "example"), "demo")
	if failure == nil || failure.Code != relay.CodeAuthFailed {
		t.Fatalf("Resolve(unknown type) = %v, want AUTH_FAILED", failure)
	}
}

func TestResolveScrubsSecretFromStoreError(t *testing.T) {
	const secret = "top-secret-value"
	// A faulty store that returns the value alongside its error, and echoes the
	// value in the error text, must still not leak it into the resolver's error
	// (spec §22).
	store := &leakyGetStore{err: errors.New("keychain exploded holding " + secret), value: secret}

	_, failure := New(store).Resolve(docWithAuth("bearer", "example"), "demo")
	if failure == nil || failure.Code != relay.CodeAuthFailed {
		t.Fatalf("Resolve(store error) = %v, want AUTH_FAILED", failure)
	}
	if strings.Contains(failure.Message, secret) {
		t.Fatalf("error message leaked the secret: %q", failure.Message)
	}
}

// leakyGetStore violates the Store contract by returning the secret together
// with an error, so tests can prove the resolver scrubs it defensively.
type leakyGetStore struct {
	value string
	err   error
}

func (l *leakyGetStore) Get(string, string) (string, error) { return l.value, l.err }
func (l *leakyGetStore) Set(string, string, string) error   { return nil }
func (l *leakyGetStore) Delete(string, string) error        { return nil }

func TestPresentReportsPresenceOnly(t *testing.T) {
	store := newFakeStore()
	resolver := New(store)

	stored, failure := resolver.Present("demo")
	if failure != nil || stored {
		t.Fatalf("Present(empty) = (%v, %v), want (false, nil)", stored, failure)
	}

	if err := store.Set(keychain.Service("demo"), keychain.AccountDefault, "secret"); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	stored, failure = resolver.Present("demo")
	if failure != nil || !stored {
		t.Fatalf("Present(stored) = (%v, %v), want (true, nil)", stored, failure)
	}
}

func TestSetAndClear(t *testing.T) {
	store := newFakeStore()
	resolver := New(store)

	if failure := resolver.Set("demo", "secret"); failure != nil {
		t.Fatalf("Set returned %v", failure)
	}
	if value, _ := store.Get(keychain.Service("demo"), keychain.AccountDefault); value != "secret" {
		t.Fatalf("stored value = %q, want %q", value, "secret")
	}

	if failure := resolver.Set("demo", ""); failure == nil || failure.Code != relay.CodeInvalidInput {
		t.Fatalf("Set(empty) = %v, want INVALID_INPUT", failure)
	}

	if failure := resolver.Clear("demo"); failure != nil {
		t.Fatalf("Clear returned %v", failure)
	}
	if _, err := store.Get(keychain.Service("demo"), keychain.AccountDefault); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("after Clear, Get = %v, want ErrNotFound", err)
	}
	// Clearing again is a no-op, not an error.
	if failure := resolver.Clear("demo"); failure != nil {
		t.Fatalf("second Clear returned %v, want nil", failure)
	}
}

func TestSetScrubsSecretFromStoreError(t *testing.T) {
	const secret = "top-secret-value"
	store := newFakeStore()
	store.setErr = errors.New("could not write " + secret)

	failure := New(store).Set("demo", secret)
	if failure == nil || failure.Code != relay.CodeAuthFailed {
		t.Fatalf("Set(store error) = %v, want AUTH_FAILED", failure)
	}
	if strings.Contains(failure.Message, secret) {
		t.Fatalf("error message leaked the secret: %q", failure.Message)
	}
}

func TestDeclaredAndCanonical(t *testing.T) {
	if _, ok := Declared(nil); ok {
		t.Fatal("Declared(nil) = true, want false")
	}
	if _, ok := Declared(&manifest.Document{Auth: &manifest.Auth{Type: "bearer"}}); !ok {
		t.Fatal("Declared(bearer) = false, want true")
	}
	if Canonical(" Bearer_Token ") != "bearer" {
		t.Fatalf("Canonical alias = %q, want bearer", Canonical(" Bearer_Token "))
	}
	if !Supported("oauth2") || Supported("mtls") {
		t.Fatal("Supported() misclassified a known or unknown auth type")
	}
}
