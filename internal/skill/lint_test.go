package skill

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"relay/internal/manifest"
)

// testManifest is a fake two-operation manifest. Its names are the anchors the
// linter recognizes, so every fixture below is explainable in terms of a real
// declaration (spec §30).
func testManifest() *manifest.Document {
	return &manifest.Document{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.KindTool,
		Metadata:   manifest.Metadata{Name: "github", Version: "1.0.0"},
		Tools: []manifest.Tool{
			{
				Name: "get_repository",
				Input: manifest.InputSchema{
					Type: "object",
					Properties: map[string]manifest.Property{
						"owner": {Type: "string"},
						"repo":  {Type: "string"},
					},
					Required: []string{"owner", "repo"},
				},
			},
			{
				Name: "list_pull_requests",
				Input: manifest.InputSchema{
					Type: "object",
					Properties: map[string]manifest.Property{
						"owner": {Type: "string"},
						"repo":  {Type: "string"},
						"state": {Type: "string"},
					},
				},
			},
		},
	}
}

func skillLines(lines ...string) string { return strings.Join(lines, "\n") }

// designSection43Skill is the good-skill fixture from docs/DESIGN.md §43. It is
// the shape the linter must accept: prose guidance, a numbered workflow, and a
// single reference to a declared operation.
func designSection43Skill() string {
	return skillLines(
		"# GitHub",
		"",
		"Use GitHub capabilities to inspect repositories and pull requests.",
		"",
		"## Repository Investigation",
		"",
		"Start with `get_repository` when repository metadata is needed.",
		"",
		"## Pull Requests",
		"",
		"For investigating a pull request:",
		"",
		"1. Identify the repository.",
		"2. List pull requests.",
		"3. Retrieve the relevant pull request.",
		"4. Retrieve changed files when necessary.",
		"",
		"Prefer targeted calls over retrieving unnecessary data.",
	)
}

// Expected messages, kept as raw strings so the punctuation matches exactly.
const (
	missingWorkflowMsg = `skill has no workflow guidance: add an arrow sequence (→ or ->), a numbered workflow, or a "when to use" rule (spec §7, §30)`
	fencedYAMLMsg      = `skill restates the input schema in a fenced yaml block; the manifest is the schema source (spec §30)`
	fencedJSONMsg      = `skill restates the input schema in a fenced json block; the manifest is the schema source (spec §30)`
	paramOwnerMsg      = `skill enumerates input parameter "owner" with its type; the manifest owns input types (spec §30)`
	sectionParamsMsg   = `skill section "Parameters" lists input fields; the manifest owns the input schema (spec §30)`
	sectionInputsMsg   = `skill section "Inputs" lists input fields; the manifest owns the input schema (spec §30)`
	unknownGetPRMsg    = `skill names operation "get_pull_request", which the manifest does not declare; a broken pointer is worse than none (spec §30)`
)

