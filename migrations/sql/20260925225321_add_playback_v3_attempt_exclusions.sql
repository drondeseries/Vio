-- +goose Up
-- Durable attempt-scoped recovery state for protocol v3 playback. It records
-- the provider candidates this attempt has confirmed as failed, so successive
-- replans union the whole chain instead of depending on the asynchronous
-- failed_at marker landing. A fresh attempt starts with an empty object; the
-- column is written only by the revision-checked append, never by the plan or
-- replan writes, so a stale attempt record can never shrink it.
ALTER TABLE playback_v3_attempts
    ADD COLUMN recovery_state JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN recovery_revision BIGINT NOT NULL DEFAULT 0;

ALTER TABLE playback_v3_attempts
    ADD CONSTRAINT playback_v3_attempts_recovery_state_object
    CHECK (jsonb_typeof(recovery_state) = 'object');

-- +goose Down
ALTER TABLE playback_v3_attempts
    DROP CONSTRAINT IF EXISTS playback_v3_attempts_recovery_state_object;

ALTER TABLE playback_v3_attempts
    DROP COLUMN IF EXISTS recovery_revision,
    DROP COLUMN IF EXISTS recovery_state;
