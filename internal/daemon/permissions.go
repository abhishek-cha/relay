package daemon

import (
	"fmt"
	"net/url"

	"relay/internal/ipc"
	"relay/internal/manifest"
	"relay/internal/permissions"
	"relay/internal/protocol/local"
	"relay/pkg/relay"
)

// permissionCheck enforces a manifest's declared capability and permission
// surface against one operation, before the credential is resolved and before
// any protocol executor runs (spec §18, §24, §25, §40).
//
// It is the daemon half of the one security boundary the CLI and the MCP
// adapter share (spec §40, §41): both route through the daemon, so both get the
// same decision for the same manifest and invocation. The check itself is the
// pure classification in internal/permissions; this function only derives the
// invocation from the manifest and frames the result.
//
// A plain denial is returned exactly as internal/permissions built it, so the
// CLI and MCP surface the identical PERMISSION_DENIED error rather than a
// reworded copy. A destructive operation that is otherwise entitled is returned
// as a distinct [permissions.CodeConfirmationRequired] error naming the tool and
// the operation, unless the caller echoed the acknowledgement for exactly that
// tool and operation (spec §25). The daemon never prompts: it reports the gate
// and the surface decides whether a human is asked, which is what keeps the MCP
// adapter from ever gaining a confirmation path of its own (spec §41).
//
// Deriving the invocation can itself fail — a local operation naming a
// primitive this build does not implement, or a target that cannot be resolved
// to an absolute path. That error is returned as-is: an invocation the daemon
// cannot classify is refused rather than run unchecked.
func (d *Daemon) permissionCheck(doc *manifest.Document, tool string, operation *manifest.Tool, input map[string]any, confirmation string) *relay.Error {
	invocation, failure := permissionInvocation(doc, operation, input)
	if failure != nil {
		return failure
	}
	policy := permissions.PolicyFromManifest(doc, d.destructiveOps)
	// Canonicalize the declared scopes by the same rule the invocation's paths
	// were resolved with, so a scope written as "~/Documents" or reached through
	// a symlink matches the absolute path the request carries (spec §25, §40).
	policy.Filesystem.Read = local.ResolveScopes(policy.Filesystem.Read)
	policy.Filesystem.Write = local.ResolveScopes(policy.Filesystem.Write)
	decision := permissions.Classify(policy, invocation)
	if !decision.Confirmation {
		// Either allowed (Err is nil) or a plain entitlement denial, surfaced
		// unchanged so every surface reports the identical error.
		return decision.Err
	}
	// The operation is entitled but destructive. A matching acknowledgement runs
	// it; anything else — including the empty acknowledgement a tool binary or an
	// MCP client sends — is refused with the gate, naming what needs confirming.
	if ipc.Confirms(confirmation, tool, operation.Name) {
		return nil
	}
	return relay.NewError(permissions.CodeConfirmationRequired,
		fmt.Sprintf("operation %q on tool %q is destructive and requires confirmation", operation.Name, tool)).
		WithDetails(map[string]any{
			"tool":        tool,
			"operation":   operation.Name,
			"destructive": true,
		})
}

// permissionInvocation states, in capability terms, what this operation is
// about to do (spec §24, §25).
//
// The requirements the daemon derives are:
//
//   - Network is the host a REST or GraphQL operation will contact, read from
//     the protocol's baseUrl. A protocol with no baseUrl asserts no network.
//   - Uses is keychain when the manifest declares an auth block, because the
//     daemon will read a credential for it (spec §21, §22).
//   - Reads and Writes are the concrete, resolved paths a local operation will
//     touch, derived from the primitive its request block names and the
//     operation input (spec §46). The executor resolves the target by the same
//     rule, so the path checked here is the path acted on. No other protocol
//     states filesystem requirements.
//
// A manifest that declares neither capabilities nor permissions predates the
// capability model, so there is nothing to enforce against it: it states no
// capability requirements here and runs as it did before this check existed,
// rather than being denied for a declaration it could not have known to make.
// Once a tool opts in by declaring either, every requirement is checked
// default-deny — a capability it did not name is refused, and a host it did not
// scope is refused (spec §24).
//
// A local capability is the one exception to that opt-in: it always touches a
// path, so it always participates, even when its manifest declares no
// capability block. A local tool that declares nothing therefore has every path
// requirement denied for want of a filesystem capability, which is the correct
// default-deny reading — a local capability has no pre-model form to preserve.
//
// The destructive gate is deliberately not part of this opt-out: it is driven
// by daemon configuration rather than by the manifest, so a destructive
// operation is gated whenever the daemon names it, whether or not the tool
// declares a capability surface.
func permissionInvocation(doc *manifest.Document, operation *manifest.Tool, input map[string]any) (permissions.Invocation, *relay.Error) {
	invocation := permissions.Invocation{Operation: operation.Name}

	if doc.Protocol.Type == "local" {
		requirement, failure := local.Requirements(operation.Request.Operation, input)
		if failure != nil {
			return permissions.Invocation{}, failure
		}
		invocation.Reads = requirement.Reads
		invocation.Writes = requirement.Writes
	} else if !participatesInCapabilityModel(doc) {
		return invocation, nil
	}
	if host := protocolHost(doc.Protocol); host != "" {
		invocation.Network = []string{host}
	}
	if doc.Auth != nil {
		invocation.Uses = []string{permissions.CapabilityKeychain}
	}
	return invocation, nil
}

// participatesInCapabilityModel reports whether a manifest has declared the
// capability and permission model this check enforces (spec §24, §25).
//
// The declaration is the opt-in: a manifest that mentions neither capabilities
// nor permissions is a pre-model tool and is left untouched, while one that
// declares either is held to exactly what it declared.
func participatesInCapabilityModel(doc *manifest.Document) bool {
	if doc == nil {
		return false
	}
	return doc.Capabilities != nil || doc.Permissions != nil
}

// protocolHost extracts the host a tool contacts from its protocol block.
//
// REST and GraphQL both reach a service over HTTP, so their baseUrl names the
// host that must be covered by the declared network allowlist. Every other
// protocol returns no host: a local capability has no network target, and a
// transport that names its destination elsewhere derives it in its executor.
//
// A baseUrl that does not resolve to a host is returned as written, so a
// malformed value is matched literally and denied rather than being mistaken
// for "no network requirement" and waved through.
func protocolHost(protocol manifest.Protocol) string {
	switch protocol.Type {
	case "rest", "graphql":
	default:
		return ""
	}
	if protocol.BaseURL == "" {
		return ""
	}
	parsed, err := url.Parse(protocol.BaseURL)
	if err != nil || parsed.Hostname() == "" {
		return protocol.BaseURL
	}
	return parsed.Hostname()
}
