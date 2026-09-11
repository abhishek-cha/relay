package skill

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// frontmatterDelimiter fences an optional YAML frontmatter block at the top of
// a SKILL.md.
const frontmatterDelimiter = "---"

// Version reports the version a skill declares in its optional YAML frontmatter
// (for example "version: 1.4.0").
//
// ok is false when the skill carries no frontmatter version marker. That is the
// backward-compatible case: a skill without a version is valid and is never
// compared against the manifest (spec §36). A frontmatter block that is present
// but not valid YAML is reported as an error rather than silently ignored, so a
// typo cannot quietly disable the manifest/skill version check.
func Version(data []byte) (version string, ok bool, err error) {
	block, found := frontmatter(data)
	if !found {
		return "", false, nil
	}
	var fm struct {
		Version string `yaml:"version"`
	}
	if err := yaml.Unmarshal(block, &fm); err != nil {
		return "", false, fmt.Errorf("parse skill frontmatter: %w", err)
	}
	version = strings.TrimSpace(fm.Version)
	if version == "" {
		return "", false, nil
	}
	return version, true, nil
}

// frontmatter extracts the YAML between the first two "---" delimiter lines
// when the document opens with one. An unterminated block is not frontmatter,
// so a stray horizontal rule cannot swallow the whole skill.
func frontmatter(data []byte) ([]byte, bool) {
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) == 0 || !isFrontmatterDelimiter(lines[0]) {
		return nil, false
	}
	for i := 1; i < len(lines); i++ {
		if isFrontmatterDelimiter(lines[i]) {
			return bytes.Join(lines[1:i], []byte("\n")), true
		}
	}
	return nil, false
}

func isFrontmatterDelimiter(line []byte) bool {
	return strings.TrimRight(string(line), " \t\r") == frontmatterDelimiter
}
