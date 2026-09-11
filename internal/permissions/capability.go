package permissions

// Capability names. These are exactly the identifiers the manifest validator
// accepts (spec §24); the set is closed here and in internal/manifest, and an
// unknown string is treated literally rather than ignored, so a typo narrows
// access instead of widening it.
const (
	CapabilityNetwork         = "network"
	CapabilityKeychain        = "keychain"
	CapabilityBrowser         = "browser"
	CapabilityFilesystemRead  = "filesystem.read"
	CapabilityFilesystemWrite = "filesystem.write"
	CapabilityShell           = "shell"
	CapabilityNotifications   = "notifications"
	CapabilityClipboard       = "clipboard"
)

// capabilitySet indexes a declaration list for presence checks.
func capabilitySet(capabilities []string) map[string]bool {
	set := make(map[string]bool, len(capabilities))
	for _, capability := range capabilities {
		set[capability] = true
	}
	return set
}
