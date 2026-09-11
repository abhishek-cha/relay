package daemon

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"relay/internal/ipc"
	"relay/internal/paths"
	"relay/internal/registry"
	"relay/pkg/relay"
)

// skillTestText is the exact embedded guidance the fake tool serves (spec §8).
const skillTestText = "# Demo Skill\n\nUse `demo ping` to check connectivity.\n"

// writeSkillToolAt writes an executable standing in for an installed Relay tool.
// When hasSkill is true it prints skillText verbatim for --skill, exactly as a
// real binary does; otherwise it exits non-zero with the runtime's no-skill
// diagnostic, so an absent skill looks like a real tool's (spec §9).
func writeSkillToolAt(t *testing.T, path, skillText string, hasSkill bool) {
	t.Helper()
	script := "#!/bin/sh\n"
	if hasSkill {
		script += "if [ \"$1\" = \"--skill\" ]; then\n" +
			"cat <<'RELAY_SKILL_TEST_HEREDOC'\n" +
			skillText +
			"RELAY_SKILL_TEST_HEREDOC\n" +
			"exit 0\n" +
			"fi\n"
	}
	script += "echo \"no skill is embedded in this tool\" >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tool: %v", err)
	}
}

// newSkillHarness builds a daemon over a throwaway Relay home with one
// registered skill-bearing tool. The registry record's Skill flag is discovery
// metadata; the text always comes from the binary (spec §15, §16).
func newSkillHarness(t *testing.T, path string, hasSkill bool) *Daemon {
	t.Helper()
	t.Setenv(paths.EnvHome, t.TempDir())
	layout := paths.Default()
	if err := registry.New(layout.Registry).Put(relay.Installation{
		Name:    "demo",
		Version: "1.0.0",
		Path:    path,
		Runtime: "relay/v1",
		Skill:   hasSkill,
	}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	return New(Config{
		Layout:   layout,
		Version:  "test",
		Keychain: newMemKeychain(),
		Log:      io.Discard,
	})
}

func skillRequestFrame(t *testing.T, tool string) []byte {
	t.Helper()
	frame, err := json.Marshal(ipc.SkillRequest{Type: ipc.FrameSkill, Tool: tool})
	if err != nil {
		t.Fatalf("marshal skill request: %v", err)
	}
	return frame
}

// serveSkill drives one skill frame through the daemon's real handler.
func serveSkill(t *testing.T, d *Daemon, tool string) ipc.SkillResponse {
	t.Helper()
	reply, err := d.Handle(context.Background(), ipc.FrameSkill, skillRequestFrame(t, tool))
	if err != nil {
		t.Fatalf("Handle(%q): %v", ipc.FrameSkill, err)
	}
	response, ok := reply.(ipc.SkillResponse)
	if !ok {
		t.Fatalf("reply is %T, want ipc.SkillResponse", reply)
	}
	return response
}

func TestDaemonServesExactEmbeddedSkill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "demo")
	writeSkillToolAt(t, path, skillTestText, true)
	d := newSkillHarness(t, path, true)

	response := serveSkill(t, d, "demo")
	if !response.Success || response.Error != nil {
		t.Fatalf("response = %+v, want success", response)
	}
	if response.Tool != "demo" {
		t.Fatalf("tool = %q, want demo", response.Tool)
	}
	if response.Skill != skillTestText {
		t.Fatalf("skill = %q, want the binary's exact text %q", response.Skill, skillTestText)
	}
}

// TestDaemonSkillReadsTheBinaryEveryTime proves the daemon keeps no cached copy:
// a binary replaced in place is served with its new text (spec §16).
func TestDaemonSkillReadsTheBinaryEveryTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "demo")
	writeSkillToolAt(t, path, "first version\n", true)
	d := newSkillHarness(t, path, true)

	if got := serveSkill(t, d, "demo"); got.Skill != "first version\n" {
		t.Fatalf("first skill = %q", got.Skill)
	}

	writeSkillToolAt(t, path, "second version\n", true)
	if got := serveSkill(t, d, "demo"); got.Skill != "second version\n" {
		t.Fatalf("daemon served a cached skill: got %q, want the replaced binary's text", got.Skill)
	}
}

func TestDaemonSkillUnknownToolIsStructuredError(t *testing.T) {
	t.Setenv(paths.EnvHome, t.TempDir())
	d := New(Config{
		Layout:   paths.Default(),
		Version:  "test",
		Keychain: newMemKeychain(),
		Log:      io.Discard,
	})

	response := serveSkill(t, d, "nope")
	if response.Success {
		t.Fatalf("unknown tool reported success: %+v", response)
	}
	if response.Error == nil || response.Error.Code != relay.CodeToolNotFound {
		t.Fatalf("error = %+v, want %s", response.Error, relay.CodeToolNotFound)
	}
}

func TestDaemonSkillWithoutSkillIsStructuredError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "demo")
	writeSkillToolAt(t, path, "", false)
	d := newSkillHarness(t, path, false)

	response := serveSkill(t, d, "demo")
	if response.Success {
		t.Fatalf("a tool with no skill reported success: %+v", response)
	}
	if response.Error == nil || response.Error.Code != relay.CodeOperationNotFound {
		t.Fatalf("error = %+v, want %s", response.Error, relay.CodeOperationNotFound)
	}
}

func TestDaemonSkillWhitespaceOnlyIsStructuredError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "demo")
	writeSkillToolAt(t, path, "   \n", true)
	d := newSkillHarness(t, path, true)

	response := serveSkill(t, d, "demo")
	if response.Error == nil || response.Error.Code != relay.CodeOperationNotFound {
		t.Fatalf("error = %+v, want %s for whitespace-only skill", response.Error, relay.CodeOperationNotFound)
	}
}

func TestDaemonSkillMalformedFrameIsStructuredError(t *testing.T) {
	t.Setenv(paths.EnvHome, t.TempDir())
	d := New(Config{Layout: paths.Default(), Version: "test", Keychain: newMemKeychain(), Log: io.Discard})

	reply, err := d.Handle(context.Background(), ipc.FrameSkill, []byte("{"))
	if err != nil {
		t.Fatalf("Handle returned a transport error: %v", err)
	}
	response, ok := reply.(ipc.SkillResponse)
	if !ok {
		t.Fatalf("reply is %T, want ipc.SkillResponse", reply)
	}
	if response.Error == nil || response.Error.Code != relay.CodeInvalidInput {
		t.Fatalf("error = %+v, want %s", response.Error, relay.CodeInvalidInput)
	}
}
