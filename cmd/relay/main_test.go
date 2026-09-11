package main

import (
	"strings"
	"testing"

	"relay/internal/permissions"
	"relay/pkg/relay"
)

// TestConfirmationRequired covers spec §25: only the daemon's distinct gate is
// recognized as confirmable; a plain permission denial is not.
func TestConfirmationRequired(t *testing.T) {
	tests := []struct {
		name       string
		structured *relay.Error
		want       bool
	}{
		{name: "nil error is not a gate", structured: nil, want: false},
		{name: "plain denial is not a gate", structured: relay.NewError(relay.CodePermissionDenied, "denied"), want: false},
		{name: "confirmation gate is confirmable", structured: relay.NewError(permissions.CodeConfirmationRequired, "confirm"), want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := confirmationRequired(test.structured); got != test.want {
				t.Fatalf("confirmationRequired(%v) = %v, want %v", test.structured, got, test.want)
			}
		})
	}
}

// TestConfirmedByHuman covers the interactive answer parsing (spec §25): only an
// explicit y/yes approves, and anything else — including end of input — refuses.
func TestConfirmedByHuman(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "y approves", input: "y\n", want: true},
		{name: "yes approves", input: "yes\n", want: true},
		{name: "case is ignored", input: "YES\n", want: true},
		{name: "y at end of input approves", input: "y", want: true},
		{name: "blank refuses", input: "\n", want: false},
		{name: "n refuses", input: "n\n", want: false},
		{name: "other text refuses", input: "sure\n", want: false},
		{name: "end of input refuses", input: "", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := confirmedByHuman(strings.NewReader(test.input))
			if err != nil {
				t.Fatalf("confirmedByHuman(%q): %v", test.input, err)
			}
			if got != test.want {
				t.Fatalf("confirmedByHuman(%q) = %v, want %v", test.input, got, test.want)
			}
		})
	}
}
