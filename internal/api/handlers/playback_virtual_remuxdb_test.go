package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/remuxdb"
)

func remuxTestVariants() []remuxdb.MediaInfo {
	return []remuxdb.MediaInfo{
		{
			Size:      2670000000,
			Container: "mkv",
			Tracks: []remuxdb.TrackDetail{
				{Kind: "video", Codec: "av1", Width: 1920, Height: 1080},
			},
		},
		{
			Size:      28979107000,
			Container: "mkv",
			Sources: []remuxdb.ProbeSource{
				{Filename: "named.movie.2024.1080p.mkv"},
			},
			Tracks: []remuxdb.TrackDetail{
				{Kind: "video", Codec: "h264", Width: 1920, Height: 1080},
				{Kind: "audio", Codec: "dts", Channels: 6},
			},
		},
	}
}

func remuxTestServer(t *testing.T, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		_ = json.NewEncoder(w).Encode(remuxTestVariants())
	}))
}

func TestMatchRemuxDBCandidatesRecordsSizeMatch(t *testing.T) {
	calls := 0
	ts := remuxTestServer(t, &calls)
	defer ts.Close()

	h := &PlaybackHandler{
		RemuxDBConfig: func(context.Context) remuxdb.Config {
			return remuxdb.Config{Enabled: true, BaseURL: ts.URL}
		},
	}
	file := &models.MediaFile{ContentID: "movie-tmdb-1", FilePath: "virtual://movie/tt0000001", MediaFolderID: 3}
	candidates := []VirtualPlaybackStream{
		{URI: "virtual://movie/tt0000001?result=a", FileSize: 28980000000, Resolution: "1080p", CodecVideo: "h264"},
		{URI: "virtual://movie/tt0000001?result=b", FileSize: 2670000000, Resolution: "1080p", CodecVideo: "av1"},
	}
	matched, enabled := h.matchRemuxDBCandidates(context.Background(), file, candidates)
	if !enabled {
		t.Fatal("enabled = false, want true for enabled config")
	}
	if calls != 1 {
		t.Fatalf("fetches = %d, want 1 shared fetch", calls)
	}
	if len(matched) != 2 {
		t.Fatalf("matched = %d releases, want 2", len(matched))
	}
	ev, ok := matched["virtual://movie/tt0000001?result=a"]
	if !ok || string(ev.MatchMethod) != string(remuxdb.MatchSizeTags) || ev.CodecVideo != "h264" {
		t.Fatalf("candidate a evidence = %+v %v", ev, ok)
	}

	backfilled := applyRemuxDBEvidence(file, matched, "virtual://movie/tt0000001?result=a")
	if backfilled == file || backfilled.CodecVideo != "h264" || backfilled.Resolution != "1080p" {
		t.Fatalf("backfilled = %+v same=%v", backfilled, backfilled == file)
	}
	if file.CodecVideo != "" {
		t.Fatal("source file mutated")
	}
}

func TestMatchRemuxDBCandidatesDisabled(t *testing.T) {
	calls := 0
	ts := remuxTestServer(t, &calls)
	defer ts.Close()

	h := &PlaybackHandler{
		RemuxDBConfig: func(context.Context) remuxdb.Config {
			return remuxdb.Config{Enabled: false, BaseURL: ts.URL}
		},
	}
	file := &models.MediaFile{ContentID: "movie-tmdb-1", FilePath: "virtual://movie/tt0000001"}
	matched, enabled := h.matchRemuxDBCandidates(context.Background(), file, []VirtualPlaybackStream{
		{URI: "virtual://movie/tt0000001?result=a", FileSize: 1},
	})
	if len(matched) != 0 || calls != 0 || enabled {
		t.Fatalf("disabled matched=%d calls=%d enabled=%v, want 0/0/false", len(matched), calls, enabled)
	}
}

