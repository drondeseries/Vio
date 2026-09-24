package virtuallibrary

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

// IndexerReleaseTTLDefault bounds how long a listed release survives without a
// fresh upsert that bumps its expiry. Callers may set ExpireAt explicitly; a
// zero value falls back to now+TTLDefault so a caller that forgets it still
// gets the documented "short-lived snapshot" behavior instead of an
// immediately-expired row.
const IndexerReleaseTTLDefault = 24 * time.Hour

// MaxIndexerReleasesPerContent caps how many live releases one title keeps.
// Ranking mirrors the display order, so the retained set is the best-scored,
// then newest/largest releases. The refresh job caps its batch to this before
// the upsert, so the per-title work is bounded even when an indexer returns
// hundreds of matches.
const MaxIndexerReleasesPerContent = 25

// maxIndexerReleasesPerContent is the internal spelling used by the store's SQL.
const maxIndexerReleasesPerContent = MaxIndexerReleasesPerContent

// maxIndexerReleasesListed bounds a single List call. Upsert keeps at most
// maxIndexerReleasesPerContent non-queued rows plus any queued rows (the cap
// never drops a pending request), so the bound leaves headroom for queued rows
// instead of hiding a user's request behind the display limit.
const maxIndexerReleasesListed = 2 * maxIndexerReleasesPerContent

// ErrIndexerReleaseNotFound reports that a queued/failed transition targeted a
// row that is missing or already expired. Callers answer a not-found result;
// they must not enqueue against a release that is no longer listed.
var ErrIndexerReleaseNotFound = errors.New("indexer release not found")

// The enqueue states a persisted release can carry. A row starts as
// not-downloaded listing state; a successful request moves it to queued and a
// provider error marks it failed so the client may retry. The store persists
// these values and the HTTP layer projects them unchanged, so the database and
// the wire share one spelling.
const (
	IndexerReleaseStateQueued        = "queued"
	IndexerReleaseStateFailed        = "failed"
	IndexerReleaseStateNotDownloaded = "not_downloaded"
)

// IndexerReleaseScope identifies the catalog title (and, for series, the
// episode) a batch of releases belongs to. EpisodeID is empty for movies.
type IndexerReleaseScope struct {
	ContentID     string
	EpisodeID     string
	MediaFolderID int
}

// IndexerRelease is one Prowlarr search result offered as a requestable
// "not downloaded" entry.
//
// DownloadURL is the server-internal URL used only to enqueue the release with
// the provider. It is deliberately excluded from JSON so no accidental
// serialization to a client can leak it.
type IndexerRelease struct {
	ID             int64
	GUID           string
	Title          string
	NormalizedName string
	Protocol       string
	Indexer        string
	IndexerID      int
	SizeBytes      int64
	FormatScore    *int
	PublishedAt    *time.Time
	DownloadURL    string `json:"-"`
	EnqueueState   string
	NZOID          string
	ExpireAt       time.Time
}

// IndexerReleaseMeta is the display metadata derived from a release's title.
// The stored row carries the raw title; resolution, codecs and HDR are parsed
// on read so the wire projection matches the stream parser every other virtual
// surface uses.
type IndexerReleaseMeta struct {
	Resolution string
	CodecVideo string
	CodecAudio string
	HDR        bool
}

// Meta parses the release title into its display metadata.
func (r IndexerRelease) Meta() IndexerReleaseMeta {
	candidate := stream.StreamCandidate{Name: r.Title, Title: r.Title}
	stream.ParseStreamDetails(&candidate)
	return IndexerReleaseMeta{
		Resolution: candidate.Resolution,
		CodecVideo: candidate.CodecVideo,
		CodecAudio: candidate.CodecAudio,
		HDR:        candidate.HDR != "",
	}
}

// IndexerReleaseStore persists the Prowlarr releases offered for a virtual
// title. It owns virtual_indexer_releases; catalog cannot import
// virtuallibrary, so this package owns the table.
type IndexerReleaseStore struct {
	pool *pgxpool.Pool
}

