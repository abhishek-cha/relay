package local

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"relay/pkg/relay"
)

const (
	// defaultGitLogLimit is how many commits git_log returns when the caller
	// does not say.
	defaultGitLogLimit = 20
	// maxGitLogLimit caps how many commits git_log will return in one call.
	maxGitLogLimit = 200
)

// gitCommand runs one git subcommand against a working tree and returns its
// stdout.
//
// The git binary is executed directly with an argument vector — never through a
// shell, so no input value can be read as shell syntax. The invocation is pinned
// to read-only inspection of the working tree:
//
//   - GIT_OPTIONAL_LOCKS=0 keeps git from refreshing and rewriting the index,
//     so a status or diff leaves the repository byte-for-byte unchanged;
//   - core.fsmonitor=false and --no-ext-diff/--no-textconv at the call sites
//     keep a user's git configuration from spawning a helper process, which a
//     plain read would otherwise allow;
//   - GIT_TERMINAL_PROMPT=0 refuses an interactive credential prompt;
//   - LC_ALL=C keeps git's own messages stable for the caller.
//
// Output is capped at [maxOutputBytes]; a command that produces more is refused
// rather than truncated. A non-zero exit is git reporting a problem with the
// request (not a git repository, unknown revision), which is invalid input.
func (e *Executor) gitCommand(ctx context.Context, repo string, args ...string) (string, *relay.Error) {
	binary := e.GitPath
	if binary == "" {
		binary = "git"
	}
	if _, err := exec.LookPath(binary); err != nil {
		return "", relay.NewError(relay.CodeProtocolError,
			"this Relay build could not find the git executable on PATH").
			WithDetails(map[string]any{"git": binary})
	}

	fullArgs := append([]string{"-C", repo, "-c", "core.fsmonitor=false"}, args...)
	command := exec.CommandContext(ctx, binary, fullArgs...)
	command.Env = append(os.Environ(),
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	)
	stdout := &limitedBuffer{limit: maxOutputBytes}
	stderr := &limitedBuffer{limit: maxOutputBytes}
	command.Stdout = stdout
	command.Stderr = stderr

	if err := command.Run(); err != nil {
		details := map[string]any{"repo": repo}
		if ctx.Err() != nil {
			return "", relay.NewError(relay.CodeTimeout,
				"git command did not finish before the deadline").WithDetails(details)
		}
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			details["exitCode"] = exitError.ExitCode()
			message := strings.TrimSpace(stderr.String())
			if message == "" {
				message = err.Error()
			}
			return "", relay.NewError(relay.CodeInvalidInput,
				fmt.Sprintf("git %s: %s", args[0], message)).WithDetails(details)
		}
		return "", relay.NewError(relay.CodeRemoteError,
			"git failed to start: "+err.Error()).WithDetails(details)
	}
	if stdout.exceeded {
		return "", tooLarge("git output", repo, maxOutputBytes)
	}
	return stdout.String(), nil
}

// gitStatus reports the working tree's branch and changed paths.
func (e *Executor) gitStatus(ctx context.Context, repo string) (any, *relay.Error) {
	output, failure := e.gitCommand(ctx, repo, "status", "--porcelain=v1", "--branch", "--untracked-files=all")
	if failure != nil {
		return nil, failure
	}
	branch, upstream, detached, entries := parsePorcelainStatus(output)
	return map[string]any{
		"repo":     repo,
		"branch":   branch,
		"upstream": upstream,
		"detached": detached,
		"clean":    len(entries) == 0,
		"count":    len(entries),
		"entries":  entries,
	}, nil
}

// parsePorcelainStatus reads `git status --porcelain=v1 --branch` output: an
// optional `##` header followed by one line per changed path.
func parsePorcelainStatus(output string) (branch, upstream string, detached bool, entries []map[string]any) {
	entries = make([]map[string]any, 0)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			branch, upstream, detached = parseBranchHeader(strings.TrimPrefix(line, "## "))
			continue
		}
		if len(line) < 3 {
			continue
		}
		status := line[:2]
		path := strings.TrimSpace(line[2:])
		entry := map[string]any{
			"status": status,
			"path":   path,
		}
		// Porcelain v1 writes a rename as "orig -> new"; report the current path
		// and keep the original alongside it.
		if from, to, found := strings.Cut(path, " -> "); found {
			entry["path"] = to
			entry["from"] = from
		}
		entries = append(entries, entry)
	}
	return branch, upstream, detached, entries
}