func TestMatchRemuxDBCandidatesAbstainsWithoutIdentity(t *testing.T) {
	calls := 0
	ts := remuxTestServer(t, &calls)
	defer ts.Close()

	h := &PlaybackHandler{
		RemuxDBConfig: func(context.Context) remuxdb.Config {
			return remuxdb.Config{Enabled: true, BaseURL: ts.URL}
		},
	}
	file := &models.MediaFile{ContentID: "movie-tmdb-1", FilePath: "virtual://movie/tt0000001"}
	matched, _ := h.matchRemuxDBCandidates(context.Background(), file, []VirtualPlaybackStream{
		{URI: "virtual://movie/tt0000001?result=a"},
	})
	if len(matched) != 0 {
		t.Fatalf("identity-free candidate matched: %+v", matched)
	}
}

func TestRemuxHintHDRRules(t *testing.T) {
	file := &models.MediaFile{}
	cand := VirtualPlaybackStream{}
	if hint := remuxHintForCandidate(file, cand); hint.HDRKnown {
		t.Fatalf("empty candidate hint marks HDR known: %+v", hint)
	}
	probed := &models.MediaFile{HDR: true, VideoTracks: []models.VideoTrack{{Codec: "hevc"}}}
	if hint := remuxHintForCandidate(probed, cand); !hint.HDRKnown || !hint.HDR {
		t.Fatalf("probed HDR hint = %+v, want known true", hint)
	}
	provider := VirtualPlaybackStream{HDR: "Dolby Vision"}
	if hint := remuxHintForCandidate(file, provider); !hint.HDRKnown || !hint.HDR {
		t.Fatalf("provider HDR hint = %+v, want known true", hint)
	}
}

func TestMatchRemuxDBCandidatesMatchesFilenameFromLabelWhenSizeZero(t *testing.T) {
	calls := 0
	ts := remuxTestServer(t, &calls)
	defer ts.Close()

	h := &PlaybackHandler{
		RemuxDBConfig: func(context.Context) remuxdb.Config {
			return remuxdb.Config{Enabled: true, BaseURL: ts.URL}
		},
	}
	file := &models.MediaFile{ContentID: "movie-tmdb-1", FilePath: "virtual://movie/tt0000001"}
	candidates := []VirtualPlaybackStream{
		{URI: "virtual://movie/tt0000001?result=named", Label: "named.movie.2024.1080p.mkv", FileSize: 0},
	}
	matched, _ := h.matchRemuxDBCandidates(context.Background(), file, candidates)
	if len(matched) != 1 {
		t.Fatalf("matched = %d releases, want 1", len(matched))
	}
	ev, ok := matched["virtual://movie/tt0000001?result=named"]
	if !ok || string(ev.MatchMethod) != string(remuxdb.MatchFilename) || ev.CodecVideo != "h264" {
		t.Fatalf("candidate named evidence = %+v %v", ev, ok)
	}
}

func TestRemuxEvidenceDoesNotLeakToAlternativeCandidates(t *testing.T) {
	matched := map[string]remuxdb.Evidence{
		"virtual://movie/tt0000001?result=candA": {
			CodecVideo:  "hevc",
			VideoTracks: []remuxdb.TrackDetail{{Kind: "video", Codec: "hevc", Width: 3840, Height: 2160}},
		},
	}
	baseFile := &models.MediaFile{
		ContentID: "movie-tmdb-1",
		FilePath:  "virtual://movie/tt0000001",
	}

	candB := "virtual://movie/tt0000001?result=candB"
	backfilledB := applyRemuxDBEvidence(baseFile, matched, candB)
	if len(backfilledB.VideoTracks) != 0 || backfilledB.CodecVideo != "" {
		t.Fatalf("candidate B inherited candidate A's RemuxDB metadata: %+v", backfilledB)
	}

	candA := "virtual://movie/tt0000001?result=candA"
	backfilledA := applyRemuxDBEvidence(baseFile, matched, candA)
	if len(backfilledA.VideoTracks) == 0 || backfilledA.CodecVideo != "hevc" {
		t.Fatalf("candidate A failed to backfill: %+v", backfilledA)
	}
}

