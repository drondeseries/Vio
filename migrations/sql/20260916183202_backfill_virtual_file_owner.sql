-- +goose Up
-- Legacy virtual media_files rows can carry virtual_owner_installation_id = 0
-- while their parent media_items row carries the real owner. The catalog treats
-- file owner 0 as "inherit the item owner" (internal/catalog/item_repo.go), so
-- the playback resolver must too, but rows written before that rule existed
-- still force owner 0 into allow_insecure_http decisions and get their LAN
-- provider URLs rejected. Backfill the file owner from the parent item.
--
-- Scoped to virtual rows (container 'virtual' or a virtual:// path) whose owner
-- is exactly 0; local files keep their NULL owner, and an item with no owner
-- leaves the file untouched. The lookup mirrors item_repo's identity join:
-- a movie file matches through content_id, an episode file through its
-- episode's series_id. Idempotent: the WHERE owner = 0 clause means a second
-- run matches nothing.
UPDATE public.media_files mf
SET virtual_owner_installation_id = (
        SELECT mi.virtual_owner_installation_id
        FROM public.media_items mi
        LEFT JOIN public.episodes ep ON ep.content_id = mf.episode_id
        WHERE mi.content_id = COALESCE(NULLIF(mf.content_id, ''), ep.series_id)
          AND mi.virtual_owner_installation_id > 0
        LIMIT 1
    ),
    updated_at = NOW()
WHERE mf.virtual_owner_installation_id = 0
  AND (mf.container = 'virtual' OR mf.file_path LIKE 'virtual://%')
  AND EXISTS (
        SELECT 1
        FROM public.media_items mi
        LEFT JOIN public.episodes ep ON ep.content_id = mf.episode_id
        WHERE mi.content_id = COALESCE(NULLIF(mf.content_id, ''), ep.series_id)
          AND mi.virtual_owner_installation_id > 0
  );

-- +goose Down
-- Intentional no-op. After the backfill, a row that was created with owner 0 is
-- indistinguishable from one that inherited its item owner legitimately.
-- Reverting would have to null/zero owners that were set correctly at write
-- time, so there is no safe inverse.
SELECT 1;
