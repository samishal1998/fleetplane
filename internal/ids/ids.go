// Package ids is the single source of truth for entity ID prefixes
// (ADR-004): "<prefix>_<ULID>" — time-ordered, lexicographically sortable,
// self-describing in logs and audit records.
package ids

import (
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"
)

type Prefix string

const (
	Resource    Prefix = "res"
	Pool        Prefix = "pool"
	Acquisition Prefix = "acq"
	Lease       Prefix = "lease"
	Operation   Prefix = "op"
	Event       Prefix = "evt"
	Token       Prefix = "tok"
	Request     Prefix = "req"
)

// New mints a fresh prefixed ID, e.g. "res_01J9ZK...".
func New(p Prefix) string { return string(p) + "_" + ulid.Make().String() }

// Parse splits and validates a prefixed ID.
func Parse(s string) (Prefix, ulid.ULID, error) {
	pre, raw, ok := strings.Cut(s, "_")
	if !ok || pre == "" || raw == "" {
		return "", ulid.ULID{}, fmt.Errorf("malformed id %q", s)
	}
	u, err := ulid.ParseStrict(raw)
	if err != nil {
		return "", ulid.ULID{}, fmt.Errorf("malformed id %q: %w", s, err)
	}
	return Prefix(pre), u, nil
}

// Is reports whether s carries prefix p and a valid ULID.
func Is(s string, p Prefix) bool {
	pre, _, err := Parse(s)
	return err == nil && pre == p
}
