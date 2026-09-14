-- +goose Up
-- +goose StatementBegin
-- Release name and group: the file stem and its trailing release-group tag,
-- filled by the scanner from the filename and by virtual registration from
-- the provider's release label. Both are display-only metadata surfaced in
-- the version flyout; edition/presentation columns remain the matching keys.

ALTER TABLE public.media_files
ADD COLUMN IF NOT EXISTS release_name TEXT NOT NULL DEFAULT '',
ADD COLUMN IF NOT EXISTS release_group TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE public.media_files
DROP COLUMN IF EXISTS release_name,
DROP COLUMN IF EXISTS release_group;
-- +goose StatementEnd