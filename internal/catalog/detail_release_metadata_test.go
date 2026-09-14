package catalog

import (
	"context"
	"reflect"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestBuildPlaybackInfo_MapsReleaseFieldsAndBackfillsTrackLanguages(t *testing.T) {
	service := &DetailService{}

	versions, _, _, _, _, _, _ := service.buildPlaybackInfo(context.Background(), []*models.MediaFile{
		{
			ID:           7,
			ContentID:    "movie-1",
			FilePath:     "/media/Movie.2023.2160p.Multi-AltMount.mkv",
			Resolution:   "2160p",
			ReleaseName:  "Movie.2023.2160p.Multi-AltMount",
			ReleaseGroup: "AltMount",
			AudioTracks: []models.AudioTrack{
				// Legacy row: no languages array, undetermined tag, and a title
				// that lists the languages. ensureTrackLanguages backfills.
				{Language: "mul", EmbeddedTitle: "English / French / Spanish", Codec: "eac3"},
				// Already-populated row is left untouched.
				{Language: "en", Languages: []string{"en", "de"}, EmbeddedTitle: "English / German", Codec: "dts"},
				// Unparseable title stays as-is.
				{Language: "", Codec: "ac3"},
			},
		},
	}, AccessFilter{}, "movie-1")

	if len(versions) != 1 {
		t.Fatalf("len(versions) = %d, want 1", len(versions))
	}
	v := versions[0]
	if v.ReleaseName != "Movie.2023.2160p.Multi-AltMount" {
		t.Fatalf("ReleaseName = %q, want Movie.2023.2160p.Multi-AltMount", v.ReleaseName)
	}
	if v.ReleaseGroup != "AltMount" {
		t.Fatalf("ReleaseGroup = %q, want AltMount", v.ReleaseGroup)
	}
	if len(v.AudioTracks) != 3 {
		t.Fatalf("audio tracks = %d, want 3", len(v.AudioTracks))
	}
	// The legacy MULTI track is backfilled from its title and the first
	// language is promoted to the display language.
	first := v.AudioTracks[0]
	if !reflect.DeepEqual(first.Languages, []string{"en", "fr", "es"}) {
		t.Fatalf("backfilled Languages = %v, want [en fr es]", first.Languages)
	}
	if first.Language != "en" {
		t.Fatalf("promoted Language = %q, want en", first.Language)
	}
	// Already-populated track is not mutated.
	if !reflect.DeepEqual(v.AudioTracks[1].Languages, []string{"en", "de"}) {
		t.Fatalf("existing Languages mutated = %v", v.AudioTracks[1].Languages)
	}
	// Unparseable empty title stays empty.
	if len(v.AudioTracks[2].Languages) != 0 {
		t.Fatalf("empty-title Languages = %v, want empty", v.AudioTracks[2].Languages)
	}
}
