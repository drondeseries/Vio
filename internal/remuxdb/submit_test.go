package remuxdb

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestBuildSubmissionMovie(t *testing.T) {
	file := &models.MediaFile{
		ContentID:   "movie-tmdb-12345",
		FilePath:    "virtual://movie/tt0012345",
		Duration:    7200,
		Container:   "mkv",
		FileSize:    5000000000,
		Bitrate:     5500,
		ReleaseName: "Movie.2024.1080p.mkv",
		VideoTracks: []models.VideoTrack{
			{Codec: "h264", Width: 1920, Height: 1080, FrameRate: "23.976"},
		},
		AudioTracks: []models.AudioTrack{
			{Codec: "aac", Channels: 6, Language: "eng", Default: true},
		},
	}
	payload, ok := BuildSubmission(file, "", "0123456789abcdef0123456789abcdef01234567", nil)
	if !ok {
		t.Fatal("expected build submission to succeed")
	}
	if payload.Kind != "movie" {
		t.Errorf("kind = %q, want movie", payload.Kind)
	}
	if payload.Filename != "Movie.2024.1080p.mkv" {
		t.Errorf("filename = %q", payload.Filename)
	}
	if payload.ExternalIDs == nil || payload.ExternalIDs.IMDbID != "tt0012345" || payload.ExternalIDs.TMDbID != 12345 {
		t.Errorf("external ids = %+v", payload.ExternalIDs)
	}
	if len(payload.Tracks) != 2 {
		t.Errorf("tracks = %d, want 2", len(payload.Tracks))
	}
	if payload.Tracks[0].Kind != "video" || payload.Tracks[0].Width != 1920 {
		t.Errorf("video track = %+v", payload.Tracks[0])
	}
}

func TestBuildSubmissionEpisodeWithNZB(t *testing.T) {
	file := &models.MediaFile{
		ContentID:     "series-tvdb-999",
		EpisodeID:     "ep-1",
		SeasonNumber:  2,
		EpisodeNumber: 5,
		FilePath:      "virtual://series/tt9999999/2/5",
		Duration:      3600,
		VideoTracks: []models.VideoTrack{
			{Codec: "hevc", Width: 3840, Height: 2160},
		},
	}
	nzb := &NzbSubmission{
		Indexer:     "nzbgeek",
		IndexerGUID: "guid123",
	}
	payload, ok := BuildSubmission(file, "show.s02e05.mkv", "", nzb)
	if !ok {
		t.Fatal("expected build submission to succeed")
	}
	if payload.Kind != "episode" {
		t.Errorf("kind = %q, want episode", payload.Kind)
	}
	if payload.Season == nil || *payload.Season != 2 || payload.Episode == nil || *payload.Episode != 5 {
		t.Errorf("season/episode = %v/%v", payload.Season, payload.Episode)
	}
	if payload.NZB == nil || payload.NZB.IndexerGUID != "guid123" {
		t.Errorf("nzb = %+v", payload.NZB)
	}
}

func TestBuildSubmissionRequiresIdentityAndVideo(t *testing.T) {
	if _, ok := BuildSubmission(nil, "", "abc", nil); ok {
		t.Fatal("nil file should fail")
	}
	empty := &models.MediaFile{Duration: 100}
	if _, ok := BuildSubmission(empty, "a.mkv", "abc", nil); ok {
		t.Fatal("file without video tracks should fail")
	}
	noIdent := &models.MediaFile{
		Duration:    100,
		VideoTracks: []models.VideoTrack{{Codec: "h264"}},
	}
	if _, ok := BuildSubmission(noIdent, "a.mkv", "", nil); ok {
		t.Fatal("file without hash or nzb should fail")
	}
}

func TestBuildSubmissionPreservesContainerStreamIndices(t *testing.T) {
	file := &models.MediaFile{
		ContentID: "movie-tmdb-7",
		Duration:  6000,
		Container: "mkv",
		VideoTracks: []models.VideoTrack{
			{Codec: "h264", Width: 1920, Height: 1080},
			{Codec: "h264", Width: 1920, Height: 1080},
		},
		AudioTracks:    []models.AudioTrack{{Index: 5, Codec: "aac", Channels: 6}},
		SubtitleTracks: []models.SubtitleTrack{{Index: 9, Codec: "srt", Language: "eng"}},
	}
	payload, ok := BuildSubmission(file, "Movie.mkv", "0123456789abcdef0123456789abcdef01234567", nil)
	if !ok {
		t.Fatal("expected build submission to succeed")
	}
	if len(payload.Tracks) != 4 {
		t.Fatalf("tracks = %d, want 4", len(payload.Tracks))
	}
	// Video has no container index; array position is the video ordinal.
	if payload.Tracks[0].Kind != "video" || payload.Tracks[0].Index != 0 {
		t.Errorf("video track 0 = %+v, want index 0", payload.Tracks[0])
	}
	if payload.Tracks[1].Kind != "video" || payload.Tracks[1].Index != 1 {
		t.Errorf("video track 1 = %+v, want index 1", payload.Tracks[1])
	}
	// Audio/subtitle indices are absolute container indices and must not be renumbered.
	if payload.Tracks[2].Kind != "audio" || payload.Tracks[2].Index != 5 {
		t.Errorf("audio track = %+v, want index 5", payload.Tracks[2])
	}
	if payload.Tracks[3].Kind != "subtitle" || payload.Tracks[3].Index != 9 {
		t.Errorf("subtitle track = %+v, want index 9", payload.Tracks[3])
	}
}

func TestBuildSubmissionLeavesUnknownContainerEmpty(t *testing.T) {
	for _, container := range []string{"", "virtual", "Virtual"} {
		file := &models.MediaFile{
			ContentID:   "movie-tmdb-7",
			Duration:    6000,
			Container:   container,
			VideoTracks: []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}},
		}
		payload, ok := BuildSubmission(file, "Movie.mkv", "0123456789abcdef0123456789abcdef01234567", nil)
		if !ok {
			t.Fatalf("container %q: expected build submission to succeed", container)
		}
		if payload.Container != "" {
			t.Errorf("container %q: payload container = %q, want empty", container, payload.Container)
		}
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("container %q: marshal: %v", container, err)
		}
		if strings.Contains(string(body), `"container"`) {
			t.Errorf("container %q: serialized payload fabricates container: %s", container, body)
		}
	}
}

func TestParseFrameRateFloat(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want float64
	}{
		{name: "rational", raw: "30000/1001", want: 30000.0 / 1001.0},
		{name: "decimal", raw: "23.976", want: 23.976},
		{name: "decimal suffix", raw: "23.976 fps", want: 23.976},
		{name: "integer", raw: "24", want: 24},
		{name: "junk", raw: "junk", want: 0},
		{name: "empty", raw: "", want: 0},
		{name: "zero denominator", raw: "24000/0", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseFrameRateFloat(tt.raw); got != tt.want {
				t.Errorf("parseFrameRateFloat(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}
