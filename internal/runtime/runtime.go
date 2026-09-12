// Package runtime implements the generic Relay tool runtime: the behaviour
// every built tool binary shares.
//
// It loads the embedded manifest and skill, parses the CLI invocation, validates
// input, and forwards the operation to the daemon over IPC. It contains no
// endpoint-specific code — one consistent runtime for every tool is the whole
// product (spec §13).
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"relay/internal/manifest"
	"relay/pkg/relay"
)

// Exit codes shared by every tool binary.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// paginateFlag is the reserved operation flag that opts a call into the
// operation's declared pagination walk (spec §20). It exists only on an
// operation whose manifest declares a pagination strategy, mirroring how the
// MCP adapter reserves the same name only there.
const paginateFlag = "paginate"

// Assets are the pieces embedded into a built tool binary: its manifest and its
// skill (spec §8).
type Assets struct {
	Manifest []byte
	Skill    []byte
}

// Invoker executes one operation. The tool runtime never contacts a service
// directly; it forwards to the daemon, which owns credentials and permissions
// (spec §13, §18, §40).
type Invoker interface {
	Invoke(ctx context.Context, req relay.InvokeRequest) (relay.InvokeResponse, error)
}

// App is a runnable tool binary.
type App struct {
	Manifest    *manifest.Document
	RawManifest []byte
	Skill       []byte
	Invoker     Invoker
	Stdout      io.Writer
	Stderr      io.Writer
}

// New builds an App from embedded assets. It does not validate the manifest:
// validation happens at build time, so a shipped binary is already known good.
func New(assets Assets, invoker Invoker, stdout, stderr io.Writer) (*App, error) {
	doc, err := manifest.Parse(assets.Manifest)
	if err != nil {
		return nil, err
	}
	return &App{
		Manifest:    doc,
		RawManifest: assets.Manifest,
		Skill:       assets.Skill,
		Invoker:     invoker,
		Stdout:      stdout,
		Stderr:      stderr,
	}, nil
}

// Run dispatches one invocation and returns the process exit code.
//
// Output contract (spec §10): results go to stdout, human diagnostics go to
// stderr, and the exit code reports success or failure. Logs never mix into
// JSON on stdout.
func (a *App) Run(ctx context.Context, args []string) int {
	jsonOut, rest := splitGlobalFlags(args)

	if len(rest) == 0 {
		fmt.Fprint(a.Stderr, a.usage())
		return ExitUsage
	}

	switch rest[0] {
	case "--describe":
		return a.describe()
	case "--manifest":
		return a.manifest()
	case "--skill":
		return a.skill()
	case "--version":
		fmt.Fprintf(a.Stdout, "%s %s\n", a.Manifest.Metadata.Name, a.Manifest.Metadata.Version)
		return ExitOK
	case "--help", "-h", "help":
		fmt.Fprint(a.Stdout, a.usage())
		return ExitOK
	}

	operation := strings.ReplaceAll(rest[0], "-", "_")
	tool := a.Manifest.Operation(operation)
	if tool == nil {
		return a.fail(jsonOut, relay.NewError(relay.CodeOperationNotFound,
			fmt.Sprintf("unknown operation %q", rest[0])))
	}

	input, paginate, outcome, inputErr := a.parseInput(tool, rest[1:])
	if inputErr != nil {
		return a.fail(jsonOut, inputErr)
	}
	if outcome == parseHelp {
		fmt.Fprint(a.Stdout, a.operationUsage(tool))
		return ExitOK
	}

	response, invokeErr := a.Invoker.Invoke(ctx, relay.InvokeRequest{
		Type:      "invoke",
		Tool:      a.Manifest.Metadata.Name,
		Operation: tool.Name,
		Input:     input,
		Paginate:  paginate,
	})
	if invokeErr != nil {
		var structured *relay.Error
		if errors.As(invokeErr, &structured) {
			return a.fail(jsonOut, structured)
		}
		return a.fail(jsonOut, relay.NewError(relay.CodeNetworkError, invokeErr.Error()))
	}
	if !response.Success || response.Error != nil {
		if response.Error == nil {
			response.Error = relay.NewError(relay.CodeRemoteError, "operation failed")
		}
		return a.fail(jsonOut, response.Error)
	}

	a.reportPagination(response)
	return a.succeed(jsonOut, response.Result)
}