func TestLint(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []Issue
	}{
		{
			name:    "empty skill",
			content: "",
			want:    []Issue{{Severity: Error, Message: "skill is empty", Line: 1}},
		},
		{
			name:    "whitespace-only skill",
			content: "\n \t\n",
			want:    []Issue{{Severity: Error, Message: "skill is empty", Line: 1}},
		},
		{
			name:    "good skill from design section 43",
			content: designSection43Skill(),
			want:    nil,
		},
		{
			name:    "arrow workflow is sufficient",
			content: skillLines("# GitHub", "", "A useful sequence is:", "", "get_repository", "→ list_pull_requests"),
			want:    nil,
		},
		{
			name:    "when-to-use rule is sufficient",
			content: skillLines("# GitHub", "", "When to use: reach for `get_repository` first."),
			want:    nil,
		},
		{
			name:    "numbered workflow is sufficient",
			content: skillLines("# GitHub", "", "When investigating:", "", "1. Retrieve the repository.", "2. List pull requests."),
			want:    nil,
		},
		{
			name:    "missing workflow warns",
			content: skillLines("# GitHub", "", "This tool talks to GitHub.", "", "It retrieves repository metadata on demand."),
			want:    []Issue{{Severity: Warning, Message: missingWorkflowMsg, Line: 1}},
		},
		{
			name:    "yaml fenced schema block",
			content: skillLines("# GitHub", "", "When to use: inspecting repositories.", "", "```yaml", "tools:", "  - name: get_repository", "    input:", "      properties:", "        owner:", "          type: string", "```"),
			want:    []Issue{{Severity: Error, Message: fencedYAMLMsg, Line: 5}},
		},
		{
			name:    "json fenced schema block",
			content: skillLines("# GitHub", "", "When to use: x.", "", "```json", "{", "  \"input\": {", "    \"properties\": {", "      \"owner\": \"string\"", "    }", "  }", "}", "```"),
			want:    []Issue{{Severity: Error, Message: fencedJSONMsg, Line: 5}},
		},
		{
			name:    "plain fence is ignored",
			content: skillLines("# GitHub", "", "When to use: x.", "", "```text", "input:", "  properties:", "```"),
			want:    nil,
		},
		{
			name:    "yaml fence without schema markers is ignored",
			content: skillLines("# GitHub", "", "When to use: x.", "", "```yaml", "greeting: hello", "count: 3", "```"),
			want:    nil,
		},
		{
			name:    "parameter type line restates schema",
			content: skillLines("# GitHub", "", "When to use: x.", "", "owner: string"),
			want:    []Issue{{Severity: Error, Message: paramOwnerMsg, Line: 5}},
		},
		{
			name:    "non-property type line is ignored",
			content: skillLines("# GitHub", "", "When to use: x.", "", "timeout: integer"),
			want:    nil,
		},
		{
			name:    "parameters section listing fields",
			content: skillLines("# GitHub", "", "When to use: x.", "", "## Parameters", "", "- owner: string", "- repo: string"),
			want:    []Issue{{Severity: Error, Message: sectionParamsMsg, Line: 5}},
		},
		{
			name:    "inputs section naming a property",
			content: skillLines("# GitHub", "", "When to use: x.", "", "## Inputs", "", "The owner and repo identify the target."),
			want:    []Issue{{Severity: Error, Message: sectionInputsMsg, Line: 5}},
		},
		{
			name:    "arguments section without fields is ignored",
			content: skillLines("# GitHub", "", "When to use: x.", "", "## Arguments", "", "Choose arguments that narrow the request as much as possible."),
			want:    nil,
		},
		{
			name:    "unknown operation reference warns",
			content: skillLines("# GitHub", "", "When to use: x.", "", "Call `get_pull_request` to fetch a single pull request."),
			want:    []Issue{{Severity: Warning, Message: unknownGetPRMsg, Line: 5}},
		},
		{
			name:    "known operation reference is quiet",
			content: skillLines("# GitHub", "", "When to use: x.", "", "Call `get_repository` then `list_pull_requests`."),
			want:    nil,
		},
		{
			name:    "property reference is quiet",
			content: skillLines("# GitHub", "", "When to use: x.", "", "Pass `owner` and `repo` to scope the request."),
			want:    nil,
		},
		{
			name:    "unknown operation in arrow sequence warns",
			content: skillLines("# GitHub", "", "A useful sequence is:", "", "get_repository", "→ get_pull_request"),
			want:    []Issue{{Severity: Warning, Message: unknownGetPRMsg, Line: 6}},
		},
		{
			name:    "issues are sorted by line",
			content: skillLines("# GitHub", "", "Call `get_pull_request` first.", "", "When to use: x.", "", "owner: string"),
			want:    []Issue{{Severity: Warning, Message: unknownGetPRMsg, Line: 3}, {Severity: Error, Message: paramOwnerMsg, Line: 7}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Lint(tt.content, testManifest())
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Lint() = %s, want %s", formatIssues(got), formatIssues(tt.want))
			}
		})
	}
}

// formatIssues renders issues with their severity and line so a failure is
// readable without dumping struct internals.
func formatIssues(issues []Issue) string {
	if len(issues) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(issues))
	for _, issue := range issues {
		parts = append(parts, fmt.Sprintf("%s@%d: %s", issue.Severity, issue.Line, issue.Message))
	}
	return "[" + strings.Join(parts, "; ") + "]"
}

func TestSeverityString(t *testing.T) {
	if got := Error.String(); got != "error" {
		t.Fatalf("Error.String() = %q, want %q", got, "error")
	}
	if got := Warning.String(); got != "warning" {
		t.Fatalf("Warning.String() = %q, want %q", got, "warning")
	}
}
