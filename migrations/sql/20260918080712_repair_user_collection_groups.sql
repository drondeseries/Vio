-- +goose Up
-- +goose StatementBegin
-- Libraries created after migration 124 did not receive the placement group
-- that exposes opted-in personal collections in the shared library view.
INSERT INTO library_collection_groups (
    library_id, label, title, sort_order, id, name, slug, kind, default_sort_mode
)
SELECT
    mf.id,
    'user-collections',
    'My collections',
    9998,
    'lcg_user_' || mf.id::text,
    'My collections',
    'user-collections',
    'user_collections',
    'manual'
FROM media_folders mf
WHERE NOT EXISTS (
    SELECT 1
    FROM library_collection_groups existing
    WHERE existing.library_id = mf.id
      AND existing.kind = 'user_collections'
)
ON CONFLICT (library_id, label) DO UPDATE
SET
    id = EXCLUDED.id,
    title = EXCLUDED.title,
    sort_order = EXCLUDED.sort_order,
    name = EXCLUDED.name,
    slug = EXCLUDED.slug,
    kind = EXCLUDED.kind,
    default_sort_mode = EXCLUDED.default_sort_mode,
    updated_at = NOW();
-- +goose StatementEnd

-- +goose Down
-- Intentionally a no-op: a rollback cannot distinguish groups inserted by
-- this repair from groups created with a library after the fix was deployed.
SELECT 1;
