package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"relay/pkg/relay"
)

// demoManifest is a realistic generated-tool manifest: two operations, including
// a multi-word operation name whose CLI verb must be kebab-cased (spec §11).
const demoManifest = `apiVersion: relay/v1
kind: Tool
metadata:
  name: demo
  version: 1.0.0
  description: A demo tool
protocol:
  type: rest
  baseUrl: https://api.example.com
tools:
  - name: get_thing
    description: Get a thing by id
    input:
      type: object
      properties:
        id:
          type: string
          description: Thing id
        limit:
          type: integer
        ratio:
          type: number
        draft:
          type: boolean
        tags:
          type: array
          items:
            type: string
        sizes:
          type: array
          items:
            type: integer
        weights:
          type: array
          items:
            type: number
        meta:
          type: object
      required:
        - id
    request:
      method: GET
      path: /things/{id}
  - name: search_items
    description: Search items
    input:
      type: object
      properties:
        query:
          type: string
      required:
        - query
    request:
      method: GET
      path: /items
      pagination:
        style: link-header
`

const demoSkill = "# Demo\n\nUse get-thing to fetch a thing.\n"

// fakeInvoker records every request and returns a scripted reply, so the runtime
// can be exercised in-process without a daemon (spec §13).
type fakeInvoker struct {
	calls []relay.InvokeRequest
	reply func(relay.InvokeRequest) (relay.InvokeResponse, error)
}

func (f *fakeInvoker) Invoke(_ context.Context, req relay.InvokeRequest) (relay.InvokeResponse, error) {
	f.calls = append(f.calls, req)
	if f.reply != nil {
		return f.reply(req)
	}
	return relay.InvokeResponse{Success: true, Result: map[string]any{"ok": true}}, nil
}

// okInvoker is the default happy-path invoker.
func okInvoker() *fakeInvoker {
	return &fakeInvoker{}
}

func newApp(t *testing.T, invoker Invoker, skill []byte) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app, err := New(Assets{Manifest: []byte(demoManifest), Skill: skill}, invoker, stdout, stderr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return app, stdout, stderr
}

func newDemoApp(t *testing.T, invoker Invoker) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	return newApp(t, invoker, []byte(demoSkill))
}

type runResult struct {
	code   int
	stdout string
	stderr string
}

func runApp(t *testing.T, app *App, stdout, stderr *bytes.Buffer, args ...string) runResult {
	t.Helper()
	stdout.Reset()
	stderr.Reset()
	code := app.Run(context.Background(), args)
	return runResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// decodeSingleJSON fails unless raw holds exactly one JSON value and nothing
// else. This is what "logs never mix into JSON stdout" means in practice.
func decodeSingleJSON(t *testing.T, raw string, target any) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(raw))
	if err := decoder.Decode(target); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\nstdout:\n%s", err, raw)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("stdout holds more than one JSON value: %v\nstdout:\n%s", err, raw)
	}
}

func singleInvocation(t *testing.T, invoker *fakeInvoker) relay.InvokeRequest {
	t.Helper()
	if len(invoker.calls) != 1 {
		t.Fatalf("expected exactly 1 invocation, got %d", len(invoker.calls))
	}
	return invoker.calls[0]
}

func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestNewRejectsMalformedManifest(t *testing.T) {
	app, err := New(Assets{Manifest: []byte("apiVersion: [oops\n")}, okInvoker(), io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected a manifest parse error")
	}
	if app != nil {
		t.Fatalf("expected a nil app on parse failure, got %+v", app)
	}
}

// New only parses; semantic validation happens at build time (spec §13), so a
// syntactically valid but semantically wrong manifest still loads.
func TestNewSkipsSemanticValidation(t *testing.T) {
	raw := "apiVersion: relay/v9\nkind: Widget\nmetadata:\n  name: Broken\ntools: []\n"
	app, err := New(Assets{Manifest: []byte(raw)}, okInvoker(), io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("New must not validate: %v", err)
	}
	if app.Manifest.Kind != "Widget" || app.Manifest.Metadata.Name != "Broken" {
		t.Fatalf("unexpected manifest: %+v", app.Manifest)
	}
	if string(app.RawManifest) != raw {
		t.Fatalf("RawManifest not preserved verbatim")
	}
}

func TestNewPreservesRawManifestAndSkill(t *testing.T) {
	app, _, _ := newDemoApp(t, okInvoker())
	if app.Manifest.Metadata.Name != "demo" || app.Manifest.Metadata.Version != "1.0.0" {
		t.Fatalf("unexpected identity: %+v", app.Manifest.Metadata)
	}
	if string(app.RawManifest) != demoManifest {
		t.Fatal("RawManifest must be the exact embedded bytes")
	}
	if string(app.Skill) != demoSkill {
		t.Fatal("Skill must be the exact embedded bytes")
	}
}

func TestRunUsageWhenNoOperation(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "no args", args: nil},
		{name: "empty args", args: []string{}},
		{name: "json only", args: []string{"--json"}},
		{name: "repeated json only", args: []string{"--json", "--json"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, stdout, stderr := newDemoApp(t, okInvoker())
			got := runApp(t, app, stdout, stderr, test.args...)
			if got.code != ExitUsage {
				t.Fatalf("exit = %d, want %d", got.code, ExitUsage)
			}
			if got.stdout != "" {
				t.Fatalf("usage must not reach stdout, got %q", got.stdout)
			}
			for _, want := range []string{"demo 1.0.0", "Usage:", "get-thing", "search-items"} {
				if !strings.Contains(got.stderr, want) {
					t.Fatalf("stderr missing %q:\n%s", want, got.stderr)
				}
			}
		})
	}
}

