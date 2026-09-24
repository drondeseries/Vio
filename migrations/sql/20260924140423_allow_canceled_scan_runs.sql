-- +goose NO TRANSACTION
-- +goose Up
-- scanqueue persists StatusCancelled as 'canceled', but 085 constrained
-- scan_runs.status to 'cancelled', so every cancellation UPDATE (MarkCancelled,
-- CancelAcceptedByLibrary) failed the CHECK in production. Widen the constraint
-- to accept both spellings, matching the historyimport precedent
-- (20260922124201_allow_cancelled_history_import_runs). The two-spelling window
-- keeps any row written by an older build readable while new cancellations land
-- as 'canceled'.
ALTER TABLE scan_runs
    DROP CONSTRAINT IF EXISTS scan_runs_status_check,
    ADD CONSTRAINT scan_runs_status_check
        CHECK (status = ANY (ARRAY[
            'accepted'::text,
            'running'::text,
            'completed'::text,
            'failed'::text,
            'cancelled'::text,
            'canceled'::text
        ]))
        NOT VALID;

ALTER TABLE scan_runs
    VALIDATE CONSTRAINT scan_runs_status_check;

-- +goose Down
-- Previous application versions already write canceled; keep accepting both
-- spellings without rewriting terminal runs or stranding cancellation requests.
SELECT 1;
