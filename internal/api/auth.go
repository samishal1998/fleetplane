package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// Permission strings (07 §3).
type Permission string

const (
	PermResourceRead    Permission = "resource.read"
	PermResourceAcquire Permission = "resource.acquire"
	PermResourceCreate  Permission = "resource.create"
	PermResourceDelete  Permission = "resource.delete"
	PermPoolRead        Permission = "pool.read"
	PermPoolWrite       Permission = "pool.write"
	PermClassRead       Permission = "class.read"
	PermClassWrite      Permission = "class.write"
	PermProviderRead    Permission = "provider.read"
	PermProviderAdmin   Permission = "provider.admin"
	PermOperationRead   Permission = "operation.read"
	PermAdmin           Permission = "admin"
)

// AllPermissions is the closed set config validation checks against.
var AllPermissions = []Permission{
	PermResourceRead, PermResourceAcquire, PermResourceCreate, PermResourceDelete,
	PermPoolRead, PermPoolWrite, PermClassRead, PermClassWrite, PermProviderRead, PermProviderAdmin,
	PermOperationRead, PermAdmin,
}

type PermSet map[Permission]struct{}

func NewPermSet(perms []string) (PermSet, error) {
	s := PermSet{}
	for _, p := range perms {
		valid := false
		for _, known := range AllPermissions {
			if Permission(p) == known {
				valid = true
				break
			}
		}
		if !valid {
			return nil, fmt.Errorf("unknown permission %q", p)
		}
		s[Permission(p)] = struct{}{}
	}
	return s, nil
}

// Allows: p is present, or the principal holds admin.
func (s PermSet) Allows(p Permission) bool {
	if _, ok := s[PermAdmin]; ok {
		return true
	}
	_, ok := s[p]
	return ok
}

// Principal is an authenticated caller.
type Principal struct {
	TokenID string
	Name    string
	Perms   PermSet
}

// TokenRecord is one configured token: only the SHA-256 of the secret is
// ever stored (ADR-008 — 256-bit random secrets need no KDF).
type TokenRecord struct {
	ID     string
	Name   string
	SHA256 [32]byte
	Perms  PermSet
}

type TokenAuthenticator struct {
	byID map[string]TokenRecord
	// dummy keeps timing uniform for unknown token IDs.
	dummy [32]byte
}

func NewTokenAuthenticator(records []TokenRecord) *TokenAuthenticator {
	a := &TokenAuthenticator{byID: map[string]TokenRecord{}}
	for _, r := range records {
		a.byID[r.ID] = r
	}
	_, _ = rand.Read(a.dummy[:])
	return a
}

// Enabled reports whether any tokens are configured. With none, the API
// runs OPEN — for local development only; boot logs a loud warning.
func (a *TokenAuthenticator) Enabled() bool { return len(a.byID) > 0 }

// Authenticate parses "Bearer flp_<id8>.<secret>".
func (a *TokenAuthenticator) Authenticate(authHeader string) (Principal, error) {
	raw, ok := strings.CutPrefix(authHeader, "Bearer ")
	if !ok {
		return Principal{}, fmt.Errorf("missing bearer token")
	}
	body, ok := strings.CutPrefix(strings.TrimSpace(raw), "flp_")
	if !ok {
		return Principal{}, fmt.Errorf("malformed token")
	}
	id, secret, ok := strings.Cut(body, ".")
	if !ok {
		return Principal{}, fmt.Errorf("malformed token")
	}
	rec, found := a.byID[id]
	want := rec.SHA256
	if !found {
		want = a.dummy // constant-time path even for unknown IDs
	}
	got := sha256.Sum256([]byte(secret))
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 || !found {
		return Principal{}, fmt.Errorf("invalid token")
	}
	return Principal{TokenID: rec.ID, Name: rec.Name, Perms: rec.Perms}, nil
}

// ParseToken derives the config record (id + digest) from a plaintext
// token, so a pre-generated token can be installed into a config without
// the secret ever being stored.
func ParseToken(plaintext, name string, perms PermSet) (TokenRecord, error) {
	body, ok := strings.CutPrefix(strings.TrimSpace(plaintext), "flp_")
	if !ok {
		return TokenRecord{}, fmt.Errorf("malformed token: want flp_<id>.<secret>")
	}
	id, secret, ok := strings.Cut(body, ".")
	if !ok || len(id) != 8 || secret == "" {
		return TokenRecord{}, fmt.Errorf("malformed token: want flp_<id8>.<secret>")
	}
	return TokenRecord{ID: id, Name: name, SHA256: sha256.Sum256([]byte(secret)), Perms: perms}, nil
}

// GenerateToken mints a fresh token: the plaintext (shown once) and the
// record for the config file.
func GenerateToken(name string, perms PermSet) (plaintext string, rec TokenRecord, err error) {
	idb := make([]byte, 4)
	if _, err = rand.Read(idb); err != nil {
		return "", TokenRecord{}, err
	}
	secretB := make([]byte, 32)
	if _, err = rand.Read(secretB); err != nil {
		return "", TokenRecord{}, err
	}
	id := hex.EncodeToString(idb)
	secret := base64.RawURLEncoding.EncodeToString(secretB)
	rec = TokenRecord{ID: id, Name: name, SHA256: sha256.Sum256([]byte(secret)), Perms: perms}
	return "flp_" + id + "." + secret, rec, nil
}

type principalKey struct{}

func principalOf(r *http.Request) Principal {
	if p, ok := r.Context().Value(principalKey{}).(Principal); ok {
		return p
	}
	return Principal{Name: "-"}
}
