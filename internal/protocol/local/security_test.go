package local

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"relay/internal/manifest"
	"relay/internal/permissions"
	"relay/pkg/relay"
)

// This file is the security half of the local executor (spec §24, §40): it
// proves that a path an operation names is refused unless the manifest declared
// a scope that contains it, and that a local tool which declares no filesystem
// capability is refused outright.
//
// The chain here is the daemon's: derive the concrete path from the operation
// input with [Requirements], project the manifest into a permissions.Policy,
// canonicalize the declared scopes with [ResolveScopes], and classify the two
// with permissions.Check. internal/daemon.permissionInvocation and
// internal/daemon.permissionCheck run exactly these steps before an executor is
// called, so a refusal proven here is a refusal at the daemon boundary.

// localManifestTemplate is a valid local manifest whose single read scope and
// write scope are substituted in. Its two operations name the primitive in
// request.operation, the shape the M10 validator requires.
const localManifestTemplate = `apiVersion: relay/v1
kind: Tool
metadata:
  name: filesystem
  version: 1.0.0
  description: Local filesystem capabilities
protocol:
  type: local
capabilities:
  - filesystem.read
  - filesystem.write
permissions:
  filesystem:
    read:
      - '%s'
    write:
      - '%s'
tools:
  - name: read_file
    description: Read a file
    input:
      type: object
      properties:
        path:
          type: string
      required:
        - path
    request:
      operation: read_file
  - name: write_file
    description: Write a file
    input:
      type: object
      properties:
        path:
          type: string
        content:
          type: string
      required:
        - path
        - content
    request:
      operation: write_file
`

func localDoc(t *testing.T, readScope, writeScope string) *manifest.Document {
	t.Helper()
	doc, err := manifest.Parse([]byte(fmt.Sprintf(localManifestTemplate, readScope, writeScope)))
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("local manifest must validate: %v", err)
	}
	return doc
}

// scopedPolicy mirrors the daemon: project the manifest, then canonicalize its
// scopes by the same rule the invocation's path is resolved with.
func scopedPolicy(doc *manifest.Document) permissions.Policy {
	policy := permissions.PolicyFromManifest(doc, nil)
	policy.Filesystem.Read = ResolveScopes(policy.Filesystem.Read)
	policy.Filesystem.Write = ResolveScopes(policy.Filesystem.Write)
	return policy
}

// invocationFor mirrors internal/daemon.permissionInvocation for a local tool.
func invocationFor(t *testing.T, doc *manifest.Document, operation string, input map[string]any) permissions.Invocation {
	t.Helper()
	tool := doc.Operation(operation)
	if tool == nil {
		t.Fatalf("manifest has no operation %q", operation)
	}
	requirement, failure := Requirements(tool.Request.Operation, input)
	if failure != nil {
		t.Fatalf("Requirements(%s): %s", tool.Request.Operation, failure.Message)
	}
	return permissions.Invocation{Operation: operation, Reads: requirement.Reads, Writes: requirement.Writes}
}

