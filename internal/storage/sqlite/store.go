package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/samimishal/fleetplane/internal/storage"
)

// OpenStore opens the database and returns the domain Store.
func OpenStore(ctx context.Context, path string) (storage.Store, error) {
	db, err := Open(ctx, path)
	if err != nil {
		return nil, err
	}
	return &store{db: db, stores: stores{q: db.reader}}, nil
}

// queryer abstracts *sql.DB / *sql.Tx.
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// stores bundles all sub-stores over one queryer. Outside Tx/View the
// queryer is the reader pool (autocommit reads); inside it is the open tx.
type stores struct{ q queryer }

func (s stores) Providers() storage.ProviderInstanceStore { return providerStore(s) }
func (s stores) Resources() storage.ResourceStore         { return resourceStore(s) }
func (s stores) Pools() storage.PoolStore                 { return poolStore(s) }
func (s stores) Acquisitions() storage.AcquisitionStore   { return acquisitionStore(s) }
func (s stores) Leases() storage.LeaseStore               { return leaseStore(s) }
func (s stores) Operations() storage.OperationStore       { return operationStore(s) }
func (s stores) Idempotency() storage.IdempotencyStore    { return idempotencyStore(s) }
func (s stores) Events() storage.EventStore               { return eventStore(s) }
func (s stores) Checkpoints() storage.CheckpointStore     { return checkpointStore(s) }

type store struct {
	db *DB
	stores
}

func (s *store) Tx(ctx context.Context, fn func(storage.TxStore) error) error {
	// The writer pool has exactly one connection and the DSN carries
	// _txlock=immediate, so BeginTx serializes all writers up front and
	// deferred-upgrade deadlocks cannot happen (ADR-003).
	tx, err := s.db.writer.BeginTx(ctx, nil)
	if err != nil {
		return mapErr(err)
	}
	if err := fn(stores{q: tx}); err != nil {
		_ = tx.Rollback()
		return err
	}
	return mapErr(tx.Commit())
}

func (s *store) View(ctx context.Context, fn func(storage.TxStore) error) error {
	tx, err := s.db.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return mapErr(err)
	}
	defer func() { _ = tx.Rollback() }()
	return fn(stores{q: tx})
}

func (s *store) Ping(ctx context.Context) error { return s.db.Ping(ctx) }

// Backup runs VACUUM INTO on a dedicated connection so kernel writes never
// queue behind it (plan R23).
func (s *store) Backup(ctx context.Context, destPath string) error {
	conn, err := open(s.db.path, "deferred", "NORMAL", 1)
	if err != nil {
		return fmt.Errorf("backup connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "VACUUM INTO ?", destPath); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	return nil
}

func (s *store) Close() error { return s.db.Close() }

// mapErr folds driver errors into the storage sentinels.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w", storage.ErrNotFound)
	}
	msg := err.Error()
	if strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "CHECK constraint failed") ||
		strings.Contains(msg, "FOREIGN KEY constraint failed") {
		return fmt.Errorf("%w: %s", storage.ErrConflict, msg)
	}
	return err
}

// Null helpers.
func ns(p *string) sql.NullString {
	if p == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *p, Valid: true}
}
func ni(p *int64) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *p, Valid: true}
}
func nb(b []byte) sql.NullString { // JSON columns: nil => NULL
	if len(b) == 0 {
		return sql.NullString{}
	}
	return sql.NullString{String: string(b), Valid: true}
}
func sp(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}
func ip(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}
func jb(v sql.NullString) []byte {
	if !v.Valid {
		return nil
	}
	return []byte(v.String)
}
