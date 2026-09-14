-- +goose Up
-- The requested media file ID is a historical identity, not a live reference:
-- a virtual candidate rotation can delete the requested file row between the
-- client's request and the attempt persist while the effective file survives.
-- The FK forced SaveAttempt to rewrite the requested ID to the effective ID,
-- corrupting the durable record (requested==effective while the plan and
-- normalized request still name the original ID) and breaking idempotent
-- replay of the original file_id. The column stays NOT NULL bigint; it now
-- stores the raw historical ID with no referential guarantee.
ALTER TABLE public.playback_v3_attempts
    DROP CONSTRAINT IF EXISTS playback_v3_attempts_requested_media_file_id_fkey;

-- +goose Down
-- Best-effort restore: re-add the FK only when no orphaned requested IDs
-- exist. Rows persisted after the Up migration may legitimately reference
-- deleted media files, so the constraint is skipped (with a NOTICE) rather
-- than failing the whole rollback.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM public.playback_v3_attempts a
        LEFT JOIN public.media_files mf ON mf.id = a.requested_media_file_id
        WHERE mf.id IS NULL
    ) THEN
        ALTER TABLE public.playback_v3_attempts
            ADD CONSTRAINT playback_v3_attempts_requested_media_file_id_fkey
            FOREIGN KEY (requested_media_file_id) REFERENCES public.media_files(id) ON DELETE CASCADE;
    ELSE
        RAISE NOTICE 'playback_v3_attempts has orphaned requested_media_file_id rows; skipping FK re-add';
    END IF;
END;
$$;
-- +goose StatementEnd
