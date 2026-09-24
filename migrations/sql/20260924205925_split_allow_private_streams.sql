-- +goose Up
-- +goose StatementBegin
-- Split virtual_library.allow_insecure_http into HTTP-manifest and private
-- stream-destination permissions. Before this migration the single toggle
-- controlled both: HTTP manifests on private hosts AND the SSRF bypass on
-- resolved stream URLs. After the split, allow_insecure_http covers HTTP
-- manifests on private/local hosts only, and allow_private_streams covers
-- private stream destinations.
--
-- Preserve the pre-split intent: an installation that had the broad opt-in
-- enabled keeps private-stream playback working via the new key.
INSERT INTO server_settings (key, value)
SELECT 'virtual_library.allow_private_streams', value
FROM server_settings
WHERE key = 'virtual_library.allow_insecure_http'
ON CONFLICT (key) DO NOTHING;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM server_settings WHERE key = 'virtual_library.allow_private_streams';
-- +goose StatementEnd
