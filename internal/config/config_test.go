package config

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultsApplied(t *testing.T) {
	cfg, err := Parse([]byte("storage: { path: /tmp/fp.db }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != ":8080" || cfg.Server.OpsAddr != "127.0.0.1:9090" {
		t.Fatalf("defaults not applied: %+v", cfg.Server)
	}
	if cfg.Server.ShutdownGrace.Std() != 20*time.Second {
		t.Fatalf("shutdownGrace default = %v", cfg.Server.ShutdownGrace.Std())
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	_, err := Parse([]byte("storage: { path: /tmp/fp.db }\nserverr: { addr: ':1' }\n"))
	if err == nil {
		t.Fatal("unknown field accepted; strict decoding is a boot requirement (ADR-007)")
	}
	if !strings.Contains(err.Error(), "serverr") {
		t.Fatalf("error does not name the unknown field: %v", err)
	}
}

func TestMissingStoragePathRejected(t *testing.T) {
	_, err := Parse([]byte("server: { addr: ':0' }\n"))
	if err == nil || !strings.Contains(err.Error(), "storage.path") {
		t.Fatalf("err = %v, want storage.path requirement", err)
	}
}

func TestDurationParsing(t *testing.T) {
	cfg, err := Parse([]byte("storage: { path: x.db }\nserver: { shutdownGrace: 1m30s }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.ShutdownGrace.Std() != 90*time.Second {
		t.Fatalf("shutdownGrace = %v, want 90s", cfg.Server.ShutdownGrace.Std())
	}
	if _, err := Parse([]byte("storage: { path: x.db }\nserver: { shutdownGrace: soon }\n")); err == nil {
		t.Fatal("invalid duration accepted")
	}
}
