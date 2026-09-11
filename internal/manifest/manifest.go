// Package manifest defines Relay's machine-readable tool description: the
// manifest a Relay tool embeds and exposes through `--describe`.
//
// The manifest is machine truth. It answers "what operations exist and what
// inputs do they require?" — never "how should an agent accomplish a workflow".
// That second question belongs to the embedded SKILL.md (spec §7).
package manifest

import (
	"fmt"
	"os"

	"relay/pkg/relay"

	"gopkg.in/yaml.v3"
)

// Manifest schema constants.
const (
	// APIVersion is the manifest schema version this build understands.
	APIVersion = "relay/v1"
	// KindTool is the only supported manifest kind.
	KindTool = "Tool"
)

// Document is a complete Relay tool manifest.
type Document struct {
	APIVersion   string       `yaml:"apiVersion" json:"apiVersion"`
	Kind         string       `yaml:"kind" json:"kind"`
	Metadata     Metadata     `yaml:"metadata" json:"metadata"`
	Runtime      Runtime      `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	Protocol     Protocol     `yaml:"protocol" json:"protocol"`
	Auth         *Auth        `yaml:"auth,omitempty" json:"auth,omitempty"`
	Capabilities []string     `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	Permissions  *Permissions `yaml:"permissions,omitempty" json:"permissions,omitempty"`
	Tools        []Tool       `yaml:"tools" json:"tools"`
}

// Metadata identifies the tool.
type Metadata struct {
	Name        string `yaml:"name" json:"name"`
	Version     string `yaml:"version" json:"version"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

// Runtime is the compatibility declaration from spec §35.
type Runtime struct {
	Name       string `yaml:"name,omitempty" json:"name,omitempty"`
	APIVersion string `yaml:"apiVersion,omitempty" json:"apiVersion,omitempty"`
}

// Protocol selects the executor.
type Protocol struct {
	Type     string `yaml:"type" json:"type"`
	BaseURL  string `yaml:"baseUrl,omitempty" json:"baseUrl,omitempty"`
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
}

// Auth describes credential requirements. Credentials themselves are owned by
// the daemon and live in the Keychain — never here (spec §21, §22).
type Auth struct {
	Type     string `yaml:"type" json:"type"`
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`
}

// Permissions is the declared policy surface (spec §25). Enforcement lands in a
// later milestone; the field exists so manifests stay forward-compatible.
type Permissions struct {
	Network    *NetworkPermission    `yaml:"network,omitempty" json:"network,omitempty"`
	Filesystem *FilesystemPermission `yaml:"filesystem,omitempty" json:"filesystem,omitempty"`
}

// NetworkPermission bounds outbound hosts.
type NetworkPermission struct {
	Hosts []string `yaml:"hosts,omitempty" json:"hosts,omitempty"`
}

// FilesystemPermission bounds filesystem access.
type FilesystemPermission struct {
	Read  []string `yaml:"read,omitempty" json:"read,omitempty"`
	Write []string `yaml:"write,omitempty" json:"write,omitempty"`
}

// Tool is one declared operation.
type Tool struct {
	Name        string      `yaml:"name" json:"name"`
	Description string      `yaml:"description" json:"description"`
	Input       InputSchema `yaml:"input" json:"input"`
	Request     Request     `yaml:"request" json:"request"`
}

// InputSchema is a JSON-Schema-compatible subset describing an operation's
// arguments. The generated CLI derives its flags from Properties.
type InputSchema struct {
	Type       string              `yaml:"type" json:"type"`
	Properties map[string]Property `yaml:"properties,omitempty" json:"properties,omitempty"`
	Required   []string            `yaml:"required,omitempty" json:"required,omitempty"`
}

// Property is a single input field.
type Property struct {
	Type        string    `yaml:"type" json:"type"`
	Description string    `yaml:"description,omitempty" json:"description,omitempty"`
	Format      string    `yaml:"format,omitempty" json:"format,omitempty"`
	Default     any       `yaml:"default,omitempty" json:"default,omitempty"`
	Enum        []any     `yaml:"enum,omitempty" json:"enum,omitempty"`
	Items       *Property `yaml:"items,omitempty" json:"items,omitempty"`
}

// Request describes how to reach the operation. The MVP models REST; GraphQL
// and gRPC extend this in a later milestone (spec §20, §44, §45).
type Request struct {
	Method  string            `yaml:"method,omitempty" json:"method,omitempty"`
	Path    string            `yaml:"path,omitempty" json:"path,omitempty"`
	Query   map[string]string `yaml:"query,omitempty" json:"query,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	Body    any               `yaml:"body,omitempty" json:"body,omitempty"`
}

// Parse decodes a YAML manifest. It does not validate — call Validate.
func Parse(data []byte) (*Document, error) {
	var doc Document
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &doc, nil
}

// Load reads and parses a manifest from disk.
func Load(path string) (*Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", path, err)
	}
	return Parse(data)
}

// Descriptor projects the manifest into the public `--describe` contract
// (spec §9). hasSkill reports whether a SKILL.md is embedded alongside it.
func (d *Document) Descriptor(hasSkill bool) relay.Descriptor {
	runtimeName := d.Runtime.Name
	if runtimeName == "" {
		runtimeName = "relay"
	}
	runtimeVersion := d.Runtime.APIVersion
	if runtimeVersion == "" {
		runtimeVersion = "v1"
	}

	descriptor := relay.Descriptor{
		APIVersion:   d.APIVersion,
		Kind:         d.Kind,
		Name:         d.Metadata.Name,
		Version:      d.Metadata.Version,
		Description:  d.Metadata.Description,
		Protocol:     d.Protocol.Type,
		Runtime:      relay.RuntimeInfo{Name: runtimeName, APIVersion: runtimeVersion},
		Skill:        hasSkill,
		Capabilities: d.Capabilities,
		Tools:        make([]relay.ToolSummary, 0, len(d.Tools)),
	}
	for _, tool := range d.Tools {
		descriptor.Tools = append(descriptor.Tools, relay.ToolSummary{
			Name:        tool.Name,
			Description: tool.Description,
		})
	}
	return descriptor
}

// Operation finds a declared operation by its canonical name. It returns nil
// when the manifest does not declare it.
func (d *Document) Operation(name string) *Tool {
	for i := range d.Tools {
		if d.Tools[i].Name == name {
			return &d.Tools[i]
		}
	}
	return nil
}

// OperationNames returns the canonical names of every declared operation.
func (d *Document) OperationNames() []string {
	names := make([]string, 0, len(d.Tools))
	for _, tool := range d.Tools {
		names = append(names, tool.Name)
	}
	return names
}
