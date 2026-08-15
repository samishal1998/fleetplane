package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/samishal1998/fleetplane/internal/ids"
	"github.com/samishal1998/fleetplane/internal/storage"
)

// --- pools ---

type poolStore struct{ q queryer }

const poolCols = `id, name, kind, spec_json, generation, observed_generation, paused, created_at, updated_at`

func (ps poolStore) Upsert(ctx context.Context, p *storage.Pool) error {
	_, err := ps.q.ExecContext(ctx, `INSERT INTO pools (`+poolCols+`) VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, kind=excluded.kind,
		  spec_json=excluded.spec_json, generation=pools.generation+1,
		  paused=excluded.paused, updated_at=excluded.updated_at`,
		string(p.ID), p.Name, p.Kind, string(p.Spec), p.Generation, p.ObservedGeneration,
		boolInt(p.Paused), p.CreatedAt, p.UpdatedAt)
	return mapErr(err)
}

func (ps poolStore) Get(ctx context.Context, id storage.PoolID) (*storage.Pool, error) {
	return scanPool(ps.q.QueryRowContext(ctx, `SELECT `+poolCols+` FROM pools WHERE id=?`, string(id)))
}

func (ps poolStore) GetByName(ctx context.Context, name string) (*storage.Pool, error) {
	return scanPool(ps.q.QueryRowContext(ctx, `SELECT `+poolCols+` FROM pools WHERE name=?`, name))
}