func TestRemuxHintDoesNotBorrowDifferentReleaseIdentity(t *testing.T) {
	file := &models.MediaFile{
		FilePath:    "virtual://movie/tt0000001?result=pinnedA",
		FileSize:    5000000000,
		ReleaseName: "Pinned.Movie.2024.2160p.mkv",
		Resolution:  "2160p",
		CodecVideo:  "hevc",
	}
	candB := VirtualPlaybackStream{
		URI: "virtual://movie/tt0000001?result=unrelatedB",
		ID:  "unrelatedB",
	}
	hintB := remuxHintForCandidate(file, candB)
	if hintB.Size != 0 || hintB.Filename != "" || hintB.Resolution != "" || hintB.CodecVideo != "" {
		t.Fatalf("candidate B borrowed release A's identity: %+v", hintB)
	}

	candA := VirtualPlaybackStream{
		URI: "virtual://movie/tt0000001?result=pinnedA",
		ID:  "pinnedA",
	}
	hintA := remuxHintForCandidate(file, candA)
	if hintA.Size != 5000000000 || hintA.Filename != "Pinned.Movie.2024.2160p.mkv" || hintA.Resolution != "2160p" || hintA.CodecVideo != "hevc" {
		t.Fatalf("candidate A failed to borrow its own file identity: %+v", hintA)
	}
}

func TestRemuxEvidenceKeyNormalizesProfileAndParamOrdering(t *testing.T) {
	want := "virtual://movie/tt0000001?result=candA"
	for _, uri := range []string{
		"virtual://movie/tt0000001?profile=1080p&result=candA",
		"virtual://movie/tt0000001?result=candA&profile=1080p",
		"virtual://movie/tt0000001?result=candA",
	} {
		if got := remuxEvidenceKey(uri); got != want {
			t.Fatalf("remuxEvidenceKey(%q) = %q, want %q", uri, got, want)
		}
	}
}

func TestApplyRemuxDBEvidenceUsesPrecomputedKey(t *testing.T) {
	key := remuxEvidenceKey("virtual://movie/tt0000001?profile=1080p&result=candA")
	matched := map[string]remuxdb.Evidence{
		key: {
			CodecVideo:  "hevc",
			VideoTracks: []remuxdb.TrackDetail{{Kind: "video", Codec: "hevc", Width: 3840, Height: 2160}},
		},
	}
	file := &models.MediaFile{ContentID: "movie-tmdb-1", FilePath: "virtual://movie/tt0000001"}
	backfilled := applyRemuxDBEvidence(file, matched, key)
	if backfilled == file || backfilled.CodecVideo != "hevc" {
		t.Fatalf("backfilled = %+v, same=%v", backfilled, backfilled == file)
	}
}

