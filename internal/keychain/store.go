package keychain

import "errors"

// ErrNotFound signals that no credential is stored for the requested
// service+account pair. Callers should map this to AUTH_REQUIRED rather
// than treating it as a hard failure — the user may simply need to log in
// for the first time.
var ErrNotFound = errors.New("no credential stored")

// Store abstracts credential persistence so the daemon depends on an
// abstraction rather than the OS (spec §21, §40). The macOS Keychain
// implementation lives in this same package; callers that need test
// doubles can provide their own implementation.
type Store interface {
	// Get retrieves the secret for the given service and account.
	// It returns ErrNotFound when no entry exists.
	Get(service, account string) (string, error)

	// Set upserts a credential. It must never embed the secret in any
	// returned error.
	Set(service, account, secret string) error

	// Delete removes a credential. It is not an error to delete something
	// that does not exist.
	Delete(service, account string) error
}
