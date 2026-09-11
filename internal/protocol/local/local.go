package local

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"relay/internal/protocol"
	"relay/pkg/relay"
)

// maxOutputBytes caps how much a single local operation may return. It matches
// the daemon's IPC frame limit and the MCP frame limit (8 MiB), so an operation
// can never hand the daemon a result the transport could not carry (spec §40).
const maxOutputBytes = 8 << 20

// Local operation names. These are the values a manifest writes in an
// operation's request block as `request.operation` (spec §46).
const (
	OpReadFile      = "read_file"
	OpWriteFile     = "write_file"
	OpListDirectory = "list_directory"
	OpStat          = "stat"
	OpGitStatus     = "git_status"
	OpGitDiff       = "git_diff"
	OpGitLog        = "git_log"
)

// Capability families.
const (
	FamilyFilesystem = "filesystem"
	FamilyGit        = "git"
)

// Access is the filesystem access a local primitive performs. It decides which
// declared scope an invocation is checked against: a primitive that only reads
// is checked against permissions.filesystem.read, and one that writes against
// permissions.filesystem.write (spec §24, §25).
type Access int

const (
	// ReadOnly is a primitive that only reads the filesystem.
	ReadOnly Access = iota
	// WriteOnly is a primitive that creates, replaces, or removes content.
	WriteOnly
)

// String names the access mode the way a manifest does.
func (a Access) String() string {
	if a == WriteOnly {
		return "write"
	}
	return "read"
}

// Capability is one local primitive's contract. It is the single source of
// truth for three consumers: the executor, which runs it; the daemon, which
// derives the concrete paths an invocation must be checked against before it
// runs; and nothing else — an operation this build does not implement is a
// runtime error, exactly like a `protocol.type` with no executor (spec §19).
type Capability struct {
	// Operation is the primitive's canonical name, as written in the manifest.
	Operation string
	// Family groups primitives: [FamilyFilesystem] or [FamilyGit].
	Family string
	// Access is the filesystem access the primitive performs.
	Access Access
	// Target is the input property that names the filesystem path the primitive
	// acts on. The daemon resolves that property and checks it against the
	// declared scope; the executor resolves the same property to do the work.
	Target string
}

// capabilities is the closed vocabulary of local primitives this build
// implements, keyed by operation name.
var capabilities = map[string]Capability{
	OpReadFile:      {Operation: OpReadFile, Family: FamilyFilesystem, Access: ReadOnly, Target: "path"},
	OpWriteFile:     {Operation: OpWriteFile, Family: FamilyFilesystem, Access: WriteOnly, Target: "path"},
	OpListDirectory: {Operation: OpListDirectory, Family: FamilyFilesystem, Access: ReadOnly, Target: "path"},
	OpStat:          {Operation: OpStat, Family: FamilyFilesystem, Access: ReadOnly, Target: "path"},
	OpGitStatus:     {Operation: OpGitStatus, Family: FamilyGit, Access: ReadOnly, Target: "repo"},
	OpGitDiff:       {Operation: OpGitDiff, Family: FamilyGit, Access: ReadOnly, Target: "repo"},
	OpGitLog:        {Operation: OpGitLog, Family: FamilyGit, Access: ReadOnly, Target: "repo"},
}

// CapabilityOf returns the contract for a local primitive. ok is false when
// this build has no such primitive.
func CapabilityOf(operation string) (Capability, bool) {
	capability, ok := capabilities[operation]
	return capability, ok
}

