package virtuallibrary

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/monitor"
)

// catalogMonitorRegistrar adapts the catalog's transactional virtual-media
// registrar to the monitor's domain-level registration interface.
type catalogMonitorRegistrar struct {
	registrar       *catalog.VirtualMediaRegistrar
	movieLibraryID  int
	seriesLibraryID int
}

var _ monitor.MediaRegistrar = (*catalogMonitorRegistrar)(nil)
var _ interface {
	Reconcile(context.Context, string, []string, []int, monitor.ReconcileEvidence) error
} = (*catalogMonitorRegistrar)(nil)

func virtualURIForMonitored(item monitor.MonitoredMedia) string {
	streamID := strings.ReplaceAll(item.StreamID, ":", "/")
	if item.MediaType == "movie" && streamID != "" {
		return "virtual://movie/" + streamID
	}
	return ""
}

func virtualEpisodeURI(item monitor.MonitoredMedia, ep monitor.VirtualEpisode) string {
	if ep.Season <= 0 || ep.Episode <= 0 {
		return ""
	}
	if item.IMDbID != "" {
		return fmt.Sprintf("virtual://series/%s/%d/%d", item.IMDbID, ep.Season, ep.Episode)
	}
	if item.TVDBID != "" {
		return fmt.Sprintf("virtual://series/tvdb/%s/%d/%d", item.TVDBID, ep.Season, ep.Episode)
	}
	if item.TMDBID != "" {
		return fmt.Sprintf("virtual://series/tmdb/%s/%d/%d", item.TMDBID, ep.Season, ep.Episode)
	}
	streamID := strings.ReplaceAll(item.StreamID, ":", "/")
	if streamID != "" {
		return fmt.Sprintf("virtual://series/%s/%d/%d", streamID, ep.Season, ep.Episode)
	}
	return ""
}

func (r *catalogMonitorRegistrar) Register(ctx context.Context, item monitor.MonitoredMedia) error {
	if r == nil || r.registrar == nil {
		return fmt.Errorf("virtual catalog registrar is unavailable")
	}
	folderID := item.MediaFolderID
	if folderID <= 0 {
		if item.MediaType == "movie" {
			folderID = r.movieLibraryID
		} else if item.MediaType == "series" {
			folderID = r.seriesLibraryID
		}
	}
	var libraryIDStr string
	if folderID > 0 {
		libraryIDStr = strconv.Itoa(folderID)
	}

	in := catalog.VirtualMedia{
		MediaType:      item.MediaType,
		LibraryID:      libraryIDStr,
		IMDbID:         item.IMDbID,
		TMDBID:         item.TMDBID,
		TVDBID:         item.TVDBID,
		Title:          item.Title,
		Year:           int(item.Year),
		Overview:       item.Overview,
		PosterPath:     item.Poster,
		BackdropPath:   item.Backdrop,
		Genres:         item.Genres,
		RuntimeMinutes: item.Runtime,
		Source:         item.SourceKey,
		VirtualURI:     virtualURIForMonitored(item),
		Episodes:       monitoredEpisodePayload(item),
		Variants:       make([]catalog.VirtualMediaVariant, 0),
	}
	_, err := r.registrar.Upsert(ctx, in)
	return err
}

// monitoredEpisodePayload maps monitor episodes onto the catalog payload.
//
// Season 0 is the provider's specials bucket and the catalog rejects
// non-positive season/episode coordinates (see catalog.validateVirtualEpisode);
// a single special must not fail the whole series registration, so specials are
// omitted here. Genuinely malformed input (a negative season, a missing field)
// is intentionally preserved so catalog validation still rejects it.
func monitoredEpisodePayload(item monitor.MonitoredMedia) []catalog.VirtualEpisode {
	episodes := make([]catalog.VirtualEpisode, 0, len(item.Episodes))
	for _, episode := range item.Episodes {
		if episode.Season == 0 {
			continue
		}
		episodes = append(episodes, catalog.VirtualEpisode{
			SeasonNumber:   episode.Season,
			EpisodeNumber:  episode.Episode,
			Title:          episode.Title,
			Overview:       episode.Overview,
			StillPath:      episode.Thumbnail,
			AirDate:        episode.Released,
			RuntimeMinutes: episode.Runtime,
			VirtualURI:     virtualEpisodeURI(item, episode),
		})
	}
	return episodes
}

// MissingVirtualMedia reports which of the given content IDs no longer exist
// in the catalog, so the monitor can evict queue entries whose media was
// genuinely removed.
func (r *catalogMonitorRegistrar) MissingVirtualMedia(ctx context.Context, contentIDs []string) (map[string]struct{}, error) {
	if r == nil || r.registrar == nil {
		return nil, fmt.Errorf("virtual catalog registrar is unavailable")
	}
	return r.registrar.MissingVirtualMediaContentIDs(ctx, contentIDs)
}

func (r *catalogMonitorRegistrar) Reconcile(ctx context.Context, source string, keepIDs []string, libraryIDs []int, evidence monitor.ReconcileEvidence) error {
	if r == nil || r.registrar == nil {
		return fmt.Errorf("virtual catalog registrar is unavailable")
	}
	if len(libraryIDs) == 0 {
		if r.movieLibraryID > 0 {
			libraryIDs = append(libraryIDs, r.movieLibraryID)
		}
		if r.seriesLibraryID > 0 {
			libraryIDs = append(libraryIDs, r.seriesLibraryID)
		}
	}
	_, err := r.registrar.ReconcileVirtualMediaVerified(ctx, 0, source, keepIDs, libraryIDs, catalog.VirtualReconcileEvidence{
		FullCycle:   evidence.FullCycle,
		SourceCount: evidence.SourceCount,
		QueueCount:  evidence.QueueCount,
	})
	if errors.Is(err, catalog.ErrVirtualReconcileRefused) {
		// Keep the catalog's source and counts in the message while preserving
		// the refusal identity so the monitor can log it loudly instead of
		// treating it as an ordinary retryable failure.
		return fmt.Errorf("%w: %v", monitor.ErrReconcileRefused, err)
	}
	return err
}
