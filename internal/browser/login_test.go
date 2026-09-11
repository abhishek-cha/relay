package browser

import (
	"testing"

	"relay/pkg/relay"
)

// TestLoginSpecFromManifest is table-driven over the manifest login block: a
// valid form or header declaration is accepted, an absent block is simply a
// tool with no session, and every incomplete or malformed block fails loudly
// rather than silently logging in with the wrong shape (spec §3, §23, §26).
func TestLoginSpecFromManifest(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		wantOK   bool
		wantCode relay.Code
		check    func(t *testing.T, spec LoginSpec)
	}{
		{
			name: "form login",
			manifest: `
protocol:
  type: browser
  baseUrl: https://api.example.com
auth:
  type: basic
  login:
    kind: form
    path: /session
    usernameField: user
    passwordField: pass
`,
			wantOK: true,
			check: func(t *testing.T, spec LoginSpec) {
				if spec.Kind != LoginForm || spec.Method != "POST" || spec.Path != "/session" {
					t.Errorf("spec = %+v, want form POST /session", spec)
				}
				if spec.BaseURL != "https://api.example.com" || spec.UsernameField != "user" || spec.PasswordField != "pass" {
					t.Errorf("spec = %+v, want base URL and declared field names", spec)
				}
			},
		},
		{
			name: "header login falls back to endpoint",
			manifest: `
protocol:
  type: browser
  endpoint: https://api.example.com
auth:
  type: bearer
  login:
    kind: header
    method: POST
    path: /token
    header: X-Session
    scheme: Token
`,
			wantOK: true,
			check: func(t *testing.T, spec LoginSpec) {
				if spec.Kind != LoginHeader || spec.BaseURL != "https://api.example.com" || spec.Header != "X-Session" || spec.Scheme != "Token" {
					t.Errorf("spec = %+v, want header login against the endpoint", spec)
				}
			},
		},
		{
			name: "no login block",
			manifest: `
protocol:
  type: browser
  baseUrl: https://api.example.com
auth:
  type: bearer
`,
			wantOK: false,
		},
		{
			name:     "malformed manifest",
			manifest: `auth: [unclosed`,
			wantCode: relay.CodeRuntimeIncompatible,
		},
		{
			name: "unknown login kind",
			manifest: `
protocol:
  baseUrl: https://api.example.com
auth:
  login:
    kind: oauth2
    path: /session
`,
			wantCode: relay.CodeInvalidInput,
		},
		{
			name: "missing path",
			manifest: `
protocol:
  baseUrl: https://api.example.com
auth:
  login:
    kind: form
    usernameField: user
    passwordField: pass
`,
			wantCode: relay.CodeInvalidInput,
		},
		{
			name: "missing form field names",
			manifest: `
protocol:
  baseUrl: https://api.example.com
auth:
  login:
    kind: form
    path: /session
`,
			wantCode: relay.CodeInvalidInput,
		},
		{
			name: "missing base URL",
			manifest: `
protocol:
  type: browser
auth:
  login:
    kind: header
    path: /session
    header: X-Session
`,
			wantCode: relay.CodeInvalidInput,
		},
		{
			name: "missing header name",
			manifest: `
protocol:
  baseUrl: https://api.example.com
auth:
  login:
    kind: header
    path: /session
`,
			wantCode: relay.CodeInvalidInput,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec, ok, failure := LoginSpecFromManifest([]byte(test.manifest))
			if test.wantCode != "" {
				if failure == nil {
					t.Fatalf("failure = nil, want code %s", test.wantCode)
				}
				if failure.Code != test.wantCode {
					t.Fatalf("code = %s, want %s", failure.Code, test.wantCode)
				}
				return
			}
			if failure != nil {
				t.Fatalf("unexpected failure: %v", failure)
			}
			if ok != test.wantOK {
				t.Fatalf("ok = %v, want %v", ok, test.wantOK)
			}
			if test.check != nil {
				test.check(t, spec)
			}
		})
	}
}
