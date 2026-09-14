-- +goose Up
CREATE TABLE verified_release_override_history (
    media_type text NOT NULL CHECK (media_type IN ('movie', 'episode')),
    provider text NOT NULL CHECK (provider IN ('tmdb', 'tvdb', 'imdb')),
    provider_id text NOT NULL CHECK (
        (provider = 'imdb' AND provider_id ~ '^tt[0-9]{1,16}$') OR
        (provider IN ('tmdb', 'tvdb') AND provider_id ~ '^[1-9][0-9]{0,19}$')
    ),
    season_number integer NOT NULL,
    episode_number integer NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    action text NOT NULL CHECK (action IN ('set', 'change', 'clear')),
    release_at timestamptz,
    evidence_note text NOT NULL CHECK (length(btrim(evidence_note)) > 0 AND octet_length(evidence_note) <= 4096),
    actor_account_id integer NOT NULL CHECK (actor_account_id > 0),
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (media_type, provider, provider_id, season_number, episode_number, revision),
    CHECK ((media_type = 'movie' AND provider <> 'tvdb' AND season_number = 0 AND episode_number = 0) OR
           (media_type = 'episode' AND season_number BETWEEN 1 AND 100000 AND episode_number BETWEEN 1 AND 100000)),
    CHECK ((action = 'clear' AND release_at IS NULL) OR (action <> 'clear' AND release_at IS NOT NULL))
);

-- +goose StatementBegin
CREATE FUNCTION reject_verified_release_history_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'verified release history is append-only';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER verified_release_history_immutable
BEFORE UPDATE OR DELETE OR TRUNCATE ON verified_release_override_history
FOR EACH STATEMENT EXECUTE FUNCTION reject_verified_release_history_mutation();

-- +goose Down
DROP TABLE verified_release_override_history;
DROP FUNCTION reject_verified_release_history_mutation();
