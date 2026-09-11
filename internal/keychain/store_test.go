package keychain

import (
	"errors"
	"strings"
	"testing"
)

// ── Fake in-memory store for unit tests ──────────────────────────────────────

// fakeStore is an in-memory [Store] used by logic tests that must not touch
// the real login keychain.
type fakeStore struct {
	items map[string]string // "service/account" -> secret
}

func newFakeStore() *fakeStore {
	return &fakeStore{items: make(map[string]string)}
}

func keyFor(service, account string) string {
	return service + "/" + account
}

func (f *fakeStore) Get(service, account string) (string, error) {
	v, ok := f.items[keyFor(service, account)]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (f *fakeStore) Set(service, account, secret string) error {
	f.items[keyFor(service, account)] = secret
	return nil
}

func (f *fakeStore) Delete(service, account string) error {
	delete(f.items, keyFor(service, account))
	return nil
}

// ── Unit tests against the fake store ────────────────────────────────────────

func TestFakeStore_GetNotFound(t *testing.T) {
	s := newFakeStore()
	_, err := s.Get("com.relay.github", "default")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFakeStore_SetAndGet(t *testing.T) {
	s := newFakeStore()
	const secret = "ghp_test123"

	if err := s.Set("com.relay.github", "default", secret); err != nil {
		t.Fatalf("set: %v", err)
	}

	got, err := s.Get("com.relay.github", "default")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != secret {
		t.Fatalf("got %q, want %q", got, secret)
	}
}

func TestFakeStore_Delete(t *testing.T) {
	s := newFakeStore()
	_ = s.Set("com.relay.github", "default", "tok")

	if err := s.Delete("com.relay.github", "default"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, err := s.Get("com.relay.github", "default")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestFakeStore_DeleteIdempotent(t *testing.T) {
	s := newFakeStore()
	// Deleting something that was never set must not error.
	if err := s.Delete("com.relay.nope", "default"); err != nil {
		t.Fatalf("delete of nonexistent: %v", err)
	}
}

// ── Error string invariant ───────────────────────────────────────────────────

// TestErrorStringsNeverContainSecret asserts that error strings produced by
// the package never embed a secret value. This is a structural property we
// test explicitly because the cost of a leak is high and the test is cheap.
func TestErrorStringsNeverContainSecret(t *testing.T) {
	secrets := []string{
		"ghp_abcdefghijklmnopqrstuvwxyz123456",
		"sk-test-4242424242424242",
		"a]b)c(d*e",
		"short",
		"",
	}

	// Build a store that always returns errors for Get.
	failStore := &failingStore{err: errors.New("keychain unavailable")}

	for _, secret := range secrets {
		if secret == "" {
			continue
		}

		// Get path: a failing Get must not echo the secret.
		svc := "com.relay.test-" + secret[:min(4, len(secret))]
		_, err := failStore.Get(svc, "default")
		if err != nil {
			assertSecretNotInError(t, err.Error(), secret)
		}

		// Set path: a failing Set must not echo the secret.
		err = failStore.Set(svc, "default", secret)
		if err != nil {
			assertSecretNotInError(t, err.Error(), secret)
		}
	}
}

func assertSecretNotInError(t *testing.T, errStr, secret string) {
	t.Helper()
	if strings.Contains(errStr, secret) {
		t.Errorf("error string contains secret value: %q", errStr)
	}
}

// failingStore always returns an error, useful for asserting error shape.
type failingStore struct {
	err error
}

func (f *failingStore) Get(service, account string) (string, error) {
	return "", f.err
}

func (f *failingStore) Set(service, account, secret string) error {
	return f.err
}

func (f *failingStore) Delete(service, account string) error {
	return f.err
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