// The usage screen prints the human kebab-case verb, never the canonical name.
func TestRunUsageListsKebabCaseOperations(t *testing.T) {
	app, stdout, stderr := newDemoApp(t, okInvoker())
	got := runApp(t, app, stdout, stderr)
	if strings.Contains(got.stderr, "get_thing") || strings.Contains(got.stderr, "search_items") {
		t.Fatalf("usage leaked canonical snake_case names:\n%s", got.stderr)
	}
}

func TestRunHelpDispatch(t *testing.T) {
	for _, arg := range []string{"--help", "-h", "help"} {
		t.Run(arg, func(t *testing.T) {
			app, stdout, stderr := newDemoApp(t, okInvoker())
			got := runApp(t, app, stdout, stderr, arg)
			if got.code != ExitOK {
				t.Fatalf("exit = %d, want %d", got.code, ExitOK)
			}
			if !strings.Contains(got.stdout, "Usage:") {
				t.Fatalf("help missing from stdout: %q", got.stdout)
			}
			if got.stderr != "" {
				t.Fatalf("help must not write to stderr, got %q", got.stderr)
			}
		})
	}
}

func TestRunVersion(t *testing.T) {
	app, stdout, stderr := newDemoApp(t, okInvoker())
	got := runApp(t, app, stdout, stderr, "--version")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d", got.code, ExitOK)
	}
	if got.stdout != "demo 1.0.0\n" {
		t.Fatalf("stdout = %q, want %q", got.stdout, "demo 1.0.0\n")
	}
	if got.stderr != "" {
		t.Fatalf("stderr = %q, want empty", got.stderr)
	}
}

func TestRunManifestIsVerbatim(t *testing.T) {
	app, stdout, stderr := newDemoApp(t, okInvoker())
	got := runApp(t, app, stdout, stderr, "--manifest")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d", got.code, ExitOK)
	}
	if got.stdout != demoManifest {
		t.Fatalf("--manifest must print the embedded bytes verbatim")
	}
	if got.stderr != "" {
		t.Fatalf("stderr = %q, want empty", got.stderr)
	}
}

func TestRunDescribe(t *testing.T) {
	tests := []struct {
		name      string
		skill     []byte
		wantSkill bool
	}{
		{name: "with skill", skill: []byte(demoSkill), wantSkill: true},
		{name: "without skill", skill: nil, wantSkill: false},
		{name: "whitespace-only skill", skill: []byte("  \n"), wantSkill: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, stdout, stderr := newApp(t, okInvoker(), test.skill)
			got := runApp(t, app, stdout, stderr, "--describe")
			if got.code != ExitOK {
				t.Fatalf("exit = %d, want %d (stderr: %s)", got.code, ExitOK, got.stderr)
			}
			if got.stderr != "" {
				t.Fatalf("stderr = %q, want empty", got.stderr)
			}
			var descriptor relay.Descriptor
			decodeSingleJSON(t, got.stdout, &descriptor)
			if descriptor.APIVersion != "relay/v1" || descriptor.Kind != "Tool" {
				t.Fatalf("unexpected apiVersion/kind: %+v", descriptor)
			}
			if descriptor.Name != "demo" || descriptor.Version != "1.0.0" {
				t.Fatalf("unexpected identity: %+v", descriptor)
			}
			if descriptor.Protocol != "rest" || descriptor.Skill != test.wantSkill {
				t.Fatalf("unexpected protocol/skill: %+v", descriptor)
			}
			// Runtime defaults keep older manifests loadable (spec §35).
			if descriptor.Runtime.Name != "relay" || descriptor.Runtime.APIVersion != "v1" {
				t.Fatalf("unexpected runtime default: %+v", descriptor.Runtime)
			}
			if len(descriptor.Tools) != 2 || descriptor.Tools[0].Name != "get_thing" || descriptor.Tools[1].Name != "search_items" {
				t.Fatalf("unexpected tools: %+v", descriptor.Tools)
			}
		})
	}
}

func TestRunSkill(t *testing.T) {
	t.Run("embedded", func(t *testing.T) {
		app, stdout, stderr := newDemoApp(t, okInvoker())
		got := runApp(t, app, stdout, stderr, "--skill")
		if got.code != ExitOK {
			t.Fatalf("exit = %d, want %d", got.code, ExitOK)
		}
		if got.stdout != demoSkill {
			t.Fatalf("stdout = %q, want %q", got.stdout, demoSkill)
		}
		if got.stderr != "" {
			t.Fatalf("stderr = %q, want empty", got.stderr)
		}
	})

	t.Run("appends trailing newline", func(t *testing.T) {
		app, stdout, stderr := newApp(t, okInvoker(), []byte("# Demo"))
		got := runApp(t, app, stdout, stderr, "--skill")
		if got.code != ExitOK {
			t.Fatalf("exit = %d, want %d", got.code, ExitOK)
		}
		if got.stdout != "# Demo\n" {
			t.Fatalf("stdout = %q, want %q", got.stdout, "# Demo\n")
		}
	})

	t.Run("absent", func(t *testing.T) {
		app, stdout, stderr := newApp(t, okInvoker(), nil)
		got := runApp(t, app, stdout, stderr, "--skill")
		if got.code != ExitError {
			t.Fatalf("exit = %d, want %d", got.code, ExitError)
		}
		if got.stdout != "" {
			t.Fatalf("stdout = %q, want empty", got.stdout)
		}
		if !strings.Contains(got.stderr, "no skill is embedded in this tool") ||
			!strings.Contains(got.stderr, string(relay.CodeOperationNotFound)) {
			t.Fatalf("stderr missing structured diagnostic: %q", got.stderr)
		}
	})

	// --skill reports its failure on stderr even under --json: the flag is a
	// metadata request, not an operation result.
	t.Run("absent under json stays on stderr", func(t *testing.T) {
		app, stdout, stderr := newApp(t, okInvoker(), nil)
		got := runApp(t, app, stdout, stderr, "--json", "--skill")
		if got.code != ExitError {
			t.Fatalf("exit = %d, want %d", got.code, ExitError)
		}
		if got.stdout != "" {
			t.Fatalf("stdout = %q, want empty", got.stdout)
		}
		if !strings.Contains(got.stderr, "no skill is embedded in this tool") {
			t.Fatalf("stderr missing diagnostic: %q", got.stderr)
		}
	})
}

