-- +goose Up
DELETE FROM server_settings WHERE key IN ('playback.virtual_provider_failover', 'virtual_playback_prewarm_enabled');

-- +goose Down
INSERT INTO server_settings (key, value)
VALUES
  ('playback.virtual_provider_failover', 'true'),
  ('virtual_playback_prewarm_enabled', 'false')
ON CONFLICT (key) DO NOTHING;
