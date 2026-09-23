package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
)

// virtualCandidatesRefreshTimeout bounds one source's provider re-list so a
// hung provider cannot hold the request (or a coalesced waiter) forever.
const virtualCandidatesRefreshTimeout = 30 * time.Second

// ErrVirtualRefreshProvider marks a provider-side refresh failure: the caller
// answers a retryable dependency problem and never touches the persisted rows.
var ErrVirtualRefreshProvider = errors.New("virtual candidate provider refresh failed")

// VirtualCandidateFiles resolves an item's media files by content or episode
// id. Either may be nil when the store cannot answer for that kind.
type VirtualCandidateFiles func(ctx context.Context, id string) ([]*models.MediaFile, error)

// VirtualCandidatesDetailReader is the watch-detail read the version-list
// surface uses; *ItemsHandler implements it.
type VirtualCandidatesDetailReader interface {
	WatchDetail(ctx context.Context, userID int, profileID, contentID string, filter catalog.AccessFilter) (*catalog.WatchDetail, error)
}

// VirtualCandidatesRefreshService is the "Refresh List" action: it force-lists
// a virtual item's provider candidates and persists them through the existing
// ReplaceVirtualCandidates path, preserving every known-working row, then
// answers the post-refresh version list in the watch-detail shape.
//
// The provider work is coalesced per source group, so concurrent refreshes of
// the same title share one provider round-trip instead of storming it. A
// provider failure is retryable and never destructive: the persisted rows are
// only replaced by a successful fresh listing.
type VirtualCandidatesRefreshService struct {
	// ListFresh force-lists provider candidates for one virtual source path.
	ListFresh VirtualPlaybackStreamLister
	// Persist replaces the stored candidates for one source row; it is the same
	// ReplaceVirtualCandidates sink the playback path uses.
	Persist VirtualPlaybackStreamSink
	// ContentFiles / EpisodeFiles resolve the item's media files so the virtual
	// source rows can be found. Nil disables that lookup.
	ContentFiles VirtualCandidateFiles
	EpisodeFiles VirtualCandidateFiles
	// Detail reads the pre- and post-refresh watch detail, which also enforces
	// access and produces the version wire shape.
	Detail VirtualCandidatesDetailReader

	flight singleflight.Group
}

// RefreshVirtualCandidates re-lists and persists the item's virtual candidates
// and returns the retained list. It is the handler behind
// POST /api/v2/media/{media_id}/virtual-candidates:refresh.
// virtualURIScheme is the URI scheme of persisted virtual candidate rows.
const virtualURIScheme = "virtual"

func (s *VirtualCandidatesRefreshService) RefreshVirtualCandidates(ctx context.Context, userID int, profileID, contentID string, filter catalog.AccessFilter) ([]catalog.FileVersion, error) {
	if s == nil || s.ListFresh == nil || s.Persist == nil || s.Detail == nil {
		return nil, apiError(http.StatusServiceUnavailable, "unavailable", "Virtual candidate refresh is unavailable")
	}
	// The pre-refresh read validates the target and the caller's access through
	// the same path the version list uses.
	if _, err := s.Detail.WatchDetail(ctx, userID, profileID, contentID, filter); err != nil {
		return nil, err
	}
	sources, err := s.virtualSources(ctx, contentID)
	if err != nil {
		return nil, apiError(http.StatusInternalServerError, "internal_error", "Failed to load virtual candidates")
	}
	if len(sources) == 0 {
		return nil, apiError(http.StatusUnprocessableEntity, "validation_failed", "The item has no virtual candidates to refresh.")
	}
	for _, source := range sources {
		key := virtualSourceRefreshKey(source)
		// Do not bind the shared work to the first caller's context: a client
		// disconnect must not cancel the provider re-list (or strand a
		// coalesced waiter on a canceled context). The shared work runs under
		// the request context detached from cancellation (but with its own
		// timeout, applied inside refreshSource); waiters still observe their
		// own cancellation while waiting via the select below.
		workCtx := context.WithoutCancel(ctx)
		refreshCh := s.flight.DoChan(key, func() (any, error) {
			return nil, s.refreshSource(workCtx, source, userID, profileID)
		})
		var refreshRes singleflight.Result
		select {
		case <-ctx.Done():
			// This waiter is going away; the shared work continues for the
			// other waiters under its detached context. Report the waiter's
			// cancellation, not the work's outcome.
			return nil, apiError(http.StatusServiceUnavailable, "unavailable", "The refresh was interrupted; try again.")
		case refreshRes = <-refreshCh:
		}
		if refreshRes.Err != nil {
			if errors.Is(refreshRes.Err, ErrVirtualRefreshProvider) {
				return nil, apiError(http.StatusServiceUnavailable, "unavailable", "The provider could not be reached; try again.")
			}
			return nil, apiError(http.StatusInternalServerError, "internal_error", "Failed to refresh virtual candidates")
		}
		// A coalesced waiter reuses the winner's persistence; nothing more to
		// do for this source.
		_ = refreshRes.Shared
	}
	detail, err := s.Detail.WatchDetail(ctx, userID, profileID, contentID, filter)
	if err != nil {
		return nil, err
	}
	return detail.Versions, nil
}