func TestAllowDeferredProbeMatchesMainBehaviorWhenRemuxDBDisabled(t *testing.T) {
	// RemuxDB disabled: the gate is main's plain deferProbe flag, so an
	// evidence-poor candidate still defers and never blocks start.
	if !allowDeferredProbe(true, false, "", "", "", "", "", "", "") {
		t.Fatal("disabled RemuxDB should defer without metadata evidence")
	}
	if allowDeferredProbe(false, false, "", "1080p", "1080p", "h264", "h264", "", "") {
		t.Fatal("deferProbe=false must never allow deferral")
	}

	// RemuxDB enabled: keep the metadata-confidence gate.
	if allowDeferredProbe(true, true, "", "", "", "", "", "", "") {
		t.Fatal("enabled RemuxDB should not defer without resolution/codec evidence")
	}
	if !allowDeferredProbe(true, true, "", "1080p", "", "h264", "", "", "") {
		t.Fatal("enabled RemuxDB should defer with local evidence")
	}
	if !allowDeferredProbe(true, true, "", "", "1080p", "", "h264", "", "") {
		t.Fatal("enabled RemuxDB should defer with candidate evidence")
	}
	if allowDeferredProbe(true, true, "", "1080p", "", "", "", "", "") {
		t.Fatal("enabled RemuxDB needs both resolution and codec evidence")
	}

	// Strict RemuxDB match (info_hash, indexer_guid, size) can defer with RemuxDB evidence.
	for _, strictMethod := range []remuxdb.MatchMethod{remuxdb.MatchInfoHash, remuxdb.MatchIndexerGUID, remuxdb.MatchSize} {
		if !allowDeferredProbe(true, true, strictMethod, "", "", "", "", "1080p", "h264") {
			t.Fatalf("strict RemuxDB match %s should defer with remux evidence", strictMethod)
		}
	}

	// Loose RemuxDB match (size_tags with 1% variance, filename stem) must NOT defer without local/candidate evidence.
	for _, looseMethod := range []remuxdb.MatchMethod{remuxdb.MatchSizeTags, remuxdb.MatchFilename} {
		if allowDeferredProbe(true, true, looseMethod, "", "", "", "", "1080p", "h264") {
			t.Fatalf("loose RemuxDB match %s must not defer with only remux evidence", looseMethod)
		}
		// But if local or candidate has resolution and codec, deferral is allowed.
		if !allowDeferredProbe(true, true, looseMethod, "1080p", "", "h264", "", "1080p", "h264") {
			t.Fatalf("loose RemuxDB match %s should defer when local evidence exists", looseMethod)
		}
		if !allowDeferredProbe(true, true, looseMethod, "", "1080p", "", "h264", "1080p", "h264") {
			t.Fatalf("loose RemuxDB match %s should defer when candidate evidence exists", looseMethod)
		}
	}
}

func TestRemuxDBEvidenceDoesNotMakeUnprobedFileSkipProbe(t *testing.T) {
	// An unprobed file has no VideoTracks and Container="virtual".
	unprobedFile := &models.MediaFile{
		ContentID: "movie-tmdb-1",
		FilePath:  "virtual://movie/tt0000001",
		Container: "virtual",
	}

	// Before RemuxDB evidence is applied, skipProbe checks on unprobedFile must be false.
	hasVideo := completeVirtualVideoEvidenceV3(unprobedFile)
	hasAudio := completeVirtualAudioEvidenceV3(unprobedFile)
	hasContainer := completeVirtualContainerEvidenceV3(unprobedFile)
	skipProbe := hasVideo && hasAudio && hasContainer
	if skipProbe {
		t.Fatal("unprobed file must not skip probe before RemuxDB")
	}

	// Even after RemuxDB evidence is backfilled, the skipProbe decision was made beforehand,
	// ensuring untrusted crowdsourced tracks never bypass local probing or set ProbeProvenanceVerified.
	matched := map[string]remuxdb.Evidence{
		"virtual://movie/tt0000001?result=a": {
			CodecVideo:  "h264",
			Resolution:  "1080p",
			Container:   "mkv",
			VideoTracks: []remuxdb.TrackDetail{{Kind: "video", Codec: "h264", Width: 1920, Height: 1080}},
			AudioTracks: []remuxdb.TrackDetail{{Kind: "audio", Codec: "aac", Channels: 2}},
		},
	}
	backfilled := applyRemuxDBEvidence(unprobedFile, matched, "virtual://movie/tt0000001?result=a")
	if len(backfilled.VideoTracks) == 0 {
		t.Fatal("expected RemuxDB evidence to backfill transient tracks")
	}

	// If skipProbe were evaluated on backfilled, it would be true - which is why evaluating
	// skipProbe BEFORE applyRemuxDBEvidence is the critical hardening invariant.
	if completeVirtualVideoEvidenceV3(backfilled) && completeVirtualAudioEvidenceV3(backfilled) && completeVirtualContainerEvidenceV3(backfilled) {
		// Verify that unprobedFile itself remains untouched and unprobed
		if len(unprobedFile.VideoTracks) != 0 {
			t.Fatal("source unprobed file was mutated")
		}
	}
}

