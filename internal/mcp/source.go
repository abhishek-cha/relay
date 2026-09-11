package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

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
	// Log receives diagnostics about skipped tools. Never stdout.
	Log io.Writer
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
	return &Server{
		Source: &RegistrySource{
			Registry: registry.New(layout.Registry),
			Read:     ExecManifestReader,
			Log:      log,
		},
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