func TestRunOperationNameMapping(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantOp   string
		wantCode int
	}{
		{name: "kebab-case verb", args: []string{"get-thing", "--id", "x"}, wantOp: "get_thing", wantCode: ExitOK},
		{name: "canonical snake_case", args: []string{"get_thing", "--id", "x"}, wantOp: "get_thing", wantCode: ExitOK},
		{name: "multi-word kebab-case", args: []string{"search-items", "--query", "q"}, wantOp: "search_items", wantCode: ExitOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invoker := okInvoker()
			app, stdout, stderr := newDemoApp(t, invoker)
			got := runApp(t, app, stdout, stderr, test.args...)
			if got.code != test.wantCode {
				t.Fatalf("exit = %d, want %d (stderr: %s)", got.code, test.wantCode, got.stderr)
			}
			request := singleInvocation(t, invoker)
			if request.Type != "invoke" || request.Tool != "demo" || request.Operation != test.wantOp {
				t.Fatalf("unexpected request: %+v", request)
			}
		})
	}
}

func TestRunUnknownOperation(t *testing.T) {
	invoker := okInvoker()
	app, stdout, stderr := newDemoApp(t, invoker)
	got := runApp(t, app, stdout, stderr, "get-missing")
	if got.code != ExitError {
		t.Fatalf("exit = %d, want %d", got.code, ExitError)
	}
	if got.stdout != "" {
		t.Fatalf("stdout = %q, want empty", got.stdout)
	}
	if !strings.Contains(got.stderr, string(relay.CodeOperationNotFound)) ||
		!strings.Contains(got.stderr, "unknown operation \"get-missing\"") {
		t.Fatalf("unexpected stderr: %q", got.stderr)
	}
	if len(invoker.calls) != 0 {
		t.Fatalf("unknown operation must not reach the invoker, got %d calls", len(invoker.calls))
	}
}

func TestRunFlagTypes(t *testing.T) {
	invoker := okInvoker()
	app, stdout, stderr := newDemoApp(t, invoker)
	got := runApp(t, app, stdout, stderr,
		"get-thing",
		"--id", "abc",
		"--limit", "5",
		"--ratio", "1.5",
		"--draft",
		"--tags", "a", "--tags", "b",
		"--sizes", "1", "--sizes", "2",
		"--weights", "1.5", "--weights", "2.5",
	)
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", got.code, ExitOK, got.stderr)
	}
	input := singleInvocation(t, invoker).Input
	want := map[string]any{
		"id":      "abc",
		"limit":   int64(5),
		"ratio":   1.5,
		"draft":   true,
		"tags":    []string{"a", "b"},
		"sizes":   []int64{1, 2},
		"weights": []float64{1.5, 2.5},
	}
	if !reflect.DeepEqual(input, want) {
		t.Fatalf("input = %#v, want %#v", input, want)
	}
}

func TestRunSuccessOutput(t *testing.T) {
	invoker := &fakeInvoker{reply: func(relay.InvokeRequest) (relay.InvokeResponse, error) {
		return relay.InvokeResponse{Success: true, Result: map[string]any{"ok": true, "count": float64(2)}}, nil
	}}
	app, stdout, stderr := newDemoApp(t, invoker)
	got := runApp(t, app, stdout, stderr, "get-thing", "--id", "x")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d", got.code, ExitOK)
	}
	if got.stderr != "" {
		t.Fatalf("stderr = %q, want empty", got.stderr)
	}
	var result map[string]any
	decodeSingleJSON(t, got.stdout, &result)
	if result["ok"] != true || result["count"] != float64(2) {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestRunJSONEnvelope(t *testing.T) {
	invoker := &fakeInvoker{reply: func(relay.InvokeRequest) (relay.InvokeResponse, error) {
		return relay.InvokeResponse{Success: true, Result: map[string]any{"ok": true}}, nil
	}}
	app, stdout, stderr := newDemoApp(t, invoker)
	got := runApp(t, app, stdout, stderr, "--json", "get-thing", "--id", "x")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d", got.code, ExitOK)
	}
	if got.stderr != "" {
		t.Fatalf("stderr = %q, want empty", got.stderr)
	}
	var envelope relay.InvokeResponse
	decodeSingleJSON(t, got.stdout, &envelope)
	if !envelope.Success || envelope.Error != nil {
		t.Fatalf("unexpected envelope: %+v", envelope)
	}
	result, ok := envelope.Result.(map[string]any)
	if !ok || result["ok"] != true {
		t.Fatalf("unexpected envelope result: %#v", envelope.Result)
	}
}