func (ps poolStore) List(ctx context.Context) ([]*storage.Pool, error) {
	rows, err := ps.q.QueryContext(ctx, `SELECT `+poolCols+` FROM pools ORDER BY id`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*storage.Pool
	for rows.Next() {
		p, err := scanPool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func scanPool(row rowScanner) (*storage.Pool, error) {
	var p storage.Pool
	var spec string
	var paused int
	err := row.Scan((*string)(&p.ID), &p.Name, &p.Kind, &spec, &p.Generation,
		&p.ObservedGeneration, &paused, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	p.Spec = []byte(spec)
	p.Paused = paused == 1
	return &p, nil
}

// --- events ---

type eventStore struct{ q queryer }

func (es eventStore) Append(ctx context.Context, ev *storage.Event) error {
	if ev.ID == "" {
		ev.ID = storage.EventID(ids.New(ids.Event))
	}
	_, err := es.q.ExecContext(ctx, `INSERT INTO events
		(id, ts, actor, request_id, idem_key, type, resource_id, pool_id, operation_id,
		 provider_instance, intent_json, plan_json, provider_request_id, outcome, details_json)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		string(ev.ID), ev.TS, nsStr(ev.Actor), nsStr(ev.RequestID), nsStr(ev.IdemKey), ev.Type,
		nullID(ev.ResourceID), nullID(ev.PoolID), nullID(ev.OperationID),
		nsStr(string(ev.Provider)), nb(ev.Intent), nb(ev.Plan),
		nsStr(ev.ProviderRequestID), nsStr(ev.Outcome), nb(ev.Details))
	return mapErr(err)
}

func (es eventStore) List(ctx context.Context, f storage.EventFilter, limit int) ([]*storage.Event, error) {
	where := []string{"1=1"}
	var args []any
	if f.After != "" {
		where, args = append(where, "id > ?"), append(args, string(f.After))
	}
	if f.SinceTS > 0 {
		where, args = append(where, "ts >= ?"), append(args, f.SinceTS)
	}
	if f.Type != "" {
		where, args = append(where, "type = ?"), append(args, f.Type)
	}
	if f.ResourceID != nil {
		where, args = append(where, "resource_id = ?"), append(args, string(*f.ResourceID))
	}
	if f.PoolID != nil {
		where, args = append(where, "pool_id = ?"), append(args, string(*f.PoolID))
	}
	args = append(args, limit)
	rows, err := es.q.QueryContext(ctx, `SELECT id, ts, actor, request_id, idem_key, type,
		resource_id, pool_id, operation_id, provider_instance, intent_json, plan_json,
		provider_request_id, outcome, details_json
		FROM events WHERE `+strings.Join(where, " AND ")+` ORDER BY id LIMIT ?`, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*storage.Event
	for rows.Next() {
		var ev storage.Event
		var actor, reqID, idemKey, resID, poolID, opID, prov, intent, plan, provReqID, outcome, details sql.NullString
		if err := rows.Scan((*string)(&ev.ID), &ev.TS, &actor, &reqID, &idemKey, &ev.Type,
			&resID, &poolID, &opID, &prov, &intent, &plan, &provReqID, &outcome, &details); err != nil {
			return nil, err
		}
		ev.Actor, ev.RequestID, ev.IdemKey = actor.String, reqID.String, idemKey.String
		if resID.Valid {
			r := storage.ResourceID(resID.String)
			ev.ResourceID = &r
		}
		if poolID.Valid {
			p := storage.PoolID(poolID.String)
			ev.PoolID = &p
		}
		if opID.Valid {
			o := storage.OperationID(opID.String)
			ev.OperationID = &o
		}
		ev.Provider = storage.ProviderInstance(prov.String)
		ev.Intent, ev.Plan, ev.Details = jb(intent), jb(plan), jb(details)
		ev.ProviderRequestID, ev.Outcome = provReqID.String, outcome.String
		out = append(out, &ev)
	}
	return out, rows.Err()
}

// --- checkpoints ---

type checkpointStore struct{ q queryer }

func (cs checkpointStore) Get(ctx context.Context, controller string) (json.RawMessage, error) {
	var cp string
	err := cs.q.QueryRowContext(ctx,
		`SELECT checkpoint_json FROM controller_checkpoints WHERE controller=?`, controller).Scan(&cp)
	if err != nil {
		return nil, mapErr(err)
	}
	return json.RawMessage(cp), nil
}

func (cs checkpointStore) Put(ctx context.Context, controller string, cp json.RawMessage, atMillis int64) error {
	_, err := cs.q.ExecContext(ctx, `INSERT INTO controller_checkpoints (controller, checkpoint_json, updated_at)
		VALUES (?,?,?)
		ON CONFLICT(controller) DO UPDATE SET checkpoint_json=excluded.checkpoint_json, updated_at=excluded.updated_at`,
		controller, string(cp), atMillis)
	return mapErr(err)
}

// --- provider instances ---

type providerStore struct{ q queryer }

func (ps providerStore) Upsert(ctx context.Context, r *storage.ProviderInstanceRecord) error {
	_, err := ps.q.ExecContext(ctx, `INSERT INTO provider_instances (name, driver, config_json, enabled, created_at, updated_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET driver=excluded.driver, config_json=excluded.config_json,
		  enabled=excluded.enabled, updated_at=excluded.updated_at`,
		string(r.Name), r.Driver, string(r.Config), boolInt(r.Enabled), r.CreatedAt, r.UpdatedAt)
	return mapErr(err)
}

func (ps providerStore) Get(ctx context.Context, name storage.ProviderInstance) (*storage.ProviderInstanceRecord, error) {
	var r storage.ProviderInstanceRecord
	var cfg string
	var enabled int
	err := ps.q.QueryRowContext(ctx, `SELECT name, driver, config_json, enabled, created_at, updated_at
		FROM provider_instances WHERE name=?`, string(name)).
		Scan((*string)(&r.Name), &r.Driver, &cfg, &enabled, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	r.Config = []byte(cfg)
	r.Enabled = enabled == 1
	return &r, nil
}

func (ps providerStore) List(ctx context.Context) ([]*storage.ProviderInstanceRecord, error) {
	rows, err := ps.q.QueryContext(ctx, `SELECT name, driver, config_json, enabled, created_at, updated_at
		FROM provider_instances ORDER BY name`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*storage.ProviderInstanceRecord
	for rows.Next() {
		var r storage.ProviderInstanceRecord
		var cfg string
		var enabled int
		if err := rows.Scan((*string)(&r.Name), &r.Driver, &cfg, &enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Config = []byte(cfg)
		r.Enabled = enabled == 1
		out = append(out, &r)
	}
	return out, rows.Err()
}
