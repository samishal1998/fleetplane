package sqlite

import (
	"context"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fp.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func TestOpenAppliesWALAndPragmas(t *testing.T) {
	db, _ := openTemp(t)
	var mode string
	if err := db.Writer().QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("writer journal_mode = %q, want wal", mode)
	}
	if err := db.Reader().QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("reader journal_mode = %q, want wal", mode)
	}
	var fk int
	if err := db.Writer().QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys = %d, want 1", fk)
	}
}

func TestMigrationsCreateBaseline(t *testing.T) {
	db, _ := openTemp(t)
	var n int
	err := db.Reader().QueryRowContext(context.Background(),
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='controller_checkpoints'").Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("controller_checkpoints table missing after migration")
	}
}

func TestReopenIsIdempotent(t *testing.T) {
	db, path := openTemp(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = db2.Close()
}

func TestRefusesNewerSchemaVersion(t *testing.T) {
	db, path := openTemp(t)
	// Simulate a database migrated by a future binary.
	_, err := db.Writer().ExecContext(context.Background(),
		"INSERT INTO goose_db_version (version_id, is_applied, tstamp) VALUES (9999, 1, datetime('now'))")
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := Open(context.Background(), path); err == nil {
		t.Fatal("opened a database with a newer schema version; downgrade guard failed (ADR-010)")
	}
}
