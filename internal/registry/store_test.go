package registry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"relay/pkg/relay"
)

func TestValidName(t *testing.T) {
	valid := []string{"github", "my-tool", "tool.v2", "a", "a1_b2"}
	for _, name := range valid {
		if !ValidName(name) {
			t.Errorf("ValidName(%q) = false, want true", name)
		}
	}
	// Every one of these would let a manifest write outside the registry
	// directory if it were accepted.
	invalid := []string{"", "..", "../escape", "a/../b", "a/b", ".hidden", "Upper", "a b", "a\b"}
	for _, name := range invalid {
		if ValidName(name) {
			t.Errorf("ValidName(%q) = true, want false", name)
		}
	}
}

func TestStoreRoundTrip(t *testing.T) {
	store := New(t.TempDir())
	installed := relay.Installation{
		Name:        "github",
		Version:     "1.0.0",
		Path:        "/tmp/github",
		InstalledAt: time.Unix(0, 0).UTC(),
		Runtime:     "relay/v1",
		Operations:  []string{"get_repository"},
	}
	if err := store.Put(installed); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := store.Get("github")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "github" || got.Version != "1.0.0" || got.Runtime != "relay/v1" {
		t.Fatalf("Get returned %+v", got)
	}

	names, err := store.Names()
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	if len(names) != 1 || names[0] != "github" {
		t.Fatalf("Names = %v", names)
	}

	if err := store.Remove("github"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := store.Get("github"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Remove: %v, want ErrNotFound", err)
	}
}

func TestStoreRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	store := New(filepath.Join(root, "registry"))

	if err := store.Put(relay.Installation{Name: "../escaped"}); err == nil {
		t.Fatal("Put accepted a traversal name")
	}
	if _, err := store.Get("../../etc/passwd"); err == nil {
		t.Fatal("Get accepted a traversal name")
	}
	if _, err := os.Stat(filepath.Join(root, "escaped.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file escaped the registry directory: %v", err)
	}
}

func TestStoreReportsCorruptRecord(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("broken"); err == nil {
		t.Fatal("expected an error for a corrupt record")
	}
}

func TestStoreFilenameWinsOverContents(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	record, err := json.Marshal(relay.Installation{Name: "impostor", Version: "9.9.9"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "github.json"), record, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get("github")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "github" {
		t.Fatalf("Name = %q, want github", got.Name)
	}
}

func TestStoreListOnMissingDirectory(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "absent"))
	installations, err := store.List()
	if err != nil {
		t.Fatalf("List on a missing directory: %v", err)
	}
	if len(installations) != 0 {
		t.Fatalf("List = %v, want empty", installations)
	}
}
