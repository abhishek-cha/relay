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
	// The four fields below declare an OAuth2 device authorization grant
	// (RFC 8628). They are additive: an oauth2 block that omits them keeps
	// meaning "a human pastes a token" (spec §21, §54).
	DeviceAuthorizationEndpoint string   `yaml:"deviceAuthorizationEndpoint,omitempty" json:"deviceAuthorizationEndpoint,omitempty"`
	TokenEndpoint               string   `yaml:"tokenEndpoint,omitempty" json:"tokenEndpoint,omitempty"`
	ClientID                    string   `yaml:"clientId,omitempty" json:"clientId,omitempty"`
	Scopes                      []string `yaml:"scopes,omitempty" json:"scopes,omitempty"`
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

// Request describes how to reach the operation. REST uses Method, Path, Query,
// Headers, and Body; GraphQL uses Document and Variables; a local capability
// uses Operation (spec §19, §20, §44, §46). The shapes coexist: a manifest sets
// only the fields its protocol reads, and the validator rejects the others.
type Request struct {
	// Method is the HTTP verb for a REST request. A gRPC operation has no HTTP
	// verb, so the literal gRPC form described below reads it as the RPC method
	// name instead (spec §45).
	Method string `yaml:"method,omitempty" json:"method,omitempty"`
	Path   string `yaml:"path,omitempty" json:"path,omitempty"`

	// Package and Service name a gRPC method literally, the way the protocol
	// block names a service (spec §45). Together with Method they address
	// /package.Service/Method without smuggling the whole path into request.path.
	// They are meaningful only for protocol.type grpc: a gRPC operation declares
	// either this triple or request.path, never both.
	Package string            `yaml:"package,omitempty" json:"package,omitempty"`
	Service string            `yaml:"service,omitempty" json:"service,omitempty"`
	Query   map[string]string `yaml:"query,omitempty" json:"query,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	Body    any               `yaml:"body,omitempty" json:"body,omitempty"`

	// Pagination declares how a REST collection operation walks its pages
	// (spec §20). It is optional and REST-only: it is meaningful solely for a
	// GET/HEAD that returns a page of a larger result set. Leaving it unset
	// keeps the operation a single request, exactly as before it existed.
	Pagination *Pagination `yaml:"pagination,omitempty" json:"pagination,omitempty"`

	// Document is the GraphQL operation text (a query or mutation). It is read
	// only when protocol.type is graphql; a REST request leaves it empty
	// (spec §44).
	Document string `yaml:"document,omitempty" json:"document,omitempty"`

	// Variables maps a GraphQL variable name to the operation input property
	// that supplies it. An empty value means the variable resolves from an input
	// property of the same name (spec §44). Variables used by the document that
	// are absent from this map also default to a same-named input property.
	Variables map[string]string `yaml:"variables,omitempty" json:"variables,omitempty"`

	// Operation names the local primitive to run, such as "read_file" or
	// "git_status" (spec §46). It is read only when protocol.type is local; a
	// local request names a primitive instead of an address, so it sets none of
	// Method, Path, Query, Headers, Body, Document, or Variables.
	Operation string `yaml:"operation,omitempty" json:"operation,omitempty"`
}

// Pagination styles understood by relay/v1 (spec §20).
const (
	// PaginationStyleLinkHeader follows the RFC 8288 `Link rel="next"`
	// response header (GitHub, Stripe, most REST APIs).
	PaginationStyleLinkHeader = "link-header"
	// PaginationStyleCursor echoes a cursor read from the JSON response body
	// back into a named request parameter.
	PaginationStyleCursor = "cursor"
)

// Cursor locations a cursor-style strategy may name (spec §20).
const (
	// CursorInQuery carries the cursor as a query parameter.
	CursorInQuery = "query"
	// CursorInBody carries the cursor inside the JSON request body.
	CursorInBody = "body"
)

// Pagination declares how one REST operation collects a multi-page result
// (spec §20). It is a property of the operation's transport, so it lives on the
// request block next to method and path. The manifest is machine truth: an
// executor follows the declared strategy, and nothing here teaches an agent how
// to page (that is SKILL.md's job, spec §7).
//
// Two styles exist. A link-header strategy reads the next page's URL from the
// RFC 8288 response header and declares no cursor fields. A cursor strategy
// reads a value out of the JSON response body (CursorField) and sends it back
// as a request parameter (CursorParam, in CursorIn).
type Pagination struct {
	// Style is "link-header" or "cursor".
	Style string `yaml:"style" json:"style"`

	// CursorParam names the request parameter carrying the cursor
	// (cursor style only).
	CursorParam string `yaml:"cursorParam,omitempty" json:"cursorParam,omitempty"`

	// CursorIn is "query" or "body" and selects where CursorParam is placed
	// (cursor style only; an empty value defaults to "query").
	CursorIn string `yaml:"cursorIn,omitempty" json:"cursorIn,omitempty"`

	// CursorField is the dotted path within the JSON response body that holds
	// the next cursor (cursor style only).
	CursorField string `yaml:"cursorField,omitempty" json:"cursorField,omitempty"`

	// HasMoreField is an optional dotted path to a bool or number that can stop
	// the walk early, before the cursor runs out (cursor style only).
	HasMoreField string `yaml:"hasMoreField,omitempty" json:"hasMoreField,omitempty"`

	// LimitParam is an optional page-size request parameter; Limit is the
	// default page size sent with it. Either style may declare them.
	LimitParam string `yaml:"limitParam,omitempty" json:"limitParam,omitempty"`
	Limit      int    `yaml:"limit,omitempty" json:"limit,omitempty"`
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
