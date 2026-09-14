package catalog

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/userstore"
)

// NextUpQuery controls what the next-up lookup returns.
type NextUpQuery struct {
	UserID           int
	ProfileID        string
	LibraryID        *int
	LibraryIDs       []int
	SeriesID         string // optional: filter to single series
	AccessFilter     AccessFilter
	Limit            int
	EnableResumable  bool       // include in-progress episodes
	EnableRewatching bool       // accepted but deferred (no-op)
	DateCutoff       *time.Time // only series with activity after this date
}

// NextUpResult is one row from the next-up query.
type NextUpResult struct {
	ContentID     string
	SeriesID      string
	SeriesTitle   string
	SeasonNumber  int
	EpisodeNumber int
	CompletedAt   time.Time // when the preceding episode was completed
	IsResumable   bool      // true if this is an in-progress item (enableResumable)
}

// NextUpRepository queries for next unwatched episodes per series.
type NextUpRepository struct {
	pool          *pgxpool.Pool
	storeProvider userstore.UserStoreProvider
	// statePageSize overrides nextUpStatePageSize for tests that need to drive
	// page boundaries without seeding hundreds of rows. Zero means the default.
	statePageSize int
}

// NewNextUpRepository creates a NextUpRepository.
func NewNextUpRepository(pool *pgxpool.Pool, storeProvider userstore.UserStoreProvider) *NextUpRepository {
	return &NextUpRepository{pool: pool, storeProvider: storeProvider}
}

// Next Up state paging constants.
const (
	// nextUpStatePageSize is the keyset page size requested from the store. The
	// walk may read several pages; the size only bounds one provider round trip.
	nextUpStatePageSize = 500

	// nextUpResumableScanLimit preserves the shipped in-progress scan window for
	// the resumable-first-episode branch. It is a row cap on the store's
	// in-progress listing, not an anchor cap.
	nextUpResumableScanLimit = 100
)

// statePageLimit returns the page size to request from the store.
func (r *NextUpRepository) statePageLimit() int {
	if r.statePageSize > 0 {
		return r.statePageSize
	}
	return nextUpStatePageSize
}

// nextUpEpisode is the catalog metadata for one episode resolved from the
// profile's Next Up state.
type nextUpEpisode struct {
	ContentID     string
	SeriesID      string
	SeriesTitle   string
	SeasonNumber  int
	EpisodeNumber int
}

// nextUpAnchor is the newest visible completion for one series. "Visible"
// means the store already excluded hidden-history rows before it returned the
// entry; catalog visibility means the episode and its parent series resolve.
type nextUpAnchor struct {
	nextUpEpisode
	UpdatedAt time.Time
}

// newerThan reports whether a should anchor the series instead of b. The
// ordering is the anchor contract: newest updated_at wins, then the highest
// (season, episode), then the highest content id.
func (a nextUpAnchor) newerThan(b nextUpAnchor) bool {
	if !a.UpdatedAt.Equal(b.UpdatedAt) {
		return a.UpdatedAt.After(b.UpdatedAt)
	}
	if a.SeasonNumber != b.SeasonNumber {
		return a.SeasonNumber > b.SeasonNumber
	}
	if a.EpisodeNumber != b.EpisodeNumber {
		return a.EpisodeNumber > b.EpisodeNumber
	}
	return a.ContentID > b.ContentID
}

// ListNextUp returns the next unwatched episode per series for the given user.
//
// The profile's progress lives only behind the user store, never in the
// catalog database, so the repository pages the store's NextUpStateStore
// capability and resolves the returned media item ids against Postgres catalog
// metadata. Anchors and the per-series successor are decided in Go after
// paging; the store's pages are keyset-bounded and the walk stops as soon as it
// has enough correctly ordered results or the state is exhausted.
//
// A store that does not implement userstore.NextUpStateStore is a hard error:
// there is no fallback that joins catalog SQL against user-state tables.
func (r *NextUpRepository) ListNextUp(ctx context.Context, q NextUpQuery) ([]NextUpResult, error) {
	if r.storeProvider == nil || q.UserID <= 0 || q.ProfileID == "" {
		return nil, nil
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}

	store, err := r.storeProvider.ForUser(ctx, q.UserID)
	if err != nil {
		return nil, fmt.Errorf("getting user store: %w", err)
	}
	stateStore, ok := store.(userstore.NextUpStateStore)
	if !ok {
		return nil, fmt.Errorf("next up: user store %T does not implement userstore.NextUpStateStore", store)
	}

	var results []NextUpResult
	var anchors map[string]nextUpAnchor
	if q.SeriesID != "" {
		results, anchors, err = r.listNextUpForSeries(ctx, q, stateStore)
	} else {
		results, anchors, err = r.listNextUpGlobal(ctx, q, stateStore, limit)
	}
	if err != nil {
		return nil, err
	}

	if q.EnableResumable {
		resumable, rErr := r.listResumableEpisodes(ctx, q, store, anchors)
		if rErr != nil {
			return nil, rErr
		}

		if q.SeriesID != "" && len(resumable) > 0 {
			// Show-detail tile: the in-progress episode wins over whatever
			// "next aired" row the completed-episodes branch produced.
			return resumable, nil
		}

		// Global next-up: dedup by series, completed-next row takes priority.
		seen := make(map[string]bool, len(results))
		for _, res := range results {
			seen[res.SeriesID] = true
		}
		for _, res := range resumable {
			if !seen[res.SeriesID] {
				results = append(results, res)
			}
		}
	}

	return results, nil
}

