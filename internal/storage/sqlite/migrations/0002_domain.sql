-- +goose Up
-- Domain schema per the reconciled plan (R3-R5, R17, R20, R22, ADR-017):
-- * phase CHECK = exactly the 8 phases of 05 §7 (no 'deleted'; tombstone = deleted_at)
-- * operations CHECK = the single 8-state journal vocabulary
-- * leases carry exclusive + partial unique index (invariant-1 DB backstop); no 'reserved' state
-- * acquisitions carry scale-on-demand binding columns; no idem_key column
-- * events use evt_<ulid> TEXT PK as the pagination cursor

CREATE TABLE provider_instances (
    name        TEXT NOT NULL PRIMARY KEY,
    driver      TEXT NOT NULL,
    config_json TEXT NOT NULL, -- secret:// references only, never values
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE TABLE pools (
    id                  TEXT NOT NULL PRIMARY KEY,
    name                TEXT NOT NULL UNIQUE,
    kind                TEXT NOT NULL,
    spec_json           TEXT NOT NULL,
    generation          INTEGER NOT NULL DEFAULT 1,
    observed_generation INTEGER NOT NULL DEFAULT 0,
    paused              INTEGER NOT NULL DEFAULT 0,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL
) STRICT;

CREATE TABLE resources (
    id                  TEXT NOT NULL PRIMARY KEY,
    name                TEXT,
    kind                TEXT NOT NULL,
    provider_instance   TEXT NOT NULL,
    class               TEXT,
    pool_id             TEXT REFERENCES pools(id),
    ownership           TEXT NOT NULL CHECK (ownership IN ('managed','adopted','observed')),
    phase               TEXT NOT NULL CHECK (phase IN ('unknown','provisioning','ready','allocated','draining','deleting','failed','orphaned')),
    external_id         TEXT,
    external_ref_json   TEXT,
    generation          INTEGER NOT NULL DEFAULT 1,
    observed_generation INTEGER NOT NULL DEFAULT 0,
    spec_json           TEXT NOT NULL,
    extension_json      TEXT,
    capacity_json       TEXT,
    exclusive_alloc     INTEGER NOT NULL DEFAULT 0,
    delete_protected    INTEGER NOT NULL DEFAULT 0,
    ready_at            INTEGER,
    last_lease_ended_at INTEGER,
    drain_started_at    INTEGER,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    deleted_at          INTEGER
) STRICT;
CREATE UNIQUE INDEX ux_res_ext   ON resources(provider_instance, external_id) WHERE external_id IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX ix_res_sched        ON resources(kind, phase, provider_instance) WHERE deleted_at IS NULL;
CREATE INDEX ix_res_pool         ON resources(pool_id) WHERE deleted_at IS NULL;

CREATE TABLE resource_labels (
    resource_id TEXT NOT NULL REFERENCES resources(id),
    k           TEXT NOT NULL,
    v           TEXT NOT NULL,
    PRIMARY KEY (resource_id, k)
) STRICT, WITHOUT ROWID;
CREATE INDEX ix_labels_kv ON resource_labels(k, v);

CREATE TABLE observed_snapshots (
    resource_id    TEXT NOT NULL PRIMARY KEY REFERENCES resources(id),
    observed_at    INTEGER NOT NULL,
    provider_phase TEXT,
    status_json    TEXT,
    raw_json       TEXT,
    observe_error  TEXT
) STRICT, WITHOUT ROWID;

CREATE TABLE acquisitions (
    id                  TEXT NOT NULL PRIMARY KEY,
    actor               TEXT NOT NULL,
    kind                TEXT NOT NULL,
    class               TEXT,
    constraints_json    TEXT,
    quantity            INTEGER NOT NULL DEFAULT 1,
    ttl_seconds         INTEGER NOT NULL DEFAULT 0,
    state               TEXT NOT NULL CHECK (state IN ('pending','provisioning','bound','failed','released','expired')),
    lease_id            TEXT,
    pending_resource_id TEXT,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL
) STRICT;
CREATE INDEX ix_acq_state ON acquisitions(state);

CREATE TABLE leases (
    id             TEXT NOT NULL PRIMARY KEY,
    acquisition_id TEXT NOT NULL REFERENCES acquisitions(id),
    resource_id    TEXT NOT NULL REFERENCES resources(id),
    holder         TEXT NOT NULL,
    capacity_json  TEXT NOT NULL,
    exclusive      INTEGER NOT NULL DEFAULT 0,
    state          TEXT NOT NULL CHECK (state IN ('active','released','expired')),
    expires_at     INTEGER,
    created_at     INTEGER NOT NULL,
    ended_at       INTEGER
) STRICT;
CREATE INDEX ix_leases_active        ON leases(resource_id) WHERE state = 'active';
CREATE UNIQUE INDEX lease_exclusive  ON leases(resource_id) WHERE state = 'active' AND exclusive = 1;
CREATE INDEX ix_leases_expiry        ON leases(expires_at) WHERE state = 'active' AND expires_at IS NOT NULL;

CREATE TABLE operations (
    id                 TEXT NOT NULL PRIMARY KEY,
    parent_id          TEXT REFERENCES operations(id),
    idem_scope         TEXT,
    idem_key           TEXT,
    kind               TEXT NOT NULL,
    resource_id        TEXT REFERENCES resources(id),
    pool_id            TEXT REFERENCES pools(id),
    provider_instance  TEXT NOT NULL,
    action_json        TEXT NOT NULL,
    state              TEXT NOT NULL CHECK (state IN ('journaled','in_flight','external_accepted','verifying','uncertain','succeeded','failed','aborted')),
    attempt            INTEGER NOT NULL DEFAULT 0,
    external_op_json   TEXT,
    external_ref_json  TEXT,
    error_class        TEXT,
    error_json         TEXT,
    next_attempt_at    INTEGER,
    verify_deadline_at INTEGER,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    terminal_at        INTEGER
) STRICT;
CREATE UNIQUE INDEX op_active_per_resource ON operations(resource_id) WHERE terminal_at IS NULL AND resource_id IS NOT NULL;
CREATE INDEX ix_ops_runnable ON operations(state, next_attempt_at) WHERE terminal_at IS NULL;
CREATE INDEX ix_ops_pool     ON operations(pool_id, kind) WHERE terminal_at IS NULL;
CREATE INDEX ix_ops_resource ON operations(resource_id, created_at);

CREATE TABLE idempotency_records (
    scope        TEXT NOT NULL,
    key          TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    state        TEXT NOT NULL CHECK (state IN ('in_progress','completed','failed')),
    -- soft reference: Begin claims the key BEFORE the operation is appended
    -- in the same tx (TxA), and SQLite checks FKs immediately.
    operation_id TEXT,
    http_status  INTEGER,
    result_json  TEXT,
    created_at   INTEGER NOT NULL,
    completed_at INTEGER,
    expires_at   INTEGER,
    PRIMARY KEY (scope, key)
) STRICT, WITHOUT ROWID;
CREATE INDEX ix_idem_gc ON idempotency_records(expires_at);

CREATE TABLE events (
    id                  TEXT NOT NULL PRIMARY KEY, -- evt_<ulid>: cursor (R22)
    ts                  INTEGER NOT NULL,
    actor               TEXT,
    request_id          TEXT,
    idem_key            TEXT,
    type                TEXT NOT NULL,
    resource_id         TEXT,
    pool_id             TEXT,
    operation_id        TEXT,
    provider_instance   TEXT,
    intent_json         TEXT,
    plan_json           TEXT,
    provider_request_id TEXT,
    outcome             TEXT,
    details_json        TEXT
) STRICT;
CREATE INDEX ix_events_res ON events(resource_id, id);
CREATE INDEX ix_events_ts  ON events(ts);
