package sqlite

// The 0004 rebuild is the delicate migration class: resources is the FK
// target of four tables and SQLite cannot alter CHECKs, so it rebuilds the
// table OUTSIDE a transaction with foreign_keys off. An empty-DB test would
// pass trivially — this one upgrades a POPULATED 0003-era database (child
// rows in labels, snapshots, leases, operations) and proves rows, indexes
// and FK integrity survive.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestMigration0004RebuildsPopulatedDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fp.db")

	// Open a raw writer (same DSN shape as production) and migrate only to
	// 0003 (pre-rebuild schema).
	w, err := open(path, "immediate", "FULL", 1)
	if err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect(string(goose.DialectSQLite3)); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(ctx, w, migrationsDir, 3); err != nil {
		t.Fatal(err)
	}

	// Populate: a resource with a label, a snapshot, a lease and an op —
	// every FK edge into resources.
	stmts := []string{
		`INSERT INTO resources (id, kind, provider_instance, ownership, phase, spec_json, created_at, updated_at)
		 VALUES ('res_MIG1', 'compute.machine', 'fake', 'managed', 'ready', '{}', 1, 1)`,
		`INSERT INTO resource_labels (resource_id, k, v) VALUES ('res_MIG1', 'env', 'test')`,
		`INSERT INTO observed_snapshots (resource_id, observed_at) VALUES ('res_MIG1', 1)`,
		`INSERT INTO acquisitions (id, actor, kind, quantity, ttl_seconds, state, created_at, updated_at)
		 VALUES ('acq_MIG1', 't', 'compute.machine', 1, 0, 'bound', 1, 1)`,
		`INSERT INTO leases (id, acquisition_id, resource_id, holder, capacity_json, exclusive, state, created_at)
		 VALUES ('lease_MIG1', 'acq_MIG1', 'res_MIG1', 't', '{}', 1, 'active', 1)`,
		`INSERT INTO operations (id, kind, resource_id, provider_instance, action_json, state, attempt, created_at, updated_at)
		 VALUES ('op_MIG1', 'resource.create', 'res_MIG1', 'fake', '{}', 'succeeded', 1, 1, 1)`,
	}
	for _, q := range stmts {
		if _, err := w.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed %q: %v", q[:40], err)
		}
	}

	// Upgrade to latest (runs the 0004 NO TRANSACTION rebuild).
	if err := goose.UpContext(ctx, w, migrationsDir); err != nil {
		t.Fatalf("0004 rebuild on populated DB: %v", err)
	}

	// Rows survived and the new column exists.
	var phase string
	var parked sql.NullInt64
	if err := w.QueryRowContext(ctx, `SELECT phase, parked_at FROM resources WHERE id='res_MIG1'`).Scan(&phase, &parked); err != nil {
		t.Fatal(err)
	}
	if phase != "ready" || parked.Valid {
		t.Fatalf("row after rebuild: phase=%q parked=%v", phase, parked)
	}
	var n int
	if err := w.QueryRowContext(ctx, `SELECT count(*) FROM resource_labels WHERE resource_id='res_MIG1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("labels after rebuild: %d %v", n, err)
	}
	// FK integrity holds (no dangling references after the rebuild).
	rows, err := w.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		t.Fatal("foreign_key_check reported violations after the rebuild")
	}
	// The new phases are accepted, old CHECK values still valid.
	if _, err := w.ExecContext(ctx,
		`INSERT INTO resources (id, kind, provider_instance, ownership, phase, spec_json, created_at, updated_at)
		 VALUES ('res_MIG2', 'compute.machine', 'fake', 'managed', 'parked', '{}', 2, 2)`); err != nil {
		t.Fatalf("parked phase rejected after rebuild: %v", err)
	}
	// Indexes were recreated: the partial unique index must still fire.
	if _, err := w.ExecContext(ctx, `UPDATE resources SET external_id='x1' WHERE id='res_MIG1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ExecContext(ctx, `UPDATE resources SET external_id='x1' WHERE id='res_MIG2'`); err == nil {
		t.Fatal("ux_res_ext not recreated: duplicate external_id accepted")
	}
	_ = w.Close()

	// And the normal Open path works on the migrated file.
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
}
