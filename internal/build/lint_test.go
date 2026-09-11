package build

import (
	"strings"
	"testing"

	"relay/internal/manifest"
)

const lintManifest = `apiVersion: relay/v1
kind: Tool
metadata:
  name: demo
  version: 1.0.0
  description: A demo tool
protocol:
  type: rest
  baseUrl: https://api.example.com
tools:
  - name: get_repo
    description: Get a repository
    input:
      type: object
      properties:
        owner:
          type: string
        repo:
          type: string
      required:
        - owner
        - repo
    request:
      method: GET
      path: /repos/{owner}/{repo}
`

func lintDoc(t *testing.T) *manifest.Document {
	t.Helper()
	doc, err := manifest.Parse([]byte(lintManifest))
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return doc
}

// A missing skill reaches the linter as the empty embed placeholder. That is
// the no-skill case loadSkill already warns about, so the gate must not treat it
// as an empty-skill error and break builds that legitimately carry no skill.
func TestLintSkillAllowsMissingSkill(t *testing.T) {
	if err := lintSkill(nil, lintDoc(t), false); err != nil {
		t.Fatalf("a build with no skill must not fail lint, got: %v", err)
	}
}

func TestLintSkillAcceptsGuidanceWithoutSchema(t *testing.T) {
	skill := "# Demo\n\nStart with get_repo, then narrow down.\n\nWatch out for pagination.\n"
	if err := lintSkill([]byte(skill), lintDoc(t), false); err != nil {
		t.Fatalf("expected a clean skill, got: %v", err)
	}
}

// The whole point of the gate: two sources of truth must not ship (spec §30).
func TestLintSkillRejectsSchemaRestatement(t *testing.T) {
	skill := "# Demo\n\n## Parameters\n- owner: string\n- repo: string\n"
	err := lintSkill([]byte(skill), lintDoc(t), false)
	if err == nil {
		t.Fatal("expected a schema-restating skill to fail the build")
	}
	if !strings.Contains(err.Error(), "§30") {
		t.Errorf("error should cite the rule it enforces, got: %v", err)
	}
}

// A weak skill is a warning, not a build failure: quality problems should not
// block a build the way a correctness problem does.
func TestLintSkillWarnsButBuildsWithoutWorkflow(t *testing.T) {
	skill := "# Demo\n\nUse this tool to inspect repositories when asked.\n"
	if err := lintSkill([]byte(skill), lintDoc(t), false); err != nil {
		t.Fatalf("a skill with no workflow is a warning, not an error, got: %v", err)
	}
}
