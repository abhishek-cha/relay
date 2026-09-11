package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// writePermissionsFile drops a permissions.yaml into dir, creating it first so a
// test can state the malformed-file case as ordinary content.
func writePermissionsFile(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, PermissionsFile), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", PermissionsFile, err)
	}
}

// TestLoadPermissionsPolicy covers spec §25's file convention, mirroring the
// trust policy: a missing file is an empty policy, a declared list is read, and
// a malformed file is a hard error rather than a silent fallback.
func TestLoadPermissionsPolicy(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		write   bool
		want    []string
		wantNil bool
		wantErr bool
	}{
		{name: "absent file is an empty policy", wantNil: true},
		{name: "declared list is read", body: "destructive:\n  - delete_repository\n  - charge_customer\n", write: true, want: []string{"delete_repository", "charge_customer"}},
		{name: "present empty list is an explicit opt-out", body: "destructive: []\n", write: true, want: []string{}},
		{name: "malformed file is a hard error", body: "destructive: [oops\n", write: true, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if test.write {
				writePermissionsFile(t, dir, test.body)
			}
			policy, err := LoadPermissionsPolicy(dir)
			if test.wantErr {
				if err == nil {
					t.Fatal("malformed policy loaded without error")
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadPermissionsPolicy: %v", err)
			}
			if test.wantNil {
				if policy.Destructive != nil {
					t.Fatalf("destructive = %v, want nil (not configured)", policy.Destructive)
				}
				return
			}
			if !slices.Equal(policy.Destructive, test.want) {
				t.Fatalf("destructive = %v, want %v", policy.Destructive, test.want)
			}
		})
	}
}

// TestResolveDestructiveOps covers the precedence spec §25 defines: an explicit
// Config.DestructiveOps wins, otherwise the file is read, and a file that never
// declared a list falls back to the default so the protection is on by default.
func TestResolveDestructiveOps(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		write    bool
		explicit []string
		want     []string
	}{
		{name: "missing file falls back to the default", want: DefaultDestructiveOps},
		{name: "present file overrides the default", body: "destructive:\n  - custom_op\n", write: true, want: []string{"custom_op"}},
		{name: "empty file list opts out", body: "destructive: []\n", write: true, want: []string{}},
		{name: "explicit config wins over the file", body: "destructive:\n  - custom_op\n", write: true, explicit: []string{"charge_customer"}, want: []string{"charge_customer"}},
		{name: "explicit config wins over a missing file", explicit: []string{"charge_customer"}, want: []string{"charge_customer"}},
		{name: "explicit empty config gates nothing", explicit: []string{}, want: []string{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if test.write {
				writePermissionsFile(t, dir, test.body)
			}
			got, err := resolveDestructiveOps(dir, test.explicit)
			if err != nil {
				t.Fatalf("resolveDestructiveOps: %v", err)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("destructive ops = %v, want %v", got, test.want)
			}
		})
	}
}

// TestResolveDestructiveOpsReturnsAClone proves the daemon never aliases the
// package default, so one daemon mutating its list cannot weaken another's.
func TestResolveDestructiveOpsReturnsAClone(t *testing.T) {
	got, err := resolveDestructiveOps(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("resolveDestructiveOps: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("default policy is empty; the default-on protection is gone")
	}
	got[0] = "mutated"
	if DefaultDestructiveOps[0] == "mutated" {
		t.Fatal("mutating the resolved list changed DefaultDestructiveOps")
	}
}

// TestResolveDestructiveOpsMalformedIsError proves a malformed file is a hard
// error rather than an empty policy, so the daemon can refuse to start.
func TestResolveDestructiveOpsMalformedIsError(t *testing.T) {
	dir := t.TempDir()
	writePermissionsFile(t, dir, "destructive: [oops\n")
	if _, err := resolveDestructiveOps(dir, nil); err == nil {
		t.Fatal("malformed policy resolved without error")
	}
}
