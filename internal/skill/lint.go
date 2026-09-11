package skill

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"relay/internal/manifest"
)

// Severity classifies a lint finding. An Error fails a build; a Warning is
// surfaced but tolerated (spec §30).
type Severity int

const (
	// Error marks a skill that violates the manifest/skill separation and must
	// fail the build (spec §30).
	Error Severity = iota
	// Warning marks guidance worth surfacing that does not block a build
	// (spec §7, §30).
	Warning
)

// String renders a Severity for messages and test failures.
func (s Severity) String() string {
	switch s {
	case Error:
		return "error"
	case Warning:
		return "warning"
	default:
		return fmt.Sprintf("severity(%d)", int(s))
	}
}

// Issue is one lint finding. Line is 1-based; a finding that is not tied to a
// single line (for example a missing workflow) is anchored at line 1.
type Issue struct {
	Severity Severity
	Message  string
	Line     int
}

// Lint checks a SKILL.md against the manifest it ships with. The manifest is
// machine truth and the skill is LLM guidance (spec §7, §30), so every
// heuristic here exists to stop a skill from becoming a second schema or from
// pointing at operations that do not exist.
//
// The rules are deliberately few and anchored to names the manifest declares
// (operation and property names). A loose scan for "anything schema-like"
// would fire on ordinary prose, which would train authors to ignore the
// linter; anchoring keeps every finding explainable and trustworthy.
// Issues are returned sorted by line (then severity, then message) so output
// is deterministic.
func Lint(text string, doc *manifest.Document) []Issue {
	if strings.TrimSpace(text) == "" {
		return []Issue{{
			Severity: Error,
			Message:  "skill is empty",
			Line:     1,
		}}
	}

	idx := newIndex(doc)
	lines := strings.Split(text, "\n")

	var issues []Issue
	issues = append(issues, lintFencedSchema(lines, idx)...)
	issues = append(issues, lintParamTypeLines(lines, idx)...)
	issues = append(issues, lintSchemaSections(lines, idx)...)
	issues = append(issues, lintWorkflow(text, lines)...)
	issues = append(issues, lintReferences(lines, idx)...)

	sortIssues(issues)
	return issues
}

// manifestIndex is the set of names the manifest owns. Heuristics only fire
// when they recognize one of these names, which is what makes them
// high-signal: a finding always traces back to a real declaration.
type manifestIndex struct {
	ops   map[string]struct{}
	props map[string]struct{}
	lower map[string]struct{} // lowercased property names
}

func newIndex(doc *manifest.Document) manifestIndex {
	idx := manifestIndex{
		ops:   map[string]struct{}{},
		props: map[string]struct{}{},
		lower: map[string]struct{}{},
	}
	if doc == nil {
		return idx
	}
	for _, tool := range doc.Tools {
		if tool.Name != "" {
			idx.ops[tool.Name] = struct{}{}
		}
		for name := range tool.Input.Properties {
			idx.props[name] = struct{}{}
			idx.lower[strings.ToLower(name)] = struct{}{}
		}
	}
	return idx
}

func (idx manifestIndex) isOp(name string) bool {
	_, ok := idx.ops[name]
	return ok
}

func (idx manifestIndex) isProp(name string) bool {
	if _, ok := idx.props[name]; ok {
		return true
	}
	_, ok := idx.lower[strings.ToLower(name)]
	return ok
}

// tick is the inline-code delimiter. It is written as a hex escape so this
// source contains no literal backtick, which is otherwise awkward to read.
const tick = "\x60"

