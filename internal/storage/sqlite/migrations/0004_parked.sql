-- +goose NO TRANSACTION
-- +goose Up
-- Parked machines (docs/12): the resources phase CHECK gains
-- parking/parked/starting and resources gains parked_at; classes gains the
-- two-stage reclaim columns. SQLite cannot alter a CHECK, so resources is
-- rebuilt via the documented 12-step procedure. This migration runs OUTSIDE
-- a transaction because PRAGMA foreign_keys is a no-op inside one and four
-- tables hold FKs into resources — the deliberate, documented exception to
-- ADR-010's "PRAGMAs live only in the DSN" rule. Migrations run on the
-- single-connection writer, so the pragma holds for every statement below.
-- The script is re-runnable: a crash between COMMIT and goose recording the
-- version re-executes it against the already-rebuilt table harmlessly
-- (parked_at can hold no data before the app has ever run on 0004).
PRAGMA foreign_keys=OFF;

DROP TABLE IF EXISTS resources_new;

BEGIN IMMEDIATE;

CREATE TABLE resources_new (
    id                  TEXT NOT NULL PRIMARY KEY,
    name                TEXT,
    kind                TEXT NOT NULL,
    provider_instance   TEXT NOT NULL,
    class               TEXT,
    pool_id             TEXT REFERENCES pools(id),
    ownership           TEXT NOT NULL CHECK (ownership IN ('managed','adopted','observed')),
    phase               TEXT NOT NULL CHECK (phase IN ('unknown','provisioning','ready','allocated','draining','deleting','failed','orphaned','parking','parked','starting')),
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
    parked_at           INTEGER,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    deleted_at          INTEGER
) STRICT;

INSERT INTO resources_new (id, name, kind, provider_instance, class, pool_id,
    ownership, phase, external_id, external_ref_json, generation,
    observed_generation, spec_json, extension_json, capacity_json,
    exclusive_alloc, delete_protected, ready_at, last_lease_ended_at,
    drain_started_at, parked_at, created_at, updated_at, deleted_at)
SELECT id, name, kind, provider_instance, class, pool_id,
    ownership, phase, external_id, external_ref_json, generation,
    observed_generation, spec_json, extension_json, capacity_json,
    exclusive_alloc, delete_protected, ready_at, last_lease_ended_at,
    drain_started_at, NULL, created_at, updated_at, deleted_at
FROM resources;

DROP TABLE resources;
ALTER TABLE resources_new RENAME TO resources;

CREATE UNIQUE INDEX ux_res_ext   ON resources(provider_instance, external_id) WHERE external_id IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX ix_res_sched        ON resources(kind, phase, provider_instance) WHERE deleted_at IS NULL;
CREATE INDEX ix_res_pool         ON resources(pool_id) WHERE deleted_at IS NULL;

ALTER TABLE classes ADD COLUMN reclaim_park TEXT;
ALTER TABLE classes ADD COLUMN reclaim_delete_after_ms INTEGER;

COMMIT;

PRAGMA foreign_keys=ON;

-- +goose Down
-- Best-effort reverse rebuild: fails if any row uses the new phases (the
-- ADR-010 downgrade guard refuses newer schemas anyway).
PRAGMA foreign_keys=OFF;
DROP TABLE IF EXISTS resources_old;
BEGIN IMMEDIATE;
CREATE TABLE resources_old (
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
INSERT INTO resources_old SELECT id, name, kind, provider_instance, class, pool_id,
    ownership, phase, external_id, external_ref_json, generation,
    observed_generation, spec_json, extension_json, capacity_json,
    exclusive_alloc, delete_protected, ready_at, last_lease_ended_at,
    drain_started_at, created_at, updated_at, deleted_at FROM resources;
DROP TABLE resources;
ALTER TABLE resources_old RENAME TO resources;
CREATE UNIQUE INDEX ux_res_ext   ON resources(provider_instance, external_id) WHERE external_id IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX ix_res_sched        ON resources(kind, phase, provider_instance) WHERE deleted_at IS NULL;
CREATE INDEX ix_res_pool         ON resources(pool_id) WHERE deleted_at IS NULL;
ALTER TABLE classes DROP COLUMN reclaim_park;
ALTER TABLE classes DROP COLUMN reclaim_delete_after_ms;
COMMIT;
PRAGMA foreign_keys=ON;
