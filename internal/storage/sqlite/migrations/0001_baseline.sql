-- +goose Up
-- Baseline: controller checkpoints (persisted controller state; also stores
-- the control-plane instance identity/OwnerID under controller='instance').
-- Domain tables land with the storage increment (I3); pragmas live in the
-- connection DSN, never here (ADR-010).
CREATE TABLE controller_checkpoints (
    controller      TEXT NOT NULL PRIMARY KEY,
    checkpoint_json TEXT NOT NULL,
    updated_at      INTEGER NOT NULL
) STRICT, WITHOUT ROWID;