// listNextUpGlobal walks the store's state pages newest-first, resolves each
// page against the catalog, and returns the next episode for each eligible
// series in (anchor updated_at DESC, series_id) order.
//
// The walk stops at:
//   - state exhaustion,
//   - the date cutoff (state is updated_at DESC, so every later row is older),
//   - or enough eligible results once no future row can tie the last selected
//     anchor. This replaces the old fixed 960-series ceiling: ineligible series
//     (no successor, suppressed by newer in-progress, hidden, or outside the
//     cutoff) never consume the budget, so an older eligible series is reached.
func (r *NextUpRepository) listNextUpGlobal(
	ctx context.Context,
	q NextUpQuery,
	stateStore userstore.NextUpStateStore,
	limit int,
) ([]NextUpResult, map[string]nextUpAnchor, error) {
	anchors := make(map[string]nextUpAnchor)
	inProgressMax := make(map[string]time.Time)
	stateIDs := make(map[string]struct{})

	var cursor *userstore.NextUpStateCursor
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}

		page, err := stateStore.ListNextUpStatePage(ctx, q.ProfileID, cursor, r.statePageLimit())
		if err != nil {
			return nil, nil, err
		}

		if err := r.accumulateNextUpState(ctx, q, page, anchors, inProgressMax, stateIDs); err != nil {
			return nil, nil, err
		}

		results, err := r.resolveNextUpResults(ctx, q, anchors, inProgressMax, stateIDs, limit)
		if err != nil {
			return nil, nil, err
		}

		cutoffReached := nextUpPageReachedCutoff(q, page)
		if page.Exhausted || cutoffReached {
			return results, anchors, nil
		}
		if len(page.Entries) == 0 || page.Next == nil {
			return results, anchors, nil
		}
		if len(results) >= limit && nextUpPageBelowThreshold(page, results[limit-1].CompletedAt) {
			return results, anchors, nil
		}

		cursor = page.Next
	}
}

// accumulateNextUpState folds one state page into the per-series anchor,
// in-progress, and any-state accumulators. Entries older than the date cutoff
// stop contributing anchors and in-progress timestamps, but still contribute
// their state ids so the successor exclusion covers the whole resolved page.
func (r *NextUpRepository) accumulateNextUpState(
	ctx context.Context,
	q NextUpQuery,
	page userstore.NextUpStatePage,
	anchors map[string]nextUpAnchor,
	inProgressMax map[string]time.Time,
	stateIDs map[string]struct{},
) error {
	if len(page.Entries) == 0 {
		return nil
	}

	resolved, err := r.resolveNextUpEpisodes(ctx, nextUpStateContentIDs(page.Entries))
	if err != nil {
		return err
	}

	cutoffReached := false
	for _, entry := range page.Entries {
		if q.DateCutoff != nil && entry.UpdatedAt.Before(*q.DateCutoff) {
			cutoffReached = true
		}
		meta, ok := resolved[entry.MediaItemID]
		if !ok {
			continue
		}
		stateIDs[entry.MediaItemID] = struct{}{}
		if cutoffReached {
			continue
		}

		candidate := nextUpAnchor{nextUpEpisode: meta, UpdatedAt: entry.UpdatedAt}
		if entry.Completed {
			if current, ok := anchors[meta.SeriesID]; !ok || candidate.newerThan(current) {
				anchors[meta.SeriesID] = candidate
			}
			continue
		}
		if entry.Position > 0 {
			if current, ok := inProgressMax[meta.SeriesID]; !ok || entry.UpdatedAt.After(current) {
				inProgressMax[meta.SeriesID] = entry.UpdatedAt
			}
		}
	}
	return nil
}

