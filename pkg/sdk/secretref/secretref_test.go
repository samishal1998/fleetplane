package secretref

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvRef(t *testing.T) {
	t.Setenv("FLEETPLANE_TEST_SECRET", "hunter2")
	s, err := NewDefault().Resolve(context.Background(), "secret://env/FLEETPLANE_TEST_SECRET")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(s.Reveal()); got != "hunter2" {
		t.Fatalf("Reveal() = %q, want %q", got, "hunter2")
	}
}

func TestEnvRefUnsetIsError(t *testing.T) {
	_, err := NewDefault().Resolve(context.Background(), "secret://env/FLEETPLANE_TEST_DOES_NOT_EXIST")
	if err == nil {
		t.Fatal("expected error for unset environment variable")
	}
}

func TestFileRefTrimsSingleTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tok")
	if err := os.WriteFile(p, []byte("value\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := "secret://file" + p // p is absolute; ref path omits the leading /
	s, err := NewDefault().Resolve(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	// Only ONE trailing newline is trimmed; inner content is untouched.
	if got := string(s.Reveal()); got != "value\n" {
		t.Fatalf("Reveal() = %q, want %q", got, "value\n")
	}
}

func TestUnknownSchemeIsError(t *testing.T) {
	_, err := NewDefault().Resolve(context.Background(), "secret://vault/kv/prod")
	if err == nil || !strings.Contains(err.Error(), "unknown scheme") {
		t.Fatalf("err = %v, want unknown-scheme error", err)
	}
}

func TestMalformedRefIsError(t *testing.T) {
	for _, ref := range []string{"", "env/NAME", "secret://", "secret://env", "secret://env/"} {
		if _, err := NewDefault().Resolve(context.Background(), ref); err == nil {
			t.Errorf("Resolve(%q) succeeded, want error", ref)
		}
	}
}

func TestSecretRedactedInStringAndSlog(t *testing.T) {
	s := NewSecret([]byte("supersecret"))
	if out := fmt.Sprintf("%v %s", s, s); strings.Contains(out, "supersecret") {
		t.Fatalf("fmt output leaks the secret: %q", out)
	}
	var sb strings.Builder
	slog.New(slog.NewTextHandler(&sb, nil)).Info("boot", "token", s)
	if strings.Contains(sb.String(), "supersecret") {
		t.Fatalf("slog output leaks the secret: %q", sb.String())
	}
}

func TestIsRef(t *testing.T) {
	if !IsRef("secret://env/X") || IsRef("plainvalue") {
		t.Fatal("IsRef misclassifies")
	}
}
