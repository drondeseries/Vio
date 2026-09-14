-- +goose Up
-- +goose StatementBegin
-- Add notify_ratings toggle to outbound webhooks. When enabled, rating an
-- item (PUT /ratings/{item_id}) fires a rating.set delivery to the webhook.
ALTER TABLE public.notification_webhooks
    ADD COLUMN notify_ratings boolean NOT NULL DEFAULT false;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE public.notification_webhooks
    DROP COLUMN IF EXISTS notify_ratings;
-- +goose StatementEnd
