package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/samishal1998/fleetplane/internal/storage"
)

type operationStore struct{ q queryer }

const opCols = `id, parent_id, idem_scope, idem_key, kind, resource_id, pool_id,
	provider_instance, action_json, state, attempt, external_op_json, external_ref_json,
	error_class, error_json, next_attempt_at, verify_deadline_at, created_at, updated_at, terminal_at`

func (os operationStore) Append(ctx context.Context, op *storage.Operation) error {
	if op.State != storage.OpJournaled {
		return fmt.Errorf("%w: operations must be appended in state journaled, got %q", storage.ErrConflict, op.State)
	}
	var parent, resID, poolID sql.NullString
	if op.ParentID != nil {
		parent = sql.NullString{String: string(*op.ParentID), Valid: true}
	}
	if op.ResourceID != nil {
		resID = sql.NullString{String: string(*op.ResourceID), Valid: true}
	}
	if op.PoolID != nil {
		poolID = sql.NullString{String: string(*op.PoolID), Valid: true}
	}
	// op_active_per_resource (partial unique) turns a second non-terminal
	// operation on the same resource into ErrConflict.
	_, err := os.q.ExecContext(ctx, `INSERT INTO operations (`+opCols+`) VALUES
		(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		string(op.ID), parent, ns(op.IdemScope), ns(op.IdemKey), string(op.Kind), resID, poolID,
		string(op.Provider), string(op.Action), string(op.State), op.Attempt,
		nb(op.ExternalOp), nb(op.ExternalRef), ns(op.ErrorClass), nb(op.ErrorDetail),
		ni(op.NextAttemptAt), ni(op.VerifyDeadlineAt), op.CreatedAt, op.UpdatedAt, ni(op.TerminalAt))
	return mapErr(err)
}

func (os operationStore) Get(ctx context.Context, id storage.OperationID) (*storage.Operation, error) {
	return scanOp(os.q.QueryRowContext(ctx, `SELECT `+opCols+` FROM operations WHERE id=?`, string(id)))
}

// Transition is the journal CAS: load, verify from-state, apply mut in
// memory, then UPDATE ... WHERE state=from — atomic even outside Tx.
func (os operationStore) Transition(ctx context.Context, id storage.OperationID, from, to storage.OpState, mut func(*storage.Operation)) error {
	op, err := os.Get(ctx, id)
	if err != nil {
		return err
	}
	if op.State != from {
		return fmt.Errorf("%w: operation %s is %q, not %q", storage.ErrConflict, id, op.State, from)
	}
	op.State = to
	now := time.Now().UnixMilli()
	op.UpdatedAt = now
	if to.Terminal() && op.TerminalAt == nil {
		op.TerminalAt = &now
	}
	if mut != nil {
		mut(op)
	}
	res, err := os.q.ExecContext(ctx, `UPDATE operations SET
		state=?, attempt=?, external_op_json=?, external_ref_json=?,
		error_class=?, error_json=?, next_attempt_at=?, verify_deadline_at=?,
		updated_at=?, terminal_at=?
		WHERE id=? AND state=?`,
		string(op.State), op.Attempt, nb(op.ExternalOp), nb(op.ExternalRef),
		ns(op.ErrorClass), nb(op.ErrorDetail), ni(op.NextAttemptAt), ni(op.VerifyDeadlineAt),
		op.UpdatedAt, ni(op.TerminalAt),
		string(id), string(from))
	if err != nil {
		return mapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: operation %s left state %q concurrently", storage.ErrConflict, id, from)
	}
	return nil
}

func (os operationStore) Due(ctx context.Context, nowMillis int64, limit int) ([]*storage.Operation, error) {
	rows, err := os.q.QueryContext(ctx, `SELECT `+opCols+` FROM operations
		WHERE terminal_at IS NULL
		  AND state IN ('journaled','external_accepted','verifying')
		  AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
		ORDER BY created_at LIMIT ?`, nowMillis, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	return collectOps(rows)
}

func (os operationStore) NonTerminal(ctx context.Context) ([]*storage.Operation, error) {
	rows, err := os.q.QueryContext(ctx,
		`SELECT `+opCols+` FROM operations WHERE terminal_at IS NULL ORDER BY created_at`)
	if err != nil {
		return nil, mapErr(err)
	}
	return collectOps(rows)
}

func (os operationStore) CountPending(ctx context.Context, pool storage.PoolID, kind storage.OpKind) (int, error) {
	var n int
	err := os.q.QueryRowContext(ctx,
		`SELECT count(*) FROM operations WHERE pool_id=? AND kind=? AND terminal_at IS NULL`,
		string(pool), string(kind)).Scan(&n)
	return n, mapErr(err)
}

func collectOps(rows *sql.Rows) ([]*storage.Operation, error) {
	defer func() { _ = rows.Close() }()
	var out []*storage.Operation
	for rows.Next() {
		op, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

func scanOp(row rowScanner) (*storage.Operation, error) {
	var op storage.Operation
	var parent, idemScope, idemKey, resID, poolID, extOp, extRef, errClass, errJSON sql.NullString
	var action string
	var nextAt, verifyAt, terminalAt sql.NullInt64
	err := row.Scan(
		(*string)(&op.ID), &parent, &idemScope, &idemKey, (*string)(&op.Kind), &resID, &poolID,
		(*string)(&op.Provider), &action, (*string)(&op.State), &op.Attempt, &extOp, &extRef,
		&errClass, &errJSON, &nextAt, &verifyAt, &op.CreatedAt, &op.UpdatedAt, &terminalAt)
	if err != nil {
		return nil, mapErr(err)
	}
	if parent.Valid {
		p := storage.OperationID(parent.String)
		op.ParentID = &p
	}
	op.IdemScope, op.IdemKey = sp(idemScope), sp(idemKey)
	if resID.Valid {
		r := storage.ResourceID(resID.String)
		op.ResourceID = &r
	}
	if poolID.Valid {
		p := storage.PoolID(poolID.String)
		op.PoolID = &p
	}
	op.Action = []byte(action)
	op.ExternalOp, op.ExternalRef = jb(extOp), jb(extRef)
	op.ErrorClass = sp(errClass)
	op.ErrorDetail = jb(errJSON)
	op.NextAttemptAt, op.VerifyDeadlineAt, op.TerminalAt = ip(nextAt), ip(verifyAt), ip(terminalAt)
	return &op, nil
}
