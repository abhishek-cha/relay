package daemon

import (
	"context"
	"fmt"
	"strings"

	"relay/internal/ipc"
	"relay/pkg/relay"
)

// skill serves one registered tool's embedded SKILL.md text (spec §8, §29).
//
// The text is read from the installed binary's --skill contract on every
// request, exactly as inspect re-reads --describe. The registry records only
// whether a skill exists, never its text, so the binary stays the single source
// of truth and a tool replaced in place is picked up without re-registering
// (spec §16).
func (d *Daemon) skill(ctx context.Context, name string) ipc.SkillResponse {
	installation, err := d.store.Get(name)
	if err != nil {
		return failedSkill(unknownTool(err, name))
	}
	text, failure := d.readSkill(ctx, installation, name)
	if failure != nil {
		return failedSkill(failure)
	}
	return ipc.SkillResponse{Success: true, Tool: name, Skill: text}
}

// readSkill runs the tool's --skill contract (spec §9).
func (d *Daemon) readSkill(ctx context.Context, installation relay.Installation, name string) (string, *relay.Error) {
	output, failure := d.run(ctx, installation.Path, "--skill")
	if failure != nil {
		// A tool that embeds no skill exits non-zero for --skill. The registry's
		// Skill flag is discovery metadata only, so it never supplies text; it is
		// used here just to pick the same OPERATION_NOT_FOUND the binary itself
		// reports, rather than a confusing RUNTIME_INCOMPATIBLE (spec §16, §26).
		if !installation.Skill {
			return "", noSkill(name)
		}
		return "", failure
	}
	text := string(output)
	if strings.TrimSpace(text) == "" {
		return "", noSkill(name)
	}
	return text, nil
}

// noSkill is the structured error for a registered tool with no embedded skill.
// It mirrors the code the runtime itself returns for --skill, so the daemon and
// the binary agree on what went wrong (spec §26).
func noSkill(name string) *relay.Error {
	return relay.NewError(relay.CodeOperationNotFound,
		fmt.Sprintf("tool %q has no embedded skill", name))
}

func failedSkill(err *relay.Error) ipc.SkillResponse {
	return ipc.SkillResponse{Success: false, Error: err}
}
