// Package permissions is Relay's capability and permission boundary: given a
// tool manifest and an operation invocation, it answers whether the call is
// allowed, and whether a destructive call needs explicit confirmation first.
//
// # Classification only — the surface asks the human
//
// This package classifies. It never prompts, prints, reads stdin, touches the
// Keychain, or performs any other I/O. [Check] returns nil, a PERMISSION_DENIED
// error, or the [CodeConfirmationRequired] sentinel, and the calling surface is
// what turns that sentinel into "Tool wants to execute github.delete_repository.
// Allow? [y/N]" (spec §25). Keeping the prompt out of here is what makes the
// decision deterministic and trivially testable, and what lets the CLI and the
// MCP adapter share one security model instead of growing two (spec §41).
//
// # One boundary for every surface
//
// The daemon is the security boundary (spec §40): tool binaries are treated as
// potentially untrusted and never reach a service directly. Both the CLI and
// the MCP adapter route through the daemon, so both consult this same Check.
// There is deliberately no MCP-only grant path and no second policy (spec §41):
// an MCP invocation and a CLI invocation carrying the same manifest and
// invocation produce the same decision.
//
// # Default deny and no silent upgrades
//
// A capability that the manifest does not declare is never granted, and an
// operation is never silently upgraded to one it did not ask for (spec §24,
// §40). A tool with no declared capabilities and no permissions can still run a
// purely local operation (one that states no requirements); the moment it asks
// for network, filesystem, or another capability, the declaration must exist
// and the request must fit inside it.
//
// # Declared model
//
// Capabilities come from the manifest's capabilities list; the network host
// allowlist and filesystem read/write scopes come from its permissions block
// (spec §25). The set of destructive operations is not expressible in the
// manifest today, so it is supplied when the [Policy] is built — the manifest
// field gap is called out in [PolicyFromManifest]. An operation invocation
// states what it is about to do in capability terms via [Invocation]; the
// daemon derives that from the resolved transport (a REST base URL becomes a
// network host, a filesystem executor becomes read/write paths).
//
// See TASKS.md milestone M11.
package permissions
