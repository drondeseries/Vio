-- +goose Up
-- Backfill missing durations on media_files from media_items and episodes.
UPDATE public.media_files AS mf
SET duration = mi.runtime * 60,
    updated_at = now()
FROM public.media_items AS mi
WHERE mf.content_id = mi.content_id
  AND (mf.duration IS NULL OR mf.duration = 0)
  AND mi.runtime > 0;

UPDATE public.media_files AS mf
SET duration = ep.runtime * 60,
    updated_at = now()
FROM public.episodes AS ep
WHERE mf.episode_id = ep.content_id
  AND (mf.duration IS NULL OR mf.duration = 0)
  AND ep.runtime > 0;

-- +goose Down
SELECT 1;
