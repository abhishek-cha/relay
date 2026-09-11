package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"relay/internal/fsutil"
	"relay/internal/sign"
	"relay/pkg/relay"
)

// ErrNotFound reports a tool with no registry record.
var ErrNotFound = errors.New("tool is not registered")

// namePattern constrains a tool name to something that is simultaneously a safe
// filename, a safe CLI verb, and a safe MCP namespace segment.
//
// The name comes from a manifest, which means it is attacker-controlled input.
// Without this check a manifest named ../../etc/passwd would let a registry
// write escape its own directory.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidName reports whether a tool name is safe to use as a path segment.
func ValidName(name string) bool {
	return namePattern.MatchString(name) && !strings.Contains(name, "..")
}

// Store is a directory of registry records, one JSON file per tool (spec §15).
type Store struct {
	dir string
}

// New returns a store rooted at dir.
func New(dir string) *Store { return &Store{dir: dir} }

// Dir is the store's directory.
func (s *Store) Dir() string { return s.dir }

// List returns every record, sorted by name.
func (s *Store) List() ([]relay.Installation, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read registry %s: %w", s.dir, err)
	}
	installations := make([]relay.Installation, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".json")
		installation, err := s.Get(name)
		if err != nil {
			return nil, err
		}
		installations = append(installations, installation)
	}
	sort.Slice(installations, func(i, j int) bool {
		return installations[i].Name < installations[j].Name
	})
	return installations, nil
}

// Get returns one record.
func (s *Store) Get(name string) (relay.Installation, error) {
	if !ValidName(name) {
		return relay.Installation{}, fmt.Errorf("invalid tool name %q", name)
	}
	data, err := os.ReadFile(s.path(name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return relay.Installation{}, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return relay.Installation{}, fmt.Errorf("read registry record %s: %w", name, err)
	}
	var installation relay.Installation
	if err := json.Unmarshal(data, &installation); err != nil {
		return relay.Installation{}, fmt.Errorf("registry record %s is corrupt: %w", name, err)
	}
	// The filename is authoritative for identity, so a hand-edited record
	// cannot quietly register itself under a different name.
	installation.Name = name
	return installation, nil
}

// Put writes one record atomically. It records no trust level, which reads back
// as TrustUnknown; callers that know a tool's trust use PutSigned.
func (s *Store) Put(installation relay.Installation) error {
	return s.put(installation, "")
}

// put writes one record and its trust level atomically.
func (s *Store) put(installation relay.Installation, trust sign.TrustLevel) error {
	if !ValidName(installation.Name) {
		return fmt.Errorf("invalid tool name %q", installation.Name)
	}
	record := storedRecord{Installation: installation, TrustLevel: trust}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode registry record: %w", err)
	}
	return fsutil.WriteFile(s.path(installation.Name), append(encoded, '\n'), 0o600)
}

// Remove deletes one record.
func (s *Store) Remove(name string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid tool name %q", name)
	}
	if err := os.Remove(s.path(name)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return fmt.Errorf("remove registry record %s: %w", name, err)
	}
	return nil
}

// Names returns every registered tool name, sorted.
func (s *Store) Names() ([]string, error) {
	installations, err := s.List()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(installations))
	for _, installation := range installations {
		names = append(names, installation.Name)
	}
	return names, nil
}

func (s *Store) path(name string) string {
	return filepath.Join(s.dir, name+".json")
}
