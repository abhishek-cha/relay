package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"relay/internal/ipc"
	"relay/internal/manifest"
	"relay/internal/paths"
	"relay/internal/registry"
	"relay/pkg/relay"
	"relay/pkg/toolruntime"
)

// manifestReadTimeout bounds how long discovery waits for one installed binary
// to answer. A hung tool must not wedge the whole tools/list call; skipping it
// is better than blocking every other tool behind it (spec §40).
const manifestReadTimeout = 10 * time.Second

// RegistryLister is the discovery-metadata source the MCP catalog is built from
// (spec §15). It is an interface rather than a concrete *registry.Store so the
// adapter can be tested without a real ~/.relay directory.
type RegistryLister interface {
	List() ([]relay.Installation, error)
}

// ManifestReader reads a tool binary's authoritative manifest. It is the
// binary, not the registry, that owns the schema (spec §9, §16); routing schema
// discovery through this seam is what keeps MCP from inventing a second schema
// source.
type ManifestReader func(ctx context.Context, path string) ([]byte, error)

// ExecManifestReader asks an installed binary for its own manifest by running
// it with --manifest, the same authoritative path the daemon uses to re-read a
// tool's schema on every invoke (spec §16).
func ExecManifestReader(ctx context.Context, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, manifestReadTimeout)
	defer cancel()

	output, err := exec.CommandContext(ctx, path, "--manifest").Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return nil, fmt.Errorf("%s --manifest failed: %v: %s", path, err, exit.Stderr)
		}
		return nil, fmt.Errorf("%s --manifest failed: %w", path, err)
	}
	return output, nil
}

// RegistrySource builds the MCP tool catalog from the registry plus each
// installed binary's manifest.
//
// The registry is discovery metadata only (spec §16): it says which tools exist
// and where their binaries live, while the binary supplies the operations and
// input schemas. A tool that cannot be read is logged and skipped rather than
// failing the whole list, so one broken install does not hide every other tool.
type RegistrySource struct {
	// Registry supplies the installed tools. Required.
	Registry RegistryLister
	// Read reads a tool's manifest from its installed path. Defaults to
	// ExecManifestReader.
	Read ManifestReader
	// Skill reads a tool's embedded SKILL.md through the daemon. It defaults to
	// DaemonSkill, which is the only path allowed to run an installed binary
	// (spec §27); the text itself is never cached (spec §16).
	Skill SkillReader
	// Log receives diagnostics about skipped tools. Never stdout.
	Log io.Writer
}

// SkillReader reads one registered tool's embedded SKILL.md through the daemon
// (spec §8, §29). The daemon re-reads the tool binary on every request, so the
// binary stays authoritative for its own skill (spec §16).
type SkillReader func(ctx context.Context, tool string) (string, *relay.Error)

// DaemonSkill is the production SkillReader: it asks the daemon over IPC, the
// same way tool invocation reaches the daemon (spec §27, §29).
//
// Reading through the daemon rather than executing the binary here is what keeps
// MCP from growing a second execution path, and with it a second credential
// route (spec §41).
type DaemonSkill struct {
	// Socket is the daemon socket. Empty means the default Relay home's socket.
	Socket string
}

// ReadSkill implements SkillReader. Transport failures become the same
// NETWORK_ERROR/TIMEOUT codes the CLI reports (spec §26).
func (d DaemonSkill) ReadSkill(ctx context.Context, tool string) (string, *relay.Error) {
	socket := d.Socket
	if socket == "" {
		socket = paths.Default().Socket()
	}

	var response ipc.SkillResponse
	if err := ipc.Call(ctx, socket, ipc.SkillRequest{Type: ipc.FrameSkill, Tool: tool}, &response); err != nil {
		if errors.Is(err, ipc.ErrTimeout) {
			return "", relay.NewError(relay.CodeTimeout,
				"the Relay daemon did not respond in time")
		}
		return "", relay.NewError(relay.CodeNetworkError,
			"the Relay daemon is not available; start it with 'relay daemon start'")
	}
	if !response.Success || response.Error != nil {
		if response.Error == nil {
			return "", relay.NewError(relay.CodeRemoteError, "the daemon returned no skill")
		}
		return "", response.Error
	}
	return response.Skill, nil
}

