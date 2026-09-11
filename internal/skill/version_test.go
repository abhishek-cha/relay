package skill

import "testing"

func TestVersion(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
		ok      bool
		wantErr bool
	}{
		{name: "no frontmatter", content: "# GitHub\n\nUse it well.\n"},
		{name: "frontmatter without version", content: "---\nname: github\n---\n\n# GitHub\n"},
		{name: "declared version", content: "---\nversion: 1.4.0\n---\n\n# GitHub\n", want: "1.4.0", ok: true},
		{name: "quoted version", content: "---\nversion: \"1.4.0\"\n---\n\n# GitHub\n", want: "1.4.0", ok: true},
		{name: "empty version is absent", content: "---\nversion: \"\"\n---\n\n# GitHub\n"},
		{name: "unterminated block is not frontmatter", content: "---\nversion: 1.4.0\n\n# GitHub\n"},
		{name: "malformed frontmatter", content: "---\nversion: [1.4.0\n---\n\n# GitHub\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := Version([]byte(tt.content))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want || ok != tt.ok {
				t.Fatalf("Version() = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}