// A --json failure is still a result: the envelope goes to stdout, stderr stays
// silent, and the code is the generic error code.
func TestRunJSONErrorEnvelopeGoesToStdout(t *testing.T) {
	app, stdout, stderr := newDemoApp(t, okInvoker())
	got := runApp(t, app, stdout, stderr, "--json", "get-missing")
	if got.code != ExitError {
		t.Fatalf("exit = %d, want %d", got.code, ExitError)
	}
	if got.stderr != "" {
		t.Fatalf("stderr = %q, want empty", got.stderr)
	}
	var envelope relay.InvokeResponse
	decodeSingleJSON(t, got.stdout, &envelope)
	if envelope.Success || envelope.Error == nil {
		t.Fatalf("expected a failure envelope, got %+v", envelope)
	}
	if envelope.Error.Code != relay.CodeOperationNotFound {
		t.Fatalf("code = %s, want %s", envelope.Error.Code, relay.CodeOperationNotFound)
	}
}

// The core stdout/stderr invariant (spec §10): JSON stdout is exactly one JSON
// document and never carries the human diagnostic prefix.
func TestRunNeverMixesDiagnosticsIntoJSONStdout(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "describe", args: []string{"--json", "--describe"}},
		{name: "success", args: []string{"--json", "get-thing", "--id", "x"}},
		{name: "missing required", args: []string{"--json", "get-thing"}},
		{name: "unknown operation", args: []string{"--json", "nope"}},
		{name: "malformed input-json", args: []string{"--json", "get-thing", "--input-json", "{"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, stdout, stderr := newDemoApp(t, okInvoker())
			got := runApp(t, app, stdout, stderr, test.args...)
			if got.stderr != "" {
				t.Fatalf("--json must not write diagnostics to stderr, got %q", got.stderr)
			}
			if strings.Contains(got.stdout, "demo: ") {
				t.Fatalf("diagnostic text mixed into JSON stdout: %q", got.stdout)
			}
			var payload any
			decodeSingleJSON(t, got.stdout, &payload)
		})
	}
}

// Conversely, without --json a failure is a diagnostic on stderr and stdout stays
// empty: the two streams never duplicate each other.
func TestRunFailuresKeepStdoutEmptyWithoutJSON(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{name: "missing required", args: []string{"get-thing"}, wantStderr: "missing required input \"id\""},
		{name: "unknown operation", args: []string{"nope"}, wantStderr: "unknown operation"},
		{name: "malformed input-json", args: []string{"get-thing", "--input-json", "{"}, wantStderr: "--input-json is not a JSON object"},
		{name: "missing input file", args: []string{"get-thing", "--input", filepath.Join(t.TempDir(), "absent.json")}, wantStderr: "read "},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, stdout, stderr := newDemoApp(t, okInvoker())
			got := runApp(t, app, stdout, stderr, test.args...)
			if got.code != ExitError {
				t.Fatalf("exit = %d, want %d", got.code, ExitError)
			}
			if got.stdout != "" {
				t.Fatalf("stdout = %q, want empty", got.stdout)
			}
			if !strings.Contains(got.stderr, string(relay.CodeInvalidInput)) &&
				!strings.Contains(got.stderr, string(relay.CodeOperationNotFound)) {
				t.Fatalf("stderr missing a structured code: %q", got.stderr)
			}
			if !strings.Contains(got.stderr, test.wantStderr) {
				t.Fatalf("stderr = %q, want it to contain %q", got.stderr, test.wantStderr)
			}
		})
	}
}

func TestRunInputFile(t *testing.T) {
	path := writeTempFile(t, "input.json", "{\"id\":\"from-file\",\"limit\":3}")
	invoker := okInvoker()
	app, stdout, stderr := newDemoApp(t, invoker)
	got := runApp(t, app, stdout, stderr, "get-thing", "--input", path)
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", got.code, ExitOK, got.stderr)
	}
	input := singleInvocation(t, invoker).Input
	// JSON numbers arrive as float64, which the manifest validator accepts for an
	// integer property (it checks the value, not the Go type).
	want := map[string]any{"id": "from-file", "limit": float64(3)}
	if !reflect.DeepEqual(input, want) {
		t.Fatalf("input = %#v, want %#v", input, want)
	}
}

func TestRunExplicitFlagsWinOverInputFile(t *testing.T) {
	path := writeTempFile(t, "input.json", "{\"id\":\"from-file\",\"limit\":3}")
	invoker := okInvoker()
	app, stdout, stderr := newDemoApp(t, invoker)
	got := runApp(t, app, stdout, stderr, "get-thing", "--input", path, "--id", "from-flag")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", got.code, ExitOK, got.stderr)
	}
	input := singleInvocation(t, invoker).Input
	if input["id"] != "from-flag" || input["limit"] != float64(3) {
		t.Fatalf("flags must win over the file: %#v", input)
	}
}

func TestRunInputJSON(t *testing.T) {
	invoker := okInvoker()
	app, stdout, stderr := newDemoApp(t, invoker)
	got := runApp(t, app, stdout, stderr, "get-thing", "--input-json", "{\"id\":\"inline\"}")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", got.code, ExitOK, got.stderr)
	}
	if input := singleInvocation(t, invoker).Input; input["id"] != "inline" {
		t.Fatalf("unexpected input: %#v", input)
	}
}

