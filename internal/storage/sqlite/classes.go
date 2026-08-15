package sqlite

import (
	"context"
	"database/sql"

	"github.com/samishal1998/fleetplane/internal/storage"
)

type classStore stores

const classCols = `name, kind, provider, spec_json, reclaim_idle_after_ms, reclaim_park, reclaim_delete_after_ms, queue_max_wait_ms, source, created_at, updated_at`

func (cs classStore) Upsert(ctx context.Context, c *storage.ClassRecord) error {
	_, err := cs.q.ExecContext(ctx, `INSERT INTO classes (`+classCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET
			kind=excluded.kind, provider=excluded.provider, spec_json=excluded.spec_json,
			reclaim_idle_after_ms=excluded.reclaim_idle_after_ms,
			reclaim_park=excluded.reclaim_park,
			reclaim_delete_after_ms=excluded.reclaim_delete_after_ms,
			queue_max_wait_ms=excluded.queue_max_wait_ms,
			source=excluded.source, updated_at=excluded.updated_at`,
		c.Name, c.Kind, string(c.Provider), string(c.Spec),
		c.ReclaimIdleAfterMs, nsStr(c.ReclaimPark), c.ReclaimDeleteAfterMs, c.QueueMaxWaitMs, c.Source, c.CreatedAt, c.UpdatedAt)
	return mapErr(err)
}

func (cs classStore) Get(ctx context.Context, name string) (*storage.ClassRecord, error) {
	return scanClass(cs.q.QueryRowContext(ctx, `SELECT `+classCols+` FROM classes WHERE name=?`, name))
}

func (cs classStore) List(ctx context.Context) ([]*storage.ClassRecord, error) {
	return cs.list(ctx, `SELECT `+classCols+` FROM classes ORDER BY name`)
}

func (cs classStore) ListBySource(ctx context.Context, source string) ([]*storage.ClassRecord, error) {
	return cs.list(ctx, `SELECT `+classCols+` FROM classes WHERE source=? ORDER BY name`, source)
}

func (cs classStore) list(ctx context.Context, query string, args ...any) ([]*storage.ClassRecord, error) {
	rows, err := cs.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*storage.ClassRecord
	for rows.Next() {
		c, err := scanClass(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (cs classStore) Delete(ctx context.Context, name string) error {
	res, err := cs.q.ExecContext(ctx, `DELETE FROM classes WHERE name=?`, name)
	if err != nil {
		return mapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

func scanClass(row rowScanner) (*storage.ClassRecord, error) {
	var c storage.ClassRecord
	var provider, spec string
	var park sql.NullString
	if err := row.Scan(&c.Name, &c.Kind, &provider, &spec,
		&c.ReclaimIdleAfterMs, &park, &c.ReclaimDeleteAfterMs, &c.QueueMaxWaitMs, &c.Source, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	c.Provider = storage.ProviderInstance(provider)
	c.Spec = []byte(spec)
	c.ReclaimPark = park.String // NULL and "" both canonicalize to auto
	return &c, nil
}
