package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"relay/internal/paths"
)

// trickleWriter writes one byte at a time so that any failure to serialize
// concurrent appends shows up as torn lines rather than a lucky atomic write.
type trickleWriter struct{ buf *bytes.Buffer }

func (w trickleWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		w.buf.WriteByte(b)
	}
	return len(p), nil
}

// errorWriter fails on every write, standing in for a full disk or a broken
// sink so tests can prove Record stays best-effort.
type errorWriter struct {
	n   int
	err error
}

func (w errorWriter) Write(p []byte) (int, error) { return w.n, w.err }

// tornWriter writes each record in two chunks with a pause in between so a
// concurrent reader reliably observes a file that ends mid-line. A real append
// is usually a single write, but a reader has no guarantee of that; this models
// the worst case the read path must tolerate.
type tornWriter struct{ f *os.File }

func (w tornWriter) Write(p []byte) (int, error) {
	half := len(p) / 2
	n, err := w.f.Write(p[:half])
	if err != nil {
		return n, err
	}
	time.Sleep(200 * time.Microsecond)
	m, err := w.f.Write(p[half:])
	return n + m, err
}

func TestEventRoundTrip(t *testing.T) {
	ts := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		in   Event
	}{
		{"success", Event{Tool: "github", Operation: "get_repository", Timestamp: ts, DurationMs: 12, Success: true}},
		{"failure-with-code", Event{Tool: "github", Operation: "list_pull_requests", Timestamp: ts, DurationMs: 340, Success: false, ErrorCode: "RATE_LIMITED"}},
		{"zero-duration", Event{Tool: "slack", Operation: "post_message", Timestamp: ts, Success: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			r := NewWriter(&buf)
			if err := r.Record(tt.in); err != nil {
				t.Fatalf("Record: %v", err)
			}
			got, err := ReadEvents(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatalf("ReadEvents: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d events, want 1", len(got))
			}
			if !got[0].Timestamp.Equal(tt.in.Timestamp) {
				t.Errorf("timestamp = %v, want %v", got[0].Timestamp, tt.in.Timestamp)
			}
			got[0].Timestamp = tt.in.Timestamp // normalize representation for comparison
			if !reflect.DeepEqual(got[0], tt.in) {
				t.Errorf("round trip = %+v, want %+v", got[0], tt.in)
			}
		})
	}
}

func TestRecordFillsTimestamp(t *testing.T) {
	var buf bytes.Buffer
	r := NewWriter(&buf)
	if err := r.Record(Event{Tool: "t", Operation: "o", Success: true}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	got, err := ReadEvents(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Timestamp.IsZero() {
		t.Fatal("Record did not fill the zero timestamp")
	}
}

func TestConcurrentAppendsStayLineValid(t *testing.T) {
	var buf bytes.Buffer
	r := NewWriter(trickleWriter{&buf})
	const goroutines, perGoroutine = 16, 25
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				_ = r.Record(Event{Tool: "tool", Operation: "op", Success: true, DurationMs: 1})
			}
		}()
	}
	wg.Wait()
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != goroutines*perGoroutine {
		t.Fatalf("got %d lines, want %d", len(lines), goroutines*perGoroutine)
	}
	for i, line := range lines {
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d is not valid JSON: %v (%q)", i, err, line)
		}
		if e.Tool != "tool" || e.Operation != "op" {
			t.Fatalf("line %d corrupted: %+v", i, e)
		}
	}
}

// TestReadEventsToleratesConcurrentAppend is the regression for the flaky
// "relay stats" read: a reader that overlaps the writer's append must never
// hard-fail. The writer here tears every line in half on purpose, so the reader
// is guaranteed to observe a file ending mid-line; before ReadEvents skipped a
// trailing fragment this failed with a JSON error on nearly every iteration.
func TestReadEventsToleratesConcurrentAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	r := NewWriter(tornWriter{f})
	var writer sync.WaitGroup
	var done atomic.Bool
	writer.Add(1)
	go func() {
		defer writer.Done()
		defer done.Store(true)
		for i := 0; i < 300; i++ {
			if err := r.Record(Event{Tool: "demo", Operation: "get_repo", Timestamp: time.Unix(int64(i), 0).UTC(), DurationMs: 1, Success: true}); err != nil {
				t.Errorf("Record: %v", err)
				return
			}
		}
	}()

	reads := 0
	var readErr error
	var corrupt *Event
	for !done.Load() {
		rf, err := os.Open(path)
		if err != nil {
			readErr = err
			break
		}
		events, err := ReadEvents(rf)
		rf.Close()
		if err != nil {
			readErr = err
			break
		}
		for i := range events {
			if events[i].Tool != "demo" || events[i].Operation != "get_repo" {
				corrupt = &events[i]
				break
			}
		}
		if corrupt != nil {
			break
		}
		reads++
	}
	writer.Wait()
	if readErr != nil {
		t.Fatalf("ReadEvents failed against a concurrent append: %v", readErr)
	}
	if corrupt != nil {
		t.Fatalf("corrupt event decoded: %+v", *corrupt)
	}
	if reads == 0 {
		t.Fatal("reader never observed the file; the test did not exercise the race")
	}
}

