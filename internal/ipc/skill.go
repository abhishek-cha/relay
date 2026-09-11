package ipc

import "relay/pkg/relay"

// FrameSkill asks the daemon for one registered tool's embedded SKILL.md text
// (spec §8, §29).
//
// The frame kind lives beside the transport because the daemon and the MCP
// adapter are its two speakers: both already share this package, while pkg/relay
// stays the public contract spoken by tool binaries. The wire shape follows the
// other request frames — a flat object discriminated by its "type" field.
const FrameSkill = "skill"

// SkillRequest asks for one tool's skill text.
type SkillRequest struct {
	Type string `json:"type"`
	Tool string `json:"tool"`
}

// SkillResponse carries a tool's embedded SKILL.md, exactly as the tool binary
// printed it for --skill.
//
// The text is never a cached copy: the daemon asks the installed binary on every
// request, because the binary is the source of truth for its own skill
// (spec §16). An absent skill is reported through the structured error shape
// (spec §26) rather than an empty success.
type SkillResponse struct {
	Success bool         `json:"success"`
	Error   *relay.Error `json:"error,omitempty"`
	Tool    string       `json:"tool,omitempty"`
	Skill   string       `json:"skill,omitempty"`
}
