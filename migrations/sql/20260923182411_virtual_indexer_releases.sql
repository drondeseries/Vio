-- +goose Up
-- Prowlarr releases surfaced by the virtual-library "Refresh List" flow for a
-- title the provider (AltMount) does not have yet. Rows are a short-lived
-- snapshot: each upsert batch prunes expired rows, and readers only see rows
-- whose expire_at is still in the future. download_url stays server-internal
-- (used to enqueue the release) and is never serialized to a client.
--
-- The id uses identity rather than serial to match every table created since
-- 20260912231311_serial_ids_to_identity.
CREATE TABLE virtual_indexer_releases (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    content_id      text NOT NULL,
    episode_id      text,
    media_folder_id integer NOT NULL,
    guid            text NOT NULL,
    title           text NOT NULL,
    normalized_name text NOT NULL,
    size_bytes      bigint NOT NULL DEFAULT 0,
    protocol        text NOT NULL DEFAULT '',
    indexer         text NOT NULL DEFAULT '',
    indexer_id      integer NOT NULL DEFAULT 0,
    download_url    text NOT NULL,
    format_score    integer,
    published_at    timestamptz,
    enqueue_state   text NOT NULL DEFAULT 'not_downloaded'
        CHECK (enqueue_state IN ('not_downloaded', 'queued', 'failed')),
    enqueued_at     timestamptz,
    nzo_id          text,
    expire_at       timestamptz NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- A movie row carries episode_id NULL, so the unique key coalesces it to the
-- empty string; episode rows use the episode id. This keeps one release per
-- (title, episode, guid) without relying on NULL-distinct behavior.
CREATE UNIQUE INDEX virtual_indexer_releases_identity_idx
    ON virtual_indexer_releases (content_id, COALESCE(episode_id, ''), guid);

-- Read path: live rows for one (content, episode) scope, newest first.
CREATE INDEX virtual_indexer_releases_scope_expire_idx
    ON virtual_indexer_releases (content_id, episode_id, expire_at);

-- +goose Down
DROP TABLE virtual_indexer_releases;
