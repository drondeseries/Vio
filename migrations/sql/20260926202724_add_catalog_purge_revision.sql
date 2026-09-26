-- +goose Up
CREATE TABLE IF NOT EXISTS catalog_purge_revision (
    id INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    revision BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO catalog_purge_revision (id, revision) VALUES (1, 0) ON CONFLICT (id) DO NOTHING;

-- +goose Down
DROP TABLE IF EXISTS catalog_purge_revision;