// Regular expressions. Each is intentionally narrow; see the rule docs.
var (
	fenceOpenRe  = regexp.MustCompile("^\\s*(" + tick + "{3,}|~{3,})[ \\t]*([A-Za-z0-9_+-]*)[ \\t]*$")
	fenceCloseRe = regexp.MustCompile("^\\s*(" + tick + "{3,}|~{3,})[ \\t]*$")

	// yamlKeyRe matches a YAML mapping key at the start of a line, used to spot
	// an operation name dumped as a key inside a fenced block.
	yamlKeyRe = regexp.MustCompile("^\\s*([A-Za-z_][A-Za-z0-9_-]*)\\s*:")
	jsonKeyRe = regexp.MustCompile("^\\s*\"([A-Za-z_][A-Za-z0-9_-]*)\"\\s*:")

	// paramTypeRe matches "name: type" lines such as "owner: string", which is
	// the schema-restating form seen in spec §30. It fires only when the name is
	// a declared input property, so prose like "Note: string" is ignored.
	paramTypeRe = regexp.MustCompile("(?i)^\\s*(?:[-*]\\s+)?([A-Za-z_][A-Za-z0-9_]*)\\s*:\\s*(string|integer|number|boolean|array|object)\\s*$")

	headingRe = regexp.MustCompile("^(#{1,6})\\s+(.*\\S)\\s*$")

	// schemaSectionRe matches headings that announce an argument listing.
	schemaSectionRe = regexp.MustCompile("(?i)\\b(parameters|inputs|arguments)\\b")

	numberedStepRe = regexp.MustCompile("^\\s*\\d+[.)]\\s+\\S")
	arrowRe        = regexp.MustCompile("→|->")
	whenToUseRe    = regexp.MustCompile("(?i)\\bwhen to use\\b")

	backtickRe = regexp.MustCompile(tick + "([^" + tick + "]+)" + tick)
	wordRe     = regexp.MustCompile("[A-Za-z_][A-Za-z0-9_]*")

	// opShapeRe matches the snake_case shape Relay uses for operation names
	// (get_repository). Restricting broken-pointer checks to this shape keeps
	// them from firing on ordinary prose words.
	opShapeRe = regexp.MustCompile("^[a-z][a-z0-9]*(_[a-z0-9]+)+$")
)

// schemaKeys are the mapping keys that only appear when a block is restating
// the manifest's input schema. Bare "type" is excluded on purpose: it shows up
// in harmless config examples and would make the rule noisy.
var schemaKeys = map[string]struct{}{
	"input":      {},
	"properties": {},
	"required":   {},
	"tools":      {},
	"apiVersion": {},
}

// lintFencedSchema catches a YAML/JSON fenced block that mirrors the manifest's
// tools — the clearest way a skill becomes a second schema. The block must both
// be labeled yaml/yml/json and contain either a schema marker or a declared
// operation name, so a fenced config example or a plain-text diagram is not
// flagged (spec §30).
func lintFencedSchema(lines []string, idx manifestIndex) []Issue {
	var issues []Issue
	inFence := false
	info := ""
	start := 0
	var buf []string

	for i, line := range lines {
		if !inFence {
			if m := fenceOpenRe.FindStringSubmatch(line); m != nil {
				inFence = true
				info = strings.ToLower(m[2])
				start = i
				buf = buf[:0]
			}
			continue
		}
		if fenceCloseRe.MatchString(line) {
			if isSchemaFence(info) && fenceMirrorsManifest(buf, idx) {
				issues = append(issues, Issue{
					Severity: Error,
					Message:  fmt.Sprintf("skill restates the input schema in a fenced %s block; the manifest is the schema source (spec §30)", displayLang(info)),
					Line:     start + 1,
				})
			}
			inFence = false
			continue
		}
		buf = append(buf, line)
	}
	return issues
}

func isSchemaFence(info string) bool {
	switch info {
	case "yaml", "yml", "json":
		return true
	default:
		return false
	}
}

func displayLang(info string) string {
	if info == "yml" {
		return "yaml"
	}
	return info
}

func fenceMirrorsManifest(buf []string, idx manifestIndex) bool {
	for _, line := range buf {
		key, ok := mappingKey(strings.TrimSpace(line))
		if !ok {
			continue
		}
		if _, isSchema := schemaKeys[key]; isSchema {
			return true
		}
		if idx.isOp(key) {
			return true
		}
	}
	return false
}

// mappingKey returns the key of a YAML or JSON mapping line, so a schema dump
// is recognized whether it is unquoted (YAML) or quoted (JSON).
func mappingKey(line string) (string, bool) {
	if m := yamlKeyRe.FindStringSubmatch(line); m != nil {
		return m[1], true
	}
	if m := jsonKeyRe.FindStringSubmatch(line); m != nil {
		return m[1], true
	}
	return "", false
}

