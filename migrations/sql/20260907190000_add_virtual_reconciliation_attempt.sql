-- +goose Up
ALTER TABLE public.media_items
    ADD COLUMN IF NOT EXISTS virtual_reconciliation_attempted_at timestamptz;

-- Separate progress cursor for scheduled episode reconciliation so collection
-- repair and episode reconciliation never postpone each other.
ALTER TABLE public.media_items
    ADD COLUMN IF NOT EXISTS virtual_episode_reconciliation_attempted_at timestamptz;

ALTER TABLE public.virtual_media_source_claims
    ADD COLUMN IF NOT EXISTS staged_until timestamptz;

ALTER TABLE public.virtual_media_file_source_claims
    ADD COLUMN IF NOT EXISTS staged_until timestamptz;

CREATE INDEX IF NOT EXISTS idx_media_items_virtual_reconciliation_attempt
    ON public.media_items (virtual_reconciliation_attempted_at, content_id)
    WHERE virtual_reconciliation_attempted_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_media_items_virtual_episode_recon_attempt
    ON public.media_items (virtual_episode_reconciliation_attempted_at, content_id)
    WHERE virtual_episode_reconciliation_attempted_at IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS public.idx_media_items_virtual_episode_recon_attempt;
DROP INDEX IF EXISTS public.idx_media_items_virtual_reconciliation_attempt;
ALTER TABLE public.media_items
    DROP COLUMN IF EXISTS virtual_episode_reconciliation_attempted_at;
ALTER TABLE public.media_items
    DROP COLUMN IF EXISTS virtual_reconciliation_attempted_at;
ALTER TABLE public.virtual_media_file_source_claims
    DROP COLUMN IF EXISTS staged_until;
ALTER TABLE public.virtual_media_source_claims
    DROP COLUMN IF EXISTS staged_until;