func TestRunInputJSONRejectsNonObject(t *testing.T) {
	for _, raw := range []string{"{", "[1,2]", "\"a string\"", "42"} {
		t.Run(raw, func(t *testing.T) {
			invoker := okInvoker()
			app, stdout, stderr := newDemoApp(t, invoker)
			got := runApp(t, app, stdout, stderr, "get-thing", "--input-json", raw)
			if got.code != ExitError {
				t.Fatalf("exit = %d, want %d", got.code, ExitError)
			}
			if !strings.Contains(got.stderr, string(relay.CodeInvalidInput)) ||
				!strings.Contains(got.stderr, "--input-json is not a JSON object") {
				t.Fatalf("unexpected stderr: %q", got.stderr)
			}
			if len(invoker.calls) != 0 {
				t.Fatalf("a bad --input-json must not reach the invoker")
			}
		})
	}
}

func TestRunInputFileErrors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "absent.json")
		app, stdout, stderr := newDemoApp(t, okInvoker())
		got := runApp(t, app, stdout, stderr, "get-thing", "--input", path)
		if got.code != ExitError {
			t.Fatalf("exit = %d, want %d", got.code, ExitError)
		}
		if !strings.Contains(got.stderr, "read "+path) {
			t.Fatalf("stderr = %q, want it to name the file", got.stderr)
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		path := writeTempFile(t, "bad.json", "{")
		app, stdout, stderr := newDemoApp(t, okInvoker())
		got := runApp(t, app, stdout, stderr, "get-thing", "--input", path)
		if got.code != ExitError {
			t.Fatalf("exit = %d, want %d", got.code, ExitError)
		}
		if !strings.Contains(got.stderr, path+" is not a JSON object") {
			t.Fatalf("stderr = %q", got.stderr)
		}
	})

	t.Run("mutually exclusive with input-json", func(t *testing.T) {
		path := writeTempFile(t, "input.json", "{\"id\":\"x\"}")
		app, stdout, stderr := newDemoApp(t, okInvoker())
		got := runApp(t, app, stdout, stderr, "get-thing", "--input", path, "--input-json", "{\"id\":\"y\"}")
		if got.code != ExitError {
			t.Fatalf("exit = %d, want %d", got.code, ExitError)
		}
		if !strings.Contains(got.stderr, "mutually exclusive") {
			t.Fatalf("stderr = %q", got.stderr)
		}
	})
}

func TestRunMissingRequiredInput(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "no flags", args: []string{"get-thing"}},
		{name: "only optional flags", args: []string{"get-thing", "--limit", "3"}},
		{name: "required supplied via input-json", args: []string{"search-items", "--input-json", "{\"query\":\"q\"}"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, stdout, stderr := newDemoApp(t, okInvoker())
			got := runApp(t, app, stdout, stderr, test.args...)
			if strings.Contains(strings.Join(test.args, " "), "input-json") {
				if got.code != ExitOK {
					t.Fatalf("input-json should satisfy required: exit = %d, stderr = %q", got.code, got.stderr)
				}
				return
			}
			if got.code != ExitError {
				t.Fatalf("exit = %d, want %d", got.code, ExitError)
			}
			if !strings.Contains(got.stderr, "missing required input") {
				t.Fatalf("stderr = %q", got.stderr)
			}
		})
	}
}

// Wrong types and unknown properties are rejected at the flag layer, because the
// flag parser is what binds a CLI argument to its declared property type.
func TestRunFlagParseErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "wrong integer type", args: []string{"get-thing", "--id", "x", "--limit", "not-a-number"}},
		{name: "wrong boolean type", args: []string{"get-thing", "--id", "x", "--draft=maybe"}},
		{name: "wrong number type", args: []string{"get-thing", "--id", "x", "--ratio", "fast"}},
		{name: "unknown property", args: []string{"get-thing", "--id", "x", "--bogus", "1"}},
		{name: "object property has no flag", args: []string{"get-thing", "--id", "x", "--meta", "{}"}},
		{name: "unexpected positional argument", args: []string{"get-thing", "--id", "x", "extra"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invoker := okInvoker()
			app, stdout, stderr := newDemoApp(t, invoker)
			got := runApp(t, app, stdout, stderr, test.args...)
			if got.code != ExitError {
				t.Fatalf("exit = %d, want %d", got.code, ExitError)
			}
			if got.stdout != "" {
				t.Fatalf("stdout = %q, want empty", got.stdout)
			}
			if !strings.Contains(got.stderr, string(relay.CodeInvalidInput)) {
				t.Fatalf("stderr = %q, want INVALID_INPUT", got.stderr)
			}
			if len(invoker.calls) != 0 {
				t.Fatalf("a parse failure must not reach the invoker")
			}
		})
	}
}

