package relay

import (
	"strconv"
	"strings"
)

// CompatibleRuntimeAPIVersion reports whether a tool that requires one runtime
// API version can run on a runtime that provides another.
//
// Both sides are minor-versioned strings such as "v1" or "v2.3". The major
// version is the compatibility boundary. An empty requirement means the tool
// did not declare one, so the runtime's own contract applies and the tool is
// accepted (spec §35).
func CompatibleRuntimeAPIVersion(required, provided string) bool {
	if required == "" {
		return true
	}
	requiredMajor, ok := majorVersion(required)
	if !ok {
		return false
	}
	providedMajor, ok := majorVersion(provided)
	if !ok {
		return false
	}
	return requiredMajor == providedMajor
}

// majorVersion extracts the leading major number of a "vN" or "vN.M" version.
func majorVersion(version string) (int, bool) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if trimmed == "" {
		return 0, false
	}
	if index := strings.IndexByte(trimmed, '.'); index >= 0 {
		trimmed = trimmed[:index]
	}
	major, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, false
	}
	return major, true
}
