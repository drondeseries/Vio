-- +goose Up
-- +goose StatementBegin
-- SiloDB: a content-keyed registry of provider-stream metadata. When a virtual
-- stream is probed, its container/codecs/range/resolution are recorded against
-- the content it belongs to. A later first play of a DIFFERENT provider stream
-- for the same content can then decide the route (stream-copy remux vs QSV
-- transcode) and skip the provider probe entirely, since the codec facts most
-- content variants share are already known. Per-stream probe detail stays in
-- media_files; this table only carries the aggregate routing signal.
CREATE TABLE IF NOT EXISTS public.virtual_stream_metadata (
    content_id   text NOT NULL,
    container    text NOT NULL DEFAULT '',
    codec_video  text NOT NULL DEFAULT '',
    codec_audio  text NOT NULL DEFAULT '',
    video_range  text NOT NULL DEFAULT '',
    resolution   text NOT NULL DEFAULT '',
    probe_count  integer NOT NULL DEFAULT 0,
    updated_at   timestamp with time zone NOT NULL DEFAULT now(),
    CONSTRAINT virtual_stream_metadata_pkey PRIMARY KEY (content_id)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS public.virtual_stream_metadata;
-- +goose StatementEnd