// lintParamTypeLines catches a line enumerating a parameter name and its type,
// for example "owner: string" or "- repo: string". Requiring the name to be a
// declared property is what makes this explainable: it is always a real input
// being copied out of the schema (spec §30).
func lintParamTypeLines(lines []string, idx manifestIndex) []Issue {
	var issues []Issue
	inFence := false
	// Lines inside a schema section are reported once, at the heading, rather
	// than once per field; lintSchemaSections owns that finding.
	inSchemaSection := false
	for i, line := range lines {
		if fenceOpenRe.MatchString(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if m := headingRe.FindStringSubmatch(line); m != nil {
			inSchemaSection = schemaSectionRe.MatchString(m[2])
			continue
		}
		if inSchemaSection {
			continue
		}
		m := paramTypeRe.FindStringSubmatch(line)
		if m == nil || !idx.isProp(m[1]) {
			continue
		}
		issues = append(issues, Issue{
			Severity: Error,
			Message:  fmt.Sprintf("skill enumerates input parameter %q with its type; the manifest owns input types (spec §30)", m[1]),
			Line:     i + 1,
		})
	}
	return issues
}

// lintSchemaSections catches a "Parameters"/"Inputs"/"Arguments" section that
// lists fields. The heading alone is not enough — the body must actually name a
// declared property or repeat a "name: type" line — so a section that merely
// discusses how to choose arguments stays quiet (spec §30).
func lintSchemaSections(lines []string, idx manifestIndex) []Issue {
	var issues []Issue
	for i, line := range lines {
		m := headingRe.FindStringSubmatch(line)
		if m == nil || !schemaSectionRe.MatchString(m[2]) {
			continue
		}
		if schemaSectionListsFields(lines[i+1:], idx) {
			issues = append(issues, Issue{
				Severity: Error,
				Message:  fmt.Sprintf("skill section %q lists input fields; the manifest owns the input schema (spec §30)", strings.TrimSpace(m[2])),
				Line:     i + 1,
			})
		}
	}
	return issues
}

func schemaSectionListsFields(body []string, idx manifestIndex) bool {
	for _, line := range body {
		if headingRe.MatchString(line) {
			return false // section ended
		}
		if paramTypeRe.MatchString(line) {
			return true
		}
		for _, word := range wordRe.FindAllString(line, -1) {
			if idx.isProp(word) {
				return true
			}
		}
	}
	return false
}

// lintWorkflow warns when a skill offers no guidance about how to sequence the
// operations. Spec §7 puts "how should an agent use these operations" at the
// heart of the skill, and spec §30 lists workflow sequences as the thing skills
// should carry, so a skill without any of them is surfaced as a warning.
//
// Three independent signals count as workflow guidance: an arrow sequence
// (→ / ->) as in spec §30, an ordered list, or an explicit "when to use" rule.
func lintWorkflow(text string, lines []string) []Issue {
	if arrowRe.MatchString(text) || whenToUseRe.MatchString(text) || hasNumberedStep(lines) {
		return nil
	}
	return []Issue{{
		Severity: Warning,
		Message:  "skill has no workflow guidance: add an arrow sequence (→ or ->), a numbered workflow, or a \"when to use\" rule (spec §7, §30)",
		Line:     1,
	}}
}

func hasNumberedStep(lines []string) bool {
	for _, line := range lines {
		if numberedStepRe.MatchString(line) {
			return true
		}
	}
	return false
}

// lintReferences warns on a broken pointer: a skill that names an operation the
// manifest does not declare. A wrong pointer is worse than no pointer because it
// sends an agent looking for something that is not there (spec §30).
//
// Candidates are backticked inline-code spans plus the bare words of an arrow
// sequence (where spec §30 writes operation names without backticks). A token
// counts only when it has the snake_case operation shape and is neither a
// declared operation nor a declared input property, so prose and parameter
// mentions stay quiet.
func lintReferences(lines []string, idx manifestIndex) []Issue {
	var issues []Issue
	seen := map[string]struct{}{}

	consider := func(token string, line int) {
		token = strings.TrimSpace(token)
		if !opShapeRe.MatchString(token) || idx.isOp(token) || idx.isProp(token) {
			return
		}
		key := fmt.Sprintf("%s@%d", token, line)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		issues = append(issues, Issue{
			Severity: Warning,
			Message:  fmt.Sprintf("skill names operation %q, which the manifest does not declare; a broken pointer is worse than none (spec §30)", token),
			Line:     line,
		})
	}

	for i, line := range lines {
		for _, m := range backtickRe.FindAllStringSubmatch(line, -1) {
			consider(m[1], i+1)
		}
		// The head of an arrow sequence may sit on the line just before the
		// first arrow, so treat a line whose successor carries an arrow as part
		// of the sequence too.
		arrowLine := arrowRe.MatchString(line)
		if i+1 < len(lines) {
			arrowLine = arrowLine || arrowRe.MatchString(lines[i+1])
		}
		if arrowLine {
			for _, token := range wordRe.FindAllString(line, -1) {
				consider(token, i+1)
			}
		}
	}
	return issues
}

func sortIssues(issues []Issue) {
	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Line != issues[j].Line {
			return issues[i].Line < issues[j].Line
		}
		if issues[i].Severity != issues[j].Severity {
			return issues[i].Severity < issues[j].Severity
		}
		return issues[i].Message < issues[j].Message
	})
}
