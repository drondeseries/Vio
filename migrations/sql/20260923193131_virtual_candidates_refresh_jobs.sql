-- +goose Up
-- One active virtual-candidates refresh job per title. The refresh re-lists a
-- virtual item's provider candidates, searches the indexers, and enriches the
-- new rows; two concurrent refreshes of the same title would duplicate the
-- provider and indexer work, so a partial unique index on the request payload's
-- content_id serializes them. The library_refresh migration (048) is the same
-- pattern keyed on library_id.
CREATE UNIQUE INDEX admin_jobs_active_virtual_refresh_idx
ON public.admin_jobs USING btree (job_type, ((request_payload->>'content_id')))
WHERE (
    status = ANY (ARRAY['queued'::text, 'running'::text])
    AND job_type = 'virtual_candidates_refresh'::text
);

-- +goose Down
DROP INDEX IF EXISTS public.admin_jobs_active_virtual_refresh_idx;
