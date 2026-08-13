// Package secretref resolves secret:// references into secret values.
//
// Configuration and persisted records only ever hold references
// (secret://env/NAME, secret://file/path); values are resolved at the point
// of use and never logged or persisted (docs/07 §4, ADR-007).
package secretref

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Secret holds a resolved secret value and redacts itself when printed or
// logged. Callers must use Reveal deliberately.
type Secret struct{ v []byte }

func NewSecret(v []byte) Secret { return Secret{v: v} }

func (s Secret) Reveal() []byte { return s.v }

func (s Secret) String() string { return "secret://[redacted]" }

func (s Secret) LogValue() slog.Value { return slog.StringValue("secret://[redacted]") }

// Resolver resolves a secret:// reference to its value.
type Resolver interface {
	Resolve(ctx context.Context, ref string) (Secret, error)
}

// SchemeFunc resolves the path portion of one reference scheme.
type SchemeFunc func(ctx context.Context, path string) ([]byte, error)

// Resolvers are immutable once built; construct with NewDefault or New.
type resolver struct {
	schemes map[string]SchemeFunc
}

// New builds a Resolver from an explicit scheme set.
func New(schemes map[string]SchemeFunc) Resolver {
	cp := make(map[string]SchemeFunc, len(schemes))
	for k, v := range schemes {
		cp[k] = v
	}
	return &resolver{schemes: cp}
}

// NewDefault returns a Resolver supporting secret://env/NAME and
// secret://file/absolute/path (file values are trimmed of one trailing
// newline, matching how secrets are commonly provisioned).
func NewDefault() Resolver {
	return New(map[string]SchemeFunc{
		"env": func(_ context.Context, path string) ([]byte, error) {
			v, ok := os.LookupEnv(path)
			if !ok {
				return nil, fmt.Errorf("environment variable %q is not set", path)
			}
			return []byte(v), nil
		},
		"file": func(_ context.Context, path string) ([]byte, error) {
			b, err := os.ReadFile("/" + path)
			if err != nil {
				return nil, err
			}
			return trimOneTrailingNewline(b), nil
		},
	})
}

const prefix = "secret://"

func (r *resolver) Resolve(ctx context.Context, ref string) (Secret, error) {
	scheme, path, err := Split(ref)
	if err != nil {
		return Secret{}, err
	}
	fn, ok := r.schemes[scheme]
	if !ok {
		return Secret{}, fmt.Errorf("secret reference %q: unknown scheme %q", ref, scheme)
	}
	v, err := fn(ctx, path)
	if err != nil {
		return Secret{}, fmt.Errorf("resolving secret reference %q: %w", ref, err)
	}
	return Secret{v: v}, nil
}

// Split parses "secret://<scheme>/<path>" into its scheme and path.
func Split(ref string) (scheme, path string, err error) {
	if !strings.HasPrefix(ref, prefix) {
		return "", "", fmt.Errorf("secret reference %q must start with %s", ref, prefix)
	}
	rest := strings.TrimPrefix(ref, prefix)
	scheme, path, ok := strings.Cut(rest, "/")
	if !ok || scheme == "" || path == "" {
		return "", "", fmt.Errorf("secret reference %q must have the form %s<scheme>/<path>", ref, prefix)
	}
	return scheme, path, nil
}

// IsRef reports whether s looks like a secret:// reference.
func IsRef(s string) bool { return strings.HasPrefix(s, prefix) }

func trimOneTrailingNewline(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
		if n := len(b); n > 0 && b[n-1] == '\r' {
			b = b[:n-1]
		}
	}
	return b
}
