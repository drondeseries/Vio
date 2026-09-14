package remuxdb

import (
	"testing"
)

func videoVariant(size int64, codec string, height int, transfer string, dv int, sources ...ProbeSource) MediaInfo {
	return MediaInfo{
		Size:    size,
		Sources: sources,
		Tracks: []TrackDetail{
			{Kind: "video", Codec: codec, Width: 1920, Height: height, ColorTransfer: transfer, DVProfile: dv},
			{Kind: "audio", Codec: "aac", Channels: 6},
		},
	}
}

func TestMatchVariantInfoHash(t *testing.T) {
	one, two := 0, 1
	versions := []MediaInfo{
		videoVariant(1000, "h264", 1080, "", 0, ProbeSource{Kind: "torrent", TorrentInfoHash: "aaa", TorrentFileIdx: &one}),
		videoVariant(2000, "hevc", 2160, "smpte2084", 8, ProbeSource{Kind: "torrent", TorrentInfoHash: "bbb", TorrentFileIdx: &two}),
	}
	got, method := MatchVariant(versions, MatchHint{InfoHash: "BBB", HasFileIdx: true, FileIdx: 1})
	if method != MatchInfoHash || got == nil || got.Size != 2000 {
		t.Fatalf("hash match = %+v %q, want 2000/info_hash", got, method)
	}
}

func TestMatchVariantInfoHashWrongFileIdxFallsThrough(t *testing.T) {
	one := 0
	versions := []MediaInfo{
		videoVariant(1000, "h264", 1080, "", 0, ProbeSource{Kind: "torrent", TorrentInfoHash: "aaa", TorrentFileIdx: &one}),
		videoVariant(1000, "h264", 1080, "", 0),
	}
	if got, _ := MatchVariant(versions, MatchHint{InfoHash: "aaa", HasFileIdx: true, FileIdx: 3}); got != nil {
		t.Fatalf("wrong fileIdx matched %+v, want nil", got)
	}
}

func TestMatchVariantIndexerGUID(t *testing.T) {
	versions := []MediaInfo{
		videoVariant(1000, "h264", 1080, "", 0, ProbeSource{Kind: "nzb", Indexer: "Geek", IndexerGUID: "g1"}),
		videoVariant(2000, "h264", 1080, "", 0, ProbeSource{Kind: "nzb", Indexer: "Geek", IndexerGUID: "g2"}),
	}
	got, method := MatchVariant(versions, MatchHint{IndexerGUID: "g2", Indexer: "geek"})
	if method != MatchIndexerGUID || got == nil || got.Size != 2000 {
		t.Fatalf("guid match = %+v %q, want 2000/indexer_guid", got, method)
	}
	if got, _ := MatchVariant(versions, MatchHint{IndexerGUID: "g2", Indexer: "Other"}); got != nil {
		t.Fatalf("cross-indexer guid matched %+v, want nil", got)
	}
}

func TestMatchVariantExactSize(t *testing.T) {
	versions := []MediaInfo{
		videoVariant(1000, "h264", 1080, "", 0),
		videoVariant(28979107000, "h264", 1080, "", 0),
	}
	got, method := MatchVariant(versions, MatchHint{Size: 28979107000})
	if method != MatchSize || got == nil || got.Size != 28979107000 {
		t.Fatalf("size match = %+v %q, want exact", got, method)
	}
}

func TestMatchVariantSizeTagsFindsRoundedRelease(t *testing.T) {
	versions := []MediaInfo{
		videoVariant(2670000000, "av1", 1080, "", 0),
		videoVariant(28979107000, "h264", 1080, "", 0),
	}
	got, method := MatchVariant(versions, MatchHint{Size: 28980000000, Resolution: "1080p", CodecVideo: "h264"})
	if method != MatchSizeTags || got == nil || got.Size != 28979107000 {
		t.Fatalf("size+tags match = %+v %q, want 28979107000/size_tags", got, method)
	}
}

