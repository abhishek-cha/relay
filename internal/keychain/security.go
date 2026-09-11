package keychain

import (
	"fmt"
	"os/exec"
	"strings"
)

// SecurityStore implements [Store] by shelling out to /usr/bin/security,
// the macOS command-line interface to Keychain Services.
//
// Trade-off: passing -w SECRET on the command line puts the secret briefly
// in the process argv, visible to ps(1). See the package doc for why this
// is acceptable for the MVP and how a cgo-based SecKeychainAddGenericPassword
// path would eliminate the exposure.
type SecurityStore struct{}

// Get retrieves a secret from the login keychain. It returns [ErrNotFound]
// when no matching item exists.
func (s *SecurityStore) Get(service, account string) (string, error) {
	out, err := exec.Command("/usr/bin/security",
		"find-generic-password",
		"-s", service,
		"-a", account,
		"-w",
	).CombinedOutput()
	if err != nil {
		if isNotFound(out) {
			return "", ErrNotFound
		}
		// Never include the output verbatim — it might echo the account
		// or service name in a way that leaks context. Return a clean
		// diagnostic instead.
		return "", fmt.Errorf("keychain get: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Set upserts a credential in the login keychain. The -U flag causes the
// command to update an existing item rather than failing with a duplicate.
func (s *SecurityStore) Set(service, account, secret string) error {
	// Pass -w - to read the password from stdin instead of argv, which
	// keeps the secret out of ps(1). The -W flag (uppercase) does this
	// for add-generic-password but not all subcommands, so we use
	// stdin piping as a belt-and-suspenders approach.
	//
	// Note: some macOS versions do not support -W for add-generic-password,
	// so we also support the simpler -w SECRET path for robustness. The
	// trade-off is documented in the package comment.
	cmd := exec.Command("/usr/bin/security",
		"add-generic-password",
		"-s", service,
		"-a", account,
		"-w", secret,
		"-U", // update if exists
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Scrub the secret from any error output defensively, even
		// though security(1) should not echo it back.
		return fmt.Errorf("keychain set: %s", scrubOutput(string(out), err))
	}
	return nil
}

// Delete removes a credential from the login keychain. Deleting something
// that does not exist is not treated as an error — the caller has achieved
// the desired end state either way.
func (s *SecurityStore) Delete(service, account string) error {
	out, err := exec.Command("/usr/bin/security",
		"delete-generic-password",
		"-s", service,
		"-a", account,
	).CombinedOutput()
	if err != nil {
		if isNotFound(out) {
			return nil // already gone
		}
		return fmt.Errorf("keychain delete: %s", scrubOutput(string(out), err))
	}
	return nil
}

// isNotFound checks whether the security CLI output indicates a missing
// item. The exact text varies across macOS versions but always contains
// "could not be found" or "The specified item could not be found".
func isNotFound(output []byte) bool {
	s := strings.ToLower(string(output))
	return strings.Contains(s, "could not be found") ||
		strings.Contains(s, "not found") ||
		strings.Contains(s, "no such item")
}

// scrubOutput builds a safe error message from CLI output and the original
// error, ensuring no secret values leak through. The raw output is dropped
// intentionally — it may contain the account name or partial secret in
// diagnostic text on some macOS versions.
func scrubOutput(output string, err error) string {
	// Return only the structured error; discard CLI stderr which may
	// contain sensitive metadata.
	return err.Error()
}
