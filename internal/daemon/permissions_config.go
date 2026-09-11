package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"
)

// PermissionsFile is the destructive-operation policy's file name under the
// Relay home's config directory, alongside trust.yaml (spec §25).
const PermissionsFile = "permissions.yaml"

// DefaultDestructiveOps is the operation list enforced when the policy file is
// absent. It names the canonical destructive operations from the design (spec
// §25) so the protection is on by default: installing this feature can never
// leave a daemon that names destructive operations with none gated. An operator
// overrides the list by declaring it in permissions.yaml.
var DefaultDestructiveOps = []string{
	"delete_repository",
	"send_message",
	"charge_customer",
}

// PermissionsPolicy is the on-disk shape of permissions.yaml.
//
// It mirrors sign.Policy: the file is optional, and its absence is an empty
// policy rather than an error, because refusing to start over a config the
// operator never wrote would be worse than using the documented default. A file
// that cannot be parsed is a hard error — silently ignoring a policy the
// operator did write would leave the daemon enforcing something other than what
// the operator asked for.
type PermissionsPolicy struct {
	// Destructive lists the operations that require explicit confirmation. A nil
	// list means "not configured" and lets the daemon apply DefaultDestructiveOps;
	// a present-but-empty list is an explicit opt-out (spec §25).
	Destructive []string `yaml:"destructive,omitempty"`
}

// LoadPermissionsPolicy reads the destructive-operation policy from the Relay
// home's config directory.
//
// A missing file is an empty policy, not an error; a malformed one is an error,
// matching the trust-policy convention this file follows. The returned zero
// policy leaves Destructive nil so the caller can tell "the operator never
// declared a list" from "the operator declared an empty one".
func LoadPermissionsPolicy(configDir string) (PermissionsPolicy, error) {
	path := filepath.Join(configDir, PermissionsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PermissionsPolicy{}, nil
		}
		return PermissionsPolicy{}, fmt.Errorf("read permissions policy %s: %w", path, err)
	}
	var policy PermissionsPolicy
	if err := yaml.Unmarshal(data, &policy); err != nil {
		return PermissionsPolicy{}, fmt.Errorf("parse permissions policy %s: %w", path, err)
	}
	return policy, nil
}

// resolveDestructiveOps chooses the destructive-operation list the daemon
// enforces (spec §25).
//
// An explicit Config.DestructiveOps wins so a caller (or a test) can set the
// list directly without touching disk. Otherwise the policy file is read, and a
// file that never declared a list falls back to DefaultDestructiveOps. The
// result is always a fresh slice: the daemon owns its copy and never aliases the
// caller's or the package default's backing array.
func resolveDestructiveOps(configDir string, explicit []string) ([]string, error) {
	if explicit != nil {
		return slices.Clone(explicit), nil
	}
	policy, err := LoadPermissionsPolicy(configDir)
	if err != nil {
		return nil, err
	}
	if policy.Destructive == nil {
		return slices.Clone(DefaultDestructiveOps), nil
	}
	return slices.Clone(policy.Destructive), nil
}
