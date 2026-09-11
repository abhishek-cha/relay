package permissions

import (
	"slices"

	"relay/internal/manifest"
)

// Policy is the declarative per-tool permission surface that classification
// runs against (spec §24, §25). It is a plain value: no method reaches outside
// it, and building one performs no I/O.
type Policy struct {
	// Tool names the tool, for error context only. It never affects the decision.
	Tool string
	// Capabilities is the set the tool declared. Anything absent is denied.
	Capabilities []string
	// Network bounds the hosts a network operation may reach. An empty
	// allowlist denies all hosts rather than allowing all of them.
	Network NetworkPolicy
	// Filesystem bounds the paths an operation may read and write. Empty scopes
	// deny access rather than allowing it.
	Filesystem FilesystemPolicy
	// Destructive lists operations that are entitled to run but require explicit
	// confirmation before they do (spec §25). Matching is exact and
	// case-sensitive, because operation names are canonical snake_case.
	Destructive []string
}

// NetworkPolicy is the network half of a policy.
type NetworkPolicy struct {
	// Hosts is an allowlist of hostnames and "*.suffix" patterns. See
	// hostAllowed for the matching semantics.
	Hosts []string
}

// FilesystemPolicy is the filesystem half of a policy.
type FilesystemPolicy struct {
	// Read and Write are filesystem scopes. A path is permitted only when it
	// equals a scope or sits beneath it. See pathWithin for the semantics.
	Read  []string
	Write []string
}

// PolicyFromManifest projects a manifest's capability and permission
// declarations into a Policy.
//
// destructive is supplied by the caller because the manifest schema has no
// field for "this operation is destructive" today. Until it does, the daemon
// owns that list (a future config or registry entry), and a tool cannot widen
// its own confirmation surface by editing its manifest. Passing nil yields a
// policy with no destructive operations.
func PolicyFromManifest(doc *manifest.Document, destructive []string) Policy {
	if doc == nil {
		return Policy{Destructive: slices.Clone(destructive)}
	}
	policy := Policy{
		Tool:         doc.Metadata.Name,
		Capabilities: slices.Clone(doc.Capabilities),
		Destructive:  slices.Clone(destructive),
	}
	if doc.Permissions != nil {
		if doc.Permissions.Network != nil {
			policy.Network.Hosts = slices.Clone(doc.Permissions.Network.Hosts)
		}
		if doc.Permissions.Filesystem != nil {
			policy.Filesystem.Read = slices.Clone(doc.Permissions.Filesystem.Read)
			policy.Filesystem.Write = slices.Clone(doc.Permissions.Filesystem.Write)
		}
	}
	return policy
}

// isDestructive reports whether an operation needs confirmation.
func (p Policy) isDestructive(operation string) bool {
	if operation == "" {
		return false
	}
	return slices.Contains(p.Destructive, operation)
}
