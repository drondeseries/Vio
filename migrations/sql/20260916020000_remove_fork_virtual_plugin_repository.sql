-- +goose Up
-- +goose StatementBegin
-- com.drondeseries.vio-virtual-library is now core in Vio. Remove the fork
-- repository from managed plugin channels so it is no longer polled or offered.
DELETE FROM public.plugin_repositories
WHERE url = 'https://raw.githubusercontent.com/drondeseries/vio-virtual-library/main/catalog.json'
   OR managed_key = 'fork-virtual-library';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
INSERT INTO public.plugin_repositories (url, display_name, enabled, managed_key, source_kind)
VALUES (
    'https://raw.githubusercontent.com/drondeseries/vio-virtual-library/main/catalog.json',
    'Vio Virtual Library',
    false,
    'fork-virtual-library',
    'vio'
)
ON CONFLICT (url) DO NOTHING;
-- +goose StatementEnd