// NewIndexerReleaseStore builds the store over an existing pool.
func NewIndexerReleaseStore(pool *pgxpool.Pool) *IndexerReleaseStore {
	return &IndexerReleaseStore{pool: pool}
}

// scopeWhere returns the shared (content, episode) predicate and its args. The
// episode comparison coalesces NULL to the empty string so a movie row matches
// scope.EpisodeID == "" and the expression index is usable.
func scopeWhere(scope IndexerReleaseScope) (string, []any) {
	return "content_id = $1 AND COALESCE(episode_id, '') = COALESCE($2, '')",
		[]any{scope.ContentID, nullableText(scope.EpisodeID)}
}

func nullableText(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func normalizeEnqueueState(state string) string {
	switch strings.TrimSpace(state) {
	case IndexerReleaseStateQueued, IndexerReleaseStateFailed:
		return strings.TrimSpace(state)
	default:
		return IndexerReleaseStateNotDownloaded
	}
}

// UpsertIndexerReleases stores one listing batch for the scope. Rows are keyed
// by (content, episode, guid); mutable fields are refreshed and expire_at is
// bumped on every upsert.
//
// A release already queued or failed keeps its enqueue_state, nzo_id and
// enqueued_at on conflict: re-listing a title must not reset a request the user
// already made. Expired rows are pruned on each batch, and the title is capped
// to the best-scored/newest live releases (queued rows are never dropped by the
// cap, so a pending request cannot lose its handle).
func (s *IndexerReleaseStore) UpsertIndexerReleases(ctx context.Context, scope IndexerReleaseScope, releases []IndexerRelease) error {
	if s == nil || s.pool == nil {
		return errors.New("indexer release store is not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin indexer release upsert: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	now := time.Now()
	for i := range releases {
		release := releases[i]
		expireAt := release.ExpireAt
		if expireAt.IsZero() {
			expireAt = now.Add(IndexerReleaseTTLDefault)
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO virtual_indexer_releases (
				content_id, episode_id, media_folder_id, guid, title, normalized_name,
				size_bytes, protocol, indexer, indexer_id, download_url, format_score,
				published_at, enqueue_state, nzo_id, expire_at, updated_at
			) VALUES (
				$1, $2, $3, $4, $5, $6,
				$7, $8, $9, $10, $11, $12,
				$13, $14, $15, $16, now()
			)
			ON CONFLICT (content_id, COALESCE(episode_id, ''), guid) DO UPDATE SET
				media_folder_id = EXCLUDED.media_folder_id,
				title = EXCLUDED.title,
				normalized_name = EXCLUDED.normalized_name,
				size_bytes = EXCLUDED.size_bytes,
				protocol = EXCLUDED.protocol,
				indexer = EXCLUDED.indexer,
				indexer_id = EXCLUDED.indexer_id,
				download_url = EXCLUDED.download_url,
				format_score = EXCLUDED.format_score,
				published_at = EXCLUDED.published_at,
				-- A queued request is a user action, not listing state: preserve
				-- its state, handle, and enqueue time across a re-list. Only rows
				-- still in an un-requested state take the incoming values.
				enqueue_state = CASE
					WHEN virtual_indexer_releases.enqueue_state IN ('queued', 'failed')
						THEN virtual_indexer_releases.enqueue_state
					ELSE EXCLUDED.enqueue_state END,
				nzo_id = CASE
					WHEN virtual_indexer_releases.enqueue_state IN ('queued', 'failed')
						THEN virtual_indexer_releases.nzo_id
					ELSE EXCLUDED.nzo_id END,
				enqueued_at = CASE
					WHEN virtual_indexer_releases.enqueue_state IN ('queued', 'failed')
						THEN virtual_indexer_releases.enqueued_at
					ELSE NULL END,
				expire_at = EXCLUDED.expire_at,
				updated_at = now()
		`,
			scope.ContentID, nullableText(scope.EpisodeID), scope.MediaFolderID, release.GUID,
			release.Title, release.NormalizedName, release.SizeBytes, release.Protocol,
			release.Indexer, release.IndexerID, release.DownloadURL, release.FormatScore,
			release.PublishedAt, normalizeEnqueueState(release.EnqueueState), nullableText(release.NZOID), expireAt,
		)
		if err != nil {
			return fmt.Errorf("upsert indexer release: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, `DELETE FROM virtual_indexer_releases WHERE expire_at <= now()`); err != nil {
		return fmt.Errorf("prune expired indexer releases: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM virtual_indexer_releases r
		WHERE r.content_id = $1 AND COALESCE(r.episode_id, '') = COALESCE($2, '')
		  AND r.enqueue_state <> 'queued'
		  AND r.id NOT IN (
			SELECT id FROM virtual_indexer_releases
			WHERE content_id = $1 AND COALESCE(episode_id, '') = COALESCE($2, '')
			  AND enqueue_state <> 'queued'
			ORDER BY format_score DESC NULLS LAST, size_bytes DESC, id DESC
			LIMIT $3
		  )
	`, scope.ContentID, nullableText(scope.EpisodeID), maxIndexerReleasesPerContent); err != nil {
		return fmt.Errorf("cap indexer releases: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit indexer release upsert: %w", err)
	}
	return nil
}

// GetIndexerRelease returns one live release by its row id, scoped to the
// content (and episode) and media folder. It is the write path's lookup: a
// request names a release the client saw on this exact item, so a row from
// another title or library must never resolve. A missing or expired row returns
// ErrIndexerReleaseNotFound.
func (s *IndexerReleaseStore) GetIndexerRelease(ctx context.Context, scope IndexerReleaseScope, id int64) (IndexerRelease, error) {
	if s == nil || s.pool == nil {
		return IndexerRelease{}, errors.New("indexer release store is not configured")
	}
	where, args := scopeWhere(scope)
	args = append(args, id, scope.MediaFolderID)
	var release IndexerRelease
	var nzoID *string
	err := s.pool.QueryRow(ctx, `
		SELECT id, guid, title, normalized_name, protocol, indexer, indexer_id,
		       size_bytes, format_score, published_at, download_url,
		       enqueue_state, nzo_id, expire_at
		FROM virtual_indexer_releases
		WHERE `+where+` AND id = $3 AND media_folder_id = $4 AND expire_at > now()
	`, args...).Scan(
		&release.ID, &release.GUID, &release.Title, &release.NormalizedName,
		&release.Protocol, &release.Indexer, &release.IndexerID, &release.SizeBytes,
		&release.FormatScore, &release.PublishedAt, &release.DownloadURL,
		&release.EnqueueState, &nzoID, &release.ExpireAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return IndexerRelease{}, ErrIndexerReleaseNotFound
		}
		return IndexerRelease{}, fmt.Errorf("get indexer release: %w", err)
	}
	if nzoID != nil {
		release.NZOID = *nzoID
	}
	return release, nil
}

// ListIndexerReleases returns the live releases for the scope, best-scored
// first (nulls last, then largest), capped to the per-content limit.
func (s *IndexerReleaseStore) ListIndexerReleases(ctx context.Context, scope IndexerReleaseScope) ([]IndexerRelease, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("indexer release store is not configured")
	}
	where, args := scopeWhere(scope)
	args = append(args, maxIndexerReleasesListed)
	rows, err := s.pool.Query(ctx, `
		SELECT id, guid, title, normalized_name, protocol, indexer, indexer_id,
		       size_bytes, format_score, published_at, download_url,
		       enqueue_state, nzo_id, expire_at
		FROM virtual_indexer_releases
		WHERE `+where+` AND expire_at > now()
		ORDER BY format_score DESC NULLS LAST, size_bytes DESC, id DESC
		LIMIT $3
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("list indexer releases: %w", err)
	}
	defer rows.Close()
	var releases []IndexerRelease
	for rows.Next() {
		var release IndexerRelease
		var nzoID *string
		if err := rows.Scan(
			&release.ID, &release.GUID, &release.Title, &release.NormalizedName,
			&release.Protocol, &release.Indexer, &release.IndexerID, &release.SizeBytes,
			&release.FormatScore, &release.PublishedAt, &release.DownloadURL,
			&release.EnqueueState, &nzoID, &release.ExpireAt,
		); err != nil {
			return nil, fmt.Errorf("scan indexer release: %w", err)
		}
		if nzoID != nil {
			release.NZOID = *nzoID
		}
		releases = append(releases, release)
	}
	return releases, rows.Err()
}

// MarkIndexerReleaseQueued records that a release was handed to the provider.
// It is idempotent: a second call for an already-queued release returns the
// existing nzo_id with alreadyQueued true, so the caller answers the same
// result without a second enqueue. A missing or expired row returns
// ErrIndexerReleaseNotFound.
func (s *IndexerReleaseStore) MarkIndexerReleaseQueued(ctx context.Context, scope IndexerReleaseScope, guid, nzoID string) (result string, alreadyQueued bool, err error) {
	if s == nil || s.pool == nil {
		return "", false, errors.New("indexer release store is not configured")
	}
	where, args := scopeWhere(scope)
	var updated string
	err = s.pool.QueryRow(ctx, `
		UPDATE virtual_indexer_releases
		SET enqueue_state = 'queued', nzo_id = $3, enqueued_at = now(), updated_at = now()
		WHERE `+where+` AND guid = $4 AND expire_at > now() AND enqueue_state <> 'queued'
		RETURNING nzo_id
	`, append(args, nzoID, guid)...).Scan(&updated)
	if err == nil {
		return updated, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, fmt.Errorf("mark indexer release queued: %w", err)
	}
	// No row was transitioned. Either it is already queued (idempotent success)
	// or it is missing/expired (not found).
	var state, existing string
	err = s.pool.QueryRow(ctx, `
		SELECT enqueue_state, COALESCE(nzo_id, '')
		FROM virtual_indexer_releases
		WHERE `+where+` AND guid = $3 AND expire_at > now()
	`, append(args, guid)...).Scan(&state, &existing)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, ErrIndexerReleaseNotFound
	}
	if err != nil {
		return "", false, fmt.Errorf("read indexer release for queue: %w", err)
	}
	if state == IndexerReleaseStateQueued {
		return existing, true, nil
	}
	// The row exists but is not queued and the guarded UPDATE matched nothing:
	// this only happens on a concurrent transition. Re-read once by returning
	// not-found rather than enqueuing twice.
	return "", false, ErrIndexerReleaseNotFound
}

// MarkIndexerReleaseFailed records a failed enqueue attempt. reason is accepted
// for the caller's logging and is not persisted (the table has no column for
// it); only the state transition matters for display. A missing or expired row
// returns ErrIndexerReleaseNotFound.
func (s *IndexerReleaseStore) MarkIndexerReleaseFailed(ctx context.Context, scope IndexerReleaseScope, guid, reason string) error {
	if s == nil || s.pool == nil {
		return errors.New("indexer release store is not configured")
	}
	_ = reason
	where, args := scopeWhere(scope)
	tag, err := s.pool.Exec(ctx, `
		UPDATE virtual_indexer_releases
		SET enqueue_state = 'failed', updated_at = now()
		WHERE `+where+` AND guid = $3 AND expire_at > now()
	`, append(args, guid)...)
	if err != nil {
		return fmt.Errorf("mark indexer release failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrIndexerReleaseNotFound
	}
	return nil
}

// PruneExpiredIndexerReleases deletes rows past their expiry. It runs on each
// upsert batch; it is exported so a scheduled task can also sweep between
// listings.
func (s *IndexerReleaseStore) PruneExpiredIndexerReleases(ctx context.Context) (int64, error) {
	if s == nil || s.pool == nil {
		return 0, errors.New("indexer release store is not configured")
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM virtual_indexer_releases WHERE expire_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("prune expired indexer releases: %w", err)
	}
	return tag.RowsAffected(), nil
}