func TestMatchVariantSizeTagsWidescreen(t *testing.T) {
	versions := []MediaInfo{
		videoVariant(1807251544, "hevc", 800, "", 0),
	}
	got, method := MatchVariant(versions, MatchHint{Size: 1807251544, Resolution: "1080p", CodecVideo: "hevc"})
	if method != MatchSize || got == nil {
		t.Fatalf("exact size match failed: got %+v %q", got, method)
	}
	gotTag, methodTag := MatchVariant(versions, MatchHint{Size: 1807000000, Resolution: "1080p", CodecVideo: "hevc"})
	if methodTag != MatchSizeTags || gotTag == nil {
		t.Fatalf("widescreen 1920x800 should match 1080p hint: got %+v %q", gotTag, methodTag)
	}
}

func TestMatchVariantHDRVetoRejectsMismatch(t *testing.T) {
	versions := []MediaInfo{
		videoVariant(28979107000, "hevc", 2160, "smpte2084", 8),
	}
	hint := MatchHint{Size: 28980000000, HDRKnown: true, HDR: false}
	if got, _ := MatchVariant(versions, hint); got != nil {
		t.Fatalf("HDR-positive variant matched SDR-positive hint: %+v", got)
	}
}

func TestMatchVariantZeroValueHDRNeverVetoes(t *testing.T) {
	versions := []MediaInfo{
		videoVariant(83355095000, "hevc", 2160, "smpte2084", 8),
	}
	got, method := MatchVariant(versions, MatchHint{Size: 83360000000, HDRKnown: false, HDR: false})
	if method != MatchSizeTags || got == nil {
		t.Fatalf("unprobed zero-HDR hint should match DV variant, got %+v %q", got, method)
	}
}

func TestMatchVariantCodecMismatchRejects(t *testing.T) {
	versions := []MediaInfo{
		videoVariant(2670000000, "av1", 1080, "", 0),
	}
	if got, _ := MatchVariant(versions, MatchHint{Size: 2675000000, CodecVideo: "h264"}); got != nil {
		t.Fatalf("av1 variant matched h264 hint: %+v", got)
	}
}

func TestMatchVariantAmbiguityAbstains(t *testing.T) {
	versions := []MediaInfo{
		videoVariant(5000, "h264", 1080, "", 0),
		videoVariant(5000, "h264", 1080, "", 0),
	}
	if got, method := MatchVariant(versions, MatchHint{Size: 5000}); got != nil || method != MatchNone {
		t.Fatalf("ambiguous match = %+v %q, want nil", got, method)
	}
}

func TestMatchVariantFilenameFallback(t *testing.T) {
	versions := []MediaInfo{
		videoVariant(0, "h264", 1080, "", 0, ProbeSource{Filename: "Movie.2020.1080p.BluRay.x264-GRP.mkv"}),
		videoVariant(0, "hevc", 2160, "smpte2084", 0, ProbeSource{Filename: "Other.2020.2160p.WEB-DL.mkv"}),
	}
	got, method := MatchVariant(versions, MatchHint{Filename: "movie.2020.1080p.bluray.x264-grp.MKV"})
	if method != MatchFilename || got == nil || got.Size != 0 || firstVideoTrack(got).Codec != "h264" {
		t.Fatalf("filename match = %+v %q, want h264/filename", got, method)
	}
	// Dots and dashes vs spaces
	gotSpaces, methodSpaces := MatchVariant(versions, MatchHint{Filename: "Movie 2020 1080p BluRay x264 GRP"})
	if methodSpaces != MatchFilename || gotSpaces == nil {
		t.Fatalf("normalized filename match failed: got %+v %q", gotSpaces, methodSpaces)
	}
}

func TestMatchVariantEmpty(t *testing.T) {
	if got, method := MatchVariant(nil, MatchHint{Size: 1}); got != nil || method != MatchNone {
		t.Fatalf("empty versions matched %+v %q", got, method)
	}
	if got, method := MatchVariant([]MediaInfo{videoVariant(1, "h264", 1080, "", 0)}, MatchHint{}); got != nil || method != MatchNone {
		t.Fatalf("empty hint matched %+v %q", got, method)
	}
}
