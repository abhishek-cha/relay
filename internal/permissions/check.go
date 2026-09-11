package permissions

import (
	"fmt"

	"relay/pkg/relay"
)

// CodeConfirmationRequired is the machine-visible sentinel [Check] returns when
// an operation is entitled to run but is destructive, so the calling surface
// must obtain explicit approval before proceeding (spec §25).
//
// It is a relay.Code so CLI and MCP can branch on it exactly like any other
// structured error. pkg/relay does not define it because it is not a failure of
// the operation — it is a gate the surface resolves, either into a prompt
// ("Allow? [y/N]") or into an MCP elicitation — and the daemon maps it to the
// surface's UX rather than returning it as a terminal error.
const CodeConfirmationRequired relay.Code = "CONFIRMATION_REQUIRED"

// Invocation states what an operation is about to do, in capability terms. It
// is the caller's declaration of requirements; [Check] verifies it against the
// Policy. An empty Invocation (no capabilities named) is a purely local
// operation and needs no declared capability.
type Invocation struct {
	// Operation is the manifest operation name, used for destructive matching
	// and error context.
	Operation string
	// Network lists the hosts the operation will contact. Any entry asserts the
	// network capability, and each host must match the allowlist.
	Network []string
	// Reads lists absolute filesystem paths the operation will read. Any entry
	// asserts filesystem.read.
	Reads []string
	// Writes lists absolute filesystem paths the operation will write. Any entry
	// asserts filesystem.write.
	Writes []string
	// Uses lists the remaining capabilities the operation needs — keychain,
	// browser, shell, notifications, clipboard — each of which must be declared.
	Uses []string
}

// Decision is the full classification of one invocation.
type Decision struct {
	// Allowed is true when the operation may proceed without intervention.
	Allowed bool
	// Confirmation is true when the operation is entitled but destructive, so
	// the surface must ask the human before running it.
	Confirmation bool
	// Err is nil when Allowed; otherwise it is PERMISSION_DENIED or
	// [CodeConfirmationRequired].
	Err *relay.Error
}

// Check classifies an invocation against a policy. It returns nil when the
// operation is permitted outright, a PERMISSION_DENIED error when it is not
// entitled to what it is asking for, and a [CodeConfirmationRequired] error
// when it is entitled but destructive and so needs confirmation (spec §25).
//
// Check is pure, deterministic, and free of I/O: the same Policy and Invocation
// always yield the same result, which is what lets the daemon and an MCP
// adapter share it as the one security model (spec §40, §41). It never prompts;
// see [Decision] and the package doc for the classification/prompting split.
func Check(policy Policy, inv Invocation) *relay.Error {
	return Classify(policy, inv).Err
}

// Classify is Check with the full decision, for callers that need to tell
// "denied" and "needs confirmation" apart without inspecting error codes.
//
// Requirements are evaluated in a fixed order — network, reads, writes, uses —
// so a tool that is missing several capabilities gets a stable, reproducible
// first error. Entitlement is checked before the destructive gate, so an
// operation that is both destructive and unauthorized reports the denial.
func Classify(policy Policy, inv Invocation) Decision {
	declared := capabilitySet(policy.Capabilities)

	for _, host := range inv.Network {
		if !declared[CapabilityNetwork] {
			return denied(inv.Operation, CapabilityNetwork,
				fmt.Sprintf("operation %q requires capability %q, which the tool does not declare",
					inv.Operation, CapabilityNetwork))
		}
		if !hostAllowed(host, policy.Network.Hosts) {
			return deniedWith(inv.Operation, CapabilityNetwork,
				fmt.Sprintf("operation %q may not contact host %q: it is not in the declared network allowlist",
					inv.Operation, host),
				map[string]any{"host": host})
		}
	}

	for _, path := range inv.Reads {
		if !declared[CapabilityFilesystemRead] {
			return denied(inv.Operation, CapabilityFilesystemRead,
				fmt.Sprintf("operation %q requires capability %q, which the tool does not declare",
					inv.Operation, CapabilityFilesystemRead))
		}
		if !pathWithin(path, policy.Filesystem.Read) {
			return deniedWith(inv.Operation, CapabilityFilesystemRead,
				fmt.Sprintf("operation %q may not read path %q: it is outside the declared read scope",
					inv.Operation, path),
				map[string]any{"path": path})
		}
	}

	for _, path := range inv.Writes {
		if !declared[CapabilityFilesystemWrite] {
			return denied(inv.Operation, CapabilityFilesystemWrite,
				fmt.Sprintf("operation %q requires capability %q, which the tool does not declare",
					inv.Operation, CapabilityFilesystemWrite))
		}
		if !pathWithin(path, policy.Filesystem.Write) {
			return deniedWith(inv.Operation, CapabilityFilesystemWrite,
				fmt.Sprintf("operation %q may not write path %q: it is outside the declared write scope",
					inv.Operation, path),
				map[string]any{"path": path})
		}
	}

	for _, capability := range inv.Uses {
		if !declared[capability] {
			return denied(inv.Operation, capability,
				fmt.Sprintf("operation %q requires capability %q, which the tool does not declare",
					inv.Operation, capability))
		}
	}

	if policy.isDestructive(inv.Operation) {
		return Decision{
			Confirmation: true,
			Err: relay.NewError(CodeConfirmationRequired,
				fmt.Sprintf("operation %q is destructive and requires confirmation", inv.Operation)).
				WithDetails(map[string]any{
					"operation":   inv.Operation,
					"destructive": true,
				}),
		}
	}

	return Decision{Allowed: true}
}

// denied builds a PERMISSION_DENIED decision naming the operation and capability.
func denied(operation, capability, message string) Decision {
	return deniedWith(operation, capability, message, nil)
}

// deniedWith is denied plus non-sensitive structured context.
func deniedWith(operation, capability, message string, extra map[string]any) Decision {
	details := map[string]any{
		"operation":  operation,
		"capability": capability,
	}
	for key, value := range extra {
		details[key] = value
	}
	return Decision{
		Err: relay.NewError(relay.CodePermissionDenied, message).WithDetails(details),
	}
}
