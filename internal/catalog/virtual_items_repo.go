package catalog

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// VirtualItemSummary is one zero-storage virtual library item aggregated with
// the health of its virtual candidates. Episode candidates roll up onto their
// series so a series reports one row per library and owner.
type VirtualItemSummary struct {
	ContentID       string
	Title           string
	ItemType        string
	LibraryID       int
	LibraryName     string
	InstallationID  int
	CandidateCount  int
	FailedCount     int
	LastDeliveredAt *time.Time
	LastSeenAt      *time.Time
	ReleaseNames    []string
}

// ListVirtualItemSummaries reads the bounded admin "Release Desk" view of
// virtual library items. It never returns provider URLs or internal file
// identifiers.
func (r *ItemRepository) ListVirtualItemSummaries(ctx context.Context, limit int) ([]VirtualItemSummary, error) {
	if r == nil || r.pool == nil {
		return nil, errors.New("item repository is not configured")
	}
	if limit <= 0 {
		limit = 100
	} else if limit > 500 {
		limit = 500
	}
	rows, err := r.pool.Query(ctx, `
		WITH virtual_candidates AS (
			SELECT
				COALESCE(ep.series_id, NULLIF(mf.content_id, '')) AS item_id,
				mf.media_folder_id,
				mf.virtual_owner_installation_id,
				mf.failed_at,
				mf.last_delivered_at,
				mf.release_name
			FROM media_files mf
			LEFT JOIN episodes ep ON ep.content_id = mf.episode_id
			WHERE mf.container = 'virtual' OR mf.file_path LIKE 'virtual://%'
		)
		SELECT
			vc.item_id,
			mi.title,
			mi.type,
			vc.media_folder_id,
			COALESCE(folder.name, '') AS library_name,
			COALESCE(MAX(NULLIF(vc.virtual_owner_installation_id, 0)), mi.virtual_owner_installation_id, 0)::int AS installation_id,
			COUNT(*)::int AS candidate_count,
			COUNT(*) FILTER (WHERE vc.failed_at IS NOT NULL)::int AS failed_count,
			MAX(vc.last_delivered_at) AS last_delivered_at,
			mi.virtual_last_seen_at AS last_seen_at,
			COALESCE(
				array_agg(DISTINCT vc.release_name ORDER BY vc.release_name)
					FILTER (WHERE COALESCE(vc.release_name, '') <> ''),
				'{}'::text[]
			) AS release_names
		FROM virtual_candidates vc
		JOIN media_items mi ON mi.content_id = vc.item_id
		LEFT JOIN media_folders folder ON folder.id = vc.media_folder_id
		GROUP BY
			vc.item_id,
			vc.media_folder_id,
			mi.title,
			mi.type,
			mi.virtual_owner_installation_id,
			mi.virtual_last_seen_at,
			folder.name,
			COALESCE(NULLIF(vc.virtual_owner_installation_id, 0), mi.virtual_owner_installation_id, 0)
		ORDER BY MAX(vc.last_delivered_at) DESC NULLS LAST, vc.item_id ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("listing virtual item summaries: %w", err)
	}
	defer rows.Close()
	out := make([]VirtualItemSummary, 0)
	for rows.Next() {
		var summary VirtualItemSummary
		if err := rows.Scan(
			&summary.ContentID,
			&summary.Title,
			&summary.ItemType,
			&summary.LibraryID,
			&summary.LibraryName,
			&summary.InstallationID,
			&summary.CandidateCount,
			&summary.FailedCount,
			&summary.LastDeliveredAt,
			&summary.LastSeenAt,
			&summary.ReleaseNames,
		); err != nil {
			return nil, fmt.Errorf("listing virtual item summaries: %w", err)
		}
		out = append(out, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing virtual item summaries: %w", err)
	}
	return out, nil
}