func TestRemuxDBSubmissionQueueBounded(t *testing.T) {
	h := &PlaybackHandler{
		RemuxDBConfig: func(context.Context) remuxdb.Config {
			return remuxdb.Config{
				Enabled:       true,
				SubmitEnabled: true,
				Token:         "tok-test",
				BaseURL:       "http://127.0.0.1:9999",
			}
		},
	}
	// Initializing queue
	probed := &models.MediaFile{
		ContentID:   "movie-tmdb-1",
		ReleaseName: "Release.2024.1080p.mkv",
		Duration:    3600,
		VideoTracks: []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}},
	}
	cand := VirtualPlaybackStream{
		URI: "virtual://movie/tt0000001?info_hash=0123456789abcdef0123456789abcdef01234567",
	}

	// Submit one probe; should enqueue without panic or blocking
	h.maybeSubmitRemuxDBEvidence(context.Background(), probed, cand)
	if h.remuxSubmitCh == nil {
		t.Fatal("expected remuxSubmitCh to be initialized")
	}
	if cap(h.remuxSubmitCh) != remuxSubmitQueueSize {
		t.Fatalf("channel capacity = %d, want %d", cap(h.remuxSubmitCh), remuxSubmitQueueSize)
	}
}

func TestRemuxDBEvidenceNeverPersistedWhenProberNil(t *testing.T) {
	calls := 0
	ts := remuxTestServer(t, &calls)
	defer ts.Close()

	persisted := false
	h := &PlaybackHandler{
		RemuxDBConfig: func(context.Context) remuxdb.Config {
			return remuxdb.Config{Enabled: true, BaseURL: ts.URL}
		},
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(ctx context.Context, path string, userID int, profileID string, ownerInstallationID int) (string, error) {
			return "https://provider.example/stream.mp4", nil
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(ctx context.Context, path string, userID int, profileID string, ownerInstallationID int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{
				{URI: "virtual://movie/tt0000001?result=a", FileSize: 28980000000, Resolution: "1080p", CodecVideo: "h264"},
			}, nil
		}),
		VirtualFileMetadataSaver: func(_ context.Context, _ int, _ string, _, _, _ []byte, _, _, _, _ string, _ bool, _ int, _ int, _ bool) error {
			persisted = true
			return nil
		},
		VirtualPlaybackSourceProber:            nil,
		VirtualPlaybackSourceProberWithHeaders: nil,
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := &models.MediaFile{ID: 10, ContentID: "movie-tmdb-1", FilePath: "virtual://movie/tt0000001"}
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource failed: %v", err)
	}
	if !resolved.AppliedRemux {
		t.Fatal("expected AppliedRemux to be true")
	}
	if resolved.Provenance == ProbeProvenanceVerified {
		t.Fatal("expected Provenance not to be ProbeProvenanceVerified")
	}
	if persisted {
		t.Fatal("unverified crowdsourced RemuxDB metadata was persisted to media_files")
	}
}

