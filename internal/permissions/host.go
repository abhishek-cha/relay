package permissions

import (
	"net"
	"strings"
)

// hostAllowed reports whether host is permitted by the allowlist patterns.
//
// Matching semantics (spec §24, §25):
//
//   - Comparison is case-insensitive; a trailing dot (the fully-qualified form)
//     and any ":port" suffix are ignored on both the host and the pattern, so
//     "API.GitHub.com:443" matches the pattern "api.github.com".
//   - A bare hostname matches exactly that host. It does NOT match a subdomain:
//     "api.github.com" does not allow "gist.api.github.com".
//   - A leading "*." wildcard matches one or more leading labels: "*.github.com"
//     allows both "api.github.com" and "gist.api.github.com", but not the apex
//     "github.com" — ask for the apex explicitly if it is needed.
//   - "*" alone matches any host. It is an explicit, readable opt-in to "any
//     host" rather than the accidental result of a malformed pattern.
//   - Any other use of "*" is treated literally, so a pattern that is not a
//     supported wildcard cannot widen access.
//
// An empty allowlist allows nothing.
func hostAllowed(host string, patterns []string) bool {
	normalized := normalizeHost(host)
	if normalized == "" {
		return false
	}
	for _, pattern := range patterns {
		if matchHost(normalized, normalizeHost(pattern)) {
			return true
		}
	}
	return false
}

// normalizeHost lowercases a host, drops a port suffix, and drops a trailing
// dot. It is deliberately lenient so the same host spelled two ways lands on
// one canonical form.
func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if trimmed, _, err := net.SplitHostPort(host); err == nil {
		host = trimmed
	}
	return strings.TrimSuffix(host, ".")
}

// matchHost applies one normalized pattern to one normalized host.
func matchHost(host, pattern string) bool {
	if pattern == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
		if suffix == "" {
			return false
		}
		return strings.HasSuffix(host, "."+suffix)
	}
	return host == pattern
}
