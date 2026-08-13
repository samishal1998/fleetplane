package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/samimishal/fleetplane/internal/storage"
)

type idempotencyStore struct{ q queryer }

// Begin claims (scope, key) atomically: INSERT OR IGNORE + read-back. Run
// inside the caller's transaction so key registration and journaled intent
// commit together (invariant 2).
func (is idempotencyStore) Begin(ctx context.Context, scope, key, requestHash string, op storage.OperationID) (storage.BeginOutcome, *storage.IdemRecord, error) {
	now := time.Now().UnixMilli()
	res, err := is.q.ExecContext(ctx, `INSERT INTO idempotency_records
		(scope, key, request_hash, state, operation_id, created_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(scope, key) DO NOTHING`,
		scope, key, requestHash, string(storage.IdemInProgress), nsStr(string(op)), now)
	if err != nil {
		return 0, nil, mapErr(err)
	}
	rec, err := is.get(ctx, scope, key)
	if err != nil {
		return 0, nil, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return storage.BeginNew, rec, nil
	}
	if rec.RequestHash != requestHash {
		return storage.BeginMismatch, rec, nil
	}
	if rec.State == storage.IdemInProgress {
		return storage.BeginInProgress, rec, nil
	}
	return storage.BeginCompleted, rec, nil
}

// Complete stores the replayable outcome — stored bytes, replayed
// byte-identically (plan R17). Must run in the same transaction as the
// side-effect's final state.
func (is idempotencyStore) Complete(ctx context.Context, scope, key string, httpStatus int, result json.RawMessage, expiresAt int64) error {
	return is.finish(ctx, scope, key, storage.IdemCompleted, httpStatus, result, expiresAt)
}

func (is idempotencyStore) Fail(ctx context.Context, scope, key string, httpStatus int, result json.RawMessage, expiresAt int64) error {
	return is.finish(ctx, scope, key, storage.IdemFailed, httpStatus, result, expiresAt)
}

func (is idempotencyStore) finish(ctx context.Context, scope, key string, state storage.IdemState, httpStatus int, result json.RawMessage, expiresAt int64) error {
	now := time.Now().UnixMilli()
	res, err := is.q.ExecContext(ctx, `UPDATE idempotency_records
		SET state=?, http_status=?, result_json=?, completed_at=?, expires_at=?
		WHERE scope=? AND key=? AND state=?`,
		string(state), httpStatus, nb(result), now, expiresAt,
		scope, key, string(storage.IdemInProgress))
	if err != nil {
		return mapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: idempotency record (%s,%s) not in_progress", storage.ErrConflict, scope, key)
	}
	return nil
}

// GC removes expired records whose referenced operation is terminal (or
// that reference no operation).
func (is idempotencyStore) GC(ctx context.Context, nowMillis int64, limit int) (int, error) {
	res, err := is.q.ExecContext(ctx, `DELETE FROM idempotency_records
		WHERE (scope, key) IN (
		  SELECT i.scope, i.key FROM idempotency_records i
		  LEFT JOIN operations o ON o.id = i.operation_id
		  WHERE i.expires_at IS NOT NULL AND i.expires_at < ?
		    AND (i.operation_id IS NULL OR o.terminal_at IS NOT NULL)
		  LIMIT ?)`, nowMillis, limit)
	if err != nil {
		return 0, mapErr(err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (is idempotencyStore) get(ctx context.Context, scope, key string) (*storage.IdemRecord, error) {
	var r storage.IdemRecord
	var opID, result sql.NullString
	var httpStatus sql.NullInt64
	var completedAt, expiresAt sql.NullInt64
	err := is.q.QueryRowContext(ctx, `SELECT scope, key, request_hash, state, operation_id,
		http_status, result_json, created_at, completed_at, expires_at
		FROM idempotency_records WHERE scope=? AND key=?`, scope, key).
		Scan(&r.Scope, &r.Key, &r.RequestHash, (*string)(&r.State), &opID,
			&httpStatus, &result, &r.CreatedAt, &completedAt, &expiresAt)
	if err != nil {
		return nil, mapErr(err)
	}
	if opID.Valid {
		o := storage.OperationID(opID.String)
		r.OperationID = &o
	}
	r.HTTPStatus = int(httpStatus.Int64)
	r.Result = jb(result)
	r.CompletedAt, r.ExpiresAt = ip(completedAt), ip(expiresAt)
	return &r, nil
}
