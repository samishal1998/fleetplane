package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/samishal1998/fleetplane/internal/phase"
	"github.com/samishal1998/fleetplane/internal/storage"
)

type resourceStore struct{ q queryer }

const resourceCols = `id, name, kind, provider_instance, class, pool_id, ownership, phase,
	external_id, external_ref_json, generation, observed_generation,
	spec_json, extension_json, capacity_json, exclusive_alloc, delete_protected,
	ready_at, last_lease_ended_at, drain_started_at, parked_at, created_at, updated_at, deleted_at`

func (rs resourceStore) Create(ctx context.Context, r *storage.Resource) error {
	var poolID sql.NullString
	if r.PoolID != nil {
		poolID = sql.NullString{String: string(*r.PoolID), Valid: true}
	}
	_, err := rs.q.ExecContext(ctx, `INSERT INTO resources (`+resourceCols+`) VALUES
		(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		string(r.ID), nsStr(r.Name), r.Kind, string(r.Provider), nsStr(r.Class), poolID,
		string(r.Ownership), string(r.Phase),
		ns(r.ExternalID), nb(r.ExternalRef), r.Generation, r.ObservedGeneration,
		string(r.Spec), nb(r.Extension), nb(r.Capacity), boolInt(r.ExclusiveAlloc), boolInt(r.DeleteProtected),
		ni(r.ReadyAt), ni(r.LastLeaseEndedAt), ni(r.DrainStartedAt), ni(r.ParkedAt), r.CreatedAt, r.UpdatedAt, ni(r.DeletedAt))
	if err != nil {
		return mapErr(err)
	}
	return rs.putLabels(ctx, r.ID, r.Labels)
}

func (rs resourceStore) putLabels(ctx context.Context, id storage.ResourceID, labels map[string]string) error {
	if _, err := rs.q.ExecContext(ctx, `DELETE FROM resource_labels WHERE resource_id=?`, string(id)); err != nil {
		return mapErr(err)
	}
	for k, v := range labels {
		if _, err := rs.q.ExecContext(ctx,
			`INSERT INTO resource_labels (resource_id, k, v) VALUES (?,?,?)`, string(id), k, v); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

func (rs resourceStore) Get(ctx context.Context, id storage.ResourceID) (*storage.Resource, error) {
	row := rs.q.QueryRowContext(ctx, `SELECT `+resourceCols+` FROM resources WHERE id=?`, string(id))
	r, err := scanResource(row)
	if err != nil {
		return nil, err
	}
	return rs.loadLabels(ctx, r)
}

func (rs resourceStore) GetByExternalID(ctx context.Context, p storage.ProviderInstance, extID string) (*storage.Resource, error) {
	row := rs.q.QueryRowContext(ctx,
		`SELECT `+resourceCols+` FROM resources WHERE provider_instance=? AND external_id=? AND deleted_at IS NULL`,
		string(p), extID)
	r, err := scanResource(row)
	if err != nil {
		return nil, err
	}
	return rs.loadLabels(ctx, r)
}

func (rs resourceStore) loadLabels(ctx context.Context, r *storage.Resource) (*storage.Resource, error) {
	rows, err := rs.q.QueryContext(ctx, `SELECT k, v FROM resource_labels WHERE resource_id=?`, string(r.ID))
	if err != nil {
		return nil, mapErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		if r.Labels == nil {
			r.Labels = map[string]string{}
		}
		r.Labels[k] = v
	}
	return r, rows.Err()
}

func (rs resourceStore) List(ctx context.Context, f storage.ResourceFilter) ([]*storage.Resource, error) {
	var where []string
	var args []any
	if !f.IncludeDeleted {
		where = append(where, "deleted_at IS NULL")
	}
	if f.Kind != "" {
		where, args = append(where, "kind=?"), append(args, f.Kind)
	}
	if f.Provider != "" {
		where, args = append(where, "provider_instance=?"), append(args, string(f.Provider))
	}
	if f.PoolID != nil {
		where, args = append(where, "pool_id=?"), append(args, string(*f.PoolID))
	}
	if f.Poolless {
		where = append(where, "pool_id IS NULL")
	}
	if f.Class != "" {
		where, args = append(where, "class=?"), append(args, f.Class)
	}
	if len(f.Phases) > 0 {
		ph := make([]string, len(f.Phases))
		for i, p := range f.Phases {
			ph[i] = "?"
			args = append(args, string(p))
		}
		where = append(where, "phase IN ("+strings.Join(ph, ",")+")")
	}
	if len(f.Ownership) > 0 {
		ow := make([]string, len(f.Ownership))
		for i, o := range f.Ownership {
			ow[i] = "?"
			args = append(args, string(o))
		}
		where = append(where, "ownership IN ("+strings.Join(ow, ",")+")")
	}
	for k, v := range f.Labels {
		where = append(where, "id IN (SELECT resource_id FROM resource_labels WHERE k=? AND v=?)")
		args = append(args, k, v)
	}
	q := `SELECT ` + resourceCols + ` FROM resources`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id"
	rows, err := rs.q.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*storage.Resource
	for rows.Next() {
		r, err := scanResource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range out {
		if _, err := rs.loadLabels(ctx, r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (rs resourceStore) UpdateSpec(ctx context.Context, r *storage.Resource, expectGen int64) error {
	res, err := rs.q.ExecContext(ctx,
		`UPDATE resources SET spec_json=?, extension_json=?, capacity_json=?, name=?,
		   delete_protected=?, generation=generation+1, updated_at=?
		 WHERE id=? AND generation=? AND deleted_at IS NULL`,
		string(r.Spec), nb(r.Extension), nb(r.Capacity), nsStr(r.Name),
		boolInt(r.DeleteProtected), r.UpdatedAt, string(r.ID), expectGen)
	if err != nil {
		return mapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: resource %s generation %d", storage.ErrStale, r.ID, expectGen)
	}
	if r.Labels != nil {
		return rs.putLabels(ctx, r.ID, r.Labels)
	}
	return nil
}

// CASPhase transitions with a from-phase predicate (plan R4) and maintains
// the reclaim-predicate side columns.
func (rs resourceStore) CASPhase(ctx context.Context, id storage.ResourceID, from, to phase.Phase, atMillis int64) error {
	set := "phase=?, updated_at=?"
	args := []any{string(to), atMillis}
	if to == phase.Ready && from != phase.Allocated {
		set += ", ready_at=?"
		args = append(args, atMillis)
	}
	if to == phase.Draining {
		set += ", drain_started_at=?"
		args = append(args, atMillis)
	}
	// docs/12: parked_at is stamped whenever the parked phase is entered
	// (incl. the starting->parked revert — restarting the stage-2 clock
	// protects recently-demanded machines) and cleared on return to ready.
	if to == phase.Parked {
		set += ", parked_at=?"
		args = append(args, atMillis)
	}
	if to == phase.Ready {
		set += ", parked_at=NULL"
	}
	args = append(args, string(id), string(from))
	res, err := rs.q.ExecContext(ctx,
		`UPDATE resources SET `+set+` WHERE id=? AND phase=? AND deleted_at IS NULL`, args...)
	if err != nil {
		return mapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: resource %s not in phase %s", storage.ErrConflict, id, from)
	}
	return nil
}

func (rs resourceStore) SetObservedGeneration(ctx context.Context, id storage.ResourceID, gen int64) error {
	return rs.execOne(ctx, `UPDATE resources SET observed_generation=? WHERE id=?`, gen, string(id))
}

func (rs resourceStore) SetExternalRef(ctx context.Context, id storage.ResourceID, extID string, ref json.RawMessage) error {
	return rs.execOne(ctx, `UPDATE resources SET external_id=?, external_ref_json=? WHERE id=?`,
		extID, nb(ref), string(id))
}

func (rs resourceStore) SetProviderFacts(ctx context.Context, id storage.ResourceID, extension, capacity json.RawMessage, atMillis int64) error {
	return rs.execOne(ctx,
		`UPDATE resources SET extension_json=COALESCE(?, extension_json),
		   capacity_json=COALESCE(?, capacity_json), updated_at=? WHERE id=?`,
		nb(extension), nb(capacity), atMillis, string(id))
}

func (rs resourceStore) SetLastLeaseEnded(ctx context.Context, id storage.ResourceID, atMillis int64) error {
	return rs.execOne(ctx, `UPDATE resources SET last_lease_ended_at=?, updated_at=? WHERE id=?`,
		atMillis, atMillis, string(id))
}

func (rs resourceStore) SetDeleteProtected(ctx context.Context, id storage.ResourceID, protected bool, atMillis int64) error {
	return rs.execOne(ctx,
		`UPDATE resources SET delete_protected=?, updated_at=? WHERE id=? AND deleted_at IS NULL`,
		boolInt(protected), atMillis, string(id))
}

func (rs resourceStore) MarkDeleted(ctx context.Context, id storage.ResourceID, atMillis int64) error {
	return rs.execOne(ctx,
		`UPDATE resources SET deleted_at=?, updated_at=? WHERE id=? AND deleted_at IS NULL`,
		atMillis, atMillis, string(id))
}

func (rs resourceStore) PutObserved(ctx context.Context, snap *storage.ObservedSnapshot) error {
	_, err := rs.q.ExecContext(ctx, `INSERT INTO observed_snapshots
		(resource_id, observed_at, provider_phase, status_json, raw_json, observe_error)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(resource_id) DO UPDATE SET observed_at=excluded.observed_at,
		  provider_phase=excluded.provider_phase, status_json=excluded.status_json,
		  raw_json=excluded.raw_json, observe_error=excluded.observe_error`,
		string(snap.ResourceID), snap.ObservedAt, nsStr(snap.ProviderPhase),
		nb(snap.Status), nb(snap.Raw), nsStr(snap.ObserveError))
	return mapErr(err)
}

func (rs resourceStore) GetObserved(ctx context.Context, id storage.ResourceID) (*storage.ObservedSnapshot, error) {
	var s storage.ObservedSnapshot
	var providerPhase, status, raw, oerr sql.NullString
	err := rs.q.QueryRowContext(ctx, `SELECT resource_id, observed_at, provider_phase, status_json, raw_json, observe_error
		FROM observed_snapshots WHERE resource_id=?`, string(id)).
		Scan((*string)(&s.ResourceID), &s.ObservedAt, &providerPhase, &status, &raw, &oerr)
	if err != nil {
		return nil, mapErr(err)
	}
	s.ProviderPhase = providerPhase.String
	s.Status, s.Raw, s.ObserveError = jb(status), jb(raw), oerr.String
	return &s, nil
}

func (rs resourceStore) execOne(ctx context.Context, q string, args ...any) error {
	res, err := rs.q.ExecContext(ctx, q, args...)
	if err != nil {
		return mapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w", storage.ErrNotFound)
	}
	return nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanResource(row rowScanner) (*storage.Resource, error) {
	var r storage.Resource
	var name, class, poolID, extID, extRef, ext, capj sql.NullString
	var readyAt, lastLease, drainStarted, parkedAt, deletedAt sql.NullInt64
	var spec string
	var excl, prot int
	err := row.Scan(
		(*string)(&r.ID), &name, &r.Kind, (*string)(&r.Provider), &class, &poolID,
		(*string)(&r.Ownership), (*string)(&r.Phase),
		&extID, &extRef, &r.Generation, &r.ObservedGeneration,
		&spec, &ext, &capj, &excl, &prot,
		&readyAt, &lastLease, &drainStarted, &parkedAt, &r.CreatedAt, &r.UpdatedAt, &deletedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	r.Name, r.Class = name.String, class.String
	if poolID.Valid {
		p := storage.PoolID(poolID.String)
		r.PoolID = &p
	}
	r.ExternalID = sp(extID)
	r.ExternalRef, r.Extension, r.Capacity = jb(extRef), jb(ext), []byte(nil)
	if capj.Valid {
		r.Capacity = []byte(capj.String)
	}
	r.Spec = []byte(spec)
	r.ExclusiveAlloc, r.DeleteProtected = excl == 1, prot == 1
	r.ReadyAt, r.LastLeaseEndedAt, r.DrainStartedAt = ip(readyAt), ip(lastLease), ip(drainStarted)
	r.ParkedAt = ip(parkedAt)
	r.DeletedAt = ip(deletedAt)
	return &r, nil
}

func nsStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