// ListSkills implements SkillSource. Discovery comes from the registry's
// recorded skill flag (spec §15); the registry never stores the text itself
// (spec §16), so this stays cheap and cannot drift from the binary.
func (s *RegistrySource) ListSkills(ctx context.Context) ([]SkillResource, error) {
	if s.Registry == nil {
		return nil, fmt.Errorf("no registry is configured")
	}
	installations, err := s.Registry.List()
	if err != nil {
		return nil, fmt.Errorf("list registry: %w", err)
	}
	resources := make([]SkillResource, 0, len(installations))
	for _, installation := range installations {
		if installation.Name == "" || !installation.Skill {
			continue
		}
		resources = append(resources, SkillResource{
			Tool:        installation.Name,
			Name:        installation.Name,
			Description: "Embedded SKILL.md for the " + installation.Name + " tool",
		})
	}
	return resources, nil
}

// ReadSkill implements SkillSource. It delegates to the configured reader, so
// the text always comes from the binary via the daemon and is never a cached
// copy (spec §16).
func (s *RegistrySource) ReadSkill(ctx context.Context, tool string) (string, *relay.Error) {
	read := s.Skill
	if read == nil {
		read = DaemonSkill{}.ReadSkill
	}
	return read(ctx, tool)
}

// ListTools implements DescriptorSource.
func (s *RegistrySource) ListTools(ctx context.Context) ([]ToolDescriptor, error) {
	if s.Registry == nil {
		return nil, fmt.Errorf("no registry is configured")
	}
	read := s.Read
	if read == nil {
		read = ExecManifestReader
	}

	installations, err := s.Registry.List()
	if err != nil {
		return nil, fmt.Errorf("list registry: %w", err)
	}

	descriptors := make([]ToolDescriptor, 0, len(installations))
	for _, installation := range installations {
		if installation.Path == "" {
			s.logf("skipping %s: registry record has no binary path", installation.Name)
			continue
		}
		raw, err := read(ctx, installation.Path)
		if err != nil {
			s.logf("skipping %s: %v", installation.Name, err)
			continue
		}
		doc, err := manifest.Parse(raw)
		if err != nil {
			s.logf("skipping %s: %v", installation.Name, err)
			continue
		}
		if doc.Metadata.Name != "" && doc.Metadata.Name != installation.Name {
			s.logf("warning: %s reports manifest name %q; using the registry name",
				installation.Name, doc.Metadata.Name)
		}
		descriptors = append(descriptors, descriptorsFor(installation.Name, doc)...)
	}
	return descriptors, nil
}

// descriptorsFor projects one tool's manifest into MCP tool descriptors.
//
// The operation name, description, and input schema are copied straight from
// the manifest, with no MCP-specific editing. That identity is the point: the
// CLI derives its verb and flags from the same fields, so if MCP reformatted
// them the two surfaces could drift (spec §29).
func descriptorsFor(toolName string, doc *manifest.Document) []ToolDescriptor {
	if toolName == "" {
		toolName = doc.Metadata.Name
	}
	descriptors := make([]ToolDescriptor, 0, len(doc.Tools))
	for _, operation := range doc.Tools {
		descriptors = append(descriptors, ToolDescriptor{
			MCPName:       toolName + "_" + operation.Name,
			ToolName:      toolName,
			OperationName: operation.Name,
			Description:   operation.Description,
			InputSchema:   inputSchemaMap(operation.Input),
		})
	}
	return descriptors
}

