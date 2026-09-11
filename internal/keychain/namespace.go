package keychain

// Service returns the Keychain service name for a tool, following the
// com.relay.<tool> namespace convention from spec §22.
//
// Example: Service("github") returns "com.relay.github".
func Service(tool string) string {
	return "com.relay." + tool
}

// AccountDefault is the default Keychain account name used when a tool
// has a single credential (spec §22).
const AccountDefault = "default"
