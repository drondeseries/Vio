-- +goose Up
DROP TABLE IF EXISTS public.virtual_stream_metadata;

-- +goose Down
CREATE TABLE IF NOT EXISTS public.virtual_stream_metadata (
    content_id  text PRIMARY KEY,
    container   text NOT NULL DEFAULT '',
    codec_video text NOT NULL DEFAULT '',
    codec_audio text NOT NULL DEFAULT '',
    video_range text NOT NULL DEFAULT '',
    resolution  text NOT NULL DEFAULT '',
    probe_count integer NOT NULL DEFAULT 1,
    updated_at  timestamp with time zone NOT NULL DEFAULT now()
);