// reportPagination writes a paginated walk's shape to stderr, leaving stdout as
// the machine result alone (spec §10, §20). It is silent for an ordinary
// invocation, so a single-request call emits nothing extra, and it names a
// truncated walk explicitly so a bounded result is never read as complete. It
// mirrors the CLI's reportPagination, prefixed with the tool's own name the way
// every other runtime diagnostic is.
func (a *App) reportPagination(response relay.InvokeResponse) {
	if response.Pages <= 0 {
		return
	}
	unit := "pages"
	if response.Pages == 1 {
		unit = "page"
	}
	if response.Truncated {
		fmt.Fprintf(a.Stderr, "%s: collected %d %s; the result is truncated and may be incomplete\n",
			a.Manifest.Metadata.Name, response.Pages, unit)
		return
	}
	fmt.Fprintf(a.Stderr, "%s: collected %d %s\n", a.Manifest.Metadata.Name, response.Pages, unit)
}

// manifest writes the raw embedded manifest bytes verbatim to stdout (spec §13,
// §16). The binary is the authoritative schema source; this returns the exact
// bytes embedded at build time with no re-encoding or JSON wrapper.
func (a *App) manifest() int {
	a.Stdout.Write(a.RawManifest)
	return ExitOK
}

// describe prints the primary discovery and registration contract (spec §9).
func (a *App) describe() int {
	descriptor := a.Manifest.Descriptor(a.hasSkill())
	if err := a.writeJSON(descriptor); err != nil {
		fmt.Fprintf(a.Stderr, "%s: %v\n", a.Manifest.Metadata.Name, err)
		return ExitError
	}
	return ExitOK
}

// skill prints the embedded guidance. A Relay binary carries its own
// documentation for AI usage (spec §8).
func (a *App) skill() int {
	if !a.hasSkill() {
		return a.fail(false, relay.NewError(relay.CodeOperationNotFound,
			"no skill is embedded in this tool"))
	}
	fmt.Fprint(a.Stdout, string(a.Skill))
	if !strings.HasSuffix(string(a.Skill), "\n") {
		fmt.Fprintln(a.Stdout)
	}
	return ExitOK
}

func (a *App) hasSkill() bool {
	return strings.TrimSpace(string(a.Skill)) != ""
}

func (a *App) succeed(jsonOut bool, result any) int {
	var payload any = result
	if jsonOut {
		payload = relay.InvokeResponse{Success: true, Result: result}
	}
	if err := a.writeJSON(payload); err != nil {
		fmt.Fprintf(a.Stderr, "%s: %v\n", a.Manifest.Metadata.Name, err)
		return ExitError
	}
	return ExitOK
}

// fail reports a structured error. Errors are diagnostics, so they go to
// stderr by default and to the stdout envelope under --json (spec §10, §26).
func (a *App) fail(jsonOut bool, structured *relay.Error) int {
	if jsonOut {
		_ = a.writeJSON(relay.InvokeResponse{Success: false, Error: structured})
		return ExitError
	}
	fmt.Fprintf(a.Stderr, "%s: %s: %s\n",
		a.Manifest.Metadata.Name, structured.Code, structured.Message)
	return ExitError
}

