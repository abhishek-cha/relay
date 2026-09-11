package local

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"relay/internal/protocol"
	"relay/pkg/relay"
)

// This file proves each local primitive against a throwaway directory and a
// throwaway git repository, so the executor is exercised the way the daemon
// calls it — through protocol.Executor with the primitive in Spec.Endpoint.

// execute runs one operation the way the daemon would and returns its body, or
// the structured failure it reported.
func execute(t *testing.T, operation string, input map[string]any) (any, *relay.Error) {
	t.Helper()
	response, err := New().Execute(context.Background(), protocol.Request{
		Tool:      "filesystem",
		Operation: operation,
		Input:     input,
		Spec:      protocol.Spec{Type: "local", Endpoint: operation},
	})
	if err != nil {
		failure, ok := err.(*relay.Error)
		if !ok {
			t.Fatalf("execute %s: unexpected error type %T: %v", operation, err, err)
		}
		return nil, failure
	}
	if response.Status != 200 {
		t.Fatalf("execute %s: status %d", operation, response.Status)
	}
	return response.Body, nil
}

// runOK runs an operation that must succeed and returns its object body.
func runOK(t *testing.T, operation string, input map[string]any) map[string]any {
	t.Helper()
	body, failure := execute(t, operation, input)
	if failure != nil {
		t.Fatalf("execute %s: %s", operation, failure.Message)
	}
	result, ok := body.(map[string]any)
	if !ok {
		t.Fatalf("execute %s: expected an object body, got %T", operation, body)
	}
	return result
}

// runFails runs an operation that must fail and returns the structured error.
func runFails(t *testing.T, operation string, input map[string]any, want relay.Code) *relay.Error {
	t.Helper()
	_, failure := execute(t, operation, input)
	if failure == nil {
		t.Fatalf("execute %s: expected failure %s, got success", operation, want)
	}
	if failure.Code != want {
		t.Fatalf("execute %s: want code %s, got %s (%s)", operation, want, failure.Code, failure.Message)
	}
	return failure
}

// ---- filesystem ------------------------------------------------------------

