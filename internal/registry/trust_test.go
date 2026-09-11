package registry

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relay/internal/sign"
	"relay/pkg/relay"
)

func TestStoreRecordsTrust(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	installed := relay.Installation{
		Name:        "github",
		Version:     "1.0.0",
		InstalledAt: time.Unix(0, 0).UTC(),
		Runtime:     "relay/v1",
	}
	if err := store.PutSigned(installed, sign.TrustVerified); err != nil {
		t.Fatalf("PutSigned: %v", err)
	}

	level, err := store.Trust("github")
	if err != nil {
		t.Fatalf("Trust: %v", err)
	}
	if level != sign.TrustVerified {
		t.Fatalf("Trust = %q, want %q", level, sign.TrustVerified)
	}

	// The wire record must still read back unchanged: trust is registry-local
	// metadata, not a change to the §16 contract.
	record, err := store.Get("github")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if record.Name != "github" || record.Version != "1.0.0" || record.Runtime != "relay/v1" {
		t.Fatalf("Get returned %+v", record)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "github.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"trustLevel": "verified"`) {
		t.Fatalf("record does not carry the trust level: %s", raw)
	}
}

func TestTrustDefaultsToUnknown(t *testing.T) {
	store := New(t.TempDir())
	if err := store.Put(relay.Installation{Name: "plain", Version: "1.0.0"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	level, err := store.Trust("plain")
	if err != nil {
		t.Fatalf("Trust: %v", err)
	}
	if level != sign.TrustUnknown {
		t.Fatalf("Trust = %q, want %q", level, sign.TrustUnknown)
	}
}

func TestTrustReportsMissingAndInvalidNames(t *testing.T) {
	store := New(t.TempDir())
	if _, err := store.Trust("absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Trust(absent) = %v, want ErrNotFound", err)
	}
	if _, err := store.Trust("../escape"); err == nil {
		t.Fatal("Trust accepted a traversal name")
	}
}
