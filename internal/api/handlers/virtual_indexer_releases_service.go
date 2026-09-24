package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// VirtualIndexerReleaseEnqueue hands a release's server-stored download URL to
// the provider and returns the provider's job id. The URL is never a client
// input.
type VirtualIndexerReleaseEnqueue func(ctx context.Context, downloadURL, name string) (string, error)

// VirtualIndexerReleaseService reads the persisted indexer releases offered for
// an item and requests one on the provider. It owns scope resolution so the
// apiv2 layer never constructs a database scope itself.
type VirtualIndexerReleaseService struct {
	// Store is the persisted indexer-release table.
	Store *virtuallibrary.IndexerReleaseStore
	// Enqueue hands the stored URL to the provider. Nil makes every request a
	// service-unavailable failure rather than a silent no-op.
	Enqueue VirtualIndexerReleaseEnqueue
	// Detail enforces access and identifies the item through the same path the
	// version list uses.
	Detail VirtualCandidatesDetailReader
	// ContentFiles / EpisodeFiles resolve the item's media files so the
	// request lookup is scoped to the item's media folder. Nil disables that
	// lookup (the content/episode scope still prevents cross-item reuse).
	ContentFiles VirtualCandidateFiles
	EpisodeFiles VirtualCandidateFiles
}

// IndexerReleaseView is the handler-safe projection of one persisted release.
// DownloadURL is deliberately not carried, so no caller can echo it.
type IndexerReleaseView struct {
	ReleaseID     string
	Title         string
	Resolution    string
	CodecVideo    string
	CodecAudio    string
	HDR           bool
	SizeBytes     int64
	Indexer       string
	PublishedAt   *time.Time
	FormatScore   *int
	Protocol      string
	DownloadState string
}

// IndexerReleaseRequestResult is the request action's outcome.
type IndexerReleaseRequestResult struct {
	ReleaseID string
	State     string
	Message   string
}

// IndexerReleasesForWatch is the optional watch-detail seam. It returns the
// live indexer releases for the item, or nil when the store is unwired so the
// watch detail stays unchanged.
func (s *VirtualIndexerReleaseService) IndexerReleasesForWatch(ctx context.Context, contentID string) ([]IndexerReleaseView, error) {
	if s == nil || s.Store == nil {
		return nil, nil
	}
	return s.ListItemIndexerReleases(ctx, contentID)
}

// ListItemIndexerReleases returns the live indexer releases offered for the
// item the client reads. The scope is read from the item's virtual source rows
// (which carry the correct content/episode/folder tuple for movies and
// episodes alike); a missing lookup falls back to the item id interpreted as
// both a movie and an episode scope.
func (s *VirtualIndexerReleaseService) ListItemIndexerReleases(ctx context.Context, contentID string) ([]IndexerReleaseView, error) {
	if s == nil || s.Store == nil {
		return nil, apiError(http.StatusServiceUnavailable, "unavailable", "Indexer releases are unavailable")
	}
	seen := map[int64]bool{}
	views := make([]IndexerReleaseView, 0)
	for _, scope := range s.itemScopes(ctx, contentID) {
		releases, err := s.Store.ListIndexerReleases(ctx, scope)
		if err != nil {
			return nil, apiError(http.StatusInternalServerError, "internal_error", "Failed to load indexer releases")
		}
		for _, release := range releases {
			if seen[release.ID] {
				continue
			}
			seen[release.ID] = true
			views = append(views, indexerReleaseViewOf(release))
		}
	}
	return views, nil
}

// itemScopes returns the distinct indexer-release scopes the item's virtual
// source rows identify. Reading the scope from the rows is the only reliable
// mapping for an episode, whose media_id is the episode id while its virtual
// files carry content_id = series id and episode_id = episode id.
func (s *VirtualIndexerReleaseService) itemScopes(ctx context.Context, contentID string) []virtuallibrary.IndexerReleaseScope {
	files := s.itemFiles(ctx, contentID)
	seen := map[string]bool{}
	scopes := make([]virtuallibrary.IndexerReleaseScope, 0, len(files))
	for _, file := range files {
		if file == nil || !isVirtualPlaybackFile(file) {
			continue
		}
		if file.ContentID == "" || file.MediaFolderID <= 0 {
			continue
		}
		key := file.ContentID + "\x00" + file.EpisodeID + "\x00" + strconv.Itoa(file.MediaFolderID)
		if seen[key] {
			continue
		}
		seen[key] = true
		scopes = append(scopes, virtuallibrary.IndexerReleaseScope{ContentID: file.ContentID, EpisodeID: file.EpisodeID, MediaFolderID: file.MediaFolderID})
	}
	if len(scopes) == 0 {
		// No virtual source rows are visible (no lookup wired, or the item is
		// not virtual): try the item id as both shapes. The store's scope
		// predicate keeps this from matching another title's rows.
		return virtualItemIndexerScopes(contentID, 0)
	}
	return scopes
}

// itemFiles collects the item's media files through both lookups.
func (s *VirtualIndexerReleaseService) itemFiles(ctx context.Context, contentID string) []*models.MediaFile {
	var files []*models.MediaFile
	if s.ContentFiles != nil {
		if contentFiles, err := s.ContentFiles(ctx, contentID); err == nil {
			files = append(files, contentFiles...)
		}
	}
	if s.EpisodeFiles != nil {
		if episodeFiles, err := s.EpisodeFiles(ctx, contentID); err == nil {
			files = append(files, episodeFiles...)
		}
	}
	return files
}

