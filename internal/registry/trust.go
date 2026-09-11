package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"relay/internal/sign"
	"relay/pkg/relay"
)

// storedRecord is the on-disk shape of a registry record: the wire
// relay.Installation plus registry-local metadata that the §16 wire type
// deliberately does not carry.
//
// Embedding Installation anonymously flattens its fields, so the file still
// reads as a plain installation record and the added trust key is ignored by any
// reader that only knows the wire type.
type storedRecord struct {
	relay.Installation
	TrustLevel sign.TrustLevel `json:"trustLevel,omitempty"`
}

// PutSigned writes a record together with the tool's trust level (spec §49).
// The install path uses this so a tool's standing is part of its registry
// record rather than recomputed by every reader.
func (s *Store) PutSigned(installation relay.Installation, trust sign.TrustLevel) error {
	return s.put(installation, trust)
}

// Trust returns a record's recorded trust level. A record written before trust
// levels existed, or by plain Put, reads back as TrustUnknown.
func (s *Store) Trust(name string) (sign.TrustLevel, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("invalid tool name %q", name)
	}
	data, err := os.ReadFile(s.path(name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return "", fmt.Errorf("read registry record %s: %w", name, err)
	}
	var record storedRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return "", fmt.Errorf("registry record %s is corrupt: %w", name, err)
	}
	if record.TrustLevel == "" {
		return sign.TrustUnknown, nil
	}
	return record.TrustLevel, nil
}
