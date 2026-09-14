-- +goose Up
-- +goose StatementBegin
-- One-time cleanup: virtual media files had the provider display label
-- stuffed into release_name and release_group (both set to the same $11
-- parameter in upsertVirtualFileVariant). Virtual files have no filename
-- to parse a release name from, so both columns should be empty strings
-- (the column default). The code fix stops writing the label for new rows;
-- this migration clears the stale values on existing virtual rows so the
-- version flyout falls back to edition_raw instead of showing the raw
-- display label with emoji and file sizes.
UPDATE public.media_files
SET release_name = '', release_group = ''
WHERE file_path LIKE 'virtual://%' AND (release_name != '' OR release_group != '');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- No-op: the down migration cannot reconstruct the original display labels
-- that were incorrectly stored. Re-running the up migration is also a
-- no-op once the rows are cleared.
SELECT 1;
-- +goose StatementEnd