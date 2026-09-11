package mcp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relay/internal/daemon"
	"relay/internal/ipc"
	"relay/internal/keychain"
	"relay/internal/paths"
	"relay/internal/protocol"
	"relay/internal/registry"
	"relay/pkg/relay"
)

// stubSkills is the narrow SkillSource seam faked for the resources handlers.
type stubSkills struct {
	resources []SkillResource
	texts     map[string]string
	listErr   error
	readErr   *relay.Error
	lastTool  string
}

func (s *stubSkills) ListSkills(context.Context) ([]SkillResource, error) {
	return s.resources, s.listErr
}

func (s *stubSkills) ReadSkill(_ context.Context, tool string) (string, *relay.Error) {
	s.lastTool = tool
	if s.readErr != nil {
		return "", s.readErr
	}
	return s.texts[tool], nil
}

func skillServer(skills SkillSource) *Server {
	s := newServer(&stubSource{tools: githubDescriptors()}, &stubInvoker{})
	s.Skills = skills
	return s
}

// relayErrorData extracts the structured relay error a protocol failure carries
// in its "data" field (spec §26, §29).
func relayErrorData(t *testing.T, resp rpcResponse) relay.Error {
	t.Helper()
	if resp.Error == nil {
		t.Fatalf("expected a protocol error, got %#v", resp.Result)
	}
	if resp.Error.Data == nil {
		t.Fatalf("protocol error carries no structured data: %#v", resp.Error)
	}
	encoded, err := json.Marshal(resp.Error.Data)
	if err != nil {
		t.Fatalf("marshal error data: %v", err)
	}
	var structured relay.Error
	if err := json.Unmarshal(encoded, &structured); err != nil {
		t.Fatalf("error data is not a relay.Error: %v", err)
	}
	return structured
}

