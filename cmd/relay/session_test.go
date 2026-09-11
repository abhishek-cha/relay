package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"relay/pkg/relay"
)

// TestSessionVerbUsageErrors pins the session verbs' argument handling: a
// missing subcommand or tool is a usage error (exit 2) rather than a failed
// daemon call (spec §10, §23).
func TestSessionVerbUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		run  func() int
	}{
		{name: "no subcommand", run: func() int { return runSession(nil) }},
		{name: "unknown subcommand", run: func() int { return runSession([]string{"frobnicate", "demo"}) }},
		{name: "login with no tool", run: func() int { return runSessionLogin(nil) }},
		{name: "clear with no tool", run: func() int { return runSessionClear(nil) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var code int
			captureStderr(t, func() { code = test.run() })
			if code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
		})
	}
}

// TestReportPagination pins the walk diagnostic (spec §10, §20): an ordinary
// invocation writes nothing, so stdout stays the machine result and stderr
// stays clean, and a truncated walk is named as incomplete rather than passed
// off as the whole collection.
func TestReportPagination(t *testing.T) {
	tests := []struct {
		name      string
		response  relay.InvokeResponse
		wantEmpty bool
		wantText  string
	}{
		{name: "single-page invocation is silent", response: relay.InvokeResponse{Pages: 0}, wantEmpty: true},
		{name: "one page is reported", response: relay.InvokeResponse{Pages: 1}, wantText: "collected 1 page"},
		{name: "many pages are reported", response: relay.InvokeResponse{Pages: 3}, wantText: "collected 3 pages"},
		{name: "a truncated walk warns", response: relay.InvokeResponse{Pages: 3, Truncated: true}, wantText: "truncated"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := captureStderr(t, func() { reportPagination(test.response) })
			if test.wantEmpty && got != "" {
				t.Fatalf("stderr = %q, want empty", got)
			}
			if test.wantText != "" && !strings.Contains(got, test.wantText) {
				t.Fatalf("stderr = %q, want it to contain %q", got, test.wantText)
			}
		})
	}
}

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = writer
	defer func() { os.Stderr = original }()
	fn()
	if err := writer.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(data)
}
