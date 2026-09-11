package manifest

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Validation rules from spec §59: required fields, schema correctness,
// duplicate operation names, invalid protocol, invalid auth.

var (
	toolNamePattern    = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	pathParamPattern   = regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)
	knownProtocols     = map[string]bool{"rest": true, "graphql": true, "grpc": true, "browser": true, "local": true}
	supportedProtocols = map[string]bool{"rest": true}
	knownAuthTypes     = map[string]bool{"api_key": true, "bearer": true, "basic": true, "oauth2": true, "client_credentials": true}
	knownCapabilities  = map[string]bool{"network": true, "keychain": true, "browser": true, "filesystem.read": true, "filesystem.write": true, "shell": true, "notifications": true, "clipboard": true}
	allowedMethods     = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}
)

// ValidationError aggregates every problem found, so one build reports the
// whole manifest rather than failing on the first issue.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("manifest invalid (%d problem(s)):\n  - %s",
		len(e.Problems), strings.Join(e.Problems, "\n  - "))
}

// Validate checks a manifest against the relay/v1 schema. It returns a
// *ValidationError listing all problems, or nil when the manifest is valid.
func (d *Document) Validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if d.APIVersion != APIVersion {
		add("apiVersion: want %q, got %q", APIVersion, d.APIVersion)
	}
	if d.Kind != KindTool {
		add("kind: want %q, got %q", KindTool, d.Kind)
	}
	if d.Metadata.Name == "" {
		add("metadata.name: required")
	} else if !toolNamePattern.MatchString(d.Metadata.Name) {
		add("metadata.name: %q must be lowercase snake_case", d.Metadata.Name)
	}
	if d.Metadata.Version == "" {
		add("metadata.version: required")
	}

	if d.Protocol.Type == "" {
		add("protocol.type: required")
	} else if !knownProtocols[d.Protocol.Type] {
		add("protocol.type: unknown protocol %q", d.Protocol.Type)
	} else if !supportedProtocols[d.Protocol.Type] {
		add("protocol.type: %q is not implemented yet (REST only for the MVP)", d.Protocol.Type)
	}
	if d.Protocol.Type == "rest" && d.Protocol.BaseURL == "" {
		add("protocol.baseUrl: required for rest")
	}

	if d.Auth != nil {
		if d.Auth.Type == "" {
			add("auth.type: required when auth is present")
		} else if !knownAuthTypes[d.Auth.Type] {
			add("auth.type: unknown auth type %q", d.Auth.Type)
		}
	}

	for _, capability := range d.Capabilities {
		if !knownCapabilities[capability] {
			add("capabilities: unknown capability %q", capability)
		}
	}

	if len(d.Tools) == 0 {
		add("tools: at least one operation is required")
	}

	seen := map[string]bool{}
	for i, tool := range d.Tools {
		where := fmt.Sprintf("tools[%d]", i)
		if tool.Name == "" {
			add("%s.name: required", where)
			continue
		}
		where = fmt.Sprintf("tools[%d] (%s)", i, tool.Name)

		if !toolNamePattern.MatchString(tool.Name) {
			add("%s.name: %q must be lowercase snake_case", where, tool.Name)
		}
		if seen[tool.Name] {
			add("%s.name: duplicate operation name %q", where, tool.Name)
		}
		seen[tool.Name] = true

		if tool.Description == "" {
			add("%s.description: required", where)
		}
		if tool.Input.Type != "" && tool.Input.Type != "object" {
			add("%s.input.type: want %q, got %q", where, "object", tool.Input.Type)
		}
		for _, required := range tool.Input.Required {
			if _, ok := tool.Input.Properties[required]; !ok {
				add("%s.input.required: %q is not a declared property", where, required)
			}
		}
		validateRequest(where, tool, add)
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return &ValidationError{Problems: problems}
	}
	return nil
}

func validateRequest(where string, tool Tool, add func(string, ...any)) {
	if tool.Request.Method == "" {
		add("%s.request.method: required", where)
	} else if !allowedMethods[strings.ToUpper(tool.Request.Method)] {
		add("%s.request.method: unsupported method %q", where, tool.Request.Method)
	}
	if tool.Request.Path == "" {
		add("%s.request.path: required", where)
		return
	}
	if !strings.HasPrefix(tool.Request.Path, "/") {
		add("%s.request.path: %q must start with /", where, tool.Request.Path)
	}

	required := map[string]bool{}
	for _, name := range tool.Input.Required {
		required[name] = true
	}
	for _, match := range pathParamPattern.FindAllStringSubmatch(tool.Request.Path, -1) {
		param := match[1]
		if _, ok := tool.Input.Properties[param]; !ok {
			add("%s.request.path: {%s} has no matching input property", where, param)
			continue
		}
		if !required[param] {
			add("%s.request.path: {%s} must be listed in input.required", where, param)
		}
	}
}
