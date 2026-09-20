-- +goose Up
-- Persist the provider URL a virtual candidate last resolved to, its parsed
-- expiry, and the candidate's durable provider identity. The neutral
-- `virtual://...?result=<id>` file_path remains the row's listing identity;
-- these columns are purely additive and let a later phase treat a candidate
-- that once played like a local file instead of an ephemeral listing position.
--
-- All columns are nullable: a row that has never resolved carries NULL, and a
-- re-list that no longer reports a URL or an identity field preserves the last
-- known value rather than clearing it.
ALTER TABLE public.media_files
    ADD COLUMN IF NOT EXISTS resolved_url text,
    ADD COLUMN IF NOT EXISTS resolved_url_expires_at timestamptz,
    ADD COLUMN IF NOT EXISTS provider_video_hash text,
    ADD COLUMN IF NOT EXISTS provider_guid text,
    ADD COLUMN IF NOT EXISTS provider_release_name text,
    ADD COLUMN IF NOT EXISTS provider_release_size bigint;

-- +goose Down
-- Dropping the columns discards the last resolved provider URL, its expiry,
-- and the durable identity. The row keeps its neutral `?result=` listing
-- identity, so this is safe but lossy: a later phase falls back to re-listing.
ALTER TABLE public.media_files
    DROP COLUMN IF EXISTS resolved_url,
    DROP COLUMN IF EXISTS resolved_url_expires_at,
    DROP COLUMN IF EXISTS provider_video_hash,
    DROP COLUMN IF EXISTS provider_guid,
    DROP COLUMN IF EXISTS provider_release_name,
    DROP COLUMN IF EXISTS provider_release_size;
