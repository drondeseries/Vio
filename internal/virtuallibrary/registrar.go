package virtuallibrary

import (
	"context"
	"fmt"
	"strings"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/monitor"
)

// catalogMonitorRegistrar adapts the catalog's transactional virtual-media
// registrar to the monitor's domain-level registration interface.
type catalogMonitorRegistrar struct {
	registrar *catalog.VirtualMediaRegistrar
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

func (r *catalogMonitorRegistrar) Register(ctx context.Context, item monitor.MonitoredMedia) error {
	if r == nil || r.registrar == nil {
		return fmt.Errorf("virtual catalog registrar is unavailable")
	}
	in := catalog.VirtualMedia{
		MediaType:      item.MediaType,
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
		})
	}
	_, err := r.registrar.Upsert(ctx, in)
	return err
}

func (r *catalogMonitorRegistrar) Reconcile(ctx context.Context, source string, keepIDs []string, libraryIDs []int) error {
	if r == nil || r.registrar == nil {
		return fmt.Errorf("virtual catalog registrar is unavailable")
	}
	_, err := r.registrar.ReconcileVirtualMedia(ctx, 0, source, keepIDs, libraryIDs)
	return err
}
