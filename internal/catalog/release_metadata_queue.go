package catalog

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type ReleaseMetadataEntry struct {
	ReleaseIdentity
	Reason           string     `json:"reason"`
	ObservedAt       time.Time  `json:"observed_at"`
	RetryRequestedAt *time.Time `json:"retry_requested_at"`
}

func recordReleaseMetadata(ctx context.Context, tx pgx.Tx, ids []ReleaseIdentity, reason string) error {
	if len(ids) == 0 {
		return nil
	}
	mediaTypes := make([]string, 0, len(ids))
	providers := make([]string, 0, len(ids))
	providerIDs := make([]string, 0, len(ids))
	seasons := make([]int32, 0, len(ids))
	episodes := make([]int32, 0, len(ids))
	for _, id := range ids {
		mediaTypes = append(mediaTypes, id.MediaType)
		providers = append(providers, id.Provider)
		providerIDs = append(providerIDs, id.ProviderID)
		seasons = append(seasons, int32(id.SeasonNumber))
		episodes = append(episodes, int32(id.EpisodeNumber))
	}
	if reason == "" {
		_, err := tx.Exec(ctx, `DELETE FROM release_metadata_queue q USING
			unnest($1::text[],$2::text[],$3::text[],$4::int[],$5::int[]) AS del(media_type,provider,provider_id,season_number,episode_number)
			WHERE q.media_type=del.media_type AND q.provider=del.provider AND q.provider_id=del.provider_id
			  AND q.season_number=del.season_number AND q.episode_number=del.episode_number`,
			mediaTypes, providers, providerIDs, seasons, episodes)
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO release_metadata_queue(media_type,provider,provider_id,season_number,episode_number,reason)
		SELECT media_type,provider,provider_id,season_number,episode_number,$6
		FROM unnest($1::text[],$2::text[],$3::text[],$4::int[],$5::int[]) AS inrow(media_type,provider,provider_id,season_number,episode_number)
		ON CONFLICT(media_type,provider,provider_id,season_number,episode_number) DO UPDATE SET reason=EXCLUDED.reason,observed_at=clock_timestamp()`,
		mediaTypes, providers, providerIDs, seasons, episodes, reason)
	return err
}

func (r *ReleaseOverrideRepository) ListNeedsMetadata(ctx context.Context, actor, limit, offset int) ([]ReleaseMetadataEntry, error) {
	if actor <= 0 {
		return nil, ErrReleaseOverrideForbidden
	}
	if limit < 1 || limit > 100 || offset < 0 {
		return nil, ErrInvalidReleaseOverride
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := requireReleaseOverrideAdmin(ctx, tx, actor); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT media_type,provider,provider_id,season_number,episode_number,reason,observed_at,retry_requested_at FROM release_metadata_queue ORDER BY observed_at,media_type,provider,provider_id,season_number,episode_number LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	entries := make([]ReleaseMetadataEntry, 0)
	for rows.Next() {
		var e ReleaseMetadataEntry
		if err := rows.Scan(&e.MediaType, &e.Provider, &e.ProviderID, &e.SeasonNumber, &e.EpisodeNumber, &e.Reason, &e.ObservedAt, &e.RetryRequestedAt); err != nil {
			rows.Close()
			return nil, err
		}
		e.ObservedAt = e.ObservedAt.UTC()
		if e.RetryRequestedAt != nil {
			date := e.RetryRequestedAt.UTC()
			e.RetryRequestedAt = &date
		}
		entries = append(entries, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return entries, tx.Commit(ctx)
}

func enqueueReleaseMetadataRetry(ctx context.Context, tx pgx.Tx, id ReleaseIdentity) (int64, error) {
	result, err := tx.Exec(ctx, `INSERT INTO metadata_refresh_debt(content_id,priority,reason_mask,next_refresh_at,updated_at)
SELECT DISTINCT mi.content_id,300,1,NOW(),NOW() FROM media_items mi
WHERE mi.type=CASE WHEN $1='episode' THEN 'series' ELSE 'movie' END AND (
 ($2='tmdb' AND mi.tmdb_id=$3) OR ($2='tvdb' AND mi.tvdb_id=$3) OR ($2='imdb' AND mi.imdb_id=$3)
 OR EXISTS(SELECT 1 FROM media_item_provider_ids p WHERE p.content_id=mi.content_id AND p.provider=$2 AND p.provider_id=$3 AND p.item_type=mi.type))
ON CONFLICT(target_type,content_id) DO UPDATE SET priority=GREATEST(metadata_refresh_debt.priority,300),reason_mask=metadata_refresh_debt.reason_mask|1,next_refresh_at=NOW(),updated_at=NOW()`, id.MediaType, id.Provider, id.ProviderID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected(), nil
}

func (r *ReleaseOverrideRepository) RetryNeedsMetadata(ctx context.Context, actor int, id ReleaseIdentity) (int64, error) {
	if actor <= 0 {
		return 0, ErrReleaseOverrideForbidden
	}
	if err := id.Validate(); err != nil {
		return 0, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := requireReleaseOverrideAdmin(ctx, tx, actor); err != nil {
		return 0, err
	}
	result, err := tx.Exec(ctx, `UPDATE release_metadata_queue SET retry_requested_at=clock_timestamp() WHERE media_type=$1 AND provider=$2 AND provider_id=$3 AND season_number=$4 AND episode_number=$5`, id.MediaType, id.Provider, id.ProviderID, id.SeasonNumber, id.EpisodeNumber)
	if err != nil {
		return 0, err
	}
	if result.RowsAffected() == 0 {
		return 0, fmt.Errorf("%w: queue entry not found", ErrInvalidReleaseOverride)
	}
	count, err := enqueueReleaseMetadataRetry(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	return count, tx.Commit(ctx)
}