func TestRemuxDBCatalogRuntimeFallbackNeverSubmitted(t *testing.T) {
	h := &PlaybackHandler{
		RemuxDBConfig: func(context.Context) remuxdb.Config {
			return remuxdb.Config{
				Enabled:       true,
				SubmitEnabled: true,
				Token:         "tok-test",
				BaseURL:       "http://127.0.0.1:9999",
			}
		},
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(ctx context.Context, path string, userID int, profileID string, ownerInstallationID int) (string, error) {
			return "https://provider.example/stream.mp4", nil
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(ctx context.Context, path string, userID int, profileID string, ownerInstallationID int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{
				{URI: "virtual://series/tt0000001/1/1?result=a&info_hash=0123456789abcdef0123456789abcdef01234567", Label: "Show.S01E01.1080p.mkv", FileSize: 1000000000, Resolution: "1080p", CodecVideo: "h264"},
			}, nil
		}),
		// Prober returns probed file with video track but 0 duration
		VirtualPlaybackSourceProber: func(ctx context.Context, sourceURL string, file *models.MediaFile) (*models.MediaFile, error) {
			p := *file
			p.VideoTracks = []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}}
			// Ensure probed duration is 0
			p.Duration = 0
			return &p, nil
		},
		EpisodeLookup: testEpisodeLookup{
			episode: &models.Episode{ContentID: "ep-1", Runtime: 45}, // 45 minutes = 2700s
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := &models.MediaFile{ID: 10, ContentID: "series-tmdb-1", EpisodeID: "ep-1", FilePath: "virtual://series/tt0000001/1/1", ReleaseName: "Show.S01E01.1080p.mkv"}
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource failed: %v", err)
	}
	// Verify resolved file gets fallback duration for player playback
	if resolved.File.Duration != 2700 {
		t.Fatalf("resolved duration = %d, want 2700 (catalog fallback)", resolved.File.Duration)
	}
	// Verify nothing was submitted to RemuxDB queue because empiricalDuration == 0
	if len(h.remuxSubmitCh) > 0 {
		t.Fatal("catalog fallback duration was submitted to RemuxDB!")
	}
}

func TestRemuxDBSubmitsEmpiricalMeasuredDuration(t *testing.T) {
	var submittedPayload remuxdb.SubmissionPayload
	submitted := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/mediainfo" && r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&submittedPayload)
			select {
			case submitted <- struct{}{}:
			default:
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	h := &PlaybackHandler{
		RemuxDBConfig: func(context.Context) remuxdb.Config {
			return remuxdb.Config{
				Enabled:       true,
				SubmitEnabled: true,
				Token:         "tok-test",
				BaseURL:       ts.URL,
			}
		},
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(ctx context.Context, path string, userID int, profileID string, ownerInstallationID int) (string, error) {
			return "https://provider.example/stream.mp4", nil
		}),
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(ctx context.Context, path string, userID int, profileID string, ownerInstallationID int) ([]VirtualPlaybackStream, error) {
			return []VirtualPlaybackStream{
				{URI: "virtual://series/tt0000001/1/1?result=a&info_hash=0123456789abcdef0123456789abcdef01234567", Label: "Show.S01E01.1080p.mkv", FileSize: 1000000000, Resolution: "1080p", CodecVideo: "h264"},
			}, nil
		}),
		// Prober returns probed file with video track and empirically measured duration
		VirtualPlaybackSourceProber: func(ctx context.Context, sourceURL string, file *models.MediaFile) (*models.MediaFile, error) {
			p := *file
			p.VideoTracks = []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080}}
			p.Duration = 3600 // measured by probe
			return &p, nil
		},
		EpisodeLookup: testEpisodeLookup{
			episode: &models.Episode{ContentID: "ep-1", Runtime: 60}, // 60 minutes = 3600s
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	file := &models.MediaFile{ID: 10, ContentID: "series-tmdb-1", EpisodeID: "ep-1", FilePath: "virtual://series/tt0000001/1/1", ReleaseName: "Show.S01E01.1080p.mkv"}
	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", false, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource failed: %v", err)
	}
	if resolved.File.Duration != 3600 {
		t.Fatalf("resolved duration = %d, want 3600", resolved.File.Duration)
	}
	select {
	case <-submitted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for probe submission")
	}
	if submittedPayload.Duration != 3600 {
		t.Fatalf("submitted duration = %v, want 3600 (empirical duration, NOT 2700 fallback)", submittedPayload.Duration)
	}
}
