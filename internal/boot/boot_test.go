package boot

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samimishal/fleetplane/internal/config"
)

func startApp(t *testing.T) (*App, context.CancelFunc, <-chan error) {
	t.Helper()
	cfg, err := config.Parse([]byte(fmt.Sprintf(
		"server: { addr: '127.0.0.1:0', opsAddr: '127.0.0.1:0', shutdownGrace: 2s }\nstorage: { path: %s }\n",
		filepath.Join(t.TempDir(), "fp.db"))))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	app, err := New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := app.Listen(); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- app.Serve(ctx) }()
	return app, cancel, done
}

func get(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	code := resp.StatusCode
	_ = resp.Body.Close()
	return code
}

func waitReady(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/health/ready")
		if err == nil {
			code := resp.StatusCode
			_ = resp.Body.Close()
			if code == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server never became ready")
}

func TestServeLifecycle(t *testing.T) {
	app, cancel, done := startApp(t)
	base := "http://" + app.MainAddr().String()
	opsBase := "http://" + app.OpsAddr().String()

	waitReady(t, base)
	if code := get(t, base+"/health/live"); code != http.StatusOK {
		t.Fatalf("live = %d", code)
	}
	// Health is served on the ops listener too (07 §8).
	if code := get(t, opsBase+"/health/ready"); code != http.StatusOK {
		t.Fatalf("ops ready = %d", code)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned error on graceful shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after context cancel")
	}
}

func TestRunFailsFastOnBadConfig(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "fp.yaml")
	if err := os.WriteFile(cfgPath, []byte("storrage: { path: x.db }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), cfgPath, log); err == nil {
		t.Fatal("Run accepted a config with an unknown field")
	}
}