// Operations returns the implemented primitive names, sorted.
func Operations() []string {
	names := make([]string, 0, len(capabilities))
	for name := range capabilities {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Requirement is the filesystem access one invocation needs, expressed as the
// concrete resolved paths the executor will act on. The daemon checks these
// against the manifest's declared scopes before the executor runs
// (spec §24, §40).
type Requirement struct {
	Reads  []string
	Writes []string
}

// Requirements derives the concrete paths one local invocation will touch from
// the operation its manifest declares and the operation's input.
//
// It returns a structured error for an unknown primitive or a target that
// cannot be resolved to an absolute path, so the daemon refuses an invocation
// it cannot classify rather than running it unchecked. The paths returned are
// produced by the executor's own [ResolvePath], so the path that is checked is
// the path that is acted on.
func Requirements(operation string, input map[string]any) (Requirement, *relay.Error) {
	capability, ok := CapabilityOf(operation)
	if !ok {
		return Requirement{}, unknownOperation(operation)
	}
	target, failure := targetPath(capability, input)
	if failure != nil {
		return Requirement{}, failure
	}
	if capability.Access == WriteOnly {
		return Requirement{Writes: []string{target}}, nil
	}
	return Requirement{Reads: []string{target}}, nil
}

// ResolveScopes canonicalizes the filesystem scopes a manifest declares so the
// scope check compares like with like: every scope goes through the same
// [ResolvePath] rule the requested path does, expanding a leading "~" and
// resolving symbolic links.
//
// Without this, a scope written as "~/Documents" could never match the
// absolute, symlink-free path an invocation carries, and the declared scope
// would be silently useless. A scope that does not resolve to an absolute path
// is dropped rather than kept verbatim: dropping narrows access, so a malformed
// scope denies instead of widening.
func ResolveScopes(scopes []string) []string {
	if len(scopes) == 0 {
		return nil
	}
	resolved := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		path, err := ResolvePath(scope)
		if err != nil {
			continue
		}
		resolved = append(resolved, path)
	}
	return resolved
}

// ResolvePath turns a path from an operation input into the absolute,
// symlink-free path that will be checked and acted on.
//
// It expands a leading "~" against the current user's home directory, requires
// the result to be absolute, and resolves symbolic links on the deepest
// existing ancestor of the path, re-appending the components that do not exist
// yet (so a write to a new file resolves its parent).
//
// Resolving is the escape guard (spec §24, §40): a symlink that sits inside a
// declared scope but points outside it resolves to its target, so the scope
// check sees where the operation would really read or write. A relative path is
// refused rather than resolved against the daemon's working directory, which is
// not a base any manifest author can reason about.
func ResolvePath(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("path is empty")
	}
	expanded, err := expandHome(raw)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(expanded) {
		return "", fmt.Errorf("path %q must be absolute or start with ~", raw)
	}
	resolved, err := resolveSymlinks(filepath.Clean(expanded))
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", raw, err)
	}
	return resolved, nil
}

// expandHome replaces a leading "~/" (or a bare "~") with the current user's
// home directory. A "~user" form is left alone: it is not absolute, so
// [ResolvePath] refuses it rather than guessing whose home was meant.
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate the home directory for %q: %w", path, err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

// resolveSymlinks resolves the longest existing prefix of path with
// filepath.EvalSymlinks and re-appends the components that do not exist yet.
// EvalSymlinks alone fails on a path whose final component has not been created
// — precisely the write_file case — so a non-existent suffix is resolved
// against its nearest existing parent instead.
func resolveSymlinks(path string) (string, error) {
	current := path
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			if len(missing) == 0 {
				return filepath.Clean(resolved), nil
			}
			parts := append([]string{resolved}, reverse(missing)...)
			return filepath.Clean(filepath.Join(parts...)), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			// The whole path is missing; there is nothing left to resolve.
			return filepath.Clean(path), nil
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// reverse returns a copy of values in reverse order.
func reverse(values []string) []string {
	reversed := make([]string, len(values))
	for i, value := range values {
		reversed[len(values)-1-i] = value
	}
	return reversed
}

// targetPath resolves the input property a primitive names as its target.
func targetPath(capability Capability, input map[string]any) (string, *relay.Error) {
	raw, ok := input[capability.Target].(string)
	if !ok || raw == "" {
		return "", relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("operation %q requires input %q to name a filesystem path",
				capability.Operation, capability.Target))
	}
	resolved, err := ResolvePath(raw)
	if err != nil {
		return "", relay.NewError(relay.CodeInvalidInput, err.Error())
	}
	return resolved, nil
}

// unknownOperation reports a primitive this build does not implement, the same
// way the daemon reports a protocol it has no executor for (spec §19).
func unknownOperation(operation string) *relay.Error {
	name := operation
	if name == "" {
		name = "(empty)"
	}
	return relay.NewError(relay.CodeProtocolError,
		fmt.Sprintf("this Relay build has no local operation %q", name)).
		WithDetails(map[string]any{"available": Operations()})
}

