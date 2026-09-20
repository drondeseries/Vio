-- +goose Up
-- Persist the request headers a virtual candidate's provider URL needs. Some
-- providers authenticate the stream through a forwarded Referer/Origin/
-- User-Agent rather than a token in the URL, so a stored URL is unusable
-- without the headers the relay would send with it. The column is nullable and
-- purely additive to the neutral `?result=` listing identity; a re-list that
-- omits the headers or a row that never resolved carries NULL, and Phase-1
-- preserve-on-omission semantics apply to it exactly like resolved_url.
ALTER TABLE public.media_files
    ADD COLUMN IF NOT EXISTS provider_request_headers jsonb;

-- +goose Down
-- Dropping the column discards the last stored header set. The row keeps its
-- neutral `?result=` listing identity and the resolved URL, so a later phase
-- falls back to listing and re-resolving (which refreshes the headers).
ALTER TABLE public.media_files
    DROP COLUMN IF EXISTS provider_request_headers;