// The runtime validates the assembled input against the operation's schema
// before it invokes anything, so a value that arrived through --input or
// --input-json and does not fit the schema is rejected here rather than
// travelling to the daemon and surfacing only from there. The daemon still
// re-validates the whole object against the same schema, because the tool
// binary is untrusted (spec §10, §40).
func TestRunJSONInputIsValidatedLocally(t *testing.T) {
	invoker := okInvoker()
	app, stdout, stderr := newDemoApp(t, invoker)
	got := runApp(t, app, stdout, stderr, "get-thing",
		"--input-json", "{\"id\":1,\"bogus\":true,\"limit\":\"three\"}")
	if got.code != ExitError {
		t.Fatalf("exit = %d, want %d (stderr: %s)", got.code, ExitError, got.stderr)
	}
	if !strings.Contains(got.stderr, string(relay.CodeInvalidInput)) {
		t.Fatalf("stderr = %q, want the %s code", got.stderr, relay.CodeInvalidInput)
	}
	if !strings.Contains(got.stderr, "unknown input") {
		t.Fatalf("stderr = %q, want the unknown-property diagnostic", got.stderr)
	}
	if len(invoker.calls) != 0 {
		t.Fatalf("input reached the invoker despite failing validation: %d call(s)", len(invoker.calls))
	}
}

func TestRunOperationHelp(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "long flag", args: []string{"get-thing", "--help"}, want: "Usage: demo get-thing"},
		{name: "short flag", args: []string{"get-thing", "-h"}, want: "Usage: demo get-thing"},
		{name: "second operation", args: []string{"search-items", "-h"}, want: "Usage: demo search-items"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invoker := okInvoker()
			app, stdout, stderr := newDemoApp(t, invoker)
			got := runApp(t, app, stdout, stderr, test.args...)
			if got.code != ExitOK {
				t.Fatalf("exit = %d, want %d (stderr: %s)", got.code, ExitOK, got.stderr)
			}
			if !strings.Contains(got.stdout, test.want) {
				t.Fatalf("stdout = %q, want it to contain %q", got.stdout, test.want)
			}
			if got.stderr != "" {
				t.Fatalf("stderr = %q, want empty", got.stderr)
			}
			if len(invoker.calls) != 0 {
				t.Fatalf("operation help must not invoke")
			}
		})
	}
}

func TestRunOperationHelpMarksRequiredFlags(t *testing.T) {
	app, stdout, stderr := newDemoApp(t, okInvoker())
	got := runApp(t, app, stdout, stderr, "get-thing", "--help")
	for _, want := range []string{"--id (string) required", "--limit (integer)", "--tags (array)"} {
		if !strings.Contains(got.stdout, want) {
			t.Fatalf("operation usage missing %q:\n%s", want, got.stdout)
		}
	}
}

func TestRunInvokerErrorPassthrough(t *testing.T) {
	tests := []struct {
		name     string
		reply    func(relay.InvokeRequest) (relay.InvokeResponse, error)
		wantCode relay.Code
	}{
		{
			name: "structured error",
			reply: func(relay.InvokeRequest) (relay.InvokeResponse, error) {
				return relay.InvokeResponse{}, relay.NewError(relay.CodeAuthRequired, "no credential")
			},
			wantCode: relay.CodeAuthRequired,
		},
		{
			name: "unstructured error becomes a network error",
			reply: func(relay.InvokeRequest) (relay.InvokeResponse, error) {
				return relay.InvokeResponse{}, errors.New("dial tcp: connection refused")
			},
			wantCode: relay.CodeNetworkError,
		},
		{
			name: "unsuccessful response carries its own error",
			reply: func(relay.InvokeRequest) (relay.InvokeResponse, error) {
				return relay.InvokeResponse{Success: false, Error: relay.NewError(relay.CodePermissionDenied, "denied")}, nil
			},
			wantCode: relay.CodePermissionDenied,
		},
		{
			name: "unsuccessful response without an error is a remote error",
			reply: func(relay.InvokeRequest) (relay.InvokeResponse, error) {
				return relay.InvokeResponse{Success: false}, nil
			},
			wantCode: relay.CodeRemoteError,
		},
		{
			name: "success flag with an error is still a failure",
			reply: func(relay.InvokeRequest) (relay.InvokeResponse, error) {
				return relay.InvokeResponse{Success: true, Error: relay.NewError(relay.CodeTimeout, "slow")}, nil
			},
			wantCode: relay.CodeTimeout,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invoker := &fakeInvoker{reply: test.reply}
			app, stdout, stderr := newDemoApp(t, invoker)
			got := runApp(t, app, stdout, stderr, "get-thing", "--id", "x")
			if got.code != ExitError {
				t.Fatalf("exit = %d, want %d", got.code, ExitError)
			}
			if got.stdout != "" {
				t.Fatalf("stdout = %q, want empty", got.stdout)
			}
			if !strings.Contains(got.stderr, string(test.wantCode)) {
				t.Fatalf("stderr = %q, want it to contain %s", got.stderr, test.wantCode)
			}

			// The same failure under --json becomes an envelope on stdout.
			jsonInvoker := &fakeInvoker{reply: test.reply}
			jsonApp, jsonStdout, jsonStderr := newDemoApp(t, jsonInvoker)
			jsonGot := runApp(t, jsonApp, jsonStdout, jsonStderr, "--json", "get-thing", "--id", "x")
			if jsonGot.code != ExitError {
				t.Fatalf("--json exit = %d, want %d", jsonGot.code, ExitError)
			}
			if jsonGot.stderr != "" {
				t.Fatalf("--json stderr = %q, want empty", jsonGot.stderr)
			}
			var envelope relay.InvokeResponse
			decodeSingleJSON(t, jsonGot.stdout, &envelope)
			if envelope.Error == nil || envelope.Error.Code != test.wantCode {
				t.Fatalf("envelope error = %+v, want code %s", envelope.Error, test.wantCode)
			}
		})
	}
}

