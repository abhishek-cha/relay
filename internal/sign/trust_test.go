package sign

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDecideTable pins the trust-level decision table from the package doc:
// local block wins over everything, then local trust, then a verified signature,
// then unknown.
func TestDecideTable(t *testing.T) {
	policy := Policy{
		Trusted: []Rule{{Name: "github"}, {Publisher: "GitHub"}, {Key: "ABCDEF"}},
		Blocked: []Rule{{Name: "malware"}, {Publisher: "BadCorp"}},
	}

	cases := []struct {
		name   string
		policy Policy
		id     Identity
		signed bool
		want   TrustLevel
	}{
		{"blocked by name", policy, Identity{Name: "malware"}, false, TrustBlocked},
		{"blocked by publisher", policy, Identity{Name: "x", Publisher: "badcorp"}, true, TrustBlocked},
		{"blocked outranks trusted", policy, Identity{Name: "github", Publisher: "badcorp"}, true, TrustBlocked},
		{"trusted by name", policy, Identity{Name: "github"}, false, TrustTrusted},
		{"trusted by publisher case-insensitively", policy, Identity{Name: "ghost", Publisher: "github"}, false, TrustTrusted},
		{"trusted by key case-insensitively", policy, Identity{Name: "ghost", KeyFingerprint: "abcdef"}, true, TrustTrusted},
		{"verified when signed", Policy{}, Identity{Name: "ghost", Publisher: "someone"}, true, TrustVerified},
		{"unknown when unsigned", Policy{}, Identity{Name: "ghost"}, false, TrustUnknown},
		{"non-matching trusted rule falls through to verified", Policy{Trusted: []Rule{{Publisher: "Other"}}}, Identity{Name: "ghost", Publisher: "someone"}, true, TrustVerified},
		{"unsigned never matches a publisher rule", Policy{Trusted: []Rule{{Publisher: "GitHub"}}}, Identity{Name: "ghost"}, false, TrustUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decide(tc.policy, tc.id, tc.signed); got != tc.want {
				t.Fatalf("Decide = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEmptyRuleNeverMatches(t *testing.T) {
	policy := Policy{Trusted: []Rule{{}}, Blocked: []Rule{{}}}
	if got := Decide(policy, Identity{Name: "demo"}, false); got != TrustUnknown {
		t.Fatalf("Decide = %q, want %q — an empty rule must match nothing", got, TrustUnknown)
	}
}

func TestLoadPolicyMissingFileIsEmpty(t *testing.T) {
	policy, err := LoadPolicy(t.TempDir())
	if err != nil {
		t.Fatalf("LoadPolicy on an absent file: %v", err)
	}
	if len(policy.Trusted) != 0 || len(policy.Blocked) != 0 {
		t.Fatalf("LoadPolicy = %+v, want empty", policy)
	}
}

func TestLoadPolicyReadsRules(t *testing.T) {
	dir := t.TempDir()
	body := "trusted:\n  - name: github\n  - publisher: GitHub\nblocked:\n  - name: malware\n"
	if err := os.WriteFile(filepath.Join(dir, PolicyFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	policy, err := LoadPolicy(dir)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if got := Decide(policy, Identity{Name: "github"}, false); got != TrustTrusted {
		t.Fatalf("trusted rule = %q, want %q", got, TrustTrusted)
	}
	if got := Decide(policy, Identity{Name: "malware"}, false); got != TrustBlocked {
		t.Fatalf("blocked rule = %q, want %q", got, TrustBlocked)
	}
}

// A malformed policy is an operator error that must not be silently ignored,
// because ignoring it would install a tool the operator tried to block.
func TestLoadPolicyRejectsMalformedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, PolicyFile), []byte("trusted: {not a list}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicy(dir); err == nil {
		t.Fatal("LoadPolicy accepted a malformed policy")
	}
}
