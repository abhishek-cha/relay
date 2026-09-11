package permissions

import (
	"path/filepath"
	"strings"
)

// pathWithin reports whether path falls inside any of the scopes.
//
// Matching semantics (spec §24, §25):
//
//   - Both the requested path and each scope are cleaned lexically with
//     filepath.Clean, so "..", "." and duplicate separators are normalized.
//   - A path is "within" a scope when it equals the scope or sits beneath it at
//     a separator boundary. "/docs/secret" is within "/docs"; "/docs-secret" is
//     not, because the boundary is a separator and not a bare string prefix.
//   - The root scope "/" contains every absolute path.
//   - Comparison is byte-for-byte and therefore case-sensitive, which is the
//     deterministic choice. Callers must resolve symlinks and expand a leading
//     "~" before calling (the daemon owns the home directory): this package
//     performs no I/O and consults no environment, so it cannot do either.
//   - Relative requested paths are effectively denied against absolute scopes;
//     scopes and paths are expected to be absolute.
//
// An empty scope list allows nothing.
func pathWithin(path string, scopes []string) bool {
	if path == "" {
		return false
	}
	cleaned := filepath.Clean(path)
	for _, scope := range scopes {
		if scope == "" {
			continue
		}
		cleanedScope := filepath.Clean(scope)
		if cleaned == cleanedScope {
			return true
		}
		if cleanedScope == string(filepath.Separator) {
			return strings.HasPrefix(cleaned, string(filepath.Separator))
		}
		if strings.HasPrefix(cleaned, cleanedScope+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