func TestSplitGlobalFlags(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantJSON bool
		wantRest []string
	}{
		{name: "empty", args: nil, wantJSON: false, wantRest: nil},
		{name: "json only", args: []string{"--json"}, wantJSON: true, wantRest: nil},
		{name: "repeated json", args: []string{"--json", "--json"}, wantJSON: true, wantRest: nil},
		{name: "json before operation", args: []string{"--json", "get-thing", "--id", "x"},
			wantJSON: true, wantRest: []string{"get-thing", "--id", "x"}},
		{name: "no json", args: []string{"get-thing", "--id", "x"},
			wantJSON: false, wantRest: []string{"get-thing", "--id", "x"}},
		{name: "json after operation is not a global flag", args: []string{"get-thing", "--json"},
			wantJSON: false, wantRest: []string{"get-thing", "--json"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			jsonOut, rest := splitGlobalFlags(test.args)
			if jsonOut != test.wantJSON {
				t.Fatalf("jsonOut = %v, want %v", jsonOut, test.wantJSON)
			}
			if !reflect.DeepEqual(rest, test.wantRest) {
				t.Fatalf("rest = %#v, want %#v", rest, test.wantRest)
			}
		})
	}
}

func TestOperationName(t *testing.T) {
	tests := map[string]string{
		"get_repository":  "get-repository",
		"list":            "list",
		"search_items":    "search-items",
		"already-kebabed": "already-kebabed",
	}
	for input, want := range tests {
		if got := operationName(input); got != want {
			t.Fatalf("operationName(%q) = %q, want %q", input, got, want)
		}
	}
}

// paginatedReply is the scripted reply a paginating operation returns: a
// machine result plus the walk's shape, exactly as the daemon reports it.
func paginatedReply(result any, pages int, truncated bool) *fakeInvoker {
	return &fakeInvoker{reply: func(relay.InvokeRequest) (relay.InvokeResponse, error) {
		return relay.InvokeResponse{Success: true, Result: result, Pages: pages, Truncated: truncated}, nil
	}}
}

// --paginate is the reserved opt-in that exists only where the manifest declares
// a pagination strategy (spec §20). On a capable operation it reaches the
// invoker as InvokeRequest.Paginate, so a built binary can walk pages itself
// rather than only the CLI's run being able to.
func TestRunPaginateForwardsToInvoker(t *testing.T) {
	invoker := paginatedReply([]string{"a", "b", "c"}, 3, false)
	app, stdout, stderr := newDemoApp(t, invoker)
	got := runApp(t, app, stdout, stderr, "search-items", "--query", "q", "--paginate")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", got.code, ExitOK, got.stderr)
	}
	request := singleInvocation(t, invoker)
	if !request.Paginate {
		t.Fatalf("--paginate must forward Paginate=true, got %+v", request)
	}
	if request.Operation != "search_items" {
		t.Fatalf("operation = %q, want search_items", request.Operation)
	}
}

// The walk's shape is a human diagnostic: it belongs on stderr, and stdout stays
// the lone machine result so a caller parsing JSON is never handed a stray line.
func TestRunPaginateReportsWalkOnStderr(t *testing.T) {
	invoker := paginatedReply([]string{"a", "b", "c"}, 3, false)
	app, stdout, stderr := newDemoApp(t, invoker)
	got := runApp(t, app, stdout, stderr, "search-items", "--query", "q", "--paginate")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", got.code, ExitOK, got.stderr)
	}
	if !strings.Contains(got.stderr, "collected 3 pages") {
		t.Fatalf("stderr = %q, want the walk report", got.stderr)
	}
	if strings.Contains(got.stdout, "collected") {
		t.Fatalf("diagnostic mixed into stdout: %q", got.stdout)
	}
	var result []string
	decodeSingleJSON(t, got.stdout, &result)
	if !reflect.DeepEqual(result, []string{"a", "b", "c"}) {
		t.Fatalf("stdout = %#v, want the machine result alone", result)
	}
}

// The reporting mirrors the CLI's reportPagination exactly: silent when no walk
// happened, singular for one page, plural otherwise, and it names a truncated
// walk so a bounded result is never read as complete (spec §20).
func TestRunPaginateWalkReporting(t *testing.T) {
	tests := []struct {
		name       string
		response   relay.InvokeResponse
		wantStderr string
	}{
		{name: "no walk is silent", response: relay.InvokeResponse{Success: true}},
		{name: "single page is singular", response: relay.InvokeResponse{Success: true, Pages: 1}, wantStderr: "collected 1 page"},
		{name: "many pages", response: relay.InvokeResponse{Success: true, Pages: 3}, wantStderr: "collected 3 pages"},
		{
			name:       "truncated walk is named",
			response:   relay.InvokeResponse{Success: true, Pages: 2, Truncated: true},
			wantStderr: "collected 2 pages; the result is truncated and may be incomplete",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invoker := &fakeInvoker{reply: func(relay.InvokeRequest) (relay.InvokeResponse, error) {
				return test.response, nil
			}}
			app, stdout, stderr := newDemoApp(t, invoker)
			got := runApp(t, app, stdout, stderr, "search-items", "--query", "q", "--paginate")
			if got.code != ExitOK {
				t.Fatalf("exit = %d, want %d (stderr: %s)", got.code, ExitOK, got.stderr)
			}
			if test.wantStderr == "" {
				if got.stderr != "" {
					t.Fatalf("stderr = %q, want empty", got.stderr)
				}
				return
			}
			if !strings.Contains(got.stderr, test.wantStderr) {
				t.Fatalf("stderr = %q, want it to contain %q", got.stderr, test.wantStderr)
			}
		})
	}
}

