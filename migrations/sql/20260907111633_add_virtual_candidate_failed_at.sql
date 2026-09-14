-- +goose Up
-- Marks a virtual candidate row as known-bad after a transport produced no
-- bytes (corrupted NZB, dead provider URL). NULL = not failed; a timestamp
-- means the release failed at stream-open. The flag is a runtime signal, not
-- permanent: a fresh candidate listing clears it, and the auto-pick skips
-- failed candidates while the dropdown still shows them (clickable) so the
-- user can retry.
ALTER TABLE public.media_files
    ADD COLUMN IF NOT EXISTS failed_at timestamptz;

-- +goose Down
ALTER TABLE public.media_files
    DROP COLUMN IF EXISTS failed_at;