// listNextUpForSeries resolves one series' newest visible completion. Unlike
// the global walk it cannot stop at the first eligible result: for the chosen
// series it must page to the end of the profile's state so the successor
// exclusion sees every state row that could follow the anchor. The anchor
// itself is never bounded by recency — a long-idle series still anchors on its
// last completed episode.
//
// This is the deliberate worst case: one series-scoped request reads the
// profile's whole state stream (bounded in memory to that one series' ids).
func (r *NextUpRepository) listNextUpForSeries(
	ctx context.Context,
	q NextUpQuery,
	stateStore userstore.NextUpStateStore,
) ([]NextUpResult, map[string]nextUpAnchor, error) {
	anchors := make(map[string]nextUpAnchor)
	stateIDs := make(map[string]struct{})

	var anchor nextUpAnchor
	haveAnchor := false
	var inProgressMax time.Time
	haveInProgress := false

	var cursor *userstore.NextUpStateCursor
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}

		page, err := stateStore.ListNextUpStatePage(ctx, q.ProfileID, cursor, r.statePageLimit())
		if err != nil {
			return nil, nil, err
		}

		if len(page.Entries) > 0 {
			resolved, err := r.resolveNextUpEpisodes(ctx, nextUpStateContentIDs(page.Entries))
			if err != nil {
				return nil, nil, err
			}
			cutoffReached := false
			for _, entry := range page.Entries {
				if q.DateCutoff != nil && entry.UpdatedAt.Before(*q.DateCutoff) {
					cutoffReached = true
				}
				meta, ok := resolved[entry.MediaItemID]
				if !ok || meta.SeriesID != q.SeriesID {
					continue
				}
				stateIDs[entry.MediaItemID] = struct{}{}
				if cutoffReached {
					continue
				}
				candidate := nextUpAnchor{nextUpEpisode: meta, UpdatedAt: entry.UpdatedAt}
				if entry.Completed {
					if !haveAnchor || candidate.newerThan(anchor) {
						anchor = candidate
						haveAnchor = true
					}
					continue
				}
				if entry.Position > 0 {
					if !haveInProgress || entry.UpdatedAt.After(inProgressMax) {
						inProgressMax = entry.UpdatedAt
						haveInProgress = true
					}
				}
			}
		}

		if page.Exhausted || page.Next == nil {
			break
		}
		cursor = page.Next
	}

	if !haveAnchor {
		return nil, anchors, nil
	}
	anchors[anchor.SeriesID] = anchor
	if !q.EnableResumable && haveInProgress && inProgressMax.After(anchor.UpdatedAt) {
		return nil, anchors, nil
	}

	successors, err := r.lookupNextUpSuccessors(ctx, []nextUpAnchor{anchor}, stateIDs)
	if err != nil {
		return nil, nil, err
	}
	successor, ok := successors[q.SeriesID]
	if !ok {
		return nil, anchors, nil
	}
	return []NextUpResult{nextUpResult(anchor, successor)}, anchors, nil
}

// resolveNextUpResults applies in-progress suppression, orders the surviving
// series by anchor (updated_at DESC, series_id), resolves each successor in one
// batched catalog query, and trims to the limit.
func (r *NextUpRepository) resolveNextUpResults(
	ctx context.Context,
	q NextUpQuery,
	anchors map[string]nextUpAnchor,
	inProgressMax map[string]time.Time,
	stateIDs map[string]struct{},
	limit int,
) ([]NextUpResult, error) {
	candidates := make([]nextUpAnchor, 0, len(anchors))
	for _, anchor := range anchors {
		// When resumable items are disabled, a series with in-progress activity
		// newer than its newest completion is mid-watch; surfacing the episode
		// after the completion would skip past the user's position.
		if !q.EnableResumable {
			if newer, ok := inProgressMax[anchor.SeriesID]; ok && newer.After(anchor.UpdatedAt) {
				continue
			}
		}
		candidates = append(candidates, anchor)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].UpdatedAt.Equal(candidates[j].UpdatedAt) {
			return candidates[i].UpdatedAt.After(candidates[j].UpdatedAt)
		}
		return candidates[i].SeriesID < candidates[j].SeriesID
	})

	successors, err := r.lookupNextUpSuccessors(ctx, candidates, stateIDs)
	if err != nil {
		return nil, err
	}

	results := make([]NextUpResult, 0, len(candidates))
	for _, anchor := range candidates {
		successor, ok := successors[anchor.SeriesID]
		if !ok {
			continue
		}
		results = append(results, nextUpResult(anchor, successor))
	}
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

