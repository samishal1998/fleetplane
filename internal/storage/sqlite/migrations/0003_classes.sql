-- +goose Up
-- Classes become storage-backed (dynamic classes): config classes are
-- seeded at boot with source='config' (config stays authoritative for
-- them); API-created classes carry source='api'.
CREATE TABLE classes (
    name                  TEXT PRIMARY KEY,
    kind                  TEXT NOT NULL,
    provider              TEXT NOT NULL,
    spec_json             TEXT NOT NULL,
    reclaim_idle_after_ms INTEGER,
    queue_max_wait_ms     INTEGER,
    source                TEXT NOT NULL CHECK (source IN ('config', 'api')),
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL
) STRICT;

-- +goose Down
DROP TABLE classes;
