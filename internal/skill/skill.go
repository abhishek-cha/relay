// Package skill validates SKILL.md content.
//
// A skill is LLM guidance, not a second schema (spec §30). It answers "how
// should an agent use these operations?" — never "what inputs does this
// operation take?", which the manifest already owns.
package skill

import (
	"bytes"
	"fmt"
	"strings"
)

// Validate checks that a skill is present and usable. It is deliberately
// shallow. Rejecting a skill that restates the manifest's schema takes the
// manifest as context, so it lives in Lint instead; the build calls Lint with
// the parsed document (spec §30).
func Validate(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return fmt.Errorf("skill is empty")
	}
	if !hasHeading(text) {
		return fmt.Errorf("skill must contain a markdown heading (for example %q)", "# GitHub")
	}
	return nil
}

func hasHeading(text string) bool {
	for _, line := range bytes.Split([]byte(text), []byte("\n")) {
		trimmed := strings.TrimSpace(string(line))
		if strings.HasPrefix(trimmed, "#") {
			return true
		}
	}
	return false
}