// parseBranchHeader reads the header's branch, upstream, and detached state.
// Git writes `main...origin/main [ahead 1]`, `No commits yet on main`, or
// `HEAD (no branch)`.
func parseBranchHeader(header string) (branch, upstream string, detached bool) {
	// The tracking detail is bracketed at the end and is not part of the names.
	if index := strings.Index(header, " ["); index >= 0 {
		header = header[:index]
	}
	if strings.HasPrefix(header, "No commits yet on ") {
		return strings.TrimPrefix(header, "No commits yet on "), "", false
	}
	if strings.HasPrefix(header, "HEAD (no branch)") {
		return "", "", true
	}
	branch, upstream, found := strings.Cut(header, "...")
	if !found {
		return header, "", false
	}
	return branch, upstream, false
}

// gitDiff returns a unified diff of unstaged or staged changes.
func (e *Executor) gitDiff(ctx context.Context, repo string, input map[string]any) (any, *relay.Error) {
	pathspec, failure := gitPathspec(input)
	if failure != nil {
		return nil, failure
	}
	staged := boolInput(input, "staged", false)
	args := []string{"diff", "--no-color", "--no-ext-diff", "--no-textconv"}
	if staged {
		args = append(args, "--cached")
	}
	if pathspec != "" {
		args = append(args, "--", pathspec)
	}
	output, failure := e.gitCommand(ctx, repo, args...)
	if failure != nil {
		return nil, failure
	}
	return map[string]any{
		"repo":   repo,
		"staged": staged,
		"path":   pathspec,
		"diff":   output,
	}, nil
}

// gitLog returns recent commits, newest first.
func (e *Executor) gitLog(ctx context.Context, repo string, input map[string]any) (any, *relay.Error) {
	pathspec, failure := gitPathspec(input)
	if failure != nil {
		return nil, failure
	}
	limit, ok := intInput(input, "limit", defaultGitLogLimit)
	if !ok || limit < 1 {
		return nil, relay.NewError(relay.CodeInvalidInput, "input \"limit\" must be a positive integer")
	}
	if limit > maxGitLogLimit {
		limit = maxGitLogLimit
	}

	// Fields are NUL-separated and records newline-separated, so a subject
	// containing spaces or punctuation cannot split a commit.
	args := []string{"log", "--no-color", "-n", strconv.Itoa(limit), "--format=%H%x00%h%x00%an%x00%aI%x00%s"}
	if pathspec != "" {
		args = append(args, "--", pathspec)
	}
	output, failure := e.gitCommand(ctx, repo, args...)
	if failure != nil {
		return nil, failure
	}
	commits := parseGitLog(output)
	return map[string]any{
		"repo":    repo,
		"path":    pathspec,
		"limit":   limit,
		"count":   len(commits),
		"commits": commits,
	}, nil
}

// parseGitLog reads the NUL-delimited --format output into commit objects.
func parseGitLog(output string) []map[string]any {
	commits := make([]map[string]any, 0)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\x00")
		if len(fields) != 5 {
			continue
		}
		commits = append(commits, map[string]any{
			"hash":      fields[0],
			"shortHash": fields[1],
			"author":    fields[2],
			"date":      fields[3],
			"subject":   fields[4],
		})
	}
	return commits
}

// gitPathspec reads the optional `path` property as a pathspec relative to the
// repository root. Absolute paths, pathspec magic, and any ".." segment are
// refused: the checked scope is the repository directory itself, so a pathspec
// that climbs out of it would inspect a path the daemon never authorized.
func gitPathspec(input map[string]any) (string, *relay.Error) {
	raw := stringInput(input, "path", "")
	if raw == "" {
		return "", nil
	}
	if filepath.IsAbs(raw) {
		return "", relay.NewError(relay.CodeInvalidInput,
			"input \"path\" must be relative to the repository root")
	}
	if strings.HasPrefix(raw, ":") {
		return "", relay.NewError(relay.CodeInvalidInput,
			"input \"path\" may not use git pathspec magic")
	}
	cleaned := filepath.Clean(raw)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", relay.NewError(relay.CodeInvalidInput,
			"input \"path\" may not escape the repository")
	}
	return filepath.ToSlash(cleaned), nil
}

// limitedBuffer collects at most limit bytes and records whether the writer
// tried to exceed it, so a runaway command is refused instead of buffered.
type limitedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.exceeded = true
		return len(p), nil
	}
	if len(p) > remaining {
		b.buffer.Write(p[:remaining])
		b.exceeded = true
		return len(p), nil
	}
	b.buffer.Write(p)
	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buffer.String() }
