-- +goose Up
-- +goose StatementBegin
ALTER TABLE media_files ADD COLUMN IF NOT EXISTS copy_safety_multi BOOLEAN;
ALTER TABLE media_files ADD COLUMN IF NOT EXISTS copy_safety_checked_size BIGINT;
ALTER TABLE media_files ADD COLUMN IF NOT EXISTS copy_safety_checked_at TIMESTAMPTZ;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE media_files DROP COLUMN IF EXISTS copy_safety_checked_at;
ALTER TABLE media_files DROP COLUMN IF EXISTS copy_safety_checked_size;
ALTER TABLE media_files DROP COLUMN IF EXISTS copy_safety_multi;
-- +goose StatementEnd
