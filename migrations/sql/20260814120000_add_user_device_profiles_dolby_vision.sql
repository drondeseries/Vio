-- +goose Up
-- +goose StatementBegin
ALTER TABLE public.user_device_profiles
    ADD COLUMN IF NOT EXISTS dolby_vision boolean NOT NULL DEFAULT false;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE public.user_device_profiles
    DROP COLUMN IF EXISTS dolby_vision;
-- +goose StatementEnd
