package skill

import "testing"

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{name: "heading", content: "# GitHub\n\nUse it well.\n"},
		{name: "nested heading", content: "## Only a subheading\n"},
		{name: "empty", content: "", wantErr: true},
		{name: "whitespace", content: "   \n\t\n", wantErr: true},
		{name: "prose without heading", content: "just some words", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := Validate([]byte(test.content))
			if test.wantErr && err == nil {
				t.Fatal("expected an error")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