// nextUpResult builds the response row for an anchor/successor pair.
func nextUpResult(anchor nextUpAnchor, successor nextUpEpisode) NextUpResult {
	return NextUpResult{
		ContentID:     successor.ContentID,
		SeriesID:      anchor.SeriesID,
		SeriesTitle:   anchor.SeriesTitle,
		SeasonNumber:  successor.SeasonNumber,
		EpisodeNumber: successor.EpisodeNumber,
		CompletedAt:   anchor.UpdatedAt,
	}
}

// nextUpStateContentIDs extracts the media item ids from a state page.
func nextUpStateContentIDs(entries []userstore.NextUpStateEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if id := strings.TrimSpace(entry.MediaItemID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// nextUpPageReachedCutoff reports whether any entry on the page is older than
// the query's date cutoff. Because the store pages updated_at DESC, that means
// every subsequent page is older too.
func nextUpPageReachedCutoff(q NextUpQuery, page userstore.NextUpStatePage) bool {
	if q.DateCutoff == nil {
		return false
	}
	for _, entry := range page.Entries {
		if entry.UpdatedAt.Before(*q.DateCutoff) {
			return true
		}
	}
	return false
}

// nextUpPageBelowThreshold reports whether the page's oldest timestamp is
// strictly older than threshold. If so, no future entry can tie or outrank the
// result anchored at threshold, so the walk may stop without skipping a
// deterministic tie held on a later page.
func nextUpPageBelowThreshold(page userstore.NextUpStatePage, threshold time.Time) bool {
	if len(page.Entries) == 0 {
		return true
	}
	return page.Entries[len(page.Entries)-1].UpdatedAt.Before(threshold)
}

// nextUpEpisodeLookupQuery resolves a batch of media item ids to episode and
// parent-series metadata. One query per state page; never per episode.
const nextUpEpisodeLookupQuery = `
	SELECT e.content_id, e.series_id, e.season_number, e.episode_number, si.title
	FROM episodes e
	JOIN media_items si ON si.content_id = e.series_id
	WHERE e.content_id = ANY($1::text[])`

// resolveNextUpEpisodes batch-resolves media item ids against the catalog.
// Ids that are not episodes (or whose series row is absent) are simply absent
// from the result, matching the old join semantics.
func (r *NextUpRepository) resolveNextUpEpisodes(ctx context.Context, contentIDs []string) (map[string]nextUpEpisode, error) {
	resolved := make(map[string]nextUpEpisode, len(contentIDs))
	if len(contentIDs) == 0 {
		return resolved, nil
	}

	rows, err := r.pool.Query(ctx, nextUpEpisodeLookupQuery, contentIDs)
	if err != nil {
		return nil, fmt.Errorf("resolving next-up episodes: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var episode nextUpEpisode
		if err := rows.Scan(
			&episode.ContentID, &episode.SeriesID, &episode.SeasonNumber,
			&episode.EpisodeNumber, &episode.SeriesTitle,
		); err != nil {
			return nil, fmt.Errorf("scanning next-up episode: %w", err)
		}
		resolved[episode.ContentID] = episode
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating next-up episodes: %w", err)
	}
	return resolved, nil
}

// nextUpSuccessorQuery resolves, for every candidate anchor, the first episode
// after it with an available (non-missing) file and no state row. The state
// ids arrive as an array because they live behind the user store; the catalog
// query only supplies the ordering and availability.
//
// The episode tie-break is (season, episode, content_id): episodes of one
// series share the first two, and content_id keeps the pick stable when a
// catalog somehow carries duplicate numbering.
const nextUpSuccessorQuery = `
	SELECT anchor.series_id, next_ep.content_id, next_ep.season_number, next_ep.episode_number
	FROM unnest($1::text[], $2::int[], $3::int[]) AS anchor(series_id, season_number, episode_number)
	JOIN media_items si ON si.content_id = anchor.series_id
	JOIN LATERAL (
		SELECT e2.content_id, e2.season_number, e2.episode_number
		FROM episodes e2
		WHERE e2.series_id = anchor.series_id
		  AND (e2.season_number, e2.episode_number) > (anchor.season_number, anchor.episode_number)
		  AND EXISTS (
			  SELECT 1 FROM media_files mf
			  WHERE mf.episode_id = e2.content_id AND mf.missing_since IS NULL
		  )
		  AND NOT (e2.content_id = ANY($4::text[]))
		ORDER BY e2.season_number, e2.episode_number, e2.content_id
		LIMIT 1
	) next_ep ON TRUE`

// lookupNextUpSuccessors resolves the successor for each candidate anchor in
// one query and returns them keyed by series id.
func (r *NextUpRepository) lookupNextUpSuccessors(
	ctx context.Context,
	candidates []nextUpAnchor,
	stateIDs map[string]struct{},
) (map[string]nextUpEpisode, error) {
	successors := make(map[string]nextUpEpisode, len(candidates))
	if len(candidates) == 0 {
		return successors, nil
	}

	seriesIDs := make([]string, len(candidates))
	seasons := make([]int, len(candidates))
	episodes := make([]int, len(candidates))
	for i, candidate := range candidates {
		seriesIDs[i] = candidate.SeriesID
		seasons[i] = candidate.SeasonNumber
		episodes[i] = candidate.EpisodeNumber
	}
	excluded := make([]string, 0, len(stateIDs))
	for id := range stateIDs {
		excluded = append(excluded, id)
	}

	rows, err := r.pool.Query(ctx, nextUpSuccessorQuery, seriesIDs, seasons, episodes, excluded)
	if err != nil {
		return nil, fmt.Errorf("resolving next-up successors: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var seriesID string
		var episode nextUpEpisode
		if err := rows.Scan(&seriesID, &episode.ContentID, &episode.SeasonNumber, &episode.EpisodeNumber); err != nil {
			return nil, fmt.Errorf("scanning next-up successor: %w", err)
		}
		episode.SeriesID = seriesID
		successors[seriesID] = episode
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating next-up successors: %w", err)
	}
	return successors, nil
}

// listResumableEpisodes finds in-progress episodes for the profile.
//
// Two callers, two semantics, one listing:
//
//   - Global next-up (q.SeriesID == ""): only contribute series the profile
//     has no completion for. Series with a completion go through the
//     "next unwatched after the last completed" path; this branch fills the gap
//     for series that have no completed anchor yet.
//   - Show-detail tile (q.SeriesID != ""): the in-progress episode wins
//     unconditionally, even when the user already finished earlier episodes of
//     the same series. ListNextUp flips its dedup priority to let this row win.
//
// The in-progress rows come from the user store (ListProgress), not from
// catalog SQL, so a SQLite-backed profile behaves the same as a Postgres one.
func (r *NextUpRepository) listResumableEpisodes(
	ctx context.Context,
	q NextUpQuery,
	store userstore.UserStore,
	completedSeries map[string]nextUpAnchor,
) ([]NextUpResult, error) {
	if store == nil {
		return nil, nil
	}

	entries, err := store.ListProgress(ctx, q.ProfileID, "in_progress", nextUpResumableScanLimit, 0)
	if err != nil {
		return nil, fmt.Errorf("listing in-progress: %w", err)
	}
	if len(entries) == 0 {
		return nil, nil
	}

	contentIDs := make([]string, 0, len(entries))
	updatedAt := make(map[string]time.Time, len(entries))
	for _, entry := range entries {
		id := strings.TrimSpace(entry.MediaItemID)
		if id == "" {
			continue
		}
		at, parseErr := time.Parse(time.RFC3339, entry.UpdatedAt)
		if parseErr != nil || at.IsZero() {
			continue
		}
		contentIDs = append(contentIDs, id)
		updatedAt[id] = at.UTC()
	}
	if len(contentIDs) == 0 {
		return nil, nil
	}

	resolved, err := r.resolveNextUpEpisodes(ctx, contentIDs)
	if err != nil {
		return nil, err
	}

	newest := make(map[string]nextUpEpisode, len(resolved))
	newestAt := make(map[string]time.Time, len(resolved))
	for _, id := range contentIDs {
		episode, ok := resolved[id]
		if !ok {
			continue
		}
		at := updatedAt[id]
		if current, ok := newestAt[episode.SeriesID]; !ok || at.After(current) {
			newest[episode.SeriesID] = episode
			newestAt[episode.SeriesID] = at
		}
	}

	seriesIDs := make([]string, 0, len(newest))
	for seriesID := range newest {
		seriesIDs = append(seriesIDs, seriesID)
	}
	sort.Strings(seriesIDs)

	results := make([]NextUpResult, 0, len(seriesIDs))
	for _, seriesID := range seriesIDs {
		if q.SeriesID != "" && seriesID != q.SeriesID {
			continue
		}
		if q.SeriesID == "" {
			if _, completed := completedSeries[seriesID]; completed {
				continue
			}
		}
		episode := newest[seriesID]
		results = append(results, NextUpResult{
			ContentID:     episode.ContentID,
			SeriesID:      episode.SeriesID,
			SeriesTitle:   episode.SeriesTitle,
			SeasonNumber:  episode.SeasonNumber,
			EpisodeNumber: episode.EpisodeNumber,
			CompletedAt:   newestAt[seriesID],
			IsResumable:   true,
		})
	}
	return results, nil
}
