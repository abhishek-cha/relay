package manifest

import (
	"strings"
	"testing"
)

const validManifest = `apiVersion: relay/v1
kind: Tool
metadata:
  name: demo
  version: 1.0.0
  description: A demo tool
protocol:
  type: rest
  baseUrl: https://api.example.com
auth:
  type: api_key
capabilities:
  - network
  - keychain
tools:
  - name: get_thing
    description: Get a thing by id
    input:
      type: object
      properties:
        id:
          type: string
      required:
        - id
    request:
      method: GET
      path: /things/{id}
`

func mustParse(t *testing.T, text string) *Document {
	t.Helper()
	doc, err := Parse([]byte(text))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return doc
}

func TestValidateAcceptsValidManifest(t *testing.T) {
	if err := mustParse(t, validManifest).Validate(); err != nil {
		t.Fatalf("expected valid manifest, got: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Document)
		wantErr string
	}{
		{
			name:    "wrong apiVersion",
			mutate:  func(d *Document) { d.APIVersion = "relay/v2" },
			wantErr: "apiVersion",
		},
		{
			name:    "missing name",
			mutate:  func(d *Document) { d.Metadata.Name = "" },
			wantErr: "metadata.name: required",
		},
		{
			name:    "name not snake_case",
			mutate:  func(d *Document) { d.Metadata.Name = "MyTool" },
			wantErr: "snake_case",
		},
		{
			name:    "missing version",
			mutate:  func(d *Document) { d.Metadata.Version = "" },
			wantErr: "metadata.version: required",
		},
		{
			name:    "unknown protocol",
			mutate:  func(d *Document) { d.Protocol.Type = "smoke-signals" },
			wantErr: "unknown protocol",
		},
		{
			name:    "unimplemented protocol",
			mutate:  func(d *Document) { d.Protocol.Type = "graphql" },
			wantErr: "not implemented yet",
		},
		{
			name:    "rest without baseUrl",
			mutate:  func(d *Document) { d.Protocol.BaseURL = "" },
			wantErr: "protocol.baseUrl: required",
		},
		{
			name:    "unknown auth type",
			mutate:  func(d *Document) { d.Auth.Type = "telepathy" },
			wantErr: "unknown auth type",
		},
		{
			name:    "unknown capability",
			mutate:  func(d *Document) { d.Capabilities = []string{"teleportation"} },
			wantErr: "unknown capability",
		},
		{
			name:    "no operations",
			mutate:  func(d *Document) { d.Tools = nil },
			wantErr: "at least one operation",
		},
		{
			name:    "duplicate operation names",
			mutate:  func(d *Document) { d.Tools = append(d.Tools, d.Tools[0]) },
			wantErr: "duplicate operation",
		},
		{
			name:    "missing operation description",
			mutate:  func(d *Document) { d.Tools[0].Description = "" },
			wantErr: "description: required",
		},
		{
			name:    "unsupported method",
			mutate:  func(d *Document) { d.Tools[0].Request.Method = "TRACE" },
			wantErr: "unsupported method",
		},
		{
			name:    "path param without property",
			mutate:  func(d *Document) { d.Tools[0].Request.Path = "/things/{slug}" },
			wantErr: "no matching input property",
		},
		{
			name: "path param not marked required",
			mutate: func(d *Document) {
				d.Tools[0].Input.Required = nil
			},
			wantErr: "must be listed in input.required",
		},
		{
			name: "required property not declared",
			mutate: func(d *Document) {
				d.Tools[0].Input.Required = []string{"id", "ghost"}
			},
			wantErr: "is not a declared property",
		},
		{
			name:    "path without leading slash",
			mutate:  func(d *Document) { d.Tools[0].Request.Path = "things/{id}" },
			wantErr: "must start with /",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doc := mustParse(t, validManifest)
			test.mutate(doc)
			err := doc.Validate()
			if err == nil {
				t.Fatalf("expected an error mentioning %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("expected error to mention %q, got: %v", test.wantErr, err)
			}
		})
	}
}

// TestValidateAggregatesProblems guards the property that one build reports the
// whole manifest rather than stopping at the first problem (spec §59).
func TestValidateAggregatesProblems(t *testing.T) {
	doc := mustParse(t, validManifest)
	doc.APIVersion = "relay/v9"
	doc.Metadata.Version = ""
	doc.Protocol.Type = "carrier-pigeon"

	err := doc.Validate()
	if err == nil {
		t.Fatal("expected an error")
	}
	validationErr, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if len(validationErr.Problems) != 3 {
		t.Fatalf("expected 3 problems, got %d: %v", len(validationErr.Problems), validationErr.Problems)
	}
}

func TestValidateExampleManifests(t *testing.T) {
	examples := []string{
		"../../examples/github/github.yaml",
		"../../examples/slack/slack.yaml",
		"../../examples/stripe/stripe.yaml",
		"../../templates/tool.yaml",
	}
	for _, path := range examples {
		t.Run(path, func(t *testing.T) {
			doc, err := Load(path)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if err := doc.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
		})
	}
}

func TestDescriptorProjection(t *testing.T) {
	doc := mustParse(t, validManifest)
	descriptor := doc.Descriptor(true)

	if descriptor.Name != "demo" || descriptor.Version != "1.0.0" {
		t.Fatalf("unexpected identity: %+v", descriptor)
	}
	if descriptor.Protocol != "rest" || !descriptor.Skill {
		t.Fatalf("unexpected protocol/skill: %+v", descriptor)
	}
	// Runtime defaults keep older manifests loadable (spec §35).
	if descriptor.Runtime.Name != "relay" || descriptor.Runtime.APIVersion != "v1" {
		t.Fatalf("unexpected runtime default: %+v", descriptor.Runtime)
	}
	if len(descriptor.Tools) != 1 || descriptor.Tools[0].Name != "get_thing" {
		t.Fatalf("unexpected tools: %+v", descriptor.Tools)
	}
}

func TestOperationLookup(t *testing.T) {
	doc := mustParse(t, validManifest)
	if doc.Operation("get_thing") == nil {
		t.Fatal("expected to find get_thing")
	}
	if doc.Operation("missing") != nil {
		t.Fatal("expected nil for an undeclared operation")
	}
}
