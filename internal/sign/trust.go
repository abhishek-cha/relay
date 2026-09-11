package sign

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// TrustLevel is a tool's standing in the local trust model (spec §49). The four
// values are the whole model; a record with any other value is meaningless.
type TrustLevel string

const (
	// TrustTrusted is the operator's explicit local allowlist. It needs no
	// signature: the operator said so.
	TrustTrusted TrustLevel = "trusted"
	// TrustVerified is a tool whose signature verified but which carries no local
	// rule of its own.
	TrustVerified TrustLevel = "verified"
	// TrustUnknown is an unsigned tool with no matching local rule. It is the
	// default, and it is not a rejection.
	TrustUnknown TrustLevel = "unknown"
	// TrustBlocked is a local policy denial. It outranks every other signal, so a
	// blocked tool is refused even when its signature verifies.
	TrustBlocked TrustLevel = "blocked"
)

// PolicyFile is the trust policy's file name under the Relay home's config
// directory.
const PolicyFile = "trust.yaml"

// Rule is one local allow/deny entry. It matches a tool when every field it
// sets matches; an all-empty rule never matches. Publisher and Key compare
// case-insensitively.
type Rule struct {
	Name      string `yaml:"name,omitempty"`
	Publisher string `yaml:"publisher,omitempty"`
	Key       string `yaml:"key,omitempty"`
}

// Policy is the local trust allowlist and blocklist (spec §49).
type Policy struct {
	Trusted []Rule `yaml:"trusted,omitempty"`
	Blocked []Rule `yaml:"blocked,omitempty"`
}

// Identity is what a policy rule can match a tool on. Publisher and
// KeyFingerprint are empty for an unsigned tool, so a rule that pins either can
// never match one.
type Identity struct {
	Name           string
	Publisher      string
	KeyFingerprint string
}

// LoadPolicy reads the trust policy from the Relay home's config directory. A
// missing file is an empty policy, not an error; a malformed one is an error,
// because silently ignoring a policy the operator wrote would be worse than
// refusing to install.
func LoadPolicy(configDir string) (Policy, error) {
	path := filepath.Join(configDir, PolicyFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Policy{}, nil
		}
		return Policy{}, fmt.Errorf("read trust policy %s: %w", path, err)
	}
	var policy Policy
	if err := yaml.Unmarshal(data, &policy); err != nil {
		return Policy{}, fmt.Errorf("parse trust policy %s: %w", path, err)
	}
	return policy, nil
}

// Decide returns a tool's trust level by the decision table documented on the
// package. signed reports whether the tool carried a signature that verified.
func Decide(policy Policy, identity Identity, signed bool) TrustLevel {
	switch {
	case matches(policy.Blocked, identity):
		return TrustBlocked
	case matches(policy.Trusted, identity):
		return TrustTrusted
	case signed:
		return TrustVerified
	default:
		return TrustUnknown
	}
}

func matches(rules []Rule, identity Identity) bool {
	for _, rule := range rules {
		if rule.matches(identity) {
			return true
		}
	}
	return false
}

func (r Rule) matches(identity Identity) bool {
	if r.Name == "" && r.Publisher == "" && r.Key == "" {
		return false
	}
	if r.Name != "" && r.Name != identity.Name {
		return false
	}
	if r.Publisher != "" && !strings.EqualFold(r.Publisher, identity.Publisher) {
		return false
	}
	if r.Key != "" && !strings.EqualFold(r.Key, identity.KeyFingerprint) {
		return false
	}
	return true
}
