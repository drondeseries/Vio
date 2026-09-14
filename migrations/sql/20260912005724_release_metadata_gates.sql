-- +goose Up
CREATE FUNCTION verified_episode_release_at(item_id text, season_no integer, episode_no integer, provider_date timestamptz)
RETURNS timestamptz LANGUAGE sql STABLE AS $$
    WITH identities AS (
        SELECT 'tmdb'::text AS provider, tmdb_id AS provider_id FROM media_items WHERE content_id=item_id
        UNION SELECT 'tvdb', tvdb_id FROM media_items WHERE content_id=item_id
        UNION SELECT 'imdb', imdb_id FROM media_items WHERE content_id=item_id
        UNION SELECT provider, provider_id FROM media_item_provider_ids WHERE content_id=item_id AND item_type='series'
    ), latest AS (
        SELECT DISTINCT ON (h.provider,h.provider_id) h.provider,h.provider_id,h.release_at
        FROM verified_release_override_history h JOIN identities i USING(provider,provider_id)
        WHERE h.media_type='episode' AND h.season_number=season_no AND h.episode_number=episode_no
        ORDER BY h.provider,h.provider_id,h.revision DESC
    )
    SELECT COALESCE((SELECT max(release_at) FROM latest), provider_date)
$$;

CREATE TABLE release_metadata_queue (
    media_type text NOT NULL,
    provider text NOT NULL,
    provider_id text NOT NULL,
    season_number integer NOT NULL,
    episode_number integer NOT NULL,
    reason text NOT NULL CHECK (reason IN ('missing_date','future_date','provider_unavailable','no_home_release')),
    observed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    retry_requested_at timestamptz,
    PRIMARY KEY(media_type,provider,provider_id,season_number,episode_number)
);

-- +goose Down
DROP TABLE release_metadata_queue;
DROP FUNCTION verified_episode_release_at(text,integer,integer,timestamptz);
