package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/samishal1998/fleetplane/internal/storage"
)

type leaseStore struct{ q queryer }

const leaseCols = `id, acquisition_id, resource_id, holder, capacity_json, exclusive, state, expires_at, created_at, ended_at`

func (ls leaseStore) Insert(ctx context.Context, l *storage.Lease) error {
	_, err := ls.q.ExecContext(ctx, `INSERT INTO leases (`+leaseCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		string(l.ID), string(l.AcquisitionID), string(l.ResourceID), l.Holder,
		string(l.Capacity), boolInt(l.Exclusive), string(l.State), ni(l.ExpiresAt), l.CreatedAt, ni(l.EndedAt))
	return mapErr(err)
}

func (ls leaseStore) Get(ctx context.Context, id storage.LeaseID) (*storage.Lease, error) {
	return scanLease(ls.q.QueryRowContext(ctx, `SELECT `+leaseCols+` FROM leases WHERE id=?`, string(id)))
}

func (ls leaseStore) ActiveByResource(ctx context.Context, id storage.ResourceID) ([]*storage.Lease, error) {
	rows, err := ls.q.QueryContext(ctx,
		`SELECT `+leaseCols+` FROM leases WHERE resource_id=? AND state='active' ORDER BY id`, string(id))
	if err != nil {
		return nil, mapErr(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*storage.Lease
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (ls leaseStore) ByResource(ctx context.Context, id storage.ResourceID) ([]*storage.Lease, error) {
	rows, err := ls.q.QueryContext(ctx,
		`SELECT `+leaseCols+` FROM leases WHERE resource_id=? ORDER BY created_at`, string(id))
	if err != nil {
		return nil, mapErr(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*storage.Lease
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (ls leaseStore) CountActive(ctx context.Context, id storage.ResourceID) (int, error) {
	var n int
	err := ls.q.QueryRowContext(ctx,
		`SELECT count(*) FROM leases WHERE resource_id=? AND state='active'`, string(id)).Scan(&n)
	return n, mapErr(err)
}

// SumActive aggregates active lease capacity per dimension in Go (capacity
// is a JSON object per lease; fleet sizes make this trivial).
func (ls leaseStore) SumActive(ctx context.Context, id storage.ResourceID) (map[string]int64, error) {
	leases, err := ls.ActiveByResource(ctx, id)
	if err != nil {
		return nil, err
	}
	sum := map[string]int64{}
	for _, l := range leases {
		var c map[string]int64
		if err := json.Unmarshal(l.Capacity, &c); err != nil {
			return nil, fmt.Errorf("lease %s capacity: %w", l.ID, err)
		}
		for k, v := range c {
			sum[k] += v
		}
	}
	return sum, nil
}

func (ls leaseStore) Transition(ctx context.Context, id storage.LeaseID, from, to storage.LeaseState, atMillis int64) error {
	ended := sql.NullInt64{}
	if to == storage.LeaseReleased || to == storage.LeaseExpired {
		ended = sql.NullInt64{Int64: atMillis, Valid: true}
	}
	res, err := ls.q.ExecContext(ctx,
		`UPDATE leases SET state=?, ended_at=COALESCE(?, ended_at) WHERE id=? AND state=?`,
		string(to), ended, string(id), string(from))
	if err != nil {
		return mapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: lease %s not in state %s", storage.ErrConflict, id, from)
	}
	return nil
}

func (ls leaseStore) Expiring(ctx context.Context, nowMillis int64, limit int) ([]*storage.Lease, error) {
	rows, err := ls.q.QueryContext(ctx, `SELECT `+leaseCols+` FROM leases
		WHERE state='active' AND expires_at IS NOT NULL AND expires_at <= ?
		ORDER BY expires_at LIMIT ?`, nowMillis, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*storage.Lease
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func scanLease(row rowScanner) (*storage.Lease, error) {
	var l storage.Lease
	var capj string
	var excl int
	var expiresAt, endedAt sql.NullInt64
	err := row.Scan((*string)(&l.ID), (*string)(&l.AcquisitionID), (*string)(&l.ResourceID),
		&l.Holder, &capj, &excl, (*string)(&l.State), &expiresAt, &l.CreatedAt, &endedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	l.Capacity = []byte(capj)
	l.Exclusive = excl == 1
	l.ExpiresAt, l.EndedAt = ip(expiresAt), ip(endedAt)
	return &l, nil
}

type acquisitionStore struct{ q queryer }

const acqCols = `id, actor, kind, class, constraints_json, quantity, ttl_seconds, state, lease_id, pending_resource_id, created_at, updated_at`

func (as acquisitionStore) Insert(ctx context.Context, a *storage.Acquisition) error {
	_, err := as.q.ExecContext(ctx, `INSERT INTO acquisitions (`+acqCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		string(a.ID), a.Actor, a.Kind, nsStr(a.Class), nb(a.Constraints), a.Quantity, a.TTLSeconds,
		string(a.State), nullID(a.LeaseID), nullID(a.PendingResourceID), a.CreatedAt, a.UpdatedAt)
	return mapErr(err)
}

func (as acquisitionStore) Get(ctx context.Context, id storage.AcquisitionID) (*storage.Acquisition, error) {
	return scanAcq(as.q.QueryRowContext(ctx, `SELECT `+acqCols+` FROM acquisitions WHERE id=?`, string(id)))
}

func (as acquisitionStore) Transition(ctx context.Context, id storage.AcquisitionID, from, to storage.AcqState, atMillis int64) error {
	res, err := as.q.ExecContext(ctx,
		`UPDATE acquisitions SET state=?, updated_at=? WHERE id=? AND state=?`,
		string(to), atMillis, string(id), string(from))
	if err != nil {
		return mapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: acquisition %s not in state %s", storage.ErrConflict, id, from)
	}
	return nil
}

func (as acquisitionStore) Bind(ctx context.Context, id storage.AcquisitionID, lease storage.LeaseID, atMillis int64) error {
	r, err := as.q.ExecContext(ctx, `UPDATE acquisitions
		SET state='bound', lease_id=?, pending_resource_id=NULL, updated_at=?
		WHERE id=? AND state IN ('pending','provisioning')`,
		string(lease), atMillis, string(id))
	if err != nil {
		return mapErr(err)
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: acquisition %s not bindable", storage.ErrConflict, id)
	}
	return nil
}

func (as acquisitionStore) SetPendingResource(ctx context.Context, id storage.AcquisitionID, res storage.ResourceID) error {
	r, err := as.q.ExecContext(ctx,
		`UPDATE acquisitions SET pending_resource_id=? WHERE id=?`, string(res), string(id))
	if err != nil {
		return mapErr(err)
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return fmt.Errorf("%w", storage.ErrNotFound)
	}
	return nil
}

func (as acquisitionStore) ClearPendingResource(ctx context.Context, id storage.AcquisitionID) error {
	_, err := as.q.ExecContext(ctx,
		`UPDATE acquisitions SET pending_resource_id=NULL WHERE id=?`, string(id))
	return mapErr(err)
}

func (as acquisitionStore) ListByState(ctx context.Context, states ...storage.AcqState) ([]*storage.Acquisition, error) {
	if len(states) == 0 {
		return nil, nil
	}
	ph := ""
	args := make([]any, len(states))
	for i, s := range states {
		if i > 0 {
			ph += ","
		}
		ph += "?"
		args[i] = string(s)
	}
	rows, err := as.q.QueryContext(ctx,
		`SELECT `+acqCols+` FROM acquisitions WHERE state IN (`+ph+`) ORDER BY id`, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*storage.Acquisition
	for rows.Next() {
		a, err := scanAcq(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func scanAcq(row rowScanner) (*storage.Acquisition, error) {
	var a storage.Acquisition
	var class, constraints, leaseID, pendingRes sql.NullString
	err := row.Scan((*string)(&a.ID), &a.Actor, &a.Kind, &class, &constraints, &a.Quantity,
		&a.TTLSeconds, (*string)(&a.State), &leaseID, &pendingRes, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	a.Class = class.String
	a.Constraints = jb(constraints)
	if leaseID.Valid {
		l := storage.LeaseID(leaseID.String)
		a.LeaseID = &l
	}
	if pendingRes.Valid {
		r := storage.ResourceID(pendingRes.String)
		a.PendingResourceID = &r
	}
	return &a, nil
}

func nullID[T ~string](p *T) sql.NullString {
	if p == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(*p), Valid: true}
}
