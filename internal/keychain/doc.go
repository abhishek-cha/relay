// Package keychain wraps macOS Keychain access so the daemon can store and
// retrieve credentials without a tool binary ever handling a secret.
//
// # Namespace
//
// Every credential lives under a Keychain item with:
//
//	service: com.relay.<tool>   (see [Service])
//	account: default            (see [AccountDefault])
//
// This keeps credentials isolated per tool (spec §22).
//
// # Security properties
//
//   - Error strings must never embed the secret value. This is the single most
//     important invariant of this package.
//   - Secrets must never appear in CLI output, MCP output, logs, telemetry,
//     manifests, or skills (spec §22, §54).
//
// # Trade-offs
//
// The MVP shells out to /usr/bin/security rather than linking Security.framework
// via cgo. This means the secret appears briefly in process argv (visible to
// ps(1) during the call). Two mitigations:
//
//  1. The window is small — the process exists only for the duration of one
//     security invocation.
//  2. A future hardening pass can replace this with SecKeychainAddGenericPassword
//     via cgo, which never exposes the secret in argv.
//
// For the MVP, the simplicity of shelling out outweighs the argv exposure,
// especially since the daemon is the only process that ever calls these
// functions and it already holds a local Unix-socket trust boundary.
//
// See TASKS.md milestone M4.
package keychain