func TestPathInsideDeclaredScopeIsAllowed(t *testing.T) {
	root := resolvedTempDir(t)
	scope := filepath.Join(root, "scope")
	if err := os.MkdirAll(scope, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := localDoc(t, scope, scope)

	failure := permissions.Check(scopedPolicy(doc), invocationFor(t, doc, OpReadFile,
		map[string]any{"path": filepath.Join(scope, "note.txt")}))
	if failure != nil {
		t.Fatalf("a path inside the declared scope must be allowed, got %s: %s", failure.Code, failure.Message)
	}
}

func TestPathOutsideDeclaredScopeIsDenied(t *testing.T) {
	root := resolvedTempDir(t)
	scope := filepath.Join(root, "scope")
	outside := filepath.Join(root, "outside")
	for _, dir := range []string{scope, outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	doc := localDoc(t, scope, scope)

	failure := permissions.Check(scopedPolicy(doc), invocationFor(t, doc, OpReadFile,
		map[string]any{"path": filepath.Join(outside, "secret.txt")}))
	if failure == nil {
		t.Fatal("a path outside the declared scope must be denied")
	}
	if failure.Code != relay.CodePermissionDenied {
		t.Fatalf("code = %s, want %s", failure.Code, relay.CodePermissionDenied)
	}
	if failure.Details["capability"] != permissions.CapabilityFilesystemRead {
		t.Errorf("capability = %v, want %s", failure.Details["capability"], permissions.CapabilityFilesystemRead)
	}
	if failure.Details["path"] != filepath.Join(outside, "secret.txt") {
		t.Errorf("denied path = %v, want the requested path", failure.Details["path"])
	}
}

// TestSymlinkEscapeIsDenied is why the target is resolved before it is checked:
// the link sits inside the declared scope, but the operation would read its
// target outside, so the resolved path fails the scope check.
func TestSymlinkEscapeIsDenied(t *testing.T) {
	root := resolvedTempDir(t)
	scope := filepath.Join(root, "scope")
	outside := filepath.Join(root, "outside")
	for _, dir := range []string{scope, outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(scope, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	doc := localDoc(t, scope, scope)

	failure := permissions.Check(scopedPolicy(doc), invocationFor(t, doc, OpReadFile,
		map[string]any{"path": filepath.Join(link, "secret.txt")}))
	if failure == nil {
		t.Fatal("a symlink pointing outside the declared scope must be denied")
	}
	if failure.Code != relay.CodePermissionDenied {
		t.Fatalf("code = %s, want %s", failure.Code, relay.CodePermissionDenied)
	}
}

// TestUndeclaredCapabilityIsRefused proves default-deny: a local tool that does
// not declare filesystem.read is refused even though it declared a read scope.
func TestUndeclaredCapabilityIsRefused(t *testing.T) {
	root := resolvedTempDir(t)
	scope := filepath.Join(root, "scope")
	if err := os.MkdirAll(scope, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := localDoc(t, scope, scope)
	doc.Capabilities = nil

	failure := permissions.Check(scopedPolicy(doc), invocationFor(t, doc, OpReadFile,
		map[string]any{"path": filepath.Join(scope, "note.txt")}))
	if failure == nil {
		t.Fatal("a local operation whose capability is undeclared must be refused")
	}
	if failure.Code != relay.CodePermissionDenied {
		t.Fatalf("code = %s, want %s", failure.Code, relay.CodePermissionDenied)
	}
	if failure.Details["capability"] != permissions.CapabilityFilesystemRead {
		t.Errorf("capability = %v, want %s", failure.Details["capability"], permissions.CapabilityFilesystemRead)
	}
}

// TestDeclaredCapabilityWithoutScopeIsRefused proves the scope list is the
// binding boundary: declaring filesystem.write but no write scope denies a
// write, because an empty scope list allows nothing.
func TestDeclaredCapabilityWithoutScopeIsRefused(t *testing.T) {
	root := resolvedTempDir(t)
	scope := filepath.Join(root, "scope")
	if err := os.MkdirAll(scope, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := localDoc(t, scope, "")

	failure := permissions.Check(scopedPolicy(doc), invocationFor(t, doc, OpWriteFile,
		map[string]any{"path": filepath.Join(scope, "note.txt"), "content": "x"}))
	if failure == nil {
		t.Fatal("a write with no declared write scope must be refused")
	}
	if failure.Code != relay.CodePermissionDenied {
		t.Fatalf("code = %s, want %s", failure.Code, relay.CodePermissionDenied)
	}
}

// TestWriteToReadScopeIsRefused proves read and write scopes are separate: a
// path that is readable is not therefore writable.
func TestWriteToReadScopeIsRefused(t *testing.T) {
	root := resolvedTempDir(t)
	readScope := filepath.Join(root, "read")
	writeScope := filepath.Join(root, "write")
	for _, dir := range []string{readScope, writeScope} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	doc := localDoc(t, readScope, writeScope)

	failure := permissions.Check(scopedPolicy(doc), invocationFor(t, doc, OpWriteFile,
		map[string]any{"path": filepath.Join(readScope, "note.txt"), "content": "x"}))
	if failure == nil {
		t.Fatal("writing to a read-only scope must be refused")
	}
	if failure.Details["capability"] != permissions.CapabilityFilesystemWrite {
		t.Errorf("capability = %v, want %s", failure.Details["capability"], permissions.CapabilityFilesystemWrite)
	}
}

// TestUndeclaredPrimitiveIsRefused proves the operation surface is the build's,
// not the manifest's: a manifest naming a primitive this build does not
// implement cannot be classified, so [Requirements] refuses it rather than
// letting it run unchecked.
func TestUndeclaredPrimitiveIsRefused(t *testing.T) {
	root := resolvedTempDir(t)
	scope := filepath.Join(root, "scope")
	doc := localDoc(t, scope, scope)
	tool := doc.Operation(OpReadFile)
	tool.Request.Operation = "delete_file"

	_, failure := Requirements(tool.Request.Operation, map[string]any{"path": filepath.Join(scope, "note.txt")})
	if failure == nil {
		t.Fatal("an unimplemented primitive must be refused")
	}
	if failure.Code != relay.CodeProtocolError {
		t.Fatalf("code = %s, want %s", failure.Code, relay.CodeProtocolError)
	}
}