// Executor runs local capabilities in-process. It satisfies protocol.Executor
// and holds no per-request state, so one instance serves concurrent
// invocations.
type Executor struct {
	// GitPath overrides the git executable. Empty means "git", resolved from
	// PATH. Tests inject a path to pin the binary under test.
	GitPath string
}

// New returns an Executor with sensible defaults.
func New() *Executor { return &Executor{} }

// Execute runs one declared local operation.
//
// The operation is named by the request block the daemon projected onto
// [protocol.Spec.Endpoint]. The target path is resolved here by the same rule
// the daemon used to authorize the call, so the checked path and the acted-on
// path are identical.
func (e *Executor) Execute(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	capability, ok := CapabilityOf(req.Spec.Endpoint)
	if !ok {
		return protocol.Response{}, unknownOperation(req.Spec.Endpoint)
	}
	target, failure := targetPath(capability, req.Input)
	if failure != nil {
		return protocol.Response{}, failure
	}

	// The concrete error type is kept here on purpose: assigning a nil
	// *relay.Error into an error interface would make `err != nil` true.
	var result any
	var executionFailure *relay.Error
	switch capability.Operation {
	case OpReadFile:
		result, executionFailure = readFile(target, req.Input)
	case OpWriteFile:
		result, executionFailure = writeFile(target, req.Input)
	case OpListDirectory:
		result, executionFailure = listDirectory(target, req.Input)
	case OpStat:
		result, executionFailure = statPath(target)
	case OpGitStatus:
		result, executionFailure = e.gitStatus(ctx, target)
	case OpGitDiff:
		result, executionFailure = e.gitDiff(ctx, target, req.Input)
	case OpGitLog:
		result, executionFailure = e.gitLog(ctx, target, req.Input)
	default:
		executionFailure = unknownOperation(capability.Operation)
	}
	if executionFailure != nil {
		return protocol.Response{}, executionFailure
	}
	return protocol.Response{Status: 200, Body: result}, nil
}

// ioFailure maps a filesystem error onto the shared error taxonomy: a missing
// path is invalid input, an OS permission denial is PERMISSION_DENIED (the
// declared-scope denial is a different, earlier check the daemon owns), and
// anything else is a backend failure.
func ioFailure(action, path string, err error) *relay.Error {
	details := map[string]any{"action": action, "path": path}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("%s %q: no such file or directory", action, path)).WithDetails(details)
	case errors.Is(err, fs.ErrPermission):
		return relay.NewError(relay.CodePermissionDenied,
			fmt.Sprintf("%s %q: permission denied", action, path)).WithDetails(details)
	default:
		return relay.NewError(relay.CodeRemoteError,
			fmt.Sprintf("%s %q: %v", action, path, err)).WithDetails(details)
	}
}

// tooLarge reports a result the 8 MiB cap refuses.
func tooLarge(what, path string, limit int) *relay.Error {
	return relay.NewError(relay.CodeRemoteError,
		fmt.Sprintf("%s for %q exceeds the %d-byte output limit", what, path, limit)).
		WithDetails(map[string]any{"path": path, "limitBytes": limit})
}

// stringInput reads an optional string property, falling back to fallback.
func stringInput(input map[string]any, name, fallback string) string {
	value, ok := input[name].(string)
	if !ok {
		return fallback
	}
	return value
}

// boolInput reads an optional boolean property, falling back to fallback.
func boolInput(input map[string]any, name string, fallback bool) bool {
	value, ok := input[name].(bool)
	if !ok {
		return fallback
	}
	return value
}

// intInput reads an optional integer property, falling back to fallback. Input
// arrives from JSON, where every number is a float64, and from the tool
// runtime's flag layer, where it may be an int.
func intInput(input map[string]any, name string, fallback int) (int, bool) {
	switch value := input[name].(type) {
	case float64:
		return int(value), value == float64(int(value))
	case int:
		return value, true
	case int64:
		return int(value), true
	default:
		return fallback, true
	}
}