func (a *App) writeJSON(payload any) error {
	encoder := json.NewEncoder(a.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(payload)
}

// splitGlobalFlags extracts flags that may precede the operation name.
func splitGlobalFlags(args []string) (jsonOut bool, rest []string) {
	for i, arg := range args {
		if arg != "--json" {
			return jsonOut, args[i:]
		}
		jsonOut = true
	}
	return jsonOut, nil
}

// parseOutcome distinguishes a help request from a normal parse.
type parseOutcome int

const (
	parseOK parseOutcome = iota
	parseHelp
)

// parseInput turns an operation's flags into its input object, then validates
// it. It also reports the reserved --paginate opt-in, which is accepted only on
// an operation whose manifest declares a pagination strategy (spec §20). Every
// failure becomes a structured error, so stderr carries exactly one diagnostic
// rather than a duplicated usage dump.
func (a *App) parseInput(tool *manifest.Tool, args []string) (map[string]any, bool, parseOutcome, *relay.Error) {
	flags := flag.NewFlagSet(tool.Name, flag.ContinueOnError)
	// The flag package would otherwise print its own message and usage before
	// returning; suppress that and report once, structurally.
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}

	// --paginate is reserved only where the manifest declares a walk. Registering
	// it solely there means an operation that cannot paginate has no such flag, so
	// asking it to is refused as INVALID_INPUT rather than silently ignored — the
	// same choice the MCP adapter makes by reserving the name only on a capable
	// operation.
	paginates := tool.Request.Pagination != nil
	var paginate bool
	if paginates {
		flags.BoolVar(&paginate, paginateFlag, false,
			"follow the operation's declared pagination and collect the whole result")
	}

	// Explicit JSON input for complex objects (spec §11).
	inputFile := flags.String("input", "", "read the operation input from a JSON file")
	inputJSON := flags.String("input-json", "", "read the operation input from a JSON string")

	values := map[string]*flagValue{}
	for name, property := range tool.Input.Properties {
		if paginates && name == paginateFlag {
			// The reserved walk opt-in wins the name, exactly as it does over MCP.
			continue
		}
		if value := newFlagValue(flags, name, property); value != nil {
			values[name] = value
		}
	}

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, false, parseHelp, nil
		}
		return nil, false, parseOK, relay.NewError(relay.CodeInvalidInput, err.Error())
	}
	if flags.NArg() > 0 {
		return nil, false, parseOK, relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("unexpected argument %q", flags.Arg(0)))
	}

	input := map[string]any{}
	switch {
	case *inputFile != "" && *inputJSON != "":
		return nil, false, parseOK, relay.NewError(relay.CodeInvalidInput,
			"--input and --input-json are mutually exclusive")
	case *inputFile != "":
		loaded, fileErr := readInputFile(*inputFile)
		if fileErr != nil {
			return nil, false, parseOK, fileErr
		}
		input = loaded
	case *inputJSON != "":
		if err := json.Unmarshal([]byte(*inputJSON), &input); err != nil {
			return nil, false, parseOK, relay.NewError(relay.CodeInvalidInput,
				fmt.Sprintf("--input-json is not a JSON object: %v", err))
		}
	}

	// Explicit flags win over file or inline JSON.
	flags.Visit(func(f *flag.Flag) {
		if value, ok := values[f.Name]; ok {
			input[f.Name] = value.get()
		}
	})

	// Validate the assembled input against the operation's schema before it
	// leaves this process. The flag path already rejects unknown flags and
	// mistyped values, but --input and --input-json bypass the flag parser, so
	// without this step an unknown property or a wrong type would travel all
	// the way to the daemon and only be reported from there (spec §10, §40).
	// The daemon still validates again, because a tool binary is untrusted and
	// the trusted component cannot assume its caller did the work.
	if failure := tool.ValidateInput(input); failure != nil {
		return nil, false, parseOK, failure
	}

	return input, paginate, parseOK, nil
}

// usage is the human-oriented help text (spec §9).
func (a *App) usage() string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s %s\n", a.Manifest.Metadata.Name, a.Manifest.Metadata.Version)
	if description := a.Manifest.Metadata.Description; description != "" {
		fmt.Fprintf(&builder, "%s\n", description)
	}
	builder.WriteString("\nUsage:\n")
	fmt.Fprintf(&builder, "  %s <operation> [flags]\n", a.Manifest.Metadata.Name)
	fmt.Fprintf(&builder, "  %s --describe | --manifest | --skill | --version | --help\n", a.Manifest.Metadata.Name)
	builder.WriteString("\nGlobal flags:\n")
	builder.WriteString("  --json    wrap results and errors in a JSON envelope\n")
	builder.WriteString("\nOperations:\n")
	for _, tool := range a.Manifest.Tools {
		fmt.Fprintf(&builder, "  %-22s %s\n", operationName(tool.Name), tool.Description)
	}
	return builder.String()
}

func (a *App) operationUsage(tool *manifest.Tool) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Usage: %s %s [flags]\n\n%s\n\nFlags:\n",
		a.Manifest.Metadata.Name, operationName(tool.Name), tool.Description)
	for name, property := range tool.Input.Properties {
		fmt.Fprintf(&builder, "  --%s (%s)%s\n", name, property.Type, requiredNote(tool, name))
	}
	if tool.Request.Pagination != nil {
		builder.WriteString("  --paginate (boolean) follow the operation's declared pagination and collect the whole result\n")
	}
	return builder.String()
}

func requiredNote(tool *manifest.Tool, name string) string {
	for _, required := range tool.Input.Required {
		if required == name {
			return " required"
		}
	}
	return ""
}

// operationName converts an operation's canonical snake_case name into the
// kebab-case verb a human types (spec §11).
func operationName(name string) string {
	return strings.ReplaceAll(name, "_", "-")
}

func readInputFile(path string) (map[string]any, *relay.Error) {
	data, err := readFile(path)
	if err != nil {
		return nil, relay.NewError(relay.CodeInvalidInput, fmt.Sprintf("read %s: %v", path, err))
	}
	var input map[string]any
	if err := json.Unmarshal(data, &input); err != nil {
		return nil, relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("%s is not a JSON object: %v", path, err))
	}
	return input, nil
}