func TestFailingWriterDoesNotFailCaller(t *testing.T) {
	sentinel := errors.New("disk full")
	tests := []struct {
		name string
		w    errorWriter
	}{
		{"hard-error", errorWriter{n: 0, err: sentinel}},
		{"partial-error", errorWriter{n: 1, err: sentinel}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewWriter(tt.w)
			defer func() {
				if rec := recover(); rec != nil {
					t.Fatalf("telemetry panicked instead of degrading: %v", rec)
				}
			}()
			// The invocation ignores telemetry errors; this is the contract.
			_ = r.Record(Event{Tool: "t", Operation: "o"})
			// The error is still observable for diagnostics.
			if err := r.Record(Event{Tool: "t", Operation: "o"}); !errors.Is(err, sentinel) {
				t.Fatalf("Record error = %v, want %v", err, sentinel)
			}
		})
	}
}

func TestEventStructurallyHoldsNoPayload(t *testing.T) {
	allowed := map[string]bool{
		"Tool": true, "Operation": true, "Timestamp": true,
		"DurationMs": true, "Success": true, "ErrorCode": true,
	}
	typ := reflect.TypeOf(Event{})
	if typ.NumField() != len(allowed) {
		t.Fatalf("Event has %d fields, want %d", typ.NumField(), len(allowed))
	}
	for i := 0; i < typ.NumField(); i++ {
		if name := typ.Field(i).Name; !allowed[name] {
			t.Errorf("unexpected field %s: Event must not carry payloads", name)
		}
	}
	raw, err := json.Marshal(Event{Tool: "t", Operation: "o", Timestamp: time.Now(), DurationMs: 1, Success: true, ErrorCode: "E"})
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	forbidden := []string{"input", "body", "request", "response", "result", "header", "token", "secret", "arg", "payload"}
	for key := range obj {
		lower := strings.ToLower(key)
		for _, f := range forbidden {
			if strings.Contains(lower, f) {
				t.Errorf("marshalled event exposes forbidden key %q", key)
			}
		}
	}
}

func TestRedactionDefenceInDepth(t *testing.T) {
	const secret = "ghp_abcdefghijklmnopqrstuvwxyz"
	var buf bytes.Buffer
	r := NewWriter(&buf, secret)
	if err := r.Record(Event{Tool: "github", Operation: "get?token=" + secret, Success: false, ErrorCode: "auth=" + secret}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("secret leaked into telemetry: %s", buf.String())
	}
	got, err := ReadEvents(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got[0].Operation, "[REDACTED]") || strings.Contains(got[0].Operation, secret) {
		t.Fatalf("operation not redacted: %q", got[0].Operation)
	}
	if strings.Contains(got[0].ErrorCode, secret) {
		t.Fatalf("error code not redacted: %q", got[0].ErrorCode)
	}
}

func TestRotationBoundsGrowth(t *testing.T) {
	layout := paths.Layout{Root: t.TempDir()}
	r := New(layout)
	r.maxBytes = 150
	defer r.Close()
	for i := 0; i < 20; i++ {
		if err := r.Record(Event{Tool: "t", Operation: "op", Timestamp: time.Unix(int64(i), 0).UTC(), DurationMs: 5, Success: true}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	live, backup := File(layout), Backup(layout)
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("expected a backup file after rotation: %v", err)
	}
	info, err := os.Stat(live)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > int64(r.maxBytes) {
		t.Errorf("live file is %d bytes, exceeds cap %d", info.Size(), r.maxBytes)
	}
	for _, path := range []string{live, backup} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		body := strings.TrimRight(string(data), "\n")
		if body == "" {
			continue
		}
		for i, line := range strings.Split(body, "\n") {
			var e Event
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				t.Fatalf("%s line %d invalid: %v", filepath.Base(path), i, err)
			}
		}
	}
}

func TestReadEvents(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    int
		wantErr bool
	}{
		{"empty", "", 0, false},
		{"blank-lines-skipped", "\n\n", 0, false},
		{"two-events", `{"tool":"a","operation":"o","success":true}` + "\n" + `{"tool":"b","operation":"p","success":false}` + "\n", 2, false},
		{"malformed", "{not json}\n", 0, true},
		// A trailing fragment with no newline is a write in flight and must be
		// skipped, leaving the complete events that precede it.
		{"torn-trailing-line", `{"tool":"a","operation":"o","success":true}` + "\n" + `{"tool":"b","operat`, 1, false},
		{"blank-then-torn-trailing", "\n" + `{"tool":"b","operat`, 0, false},
		// Damage that is no longer the tail is still surfaced: the malformed
		// line is newline-terminated, so it cannot be an in-flight append.
		{"malformed-mid-file", `{"tool":"a","operation":"o","success":true}` + "\n" + "{not json}\n" + `{"tool":"c","operation":"q","success":true}` + "\n", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReadEvents(strings.NewReader(tt.in))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadEvents: %v", err)
			}
			if len(got) != tt.want {
				t.Fatalf("got %d events, want %d", len(got), tt.want)
			}
		})
	}
}