// refreshSource force-lists one virtual source group and persists the result.
func (s *VirtualCandidatesRefreshService) refreshSource(ctx context.Context, source *models.MediaFile, userID int, profileID string) error {
	if source == nil {
		return nil
	}
	listCtx, cancel := context.WithTimeout(ctx, virtualCandidatesRefreshTimeout)
	defer cancel()
	streams, err := s.ListFresh.ListVirtualPlaybackStreams(
		listCtx, source.FilePath, userID, profileID, source.VirtualOwnerInstallationID,
	)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrVirtualRefreshProvider, err)
	}
	if len(streams) == 0 {
		// A zero-count listing is a provider hiccup, not an empty title. It must
		// not reach ReplaceVirtualCandidates: persisting it would sweep or
		// rewrite healthy stored candidates. Surface the same retryable provider
		// failure the caller already understands, and leave the rows untouched.
		return fmt.Errorf("%w: provider returned no candidates", ErrVirtualRefreshProvider)
	}
	// Persist under the listing timeout, not the detached caller context: the
	// database write must not outlive the provider budget that bounds this
	// refresh. (The detached work context only shields the re-list from a
	// waiter disconnect; it must not make persistence unbounded.)
	if err := s.Persist(listCtx, source, streams); err != nil {
		return err
	}
	return nil
}

// virtualSources groups the item's virtual media files into one source row per
// persisted candidate group. A provider-neutral row (no ?result=) is preferred
// so its exact file_path is preserved; a candidate-only item synthesizes the
// neutral path the group already uses.
func (s *VirtualCandidatesRefreshService) virtualSources(ctx context.Context, contentID string) ([]*models.MediaFile, error) {
	var files []*models.MediaFile
	if s.ContentFiles != nil {
		contentFiles, err := s.ContentFiles(ctx, contentID)
		if err != nil {
			return nil, err
		}
		files = append(files, contentFiles...)
	}
	if s.EpisodeFiles != nil {
		episodeFiles, err := s.EpisodeFiles(ctx, contentID)
		if err != nil {
			return nil, err
		}
		files = append(files, episodeFiles...)
	}
	byGroup := map[string]*models.MediaFile{}
	for _, file := range files {
		if !isVirtualPlaybackFile(file) {
			continue
		}
		if _, ok := virtualCandidateGroupURI(file.FilePath); !ok {
			continue
		}
		key := virtualSourceRefreshKey(file)
		existing, exists := byGroup[key]
		if !exists {
			byGroup[key] = file
			continue
		}
		if virtualResultCandidateID(existing.FilePath) != "" && virtualResultCandidateID(file.FilePath) == "" {
			byGroup[key] = file
		}
	}
	sources := make([]*models.MediaFile, 0, len(byGroup))
	for _, file := range byGroup {
		source := *file
		if virtualResultCandidateID(source.FilePath) != "" {
			if group, ok := virtualCandidateGroupURI(source.FilePath); ok {
				source.FilePath = group
			}
		}
		sources = append(sources, &source)
	}
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].FilePath != sources[j].FilePath {
			return sources[i].FilePath < sources[j].FilePath
		}
		return sources[i].ID < sources[j].ID
	})
	return sources, nil
}

// virtualSourceRefreshKey identifies one persisted candidate group: the
// provider-neutral path plus the rows' episode, library and owner scope, the
// same tuple ReplaceVirtualCandidates replaces.
func virtualSourceRefreshKey(file *models.MediaFile) string {
	if file == nil {
		return ""
	}
	group, _ := virtualCandidateGroupURI(file.FilePath)
	return fmt.Sprintf("%s\x00%s\x00%d\x00%d", group, file.EpisodeID, file.MediaFolderID, file.VirtualOwnerInstallationID)
}

// virtualCandidateGroupURI mirrors the scanner's candidate group: the virtual
// URI without its result= pick. It is the identity the stored candidates share.
func virtualCandidateGroupURI(raw string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != virtualURIScheme || parsed.Host == "" {
		return "", false
	}
	query := parsed.Query()
	query.Del("result")
	parsed.RawQuery = query.Encode()
	parsed.Fragment = ""
	return parsed.String(), true
}
