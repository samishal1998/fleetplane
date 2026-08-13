// Package sqlite owns the SQLite connections and schema migrations
// (ADR-003, ADR-010). Higher layers see only the domain Store interfaces in
// internal/storage; nothing above this package touches database/sql.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"strconv"
	"strings"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const migrationsDir = "migrations"

// DB holds the two connection pools of ADR-003: a single-connection writer
// (BEGIN IMMEDIATE, synchronous=FULL — the operation journal must survive
// power loss) and a reader pool for concurrent snapshot reads.
type DB struct {
	writer *sql.DB
	reader *sql.DB
}

// Open opens (creating if needed) the database, applies the connection
// pragmas via DSN, refuses databases newer than this binary knows, and runs
// pending migrations.
func Open(ctx context.Context, path string) (*DB, error) {
	w, err := open(path, "immediate", "FULL", 1)
	if err != nil {
		return nil, fmt.Errorf("open writer: %w", err)
	}
	if err := migrate(ctx, w); err != nil {
		_ = w.Close()
		return nil, err
	}
	r, err := open(path, "deferred", "NORMAL", 4)
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("open reader: %w", err)
	}
	db := &DB{writer: w, reader: r}
	if err := db.Ping(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func open(path, txlock, synchronous string, conns int) (*sql.DB, error) {
	dsn := "file:" + path + "?_txlock=" + txlock +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=temp_store(MEMORY)" +
		"&_pragma=journal_size_limit(67108864)" +
		"&_pragma=synchronous(" + url.QueryEscape(synchronous) + ")"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(conns)
	db.SetMaxIdleConns(conns)
	db.SetConnMaxLifetime(0)
	return db, nil
}

// Writer returns the single-connection write handle. All mutating
// transactions run here (BEGIN IMMEDIATE via the DSN _txlock).
func (d *DB) Writer() *sql.DB { return d.writer }

// Reader returns the read pool (WAL snapshot reads).
func (d *DB) Reader() *sql.DB { return d.reader }

func (d *DB) Ping(ctx context.Context) error {
	if err := d.writer.PingContext(ctx); err != nil {
		return fmt.Errorf("writer ping: %w", err)
	}
	if err := d.reader.PingContext(ctx); err != nil {
		return fmt.Errorf("reader ping: %w", err)
	}
	return nil
}

func (d *DB) Close() error {
	var errs []error
	if err := d.writer.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := d.reader.Close(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("close: %v", errs)
	}
	return nil
}

// migrate refuses schemas newer than the binary (downgrade guard, ADR-010),
// then applies pending migrations. Each .sql file runs in its own
// transaction (goose default); pragmas never appear in migrations.
func migrate(ctx context.Context, db *sql.DB) error {
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect(string(goose.DialectSQLite3)); err != nil {
		return err
	}
	max, err := maxMigrationVersion()
	if err != nil {
		return err
	}
	current, err := goose.EnsureDBVersionContext(ctx, db)
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	if current > max {
		return fmt.Errorf("database schema version %d is newer than this binary's max %d: refusing to start (downgrade guard, ADR-010)", current, max)
	}
	if err := goose.UpContext(ctx, db, migrationsDir); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	return nil
}

func maxMigrationVersion() (int64, error) {
	entries, err := fs.ReadDir(migrationsFS, migrationsDir)
	if err != nil {
		return 0, err
	}
	var max int64
	for _, e := range entries {
		name := e.Name()
		num, _, ok := strings.Cut(name, "_")
		if !ok {
			continue
		}
		v, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("migration filename %q: %w", name, err)
		}
		if v > max {
			max = v
		}
	}
	if max == 0 {
		return 0, fmt.Errorf("no embedded migrations found")
	}
	return max, nil
}
