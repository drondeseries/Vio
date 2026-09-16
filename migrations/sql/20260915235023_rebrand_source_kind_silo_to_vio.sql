-- +goose NO TRANSACTION
-- +goose Up
-- The rebrand renamed the source kind constant from "silo" to "vio" in Go, but
-- the original CHECK constraint only allowed the legacy values. Accept both
-- "silo" (legacy rows, external clients) and "vio" (new writes) so the old
-- value is not rejected during rolling upgrades or from v1/v2 clients.
ALTER TABLE public.plugin_repositories
    DROP CONSTRAINT IF EXISTS plugin_repositories_source_kind_check;

ALTER TABLE public.plugin_repositories
    ADD CONSTRAINT plugin_repositories_source_kind_check
    CHECK (source_kind IN ('silo', 'vio', 'approved_community', 'external'));
-- +goose Down
ALTER TABLE public.plugin_repositories
    DROP CONSTRAINT IF EXISTS plugin_repositories_source_kind_check;

ALTER TABLE public.plugin_repositories
    ADD CONSTRAINT plugin_repositories_source_kind_check
    CHECK (source_kind IN ('silo', 'approved_community', 'external'));
