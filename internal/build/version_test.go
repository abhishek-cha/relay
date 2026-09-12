package build

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"relay/internal/manifest"
	"relay/internal/skill"
)

// TestCheckSkillVersion covers spec §36: when a skill declares a version it
// must equal the manifest's metadata.version; when it declares none the build
// must stay green (backward compatibility).
func TestCheckSkillVersion(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string // substring the error must contain; empty means no error
	}{
		{name: "no skill", content: ""},
		{name: "absent version builds", content: "# Demo\n\nUse it well.\n"},
		{name: "matching version builds", content: "---\nversion: 1.0.0\n---\n\n# Demo\n\nUse it well.\n"},
		{name: "mismatched version fails", content: "---\nversion: 2.0.0\n---\n\n# Demo\n\nUse it well.\n", wantErr: "§36"},
		{name: "malformed frontmatter fails", content: "---\nversion: [1.0.0\n---\n\n# Demo\n", wantErr: "frontmatter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkSkillVersion("SKILL.md", []byte(tt.content), lintDoc(t))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// The mismatch message must be actionable: the author needs to see both
// versions to reconcile them.
func TestCheckSkillVersionReportsBothVersions(t *testing.T) {
	err := checkSkillVersion("SKILL.md", []byte("---\nversion: 2.0.0\n---\n\n# Demo\n"), lintDoc(t))
	if err == nil {
		t.Fatal("expected a mismatch to fail the build")
	}
	for _, want := range []string{"2.0.0", "1.0.0", "§36"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err.Error(), want)
		}
	}
}

// TestExamplesPassSkillGates guards backward compatibility: every checked-in
// example must still pass the same gates relay build runs, so a version-less
// skill keeps building and none restates the schema.
func TestExamplesPassSkillGates(t *testing.T) {
	for _, name := range []string{"browser", "github", "slack", "stripe"} {
		t.Run(name, func(t *testing.T) {
			doc, err := manifest.Parse(mustReadFile(t, filepath.Join("..", "..", "examples", name, name+".yaml")))
			if err != nil {
				t.Fatalf("parse manifest: %v", err)
			}
			path := filepath.Join("..", "..", "examples", name, "SKILL.md")
			data, err := os.ReadFile(path)
			if err != nil {
				if os.IsNotExist(err) {
					return // this example ships no skill
				}
				t.Fatalf("read skill: %v", err)
			}
			if err := skill.Validate(data); err != nil {
				t.Fatalf("validate skill: %v", err)
			}
			if err := lintSkill(data, doc, false); err != nil {
				t.Fatalf("lint skill: %v", err)
			}
			if err := checkSkillVersion(path, data, doc); err != nil {
				t.Fatalf("skill version: %v", err)
			}
		})
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}
