package manifest

import (
	"strings"
	"testing"

	"relay/pkg/relay"
)

func TestValidateInput(t *testing.T) {
	tool := Tool{
		Name: "get_repository",
		Input: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"owner": {Type: "string"},
				"repo":  {Type: "string"},
				"state": {Type: "string", Enum: []any{"open", "closed"}},
				"limit": {Type: "integer"},
				"draft": {Type: "boolean"},
				"tags":  {Type: "array"},
				"note":  {},
			},
			Required: []string{"owner", "repo"},
		},
	}

	tests := []struct {
		name      string
		input     map[string]any
		wantCode  relay.Code
		wantMatch string
	}{
		{
			name:  "valid",
			input: map[string]any{"owner": "openai", "repo": "relay", "state": "open", "limit": float64(10), "draft": true},
		},
		{
			name:      "missing required",
			input:     map[string]any{"owner": "openai"},
			wantCode:  relay.CodeInvalidInput,
			wantMatch: "missing required input",
		},
		{
			name:      "null counts as absent",
			input:     map[string]any{"owner": "openai", "repo": nil},
			wantCode:  relay.CodeInvalidInput,
			wantMatch: "missing required input",
		},
		{
			name:      "unknown input",
			input:     map[string]any{"owner": "openai", "repo": "relay", "extra": "x"},
			wantCode:  relay.CodeInvalidInput,
			wantMatch: "unknown input",
		},
		{
			name:      "wrong type",
			input:     map[string]any{"owner": 1, "repo": "relay"},
			wantCode:  relay.CodeInvalidInput,
			wantMatch: "must be a string",
		},
		{
			name:      "fractional integer",
			input:     map[string]any{"owner": "openai", "repo": "relay", "limit": 1.5},
			wantCode:  relay.CodeInvalidInput,
			wantMatch: "must be an integer",
		},
		{
			name:  "integer accepts a whole float",
			input: map[string]any{"owner": "openai", "repo": "relay", "limit": 10.0},
		},
		{
			name:      "bad enum",
			input:     map[string]any{"owner": "openai", "repo": "relay", "state": "merged"},
			wantCode:  relay.CodeInvalidInput,
			wantMatch: "must be one of: open, closed",
		},
		{
			name:      "array accepts an array only",
			input:     map[string]any{"owner": "openai", "repo": "relay", "tags": "x"},
			wantCode:  relay.CodeInvalidInput,
			wantMatch: "must be an array",
		},
		{
			name:  "untyped property accepts anything",
			input: map[string]any{"owner": "openai", "repo": "relay", "note": 42},
		},
		{
			name:  "explicit null on an optional property is ignored",
			input: map[string]any{"owner": "openai", "repo": "relay", "state": nil},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := tool.ValidateInput(test.input)
			if test.wantCode == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected %s, got nil", test.wantCode)
			}
			if err.Code != test.wantCode {
				t.Fatalf("code = %s, want %s", err.Code, test.wantCode)
			}
			if !strings.Contains(err.Message, test.wantMatch) {
				t.Fatalf("message = %q, want it to contain %q", err.Message, test.wantMatch)
			}
		})
	}
}
