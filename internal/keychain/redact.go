package keychain

import "strings"

// Redact returns s with every occurrence of each secret replaced by
// "[REDACTED]". This is the canonical scrubbing helper for CLI output,
// MCP output, logs, and telemetry (spec §22, §54).
//
// Empty and zero-length secrets are ignored — passing them is harmless
// but has no effect.
func Redact(s string, secrets ...string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		s = strings.ReplaceAll(s, secret, "[REDACTED]")
	}
	return s
}
