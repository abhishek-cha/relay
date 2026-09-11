package keychain

import (
	"errors"
	"os"
	"testing"
)

// RELAY_TEST_KEYCHAIN must be set to "1" (or any truthy value) to run tests
// that touch the real macOS login keychain. These tests are skipped by
// default so that CI and ordinary go test ./... never pollute the user keychain.
//
// Run with:
//
//	RELAY_TEST_KEYCHAIN=1 go test ./internal/keychain/ -v -run TestSecurity
func requireRealKeychain(t *testing.T) {
	t.Helper()
	if os.Getenv("RELAY_TEST_KEYCHAIN") == "" {
		t.Skip("RELAY_TEST_KEYCHAIN not set; skipping real keychain test")
	}
}

func TestSecurityStore_SetGetDelete(t *testing.T) {
	requireRealKeychain(t)

	store := &SecurityStore{}
	svc := Service("relay-test-integration")
	const account = "test-account"
	const secret = "integration-test-secret-value"

	// Clean up before and after.
	_ = store.Delete(svc, account)
	defer store.Delete(svc, account)

	// Set
	if err := store.Set(svc, account, secret); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Get
	got, err := store.Get(svc, account)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != secret {
		t.Fatalf("Get returned %q, want %q", got, secret)
	}

	// Delete
	if err := store.Delete(svc, account); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Confirm gone
	_, err = store.Get(svc, account)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestSecurityStore_GetNotFound(t *testing.T) {
	requireRealKeychain(t)

	store := &SecurityStore{}
	_, err := store.Get("com.relay.nonexistent-test-tool-xyzzy", "nobody")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing item, got %v", err)
	}
}

func TestSecurityStore_DeleteIdempotent(t *testing.T) {
	requireRealKeychain(t)

	store := &SecurityStore{}
	// Deleting something that does not exist must not error.
	if err := store.Delete("com.relay.never-existed", "ghost"); err != nil {
		t.Fatalf("Delete of nonexistent: %v", err)
	}
}
