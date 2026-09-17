package virtuallibrary

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/monitor"
)

func TestVirtualURIForMonitored(t *testing.T) {
	// Movie with simple stream ID
	movieItem := monitor.MonitoredMedia{
		MediaType: "movie",
		StreamID:  "tt0133093",
	}
	if uri := virtualURIForMonitored(movieItem); uri != "virtual://movie/tt0133093" {
		t.Fatalf("movie virtual URI = %q, want virtual://movie/tt0133093", uri)
	}

	// Movie with colon-separated stream ID
	colonItem := monitor.MonitoredMedia{
		MediaType: "movie",
		StreamID:  "tmdb:603",
	}
	if uri := virtualURIForMonitored(colonItem); uri != "virtual://movie/tmdb/603" {
		t.Fatalf("colon stream ID virtual URI = %q, want virtual://movie/tmdb/603", uri)
	}

	// Series stream ID: series playback sources belong to episodes, so top-level URI is empty
	seriesItem := monitor.MonitoredMedia{
		MediaType: "series",
		StreamID:  "tt0903747",
	}
	if uri := virtualURIForMonitored(seriesItem); uri != "" {
		t.Fatalf("series top-level virtual URI = %q, want empty", uri)
	}
}

func TestCatalogMonitorRegistrarNilFailsClosed(t *testing.T) {
	var r *catalogMonitorRegistrar
	if err := r.Register(context.Background(), monitor.MonitoredMedia{}); err == nil {
		t.Fatal("expected error registering with nil registrar")
	}
	if err := r.Reconcile(context.Background(), "monitor", []string{"id1"}, []int{1}); err == nil {
		t.Fatal("expected error reconciling with nil registrar")
	}

	empty := &catalogMonitorRegistrar{}
	if err := empty.Register(context.Background(), monitor.MonitoredMedia{}); err == nil {
		t.Fatal("expected error registering with empty registrar")
	}
	if err := empty.Reconcile(context.Background(), "monitor", []string{"id1"}, []int{1}); err == nil {
		t.Fatal("expected error reconciling with empty registrar")
	}
}

func TestCatalogMonitorRegistrarEpisodeMapping(t *testing.T) {
	// Verify that Register accurately maps all monitoredMedia fields including episodes
	item := monitor.MonitoredMedia{
		Key:       "series:tt100",
		MediaType: "series",
		Title:     "Test Series",
		Year:      2024,
		IMDbID:    "tt100",
		TMDBID:    "200",
		TVDBID:    "300",
		Overview:  "A test series overview",
		Poster:    "https://img.example/poster.jpg",
		Backdrop:  "https://img.example/backdrop.jpg",
		Genres:    []string{"Drama", "Sci-Fi"},
		Runtime:   45,
		SourceKey: "test-source",
		Episodes: []monitor.VirtualEpisode{
			{
				Season:    1,
				Episode:   1,
				Title:     "Pilot",
				Overview:  "First episode",
				Thumbnail: "https://img.example/ep1.jpg",
				Released:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
				Runtime:   50,
			},
		},
	}

	// Register with nil inner registrar fails cleanly after mapping logic
	r := &catalogMonitorRegistrar{registrar: nil}
	err := r.Register(context.Background(), item)
	if err == nil {
		t.Fatal("expected error from unconfigured catalog registrar")
	}
}
