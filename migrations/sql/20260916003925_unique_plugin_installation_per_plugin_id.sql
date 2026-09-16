-- +goose NO TRANSACTION
-- +goose Up
-- InstallRemote never checked for an existing installation before creating a
-- new row, so repeated clicks produced duplicate plugin_installations rows.
-- Clean up any existing duplicates (keep the lowest id per plugin_id, which is
-- the one with runtime configs and active bindings), then enforce one non-builtin
-- installation per plugin_id at the schema level.
DELETE FROM public.plugin_installations
WHERE id IN (
    SELECT pi.id
    FROM public.plugin_installations pi
    INNER JOIN (
        SELECT plugin_id, MIN(id) AS keep_id
        FROM public.plugin_installations
        GROUP BY plugin_id
    ) dup ON pi.plugin_id = dup.plugin_id AND pi.id != dup.keep_id
    WHERE pi.kind = 'plugin'
);

CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS
    idx_plugin_installations_plugin_id
    ON public.plugin_installations (plugin_id)
    WHERE kind = 'plugin';
-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS public.idx_plugin_installations_plugin_id;