// An operation whose manifest declares no pagination does not have the flag, so
// asking it to paginate is refused as INVALID_INPUT rather than silently ignored
// the same refuse-don't-pretend contract the MCP adapter enforces.
func TestRunPaginateRefusedWithoutStrategy(t *testing.T) {
	t.Run("stderr diagnostic", func(t *testing.T) {
		invoker := okInvoker()
		app, stdout, stderr := newDemoApp(t, invoker)
		got := runApp(t, app, stdout, stderr, "get-thing", "--id", "x", "--paginate")
		if got.code != ExitError {
			t.Fatalf("exit = %d, want %d", got.code, ExitError)
		}
		if got.stdout != "" {
			t.Fatalf("stdout = %q, want empty", got.stdout)
		}
		if !strings.Contains(got.stderr, string(relay.CodeInvalidInput)) {
			t.Fatalf("stderr = %q, want INVALID_INPUT", got.stderr)
		}
		if len(invoker.calls) != 0 {
			t.Fatalf("a refused walk must not reach the invoker, got %d call(s)", len(invoker.calls))
		}
	})

	t.Run("json envelope", func(t *testing.T) {
		invoker := okInvoker()
		app, stdout, stderr := newDemoApp(t, invoker)
		got := runApp(t, app, stdout, stderr, "--json", "get-thing", "--id", "x", "--paginate")
		if got.code != ExitError {
			t.Fatalf("exit = %d, want %d", got.code, ExitError)
		}
		if got.stderr != "" {
			t.Fatalf("stderr = %q, want empty under --json", got.stderr)
		}
		var envelope relay.InvokeResponse
		decodeSingleJSON(t, got.stdout, &envelope)
		if envelope.Error == nil || envelope.Error.Code != relay.CodeInvalidInput {
			t.Fatalf("envelope error = %+v, want INVALID_INPUT", envelope.Error)
		}
		if len(invoker.calls) != 0 {
			t.Fatalf("a refused walk must not reach the invoker, got %d call(s)", len(invoker.calls))
		}
	})
}

// --paginate composes with the global --json flag: the envelope is the lone JSON
// document on stdout and the walk diagnostic still goes to stderr.
func TestRunPaginateWithJSONEnvelope(t *testing.T) {
	invoker := paginatedReply(map[string]any{"items": []string{"a", "b", "c"}}, 3, false)
	app, stdout, stderr := newDemoApp(t, invoker)
	got := runApp(t, app, stdout, stderr, "--json", "search-items", "--query", "q", "--paginate")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", got.code, ExitOK, got.stderr)
	}
	if !strings.Contains(got.stderr, "collected 3 pages") {
		t.Fatalf("stderr = %q, want the walk report", got.stderr)
	}
	if !singleInvocation(t, invoker).Paginate {
		t.Fatal("--paginate must still be forwarded under --json")
	}
	var envelope relay.InvokeResponse
	decodeSingleJSON(t, got.stdout, &envelope)
	if !envelope.Success || envelope.Error != nil {
		t.Fatalf("unexpected envelope: %+v", envelope)
	}
	if strings.Contains(got.stdout, "collected") {
		t.Fatalf("diagnostic mixed into JSON stdout: %q", got.stdout)
	}
}

// The reserved flag is advertised only where it is accepted: it shows up in a
// capable operation's usage and nowhere else, so --help never promises a flag
// the operation will refuse (spec 20).
func TestRunOperationHelpAdvertisesPaginateOnlyWhenDeclared(t *testing.T) {
	app, stdout, _ := newDemoApp(t, okInvoker())
	if got := runApp(t, app, stdout, &bytes.Buffer{}, "search-items", "--help"); !strings.Contains(got.stdout, "--paginate") {
		t.Fatalf("paginating operation usage missing --paginate:\n%s", got.stdout)
	}
	app, stdout, _ = newDemoApp(t, okInvoker())
	if got := runApp(t, app, stdout, &bytes.Buffer{}, "get-thing", "--help"); strings.Contains(got.stdout, "--paginate") {
		t.Fatalf("non-paginating operation usage advertised --paginate:\n%s", got.stdout)
	}
}

// The discovery paths keep working exactly as before: a trailing global-looking
// argument never turns --describe, --skill, or --manifest into an invocation or
// mixes a walk diagnostic into their output (spec §9, §13).
func TestRunPaginateDoesNotDisturbDiscoveryPaths(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "describe", args: []string{"--describe", "--paginate"}},
		{name: "skill", args: []string{"--skill", "--paginate"}},
		{name: "manifest", args: []string{"--manifest", "--paginate"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invoker := okInvoker()
			app, stdout, stderr := newDemoApp(t, invoker)
			got := runApp(t, app, stdout, stderr, test.args...)
			if got.code != ExitOK {
				t.Fatalf("exit = %d, want %d (stderr: %s)", got.code, ExitOK, got.stderr)
			}
			if got.stderr != "" {
				t.Fatalf("stderr = %q, want empty", got.stderr)
			}
			if got.stdout == "" {
				t.Fatal("discovery path produced no output")
			}
			if len(invoker.calls) != 0 {
				t.Fatalf("a discovery path must not invoke, got %d call(s)", len(invoker.calls))
			}
		})
	}
}
