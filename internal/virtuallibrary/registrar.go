package virtuallibrary

import (
	"context"
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
	Reconcile(context.Context, string, []string, []int) error
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
		Episodes:       make([]catalog.VirtualEpisode, 0, len(item.Episodes)),
		Variants:       make([]catalog.VirtualMediaVariant, 0),
	}
	for _, episode := range item.Episodes {
		in.Episodes = append(in.Episodes, catalog.VirtualEpisode{
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
	_, err := r.registrar.Upsert(ctx, in)
	return err
}

func (r *catalogMonitorRegistrar) Reconcile(ctx context.Context, source string, keepIDs []string, libraryIDs []int) error {
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
	_, err := r.registrar.ReconcileVirtualMedia(ctx, 0, source, keepIDs, libraryIDs)
	return err
}
