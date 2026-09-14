package remuxdb

import (
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
)

func TestApplyEvidenceFillsMissingOnly(t *testing.T) {
	ev := Evidence{
		Container:  "mkv",
		CodecVideo: "hevc",
		Resolution: "2160p",
		HDR:        true,
		HDRKnown:   true,
		Duration:   6583.0,
		Bitrate:    58828983,
		VideoTracks: []TrackDetail{
			{Kind: "video", Codec: "hevc", Width: 3840, Height: 2160, DVProfile: 8, ColorTransfer: "smpte2084"},
		},
		AudioTracks: []TrackDetail{
			{Kind: "audio", Codec: "truehd", Channels: 8, Language: "eng"},
		},
	}
	file := &models.MediaFile{ContentID: "movie-1", Resolution: "2160p"}
	if !ApplyEvidence(ev, file) {
		t.Fatal("evidence not applied")
	}
	if file.CodecVideo != "hevc" || !file.HDR || file.CodecAudio != "truehd" || file.AudioChannels != 8 {
		t.Fatalf("file = %+v", file)
	}
	if file.Resolution != "2160p" {
		t.Fatalf("resolution overwritten: %q", file.Resolution)
	}
	if len(file.VideoTracks) != 1 || file.VideoTracks[0].DVProfile != 8 {
		t.Fatalf("video tracks = %+v", file.VideoTracks)
	}
}

func TestApplyEvidenceNeverOverwritesProbeDetail(t *testing.T) {
	ev := Evidence{
		CodecVideo:  "hevc",
		VideoTracks: []TrackDetail{{Kind: "video", Codec: "hevc", Height: 2160}},
		AudioTracks: []TrackDetail{{Kind: "audio", Codec: "truehd", Channels: 8}},
	}
	file := &models.MediaFile{
		CodecVideo:  "h264",
		VideoTracks: []models.VideoTrack{{Codec: "h264", Height: 1080}},
		AudioTracks: []models.AudioTrack{{Codec: "aac", Channels: 2}},
		CodecAudio:  "aac",
		Resolution:  "1080p",
	}
	if !ApplyEvidence(ev, file) {
		t.Fatal("evidence not applied")
	}
	if file.CodecVideo != "h264" || file.Resolution != "1080p" || file.CodecAudio != "aac" {
		t.Fatalf("probe detail overwritten: %+v", file)
	}
	if len(file.VideoTracks) != 1 || file.VideoTracks[0].Codec != "h264" {
		t.Fatalf("probe tracks overwritten: %+v", file.VideoTracks)
	}
	if len(file.AudioTracks) != 1 || file.AudioTracks[0].Codec != "aac" {
		t.Fatalf("probe audio overwritten: %+v", file.AudioTracks)
	}
}

func TestApplyEvidenceRequiresVideo(t *testing.T) {
	file := &models.MediaFile{}
	if ApplyEvidence(Evidence{AudioTracks: []TrackDetail{{Kind: "audio"}}}, file) {
		t.Fatal("audio-only evidence applied")
	}
	if ApplyEvidence(Evidence{}, nil) {
		t.Fatal("nil file applied")
	}
}

func TestApplyEvidenceDetectsUppercaseHDR(t *testing.T) {
	ev := Evidence{
		VideoTracks: []TrackDetail{
			{Kind: "video", Codec: "hevc", Width: 3840, Height: 2160, ColorTransfer: " SMPTE2084 "},
		},
	}
	file := &models.MediaFile{}
	ApplyEvidence(ev, file)
	if !file.HDR {
		t.Fatalf("file.HDR = false, want true for SMPTE2084 transfer")
	}
	if len(file.VideoTracks) != 1 || file.VideoTracks[0].VideoRange != "HDR" {
		t.Fatalf("video tracks = %+v, want HDR range", file.VideoTracks)
	}
}

func TestApplyEvidenceRoutesExternalSubtitles(t *testing.T) {
	ev := Evidence{
		VideoTracks: []TrackDetail{{Kind: "video", Codec: "h264", Width: 1920, Height: 1080}},
		SubtitleTracks: []TrackDetail{
			{Kind: "subtitle", Index: 2, Codec: "subrip", Language: "eng"},
			{Kind: "subtitle", Index: 3, Codec: "srt", Language: "fra", Title: "French", IsExternal: true},
		},
	}
	file := &models.MediaFile{}
	if !ApplyEvidence(ev, file) {
		t.Fatal("evidence not applied")
	}
	if len(file.SubtitleTracks) != 1 {
		t.Fatalf("embedded subtitle tracks = %d, want 1: %+v", len(file.SubtitleTracks), file.SubtitleTracks)
	}
	if file.SubtitleTracks[0].Codec != "subrip" || file.SubtitleTracks[0].External {
		t.Fatalf("embedded track = %+v, want non-external subrip", file.SubtitleTracks[0])
	}
	if len(file.ExternalSubtitles) != 1 {
		t.Fatalf("external subtitles = %d, want 1: %+v", len(file.ExternalSubtitles), file.ExternalSubtitles)
	}
	ext := file.ExternalSubtitles[0]
	if ext.Format != "srt" || ext.Language != "fra" || ext.Title != "French" {
		t.Fatalf("external subtitle = %+v, want srt/fra/French", ext)
	}
}
