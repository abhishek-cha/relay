// Package keychain wraps macOS Keychain access, namespaced as
// service com.relay.<tool> / account default (spec §22).
//
// Secrets stored here must never appear in CLI output, MCP output, logs,
// telemetry, manifests, or skills.
//
// See TASKS.md milestone M4.
package keychain