func TestInitializeAdvertisesResourcesCapability(t *testing.T) {
	s := newServer(&stubSource{}, &stubInvoker{})
	_, responses := serve(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`)

	caps, ok := resultMap(t, requireOne(t, responses))["capabilities"].(map[string]any)
	if !ok {
		t.Fatal("capabilities is not an object")
	}
	if _, ok := caps["resources"]; !ok {
		t.Fatalf("resources capability not advertised: %#v", caps)
	}
}

func TestResourcesListAdvertisesEmbeddedSkills(t *testing.T) {
	skills := &stubSkills{resources: []SkillResource{{
		Tool:        "github",
		Name:        "github",
		Description: "Embedded SKILL.md for the github tool",
	}}}
	_, responses := serve(t, skillServer(skills), `{"jsonrpc":"2.0","id":2,"method":"resources/list"}`)

	m := resultMap(t, requireOne(t, responses))
	resources, ok := m["resources"].([]any)
	if !ok || len(resources) != 1 {
		t.Fatalf("resources = %#v, want one entry", m["resources"])
	}
	entry := resources[0].(map[string]any)
	if entry["uri"] != "relay://skill/github" {
		t.Fatalf("uri = %v, want relay://skill/github (§29)", entry["uri"])
	}
	if entry["mimeType"] != SkillMIMEType {
		t.Fatalf("mimeType = %v, want %q", entry["mimeType"], SkillMIMEType)
	}
	if entry["name"] != "github" {
		t.Fatalf("name = %v", entry["name"])
	}
}

func TestResourcesListWithoutSourceIsEmpty(t *testing.T) {
	s := newServer(&stubSource{tools: githubDescriptors()}, &stubInvoker{})
	_, responses := serve(t, s, `{"jsonrpc":"2.0","id":3,"method":"resources/list"}`)

	m := resultMap(t, requireOne(t, responses))
	resources, ok := m["resources"].([]any)
	if !ok || len(resources) != 0 {
		t.Fatalf("resources = %#v, want an empty list rather than a fault", m["resources"])
	}
}

func TestResourcesListSourceErrorIsProtocolError(t *testing.T) {
	skills := &stubSkills{listErr: context.DeadlineExceeded}
	_, responses := serve(t, skillServer(skills), `{"jsonrpc":"2.0","id":4,"method":"resources/list"}`)

	resp := requireOne(t, responses)
	if resp.Error == nil || resp.Error.Code != CodeInternalError {
		t.Fatalf("error = %#v, want %d", resp.Error, CodeInternalError)
	}
}

func TestResourcesReadReturnsExactSkillText(t *testing.T) {
	const text = "# GitHub Skill\n\nUse `github_get_repository` first.\n"
	skills := &stubSkills{texts: map[string]string{"github": text}}
	_, responses := serve(t, skillServer(skills),
		`{"jsonrpc":"2.0","id":5,"method":"resources/read","params":{"uri":"relay://skill/github"}}`)

	if skills.lastTool != "github" {
		t.Fatalf("read tool = %q, want github", skills.lastTool)
	}
	m := resultMap(t, requireOne(t, responses))
	contents, ok := m["contents"].([]any)
	if !ok || len(contents) != 1 {
		t.Fatalf("contents = %#v, want one entry", m["contents"])
	}
	first := contents[0].(map[string]any)
	if first["text"] != text {
		t.Fatalf("text = %q, want %q", first["text"], text)
	}
	if first["uri"] != "relay://skill/github" || first["mimeType"] != SkillMIMEType {
		t.Fatalf("content = %#v", first)
	}
}

func TestResourcesReadUnknownToolIsStructuredError(t *testing.T) {
	skills := &stubSkills{readErr: relay.NewError(relay.CodeToolNotFound,
		"tool \"nope\" is not registered; run 'relay install <path>'")}
	_, responses := serve(t, skillServer(skills),
		`{"jsonrpc":"2.0","id":6,"method":"resources/read","params":{"uri":"relay://skill/nope"}}`)

	resp := requireOne(t, responses)
	if resp.Error == nil || resp.Error.Code != CodeInvalidParams {
		t.Fatalf("error = %#v, want JSON-RPC %d", resp.Error, CodeInvalidParams)
	}
	if structured := relayErrorData(t, resp); structured.Code != relay.CodeToolNotFound {
		t.Fatalf("structured code = %q, want %q", structured.Code, relay.CodeToolNotFound)
	}
}

func TestResourcesReadUnknownURIIsStructuredError(t *testing.T) {
	for _, uri := range []string{"relay://tools/github", "relay://skill/../etc", "https://example.test/skill"} {
		skills := &stubSkills{texts: map[string]string{"github": "x"}}
		frame := `{"jsonrpc":"2.0","id":7,"method":"resources/read","params":{"uri":"` + uri + `"}}`
		_, responses := serve(t, skillServer(skills), frame)
		resp := requireOne(t, responses)
		if resp.Error == nil {
			t.Fatalf("uri %q was accepted: %#v", uri, resp.Result)
		}
		if structured := relayErrorData(t, resp); structured.Code != relay.CodeInvalidInput {
			t.Fatalf("uri %q structured code = %q, want %q", uri, structured.Code, relay.CodeInvalidInput)
		}
		if skills.lastTool != "" {
			t.Fatalf("uri %q reached the skill reader as %q", uri, skills.lastTool)
		}
	}
}

func TestResourcesReadMissingParams(t *testing.T) {
	s := skillServer(&stubSkills{})
	_, responses := serve(t, s,
		`{"jsonrpc":"2.0","id":8,"method":"resources/read"}`,
		`{"jsonrpc":"2.0","id":9,"method":"resources/read","params":{}}`,
		`{"jsonrpc":"2.0","id":10,"method":"resources/read","params":"not-an-object"}`,
	)
	if len(responses) != 3 {
		t.Fatalf("got %d responses, want 3", len(responses))
	}
	for i, resp := range responses {
		if resp.Error == nil || resp.Error.Code != CodeInvalidParams {
			t.Fatalf("case %d error = %#v, want %d", i, resp.Error, CodeInvalidParams)
		}
	}
}

// --- end-to-end: the daemon path behind MCP (§27, §29, §41) ---------------

const mcpSkillText = "# Demo Skill\n\nSecret-free guidance.\n"

// realSkillToolScript writes an executable that serves mcpSkillText for --skill
// and exits non-zero otherwise, standing in for an installed Relay tool.
func realSkillToolScript(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "demo")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--skill\" ]; then\n" +
		"cat <<'RELAY_MCP_SKILL_HEREDOC'\n" +
		content +
		"RELAY_MCP_SKILL_HEREDOC\n" +
		"exit 0\n" +
		"fi\n" +
		"echo \"no skill is embedded in this tool\" >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tool: %v", err)
	}
	return path
}

// TestSecurityMCPSkillParityAndNoCredentialRoute proves §29 parity end to end:
// the MCP resource text is byte-identical to what the daemon serves over the
// skill IPC frame, and reading a skill neither executes the tool nor reaches the
// Keychain, so MCP gains no credential route (spec §41).
func TestSecurityMCPSkillParityAndNoCredentialRoute(t *testing.T) {
	t.Setenv(paths.EnvHome, t.TempDir())
	layout := paths.Default()

	// A Unix socket path must stay under macOS's 104-byte sun_path limit, and
	// t.TempDir embeds the (long) test name, so bind the daemon on a short
	// throwaway path rather than the layout default.
	socketDir, err := os.MkdirTemp("", "relay-skill-")
	if err != nil {
		t.Fatalf("socket dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(socketDir) })

	store := registry.New(layout.Registry)
	if err := store.Put(relay.Installation{
		Name:    "demo",
		Version: "1.0.0",
		Path:    realSkillToolScript(t, mcpSkillText),
		Runtime: "relay/v1",
		Skill:   true,
	}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	// A credential exists and a credential-thirsty manifest is registered, so a
	// skill read that touched either would be visible below.
	kc := newSecMCPKeychain()
	if err := kc.Set(keychain.Service("demo"), keychain.AccountDefault, mcpSecuritySentinel); err != nil {
		t.Fatalf("seed keychain: %v", err)
	}
	executor := &secMCPExecutor{response: protocol.Response{Status: 200, Body: "ok"}}
	d := daemon.New(daemon.Config{
		Layout:    layout,
		Socket:    filepath.Join(socketDir, "d.sock"),
		Version:   "test",
		Executors: map[string]protocol.Executor{"rest": executor},
		Keychain:  kc,
		Log:       io.Discard,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	go func() { _ = d.Run(ctx, func(socket string) { ready <- socket }) }()

	var socket string
	select {
	case socket = <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon did not start within 5s")
	}

	// The CLI path: one skill frame straight into the daemon's socket.
	var direct ipc.SkillResponse
	if err := ipc.Call(ctx, socket, ipc.SkillRequest{Type: ipc.FrameSkill, Tool: "demo"}, &direct); err != nil {
		t.Fatalf("direct skill call: %v", err)
	}
	if !direct.Success || direct.Skill != mcpSkillText {
		t.Fatalf("direct skill = %+v, want %q", direct, mcpSkillText)
	}

	var log strings.Builder
	source := &RegistrySource{
		Registry: store,
		Skill:    DaemonSkill{Socket: socket}.ReadSkill,
		Log:      &log,
	}
	s := &Server{
		Source:  source,
		Skills:  source,
		Invoker: &stubInvoker{},
		Info:    ServerInfo{Name: "relay-test", Version: "0.0.1"},
		Log:     &log,
	}
	raw, responses := serve(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"relay://skill/demo"}}`,
	)
	if len(responses) != 2 {
		t.Fatalf("got %d responses, want 2", len(responses))
	}

	resources, ok := resultMap(t, responses[0])["resources"].([]any)
	if !ok || len(resources) != 1 {
		t.Fatalf("resources/list = %#v, want the demo skill", responses[0].Result)
	}
	if resources[0].(map[string]any)["uri"] != "relay://skill/demo" {
		t.Fatalf("advertised uri = %#v", resources[0])
	}

	contents, ok := resultMap(t, responses[1])["contents"].([]any)
	if !ok || len(contents) != 1 {
		t.Fatalf("resources/read = %#v, want one content entry", responses[1].Result)
	}
	text, _ := contents[0].(map[string]any)["text"].(string)
	if text != direct.Skill {
		t.Fatalf("MCP and CLI disagree on the skill: MCP %q, CLI %q", text, direct.Skill)
	}
	if text != mcpSkillText {
		t.Fatalf("MCP skill = %q, want %q", text, mcpSkillText)
	}

	if strings.Contains(raw, mcpSecuritySentinel) {
		t.Fatalf("a credential leaked into a JSON-RPC frame: %s", raw)
	}
	if strings.Contains(log.String(), mcpSecuritySentinel) {
		t.Fatalf("a credential leaked into the diagnostic log: %s", log.String())
	}
	if len(executor.requests) != 0 {
		t.Fatal("reading a skill executed the tool, which would be a second execution route")
	}
}
