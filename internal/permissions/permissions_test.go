package permissions

import (
	"testing"

	"relay/internal/manifest"
	"relay/pkg/relay"
)

// allowCode is the table sentinel for "no error".
const allowCode relay.Code = ""

func TestCheck(t *testing.T) {
	full := Policy{
		Tool:         "github",
		Capabilities: []string{CapabilityNetwork, CapabilityKeychain, CapabilityFilesystemRead, CapabilityFilesystemWrite},
		Network:      NetworkPolicy{Hosts: []string{"api.github.com", "*.githubusercontent.com"}},
		Filesystem:   FilesystemPolicy{Read: []string{"/Users/abhishek/Documents"}, Write: []string{"/Users/abhishek/Documents/out"}},
		Destructive:  []string{"delete_repository"},
	}

	tests := []struct {
		name        string
		policy      Policy
		inv         Invocation
		wantCode    relay.Code
		wantConfirm bool
	}{
		{
			name:     "allow declared network host",
			policy:   full,
			inv:      Invocation{Operation: "get_repository", Network: []string{"api.github.com"}},
			wantCode: allowCode,
		},
		{
			name:     "allow wildcard host match",
			policy:   full,
			inv:      Invocation{Operation: "get_asset", Network: []string{"raw.githubusercontent.com"}},
			wantCode: allowCode,
		},
		{
			name:     "allow wildcard host match at depth",
			policy:   full,
			inv:      Invocation{Operation: "get_asset", Network: []string{"objects.raw.githubusercontent.com"}},
			wantCode: allowCode,
		},
		{
			name:     "host match is case-insensitive and ignores port",
			policy:   full,
			inv:      Invocation{Operation: "get_repository", Network: []string{"API.GitHub.com:443"}},
			wantCode: allowCode,
		},
		{
			name:     "deny by missing capability",
			policy:   Policy{Tool: "local", Capabilities: []string{CapabilityFilesystemRead}},
			inv:      Invocation{Operation: "fetch", Network: []string{"api.github.com"}},
			wantCode: relay.CodePermissionDenied,
		},
		{
			name:     "deny by host not in allowlist",
			policy:   full,
			inv:      Invocation{Operation: "get_repository", Network: []string{"evil.example.com"}},
			wantCode: relay.CodePermissionDenied,
		},
		{
			name:     "bare hostname does not match subdomain",
			policy:   full,
			inv:      Invocation{Operation: "get_repository", Network: []string{"gist.api.github.com"}},
			wantCode: relay.CodePermissionDenied,
		},
		{
			name:     "wildcard does not match apex",
			policy:   full,
			inv:      Invocation{Operation: "get_asset", Network: []string{"githubusercontent.com"}},
			wantCode: relay.CodePermissionDenied,
		},
		{
			name:     "allow read inside declared scope",
			policy:   full,
			inv:      Invocation{Operation: "read_note", Reads: []string{"/Users/abhishek/Documents/a/b.md"}},
			wantCode: allowCode,
		},
		{
			name:     "allow write inside declared scope",
			policy:   full,
			inv:      Invocation{Operation: "write_note", Writes: []string{"/Users/abhishek/Documents/out/report.txt"}},
			wantCode: allowCode,
		},
		{
			name:     "deny read outside declared scope",
			policy:   full,
			inv:      Invocation{Operation: "read_note", Reads: []string{"/Users/abhishek/Secrets/key.pem"}},
			wantCode: relay.CodePermissionDenied,
		},
		{
			name:     "deny write into sibling prefix",
			policy:   full,
			inv:      Invocation{Operation: "write_note", Writes: []string{"/Users/abhishek/Documents/outside/x"}},
			wantCode: relay.CodePermissionDenied,
		},
		{
			name:     "deny read path that escapes via dotdot",
			policy:   full,
			inv:      Invocation{Operation: "read_note", Reads: []string{"/Users/abhishek/Documents/../Secrets/key.pem"}},
			wantCode: relay.CodePermissionDenied,
		},
		{
			name:     "deny missing filesystem read capability",
			policy:   Policy{Tool: "net", Capabilities: []string{CapabilityNetwork}, Network: NetworkPolicy{Hosts: []string{"api.github.com"}}},
			inv:      Invocation{Operation: "read_note", Reads: []string{"/tmp/x"}},
			wantCode: relay.CodePermissionDenied,
		},
		{
			name:        "destructive operation needs confirmation",
			policy:      full,
			inv:         Invocation{Operation: "delete_repository", Network: []string{"api.github.com"}},
			wantCode:    CodeConfirmationRequired,
			wantConfirm: true,
		},
		{
			name:     "destructive denial wins over confirmation",
			policy:   Policy{Capabilities: []string{CapabilityNetwork}, Network: NetworkPolicy{Hosts: []string{"api.github.com"}}, Destructive: []string{"delete_repository"}},
			inv:      Invocation{Operation: "delete_repository", Network: []string{"evil.example.com"}},
			wantCode: relay.CodePermissionDenied,
		},
		{
			name:     "no capabilities allows purely local operation",
			policy:   Policy{Tool: "pure"},
			inv:      Invocation{Operation: "slugify"},
			wantCode: allowCode,
		},
		{
			name:     "no capabilities denies network access",
			policy:   Policy{Tool: "pure"},
			inv:      Invocation{Operation: "fetch", Network: []string{"api.github.com"}},
			wantCode: relay.CodePermissionDenied,
		},
		{
			name:     "declared network with empty allowlist denies all hosts",
			policy:   Policy{Capabilities: []string{CapabilityNetwork}},
			inv:      Invocation{Operation: "fetch", Network: []string{"api.github.com"}},
			wantCode: relay.CodePermissionDenied,
		},
		{
			name:     "undeclared auxiliary capability is denied",
			policy:   full,
			inv:      Invocation{Operation: "notify", Uses: []string{CapabilityNotifications}},
			wantCode: relay.CodePermissionDenied,
		},
		{
			name:     "declared auxiliary capability is allowed",
			policy:   full,
			inv:      Invocation{Operation: "store", Uses: []string{CapabilityKeychain}},
			wantCode: allowCode,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := Classify(test.policy, test.inv)

			var gotCode relay.Code
			if decision.Err != nil {
				gotCode = decision.Err.Code
			}
			if gotCode != test.wantCode {
				t.Fatalf("Classify(%+v) code = %q, want %q (err: %v)", test.inv, gotCode, test.wantCode, decision.Err)
			}
			if decision.Confirmation != test.wantConfirm {
				t.Fatalf("Classify(%+v) confirmation = %v, want %v", test.inv, decision.Confirmation, test.wantConfirm)
			}
			if test.wantCode == allowCode {
				if !decision.Allowed || decision.Err != nil {
					t.Fatalf("Classify(%+v) = %+v, want allowed with nil error", test.inv, decision)
				}
			} else if decision.Allowed {
				t.Fatalf("Classify(%+v) reported allowed despite code %q", test.inv, gotCode)
			}

			// Check must agree with Classify; they are the same decision.
			if got := Check(test.policy, test.inv); (got == nil) != (decision.Err == nil) {
				t.Fatalf("Check(%+v) = %v, disagrees with Classify %v", test.inv, got, decision.Err)
			}
		})
	}
}

