package daemon

import (
	"fmt"
	"net/url"

	"relay/internal/manifest"
	"relay/internal/permissions"
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
// A denial is returned exactly as internal/permissions built it, so the CLI and
// MCP surface the identical PERMISSION_DENIED error rather than a reworded
// copy. A destructive operation that is otherwise entitled is turned into a
// PERMISSION_DENIED error that says confirmation is required; there is no
// interactive prompt yet, so the call is refused rather than run.
func (d *Daemon) permissionCheck(doc *manifest.Document, operation string) *relay.Error {
	policy := permissions.PolicyFromManifest(doc, d.destructiveOps)
	failure := permissions.Check(policy, permissionInvocation(doc, operation))
	if failure == nil {
		return nil
	}
	if failure.Code == permissions.CodeConfirmationRequired {
		return relay.NewError(relay.CodePermissionDenied,
			fmt.Sprintf("operation %q requires confirmation before it can run", operation)).
			WithDetails(failure.Details)
	}
	return failure
}

// permissionInvocation states, in capability terms, what this operation is
// about to do (spec §24, §25).
//
// Only the requirements the daemon can derive today are declared:
//
//   - Network is the host a REST or GraphQL operation will contact, read from
//     the protocol's baseUrl. A protocol with no baseUrl asserts no network.
//   - Uses is keychain when the manifest declares an auth block, because the
//     daemon will read a credential for it (spec §21, §22).
//   - Reads and Writes stay empty: there is no local executor yet, and it — not
//     the daemon — is the place that will know an operation's concrete paths
//     (spec §19). When one lands it widens this invocation.
//
// A manifest that declares neither capabilities nor permissions predates the
// capability model, so there is nothing to enforce against it: it states no
// capability requirements here and runs as it did before this check existed,
// rather than being denied for a declaration it could not have known to make.
// Once a tool opts in by declaring either, every requirement is checked
// default-deny — a capability it did not name is refused, and a host it did not
// scope is refused (spec §24).
//
// The destructive gate is deliberately not part of this opt-out: it is driven
// by daemon configuration rather than by the manifest, so a destructive
// operation is gated whenever the daemon names it, whether or not the tool
// declares a capability surface.
func permissionInvocation(doc *manifest.Document, operation string) permissions.Invocation {
	invocation := permissions.Invocation{Operation: operation}
	if !participatesInCapabilityModel(doc) {
		return invocation
	}
	if host := protocolHost(doc.Protocol); host != "" {
		invocation.Network = []string{host}
	}
	if doc.Auth != nil {
		invocation.Uses = []string{permissions.CapabilityKeychain}
	}
	return invocation
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
