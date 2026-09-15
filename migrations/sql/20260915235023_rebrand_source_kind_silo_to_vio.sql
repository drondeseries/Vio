-- +goose NO TRANSACTION
-- +goose Up
-- The rebrand renamed the source kind constant from "silo" to "vio" in Go, but
-- the CHECK constraint created in 20260709191109 still only allows the legacy
-- values. Drop the old constraint first so the UPDATE doesn't violate it, then
-- add the new constraint.
ALTER TABLE public.plugin_repositories
    DROP CONSTRAINT IF EXISTS plugin_repositories_source_kind_check;

UPDATE public.plugin_repositories
SET source_kind = 'vio',
    updated_at = NOW()
WHERE source_kind = 'silo';

ALTER TABLE public.plugin_repositories
    ADD CONSTRAINT plugin_repositories_source_kind_check
    CHECK (source_kind IN ('vio', 'approved_community', 'external'));
-- +goose Down
UPDATE public.plugin_repositories
SET source_kind = 'silo',
    updated_at = NOW()
WHERE source_kind = 'vio';

ALTER TABLE public.plugin_repositories
    DROP CONSTRAINT IF EXISTS plugin_repositories_source_kind_check;

ALTER TABLE public.plugin_repositories
    ADD CONSTRAINT plugin_repositories_source_kind_check
    CHECK (source_kind IN ('silo', 'approved_community', 'external'));