func TestCheckDenialDetails(t *testing.T) {
	policy := Policy{Capabilities: []string{CapabilityNetwork}, Network: NetworkPolicy{Hosts: []string{"api.github.com"}}}
	err := Check(policy, Invocation{Operation: "get_repository", Network: []string{"evil.example.com"}})
	if err == nil {
		t.Fatal("want PERMISSION_DENIED, got nil")
	}
	if err.Code != relay.CodePermissionDenied {
		t.Fatalf("code = %q, want %q", err.Code, relay.CodePermissionDenied)
	}
	if err.Details["capability"] != CapabilityNetwork {
		t.Errorf("details capability = %v, want %q", err.Details["capability"], CapabilityNetwork)
	}
	if err.Details["host"] != "evil.example.com" {
		t.Errorf("details host = %v, want evil.example.com", err.Details["host"])
	}
}

func TestPolicyFromManifest(t *testing.T) {
	doc := &manifest.Document{
		Metadata:     manifest.Metadata{Name: "github"},
		Capabilities: []string{CapabilityNetwork, CapabilityKeychain},
		Permissions: &manifest.Permissions{
			Network:    &manifest.NetworkPermission{Hosts: []string{"api.github.com"}},
			Filesystem: &manifest.FilesystemPermission{Read: []string{"/data"}, Write: []string{"/data/out"}},
		},
	}
	policy := PolicyFromManifest(doc, []string{"delete_repository"})

	if policy.Tool != "github" {
		t.Errorf("Tool = %q, want github", policy.Tool)
	}
	if len(policy.Capabilities) != 2 || policy.Capabilities[0] != CapabilityNetwork {
		t.Errorf("Capabilities = %v, want [network keychain]", policy.Capabilities)
	}
	if len(policy.Network.Hosts) != 1 || policy.Network.Hosts[0] != "api.github.com" {
		t.Errorf("Network.Hosts = %v", policy.Network.Hosts)
	}
	if len(policy.Filesystem.Read) != 1 || policy.Filesystem.Read[0] != "/data" {
		t.Errorf("Filesystem.Read = %v", policy.Filesystem.Read)
	}
	if len(policy.Filesystem.Write) != 1 || policy.Filesystem.Write[0] != "/data/out" {
		t.Errorf("Filesystem.Write = %v", policy.Filesystem.Write)
	}
	if !policy.isDestructive("delete_repository") {
		t.Error("expected delete_repository to be destructive")
	}

	// A manifest with no permissions block must not panic and must deny network.
	bare := PolicyFromManifest(&manifest.Document{Metadata: manifest.Metadata{Name: "bare"}}, nil)
	if err := Check(bare, Invocation{Operation: "fetch", Network: []string{"api.github.com"}}); err == nil {
		t.Fatal("bare manifest should deny network, got nil")
	}
	// Nil document is tolerated too.
	if err := Check(PolicyFromManifest(nil, nil), Invocation{Operation: "slugify"}); err != nil {
		t.Fatalf("nil document with a local operation should be allowed, got %v", err)
	}
}