// inputSchemaMap converts a manifest input schema into the map form MCP's
// inputSchema field needs. It round-trips through JSON so the advertised schema
// is byte-identical to the one the CLI validates against.
func inputSchemaMap(input manifest.InputSchema) map[string]any {
	encoded, err := json.Marshal(input)
	if err != nil {
		return map[string]any{"type": "object"}
	}
	var schema map[string]any
	if err := json.Unmarshal(encoded, &schema); err != nil {
		return map[string]any{"type": "object"}
	}
	if _, ok := schema["type"]; !ok {
		schema["type"] = "object"
	}
	return schema
}

// OperationInvoker is the narrow IPC seam the daemon invoker depends on. It
// matches the tool runtime's own invoker, so anything that can serve a tool
// binary can serve MCP too.
type OperationInvoker interface {
	Invoke(ctx context.Context, req relay.InvokeRequest) (relay.InvokeResponse, error)
}

// DaemonInvoker executes MCP tool calls through the daemon over the same IPC
// path the CLI uses (spec §27, §41).
//
// It deliberately delegates to the tool runtime's DaemonInvoker rather than
// re-implementing the socket handshake. Duplicating that logic is exactly how a
// second, MCP-only execution path — and with it a second credential route —
// would appear (spec §41). Auth, permissions, and error mapping therefore
// behave identically on both surfaces.
type DaemonInvoker struct {
	// Operations is the IPC invoker. Production wires it to
	// toolruntime.DaemonInvoker.
	Operations OperationInvoker
}

// Invoke implements Invoker. Transport failures become the same NETWORK_ERROR
// the CLI reports, so a missing daemon surfaces as a tool error rather than
// crashing the MCP server (spec §26, §29).
func (d DaemonInvoker) Invoke(ctx context.Context, tool, operation string, input map[string]any) (any, *relay.Error) {
	if d.Operations == nil {
		return nil, relay.NewError(relay.CodeNetworkError,
			"the Relay daemon is not available; start it with 'relay daemon start'")
	}
	if input == nil {
		input = map[string]any{}
	}

	response, err := d.Operations.Invoke(ctx, relay.InvokeRequest{
		Type:      relay.FrameInvoke,
		Tool:      tool,
		Operation: operation,
		Input:     input,
	})
	if err != nil {
		var structured *relay.Error
		if errors.As(err, &structured) {
			return nil, structured
		}
		return nil, relay.NewError(relay.CodeNetworkError, err.Error())
	}
	if !response.Success || response.Error != nil {
		if response.Error == nil {
			return nil, relay.NewError(relay.CodeRemoteError, "operation failed")
		}
		return nil, response.Error
	}
	return response.Result, nil
}

// New builds the production MCP server: discovery from the registry plus each
// installed binary's manifest, invocation through the daemon over the tool
// runtime's IPC path (spec §27, §41). The Relay home is passed explicitly so a
// caller can honor --home and tests can point at a temporary directory.
func New(layout paths.Layout, version string, log io.Writer) *Server {
	source := &RegistrySource{
		Registry: registry.New(layout.Registry),
		Read:     ExecManifestReader,
		Skill:    DaemonSkill{Socket: layout.Socket()}.ReadSkill,
		Log:      log,
	}
	return &Server{
		Source:  source,
		Skills:  source,
		Invoker: DaemonInvoker{Operations: toolruntime.DaemonInvoker{}},
		Info:    ServerInfo{Name: relay.RuntimeName, Version: version},
		Log:     log,
	}
}

// Run serves a default-wired MCP server over stdio until in is closed. It is
// the entrypoint cmd/relay calls for `relay mcp`.
func Run(ctx context.Context, layout paths.Layout, version string, in io.Reader, out, log io.Writer) error {
	return New(layout, version, log).Serve(ctx, in, out)
}

// logf writes a diagnostic to the source's log writer, if any.
func (s *RegistrySource) logf(format string, args ...any) {
	if s.Log != nil {
		fmt.Fprintf(s.Log, format+"\n", args...)
	}
}
