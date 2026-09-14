-- +goose Up
-- +goose StatementBegin
-- Multi-audio (MULTI/DUAL) language support. Probed audio tracks now carry a
-- `languages` array parsed from their titles ("English / French"), and the
-- JSONB track-language extractor must include those codes so the
-- audio_language_codes/subtitle_language_codes GIN indexes stay complete.
-- The two STORED generated columns must be dropped and re-added to force
-- recomputation; Postgres does not auto-recompute STORED columns when the
-- referenced function changes (see migration 121 for the cost profile).
--
-- probe_version gates the re-probe: rows scanned before this migration
-- carry probe_version 0, so the scanner re-probes them once and populates
-- the new languages arrays.

-- Replace the JSONB track-language extractor: UNION ALL of the per-track
-- `language` code and the track's `languages[]` codes, both canonicalized,
-- deduplicated via array_agg(DISTINCT).
CREATE OR REPLACE FUNCTION public.jsonb_track_language_codes(tracks jsonb)
RETURNS text[]
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT array_agg(DISTINCT public.canonical_language_code(code))
    FROM (
        SELECT elem->>'language' AS code
        FROM jsonb_array_elements(COALESCE(tracks, '[]'::jsonb)) AS elem
        WHERE (elem->>'language') IS NOT NULL AND (elem->>'language') <> ''
        UNION ALL
        SELECT lang
        FROM jsonb_array_elements(COALESCE(tracks, '[]'::jsonb)) AS elem,
             LATERAL jsonb_array_elements_text(
                 CASE WHEN jsonb_typeof(elem->'languages') = 'array' THEN elem->'languages' ELSE '[]'::jsonb END
             ) AS lang
        WHERE lang IS NOT NULL AND lang <> ''
    ) codes
    WHERE code IS NOT NULL AND code <> ''
$$;

-- The UPDATE trigger from 142_episode_catalog_entries.sql names the two
-- STORED columns in its WHEN clause, so the columns cannot be dropped while
-- it exists. Drop it around the column swap and recreate it verbatim.
DROP TRIGGER IF EXISTS trg_episode_catalog_entries_media_files_update ON public.media_files;

DROP INDEX IF EXISTS idx_media_files_audio_lang_gin;
DROP INDEX IF EXISTS idx_media_files_subtitle_lang_gin;

ALTER TABLE public.media_files DROP COLUMN IF EXISTS audio_language_codes;
ALTER TABLE public.media_files DROP COLUMN IF EXISTS subtitle_language_codes;

ALTER TABLE public.media_files
ADD COLUMN IF NOT EXISTS audio_language_codes text[]
  GENERATED ALWAYS AS (public.jsonb_track_language_codes(audio_tracks)) STORED;

ALTER TABLE public.media_files
ADD COLUMN IF NOT EXISTS subtitle_language_codes text[]
  GENERATED ALWAYS AS (public.jsonb_track_language_codes(subtitle_tracks)) STORED;

CREATE INDEX IF NOT EXISTS idx_media_files_audio_lang_gin
ON public.media_files USING gin (audio_language_codes);

CREATE INDEX IF NOT EXISTS idx_media_files_subtitle_lang_gin
ON public.media_files USING gin (subtitle_language_codes);

CREATE TRIGGER trg_episode_catalog_entries_media_files_update
AFTER UPDATE ON public.media_files
FOR EACH ROW
WHEN (
    OLD.episode_id IS DISTINCT FROM NEW.episode_id OR
    OLD.media_folder_id IS DISTINCT FROM NEW.media_folder_id OR
    OLD.missing_since IS DISTINCT FROM NEW.missing_since OR
    OLD.resolution IS DISTINCT FROM NEW.resolution OR
    OLD.bitrate IS DISTINCT FROM NEW.bitrate OR
    OLD.hdr IS DISTINCT FROM NEW.hdr OR
    OLD.video_tracks IS DISTINCT FROM NEW.video_tracks OR
    OLD.external_subtitles IS DISTINCT FROM NEW.external_subtitles OR
    OLD.audio_language_codes IS DISTINCT FROM NEW.audio_language_codes OR
    OLD.subtitle_language_codes IS DISTINCT FROM NEW.subtitle_language_codes
)
EXECUTE FUNCTION public.episode_catalog_entries_media_files_trigger();

ALTER TABLE public.media_files
ADD COLUMN IF NOT EXISTS probe_version INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Rollback to the per-track `language`-only extractor and the previous
-- probe_version shape. The languages arrays remain in the JSONB (harmless
-- extra keys) but are no longer indexed; rows keep whatever probe_version the
-- up migration's re-probe wrote.

DROP TRIGGER IF EXISTS trg_episode_catalog_entries_media_files_update ON public.media_files;

DROP INDEX IF EXISTS idx_media_files_audio_lang_gin;
DROP INDEX IF EXISTS idx_media_files_subtitle_lang_gin;

ALTER TABLE public.media_files DROP COLUMN IF EXISTS audio_language_codes;
ALTER TABLE public.media_files DROP COLUMN IF EXISTS subtitle_language_codes;

CREATE OR REPLACE FUNCTION public.jsonb_track_language_codes(tracks jsonb)
RETURNS text[]
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT array_agg(public.canonical_language_code(elem->>'language'))
    FROM jsonb_array_elements(COALESCE(tracks, '[]'::jsonb)) AS elem
    WHERE (elem->>'language') IS NOT NULL AND (elem->>'language') <> ''
$$;

ALTER TABLE public.media_files
ADD COLUMN IF NOT EXISTS audio_language_codes text[]
  GENERATED ALWAYS AS (public.jsonb_track_language_codes(audio_tracks)) STORED;

ALTER TABLE public.media_files
ADD COLUMN IF NOT EXISTS subtitle_language_codes text[]
  GENERATED ALWAYS AS (public.jsonb_track_language_codes(subtitle_tracks)) STORED;

CREATE INDEX IF NOT EXISTS idx_media_files_audio_lang_gin
ON public.media_files USING gin (audio_language_codes);

CREATE INDEX IF NOT EXISTS idx_media_files_subtitle_lang_gin
ON public.media_files USING gin (subtitle_language_codes);

CREATE TRIGGER trg_episode_catalog_entries_media_files_update
AFTER UPDATE ON public.media_files
FOR EACH ROW
WHEN (
    OLD.episode_id IS DISTINCT FROM NEW.episode_id OR
    OLD.media_folder_id IS DISTINCT FROM NEW.media_folder_id OR
    OLD.missing_since IS DISTINCT FROM NEW.missing_since OR
    OLD.resolution IS DISTINCT FROM NEW.resolution OR
    OLD.bitrate IS DISTINCT FROM NEW.bitrate OR
    OLD.hdr IS DISTINCT FROM NEW.hdr OR
    OLD.video_tracks IS DISTINCT FROM NEW.video_tracks OR
    OLD.external_subtitles IS DISTINCT FROM NEW.external_subtitles OR
    OLD.audio_language_codes IS DISTINCT FROM NEW.audio_language_codes OR
    OLD.subtitle_language_codes IS DISTINCT FROM NEW.subtitle_language_codes
)
EXECUTE FUNCTION public.episode_catalog_entries_media_files_trigger();

ALTER TABLE public.media_files DROP COLUMN IF EXISTS probe_version;
-- +goose StatementEnd