// RequestIndexerRelease resolves one stored release scoped to the item,
// re-checks access through the watch-detail path, enqueues the server-stored
// URL, and records the provider job id. It is idempotent: an already-queued
// release returns its existing state without a second enqueue.
func (s *VirtualIndexerReleaseService) RequestIndexerRelease(ctx context.Context, userID int, profileID, contentID string, releaseID int64, filter catalog.AccessFilter) (IndexerReleaseRequestResult, error) {
	if s == nil || s.Store == nil || s.Enqueue == nil || s.Detail == nil {
		return IndexerReleaseRequestResult{}, apiError(http.StatusServiceUnavailable, "unavailable", "Indexer release requests are unavailable")
	}
	// Access and item identity come from the same detail read the version list
	// uses. A foreign or unknown item is answered there.
	if _, err := s.Detail.WatchDetail(ctx, userID, profileID, contentID, filter); err != nil {
		return IndexerReleaseRequestResult{}, err
	}
	release, scope, err := s.findRelease(ctx, contentID, releaseID)
	if err != nil {
		return IndexerReleaseRequestResult{}, err
	}
	if release.EnqueueState == virtuallibrary.IndexerReleaseStateQueued {
		return IndexerReleaseRequestResult{ReleaseID: idOfIndexerRelease(release.ID), State: virtuallibrary.IndexerReleaseStateQueued}, nil
	}
	nzoID, err := s.Enqueue(ctx, release.DownloadURL, release.Title)
	if err != nil {
		// A failed enqueue is recorded so the row shows retryable and the
		// caller can try again; the provider's message is not echoed.
		if markErr := s.Store.MarkIndexerReleaseFailed(ctx, scope, release.GUID, "enqueue failed"); markErr != nil {
			_ = markErr
		}
		return IndexerReleaseRequestResult{ReleaseID: idOfIndexerRelease(release.ID), State: virtuallibrary.IndexerReleaseStateFailed, Message: "The provider could not queue this release; try again."}, nil
	}
	if _, _, err := s.Store.MarkIndexerReleaseQueued(ctx, scope, release.GUID, nzoID); err != nil {
		if errors.Is(err, virtuallibrary.ErrIndexerReleaseNotFound) {
			return IndexerReleaseRequestResult{}, apiError(http.StatusNotFound, "not_found", "Release not found")
		}
		return IndexerReleaseRequestResult{}, apiError(http.StatusInternalServerError, "internal_error", "Failed to record the release request")
	}
	return IndexerReleaseRequestResult{ReleaseID: idOfIndexerRelease(release.ID), State: virtuallibrary.IndexerReleaseStateQueued}, nil
}

// findRelease resolves the stored row by id, scoped to the item's content id
// (and episode id where applicable) and media folder, derived from the item's
// own virtual source rows. A release persisted under another title or library
// never resolves.
func (s *VirtualIndexerReleaseService) findRelease(ctx context.Context, contentID string, releaseID int64) (virtuallibrary.IndexerRelease, virtuallibrary.IndexerReleaseScope, error) {
	for _, scope := range s.itemScopes(ctx, contentID) {
		release, err := s.Store.GetIndexerRelease(ctx, scope, releaseID)
		if err == nil {
			return release, scope, nil
		}
		if !errors.Is(err, virtuallibrary.ErrIndexerReleaseNotFound) {
			return virtuallibrary.IndexerRelease{}, scope, apiError(http.StatusInternalServerError, "internal_error", "Failed to load the release")
		}
	}
	return virtuallibrary.IndexerRelease{}, virtuallibrary.IndexerReleaseScope{}, apiError(http.StatusNotFound, "not_found", "Release not found")
}

// virtualItemIndexerScopes returns the movie and episode scopes for one item
// id. The item id is the client's media_id, which is also the episode id for an
// episode; the two scopes never overlap.
func virtualItemIndexerScopes(contentID string, folderID int) []virtuallibrary.IndexerReleaseScope {
	return []virtuallibrary.IndexerReleaseScope{
		{ContentID: contentID, EpisodeID: "", MediaFolderID: folderID},
		{ContentID: contentID, EpisodeID: contentID, MediaFolderID: folderID},
	}
}

// IndexerReleaseScopeForItem is the scope the refresh job writes for one item.
func IndexerReleaseScopeForItem(contentID, episodeID string, folderID int) virtuallibrary.IndexerReleaseScope {
	return virtuallibrary.IndexerReleaseScope{ContentID: contentID, EpisodeID: episodeID, MediaFolderID: folderID}
}

func indexerReleaseViewOf(release virtuallibrary.IndexerRelease) IndexerReleaseView {
	meta := release.Meta()
	return IndexerReleaseView{
		ReleaseID:     idOfIndexerRelease(release.ID),
		Title:         release.Title,
		Resolution:    meta.Resolution,
		CodecVideo:    meta.CodecVideo,
		CodecAudio:    meta.CodecAudio,
		HDR:           meta.HDR,
		SizeBytes:     release.SizeBytes,
		Indexer:       release.Indexer,
		PublishedAt:   release.PublishedAt,
		FormatScore:   release.FormatScore,
		Protocol:      release.Protocol,
		DownloadState: normalizeIndexerDownloadState(release.EnqueueState),
	}
}

func normalizeIndexerDownloadState(state string) string {
	switch strings.TrimSpace(state) {
	case virtuallibrary.IndexerReleaseStateQueued, virtuallibrary.IndexerReleaseStateFailed:
		return strings.TrimSpace(state)
	default:
		return virtuallibrary.IndexerReleaseStateNotDownloaded
	}
}

func idOfIndexerRelease(id int64) string {
	return strconv.FormatInt(id, 10)
}