func TestReadFile(t *testing.T) {
	root := resolvedTempDir(t)
	path := filepath.Join(root, "note.txt")
	if err := os.WriteFile(path, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	result := runOK(t, OpReadFile, map[string]any{"path": path})
	if result["content"] != "hello\n" {
		t.Errorf("content = %v", result["content"])
	}
	if result["encoding"] != "utf-8" {
		t.Errorf("encoding = %v", result["encoding"])
	}
	if result["size"] != 6 {
		t.Errorf("size = %v", result["size"])
	}
	if result["path"] != path {
		t.Errorf("path = %v, want %v", result["path"], path)
	}
}

func TestReadFileBase64(t *testing.T) {
	root := resolvedTempDir(t)
	path := filepath.Join(root, "blob.bin")
	if err := os.WriteFile(path, []byte{0x00, 0xff, 0x10}, 0o600); err != nil {
		t.Fatal(err)
	}

	result := runOK(t, OpReadFile, map[string]any{"path": path, "encoding": "base64"})
	if result["content"] != "AP8Q" {
		t.Errorf("content = %v, want base64 of 00 ff 10", result["content"])
	}
}

func TestReadFileRejectsDirectory(t *testing.T) {
	root := resolvedTempDir(t)
	runFails(t, OpReadFile, map[string]any{"path": root}, relay.CodeInvalidInput)
}

func TestReadFileRefusesOversizedContent(t *testing.T) {
	root := resolvedTempDir(t)
	path := filepath.Join(root, "big.bin")
	if err := os.WriteFile(path, make([]byte, maxOutputBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	failure := runFails(t, OpReadFile, map[string]any{"path": path}, relay.CodeRemoteError)
	if !strings.Contains(failure.Message, "output limit") {
		t.Errorf("message = %q, want it to name the output limit", failure.Message)
	}
}

func TestWriteFileCreatesAndReplaces(t *testing.T) {
	root := resolvedTempDir(t)
	path := filepath.Join(root, "out", "artifact.txt")

	result := runOK(t, OpWriteFile, map[string]any{
		"path":        path,
		"content":     "first",
		"create_dirs": true,
	})
	if result["created"] != true {
		t.Errorf("created = %v, want true", result["created"])
	}
	if result["bytesWritten"] != 5 {
		t.Errorf("bytesWritten = %v", result["bytesWritten"])
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "first" {
		t.Fatalf("file after write = %q (%v)", data, err)
	}

	result = runOK(t, OpWriteFile, map[string]any{"path": path, "content": "second"})
	if result["created"] != false {
		t.Errorf("created on replace = %v, want false", result["created"])
	}
	data, _ = os.ReadFile(path)
	if string(data) != "second" {
		t.Errorf("file after replace = %q", data)
	}
}

func TestWriteFileBase64(t *testing.T) {
	root := resolvedTempDir(t)
	path := filepath.Join(root, "decoded.bin")
	runOK(t, OpWriteFile, map[string]any{
		"path":     path,
		"content":  "aGk=",
		"encoding": "base64",
	})
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "hi" {
		t.Fatalf("decoded file = %q (%v)", data, err)
	}
}

func TestListDirectory(t *testing.T) {
	root := resolvedTempDir(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b.txt"), []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}

	flat := runOK(t, OpListDirectory, map[string]any{"path": root})
	if flat["count"] != 2 {
		t.Errorf("flat count = %v, want 2", flat["count"])
	}

	all := runOK(t, OpListDirectory, map[string]any{"path": root, "recursive": true})
	if all["count"] != 3 {
		t.Errorf("recursive count = %v, want 3", all["count"])
	}

	filtered := runOK(t, OpListDirectory, map[string]any{
		"path":      root,
		"recursive": true,
		"pattern":   "*.txt",
	})
	if filtered["count"] != 2 {
		t.Errorf("filtered count = %v, want 2", filtered["count"])
	}
}

func TestStat(t *testing.T) {
	root := resolvedTempDir(t)
	path := filepath.Join(root, "stat.txt")
	if err := os.WriteFile(path, []byte("12345"), 0o640); err != nil {
		t.Fatal(err)
	}

	result := runOK(t, OpStat, map[string]any{"path": path})
	if result["type"] != "file" {
		t.Errorf("type = %v", result["type"])
	}
	if result["size"] != int64(5) {
		t.Errorf("size = %v", result["size"])
	}
	if result["mode"] != "-rw-r-----" {
		t.Errorf("mode = %v", result["mode"])
	}

	dir := runOK(t, OpStat, map[string]any{"path": root})
	if dir["type"] != "directory" {
		t.Errorf("directory type = %v", dir["type"])
	}
}

func TestStatMissingPath(t *testing.T) {
	root := resolvedTempDir(t)
	runFails(t, OpStat, map[string]any{"path": filepath.Join(root, "nope")}, relay.CodeInvalidInput)
}

// ---- git -------------------------------------------------------------------

func TestGitStatusCleanThenDirty(t *testing.T) {
	repo := initRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commitAll(t, repo, "initial")

	clean := runOK(t, OpGitStatus, map[string]any{"repo": repo})
	if clean["clean"] != true || clean["count"] != 0 {
		t.Errorf("clean status = %v (count %v)", clean["clean"], clean["count"])
	}
	if clean["branch"] != "main" {
		t.Errorf("branch = %v, want main", clean["branch"])
	}

	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirty := runOK(t, OpGitStatus, map[string]any{"repo": repo})
	if dirty["clean"] != false || dirty["count"] != 1 {
		t.Errorf("dirty status = %v (count %v)", dirty["clean"], dirty["count"])
	}
	entries, ok := dirty["entries"].([]map[string]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("entries = %#v", dirty["entries"])
	}
	if entries[0]["path"] != "tracked.txt" {
		t.Errorf("entry path = %v", entries[0]["path"])
	}
}

func TestGitDiffShowsChange(t *testing.T) {
	repo := initRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commitAll(t, repo, "initial")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	result := runOK(t, OpGitDiff, map[string]any{"repo": repo})
	diff, _ := result["diff"].(string)
	if !strings.Contains(diff, "-one") || !strings.Contains(diff, "+two") {
		t.Errorf("diff = %q, want it to contain -one and +two", diff)
	}

	staged := runOK(t, OpGitDiff, map[string]any{"repo": repo, "staged": true})
	if staged["diff"] != "" {
		t.Errorf("staged diff = %q, want empty before staging", staged["diff"])
	}
}

func TestGitDiffPathSpecIsRepoRelative(t *testing.T) {
	repo := initRepo(t)
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("one\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commitAll(t, repo, "initial")
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("two\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	result := runOK(t, OpGitDiff, map[string]any{"repo": repo, "path": "a.txt"})
	diff, _ := result["diff"].(string)
	if !strings.Contains(diff, "a.txt") || strings.Contains(diff, "b.txt") {
		t.Errorf("pathspec diff = %q, want only a.txt", diff)
	}
}

func TestGitDiffRejectsEscapingPathSpec(t *testing.T) {
	repo := initRepo(t)
	for _, raw := range []string{"/etc/passwd", ":(top)secret", "../outside"} {
		runFails(t, OpGitDiff, map[string]any{"repo": repo, "path": raw}, relay.CodeInvalidInput)
	}
}

func TestGitLogNewestFirst(t *testing.T) {
	repo := initRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commitAll(t, repo, "first commit")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commitAll(t, repo, "second commit")

	result := runOK(t, OpGitLog, map[string]any{"repo": repo, "limit": 1})
	if result["count"] != 1 {
		t.Fatalf("count = %v, want 1", result["count"])
	}
	commits, ok := result["commits"].([]map[string]any)
	if !ok || len(commits) != 1 {
		t.Fatalf("commits = %#v", result["commits"])
	}
	if commits[0]["subject"] != "second commit" {
		t.Errorf("subject = %v, want the newest commit", commits[0]["subject"])
	}
	if commits[0]["author"] != "Relay Test" {
		t.Errorf("author = %v", commits[0]["author"])
	}
}

func TestGitRejectsNonRepository(t *testing.T) {
	root := resolvedTempDir(t)
	runFails(t, OpGitStatus, map[string]any{"repo": root}, relay.CodeInvalidInput)
}

// ---- operation dispatch ----------------------------------------------------

func TestUnknownOperationIsProtocolError(t *testing.T) {
	failure := runFails(t, "docker_run", map[string]any{"path": "/tmp"}, relay.CodeProtocolError)
	available, ok := failure.Details["available"].([]string)
	if !ok || len(available) == 0 {
		t.Errorf("details.available = %#v, want the implemented operations", failure.Details["available"])
	}
}

func TestRequirementsNamesTheCheckedPath(t *testing.T) {
	root := resolvedTempDir(t)
	path := filepath.Join(root, "x.txt")

	read, failure := Requirements(OpReadFile, map[string]any{"path": path})
	if failure != nil {
		t.Fatal(failure.Message)
	}
	if len(read.Reads) != 1 || read.Reads[0] != path || len(read.Writes) != 0 {
		t.Errorf("read requirements = %#v", read)
	}

	write, failure := Requirements(OpWriteFile, map[string]any{"path": path, "content": "x"})
	if failure != nil {
		t.Fatal(failure.Message)
	}
	if len(write.Writes) != 1 || write.Writes[0] != path || len(write.Reads) != 0 {
		t.Errorf("write requirements = %#v", write)
	}

	if _, failure := Requirements("nope", nil); failure == nil || failure.Code != relay.CodeProtocolError {
		t.Errorf("unknown operation = %v", failure)
	}
	if _, failure := Requirements(OpReadFile, map[string]any{"path": "relative/file"}); failure == nil || failure.Code != relay.CodeInvalidInput {
		t.Errorf("relative path = %v", failure)
	}
}

// ---- path resolution -------------------------------------------------------

func TestResolvePathExpandsHome(t *testing.T) {
	home := resolvedTempDir(t)
	t.Setenv("HOME", home)

	resolved, err := ResolvePath("~/notes/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "notes", "a.txt")
	if resolved != want {
		t.Errorf("resolved = %q, want %q", resolved, want)
	}
}

func TestResolvePathRejectsRelative(t *testing.T) {
	if _, err := ResolvePath("notes/a.txt"); err == nil {
		t.Fatal("expected a relative path to be refused")
	}
}

// TestResolvePathResolvesSymlinkToItsTarget is the escape guard's core: a link
// that lives inside a scope but points outside resolves to the outside target,
// so the scope check sees where an operation would really act.
func TestResolvePathResolvesSymlinkToItsTarget(t *testing.T) {
	root := resolvedTempDir(t)
	inside := filepath.Join(root, "inside")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(inside, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	resolved, err := ResolvePath(filepath.Join(link, "secret.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(outside, "secret.txt")
	if resolved != want {
		t.Fatalf("resolved = %q, want the link target %q", resolved, want)
	}
	if strings.HasPrefix(resolved, inside+string(filepath.Separator)) {
		t.Fatalf("resolved path %q still appears to sit inside %q", resolved, inside)
	}
}

func TestResolveScopesDropsUnresolvable(t *testing.T) {
	if got := ResolveScopes([]string{"relative/path"}); len(got) != 0 {
		t.Errorf("ResolveScopes kept an unresolvable scope: %#v", got)
	}
	if got := ResolveScopes(nil); got != nil {
		t.Errorf("ResolveScopes(nil) = %#v, want nil", got)
	}
}

// ---- helpers ---------------------------------------------------------------

// resolvedTempDir returns a temp directory with symlinks resolved, so it can be
// compared against a path the executor resolved. On macOS t.TempDir() lives
// under /var, which is itself a symlink to /private/var.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// gitTestEnv pins the identity and disables host git configuration, so a commit
// made here does not depend on the machine's global config.
func gitTestEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Relay Test",
		"GIT_AUTHOR_EMAIL=relay@example.test",
		"GIT_COMMITTER_NAME=Relay Test",
		"GIT_COMMITTER_EMAIL=relay@example.test",
	)
}

func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := resolvedTempDir(t)
	runGit(t, repo, "init", "-q")
	// Pin the branch name so the test does not depend on git's default.
	runGit(t, repo, "symbolic-ref", "HEAD", "refs/heads/main")
	return repo
}

func commitAll(t *testing.T, repo, message string) {
	t.Helper()
	runGit(t, repo, "add", "-A")
	runGit(t, repo,
		"-c", "user.name=Relay Test",
		"-c", "user.email=relay@example.test",
		"-c", "commit.gpgsign=false",
		"-c", "core.hooksPath=/dev/null",
		"commit", "-q", "-m", message,
	)
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	command.Env = gitTestEnv()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}
