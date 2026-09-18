package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// writePlaybackTestFFmpegDecodeFailureBeforeManifest emits the decoder's
// invalid-bitstream failure past the rejection threshold and only then writes a
// ready manifest, so a start that waits for the manifest observes the rejected
// source deterministically. The existing helper writes the manifest first and
// leans on a later wait, which races the decode rejection.
func writePlaybackTestFFmpegDecodeFailureBeforeManifest(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ffmpeg-decode-fail-first.sh")
	script := "#!/bin/sh\n" +
		"i=0\n" +
		"while [ $i -lt 20 ]; do echo \"[hevc @ 0x1] Error submitting packet to decoder: Invalid data found when processing input\" >&2; i=$((i+1)); done\n" +
		"sleep 0.5\n" +
		"last=\"\"\n" +
		"for arg in \"$@\"; do last=\"$arg\"; done\n" +
		"case \"$last\" in\n" +
		"  *.m3u8) out=\"$(dirname \"$last\")\"; mkdir -p \"$out\"; " +
		"printf x > \"$out/init.mp4\"; printf x > \"$out/seg_0.m4s\"; " +
		"printf x > \"$out/seg_1.m4s\"; printf x > \"$out/seg_2.m4s\"; " +
		"printf '#EXTM3U\\n#EXT-X-VERSION:7\\n#EXT-X-TARGETDURATION:2\\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\\n#EXT-X-MAP:URI=\"init.mp4\"\\n" +
		"#EXTINF:2.0,\\nseg_0.m4s\\n#EXTINF:2.0,\\nseg_1.m4s\\n" +
		"#EXTINF:2.0,\\nseg_2.m4s\\n' > \"$last\" ;;\n" +
		"esac\n" +
		"sleep 30\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return path
}

// decodeRotationFixture is a virtual HLS-transcode handler whose provider
// candidates are named in order. The first N transcode starts are configured to
// reject the source during startup; later starts are ready. It records the
// exclusion list every detailed resolve received and the software-decode flag of
// every spawn.
type decodeRotationFixture struct {
	handler      *PlaybackHandler
	file         *models.MediaFile
	candidateIDs []string

	mu              sync.Mutex
	resolveExcluded [][]string
	marked          []string
	softwareSpawns  int
	transcodeCalls  int32
}

func newDecodeRotationFixture(t *testing.T, candidateIDs []string, softwareFallback string, decodeFailCalls int32) *decodeRotationFixture {
	t.Helper()
	source := v3HandlerFixtureFile(t)
	source.ID = 610
	source.ContentID = "movie-rot"
	source.FilePath = "virtual://movie/tt-rot"
	source.VirtualOwnerInstallationID = 5
	source.Container = "virtual"
	source.CodecVideo = "hevc"
	source.Resolution = "1080p"
	source.Bitrate = 8_000
	source.VideoTracks = []models.VideoTrack{{
		Codec: "hevc", Profile: "Main", Level: 120, Width: 1920, Height: 1080,
		FrameRate: "24000/1001", Bitrate: 8_000, BitDepth: 8,
		VideoRange: "SDR", VideoRangeType: "SDR",
	}}

	handler := NewPlaybackHandler(playback.NewSessionManager(0, 0), mapPlaybackFileResolver{files: map[int]*models.MediaFile{source.ID: source}})
	decodeFFmpeg := writePlaybackTestFFmpegDecodeFailureBeforeManifest(t)
	readyFFmpeg := writePlaybackTestFFmpeg(t)
	baseConfig := playbackTestConfig(decodeFFmpeg, t.TempDir())
	handler.PlaybackConfig = func() config.PlaybackConfig {
		cfg := baseConfig()
		cfg.HWAccel = "qsv"
		cfg.SoftwareFallback = softwareFallback
		return cfg
	}
	stubCopySeekAnchorV3(handler)
	presetLocalRegistryV3(handler, playback.NewTransformationRegistryV3([]playback.TransformationSpecV3{
		{Name: playback.TransformationAudioToAACV3, RecipeVersion: playback.TransformationAudioToAACRecipeVersionV3, Available: true},
		{Name: playback.TransformationVideoToH264V3, RecipeVersion: playback.TransformationVideoToH264RecipeVersionV3, Available: true},
	}))
	handler.ItemAccess = allowAllPlaybackItemAccess{}
	handler.SettingsRepo = &mutablePlaybackSettingsV3{values: map[string]string{
		"transcode_enabled":                      "true",
		"allow_4k_transcode":                     "true",
		"playback.max_virtual_failover_attempts": "3",
	}}

	f := &decodeRotationFixture{handler: handler, file: source, candidateIDs: candidateIDs}
	resolved := func(id string) ResolvedVirtualMedia {
		return ResolvedVirtualMedia{
			URL:         "http://127.0.0.1:9/stream?result=" + id,
			URI:         "virtual://movie/tt-rot?result=" + id,
			CandidateID: id,
		}
	}
	handler.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
		return "http://127.0.0.1:9/stream?path=" + path, nil
	})
	handler.VirtualPlaybackStreamLister = VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
		out := make([]VirtualPlaybackStream, 0, len(candidateIDs))
		for _, id := range candidateIDs {
			out = append(out, VirtualPlaybackStream{
				ID: id, URI: "virtual://movie/tt-rot?result=" + id,
				Resolution: source.Resolution, CodecVideo: source.CodecVideo, CodecAudio: source.CodecAudio, Container: "mkv",
			})
		}
		return out, nil
	})
	handler.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(_ context.Context, virtualURI string, _ int, _ int, _ string, _ bool, excluded []string, preferred string) (ResolvedVirtualMedia, error) {
		f.mu.Lock()
		f.resolveExcluded = append(f.resolveExcluded, append([]string(nil), excluded...))
		f.mu.Unlock()
		// Mirror the real resolver: the URI's pinned result= wins when it is
		// still eligible, then the preferred candidate, then the ranked list.
		if parsed, err := url.Parse(virtualURI); err == nil {
			if id := strings.TrimSpace(parsed.Query().Get("result")); id != "" && !containsStringExactV3(excluded, id) {
				return resolved(id), nil
			}
		}
		if preferred != "" && !containsStringExactV3(excluded, preferred) {
			return resolved(preferred), nil
		}
		for _, id := range candidateIDs {
			if !containsStringExactV3(excluded, id) {
				return resolved(id), nil
			}
		}
		return ResolvedVirtualMedia{}, errors.New("no eligible virtual candidate")
	})
	handler.VirtualPlaybackSourceProber = func(_ context.Context, _ string, file *models.MediaFile) (*models.MediaFile, error) {
		file.VideoTracks = source.VideoTracks
		file.AudioTracks = source.AudioTracks
		file.CodecVideo, file.CodecAudio, file.Resolution, file.Container = "hevc", "aac", "1080p", "mkv"
		return file, nil
	}
	handler.StartTranscodeFunc = func(ctx context.Context, opts playback.TranscodeOpts) (*playback.TranscodeSession, error) {
		call := atomic.AddInt32(&f.transcodeCalls, 1)
		if call <= decodeFailCalls {
			opts.FFmpegPath = decodeFFmpeg
		} else {
			opts.FFmpegPath = readyFFmpeg
		}
		if opts.SoftwareVideoDecode {
			f.mu.Lock()
			f.softwareSpawns++
			f.mu.Unlock()
		}
		return playback.StartTranscode(ctx, opts)
	}
	handler.TranscodeManager().OnSourceRejected = func(_ context.Context, _ int, canonical string) error {
		f.mu.Lock()
		f.marked = append(f.marked, canonical)
		f.mu.Unlock()
		return nil
	}
	return f
}

func (f *decodeRotationFixture) request() playback.StartRequestV3 {
	start := v3HandlerStartRequest()
	start.FileID = f.file.ID
	start.QualityPreference = "auto"
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3] = playback.DeliveryCapabilityV3{
		Enabled: true, SupportedOnDevice: true,
		Containers: []string{"hls"}, VideoCodecs: []string{"h264"}, AudioDecodeCodecs: []string{"aac"},
	}
	return start
}

func (f *decodeRotationFixture) start(t *testing.T, start playback.StartRequestV3) (int, playback.DecisionResponseV3) {
	t.Helper()
	rr := httptest.NewRecorder()
	f.handler.HandleStartPlayback(rr, httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(marshalV3StartRequest(t, start))).WithContext(newAuthorizedPlaybackContext()))
	var response playback.DecisionResponseV3
	if rr.Body.Len() > 0 {
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode start response: %v; body = %s", err, rr.Body.String())
		}
	}
	return rr.Code, response
}

func (f *decodeRotationFixture) sessionVirtualURI(t *testing.T, sessionID string) string {
	t.Helper()
	session, err := f.handler.sessionMgr.GetSession(sessionID)
	if err != nil || session == nil {
		t.Fatalf("GetSession(%q): %v", sessionID, err)
	}
	return session.VirtualSourceURI
}

func (f *decodeRotationFixture) recordedExclusions() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.resolveExcluded))
	copy(out, f.resolveExcluded)
	return out
}

// waitForMarked waits on the observable rejection stamp the async transcode
// callback writes, rather than racing it with a fixed sleep.
func (f *decodeRotationFixture) waitForMarked(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		marked := len(f.marked)
		f.mu.Unlock()
		if marked > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("rejected source was never stamped on the candidate")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func containsExclusion(exclusions [][]string, want ...string) bool {
	for _, got := range exclusions {
		if len(got) != len(want) {
			continue
		}
		match := true
		for i := range want {
			if got[i] != want[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// TestVirtualStartRotatesDecodeRejectedCandidate drives an auto start whose first
// provider release is rejected during startup. The server must substitute the
// second release before committing, so the client receives a playable plan (no
// 422) bound to the second candidate and the requested catalog id is preserved.
func TestVirtualStartRotatesDecodeRejectedCandidate(t *testing.T) {
	f := newDecodeRotationFixture(t, []string{"A", "B"}, "", 1)

	code, response := f.start(t, f.request())
	if code != http.StatusCreated {
		t.Fatalf("start status = %d, want %d; body = %+v", code, http.StatusCreated, response)
	}
	if response.Terminal != nil {
		t.Fatalf("auto start terminalled instead of rotating: %+v", response.Terminal)
	}
	if response.PlaybackPlan == nil || response.PlaybackPlan.Delivery != playback.DeliveryTranscodeHLSV3 {
		t.Fatalf("start plan = %#v, want a server-transcode plan", response.PlaybackPlan)
	}
	if response.PlaybackPlan.EffectiveMediaFileID != f.file.ID {
		t.Fatalf("effective file = %d, want the requested catalog id %d", response.PlaybackPlan.EffectiveMediaFileID, f.file.ID)
	}
	if got := f.sessionVirtualURI(t, response.SessionID); got != "virtual://movie/tt-rot?result=B" {
		t.Fatalf("effective virtual uri = %q, want the rotated candidate B", got)
	}
	if !containsExclusion(f.recordedExclusions(), "A") {
		t.Fatalf("rotation never excluded the rejected candidate; exclusions = %v", f.recordedExclusions())
	}
	f.waitForMarked(t)
}

// TestVirtualExplicitStartDecodeRejectionTerminals proves an explicit version
// pin is never substituted: a decode rejection terminalls with
// source_decode_failed plus the version-list hint, and the rejection is still
// stamped on the candidate.
func TestVirtualExplicitStartDecodeRejectionTerminals(t *testing.T) {
	f := newDecodeRotationFixture(t, []string{"A", "B"}, "", 1)
	start := f.request()
	start.FileSelection = playback.FileSelectionExplicitV3

	code, response := f.start(t, start)
	if code != http.StatusCreated {
		t.Fatalf("start status = %d, want %d; body = %+v", code, http.StatusCreated, response)
	}
	if response.PlaybackPlan != nil {
		t.Fatalf("explicit pin was substituted: %#v", response.PlaybackPlan)
	}
	if response.Terminal == nil || response.Terminal.Reason != sourceDecodeFailedReasonV3 {
		t.Fatalf("terminal = %#v, want reason %q", response.Terminal, sourceDecodeFailedReasonV3)
	}
	if !strings.Contains(response.Terminal.Message, "version list") {
		t.Fatalf("explicit decode terminal missing the alternate hint: %q", response.Terminal.Message)
	}
	for _, excluded := range f.recordedExclusions() {
		if len(excluded) > 0 {
			t.Fatalf("explicit pin excluded candidates %v; an explicit pin is never substituted", excluded)
		}
	}
	f.waitForMarked(t)
}

// TestVirtualDecodeRotationBoundedExhaustion proves rotation is bounded by the
// configured failover attempts and excludes every rejected id, so a provider
// that only offers bad releases cannot loop forever.
func TestVirtualDecodeRotationBoundedExhaustion(t *testing.T) {
	f := newDecodeRotationFixture(t, []string{"A", "B", "C"}, "", 3)

	code, response := f.start(t, f.request())
	if code != http.StatusCreated {
		t.Fatalf("start status = %d, want %d; body = %+v", code, http.StatusCreated, response)
	}
	if response.Terminal == nil || response.Terminal.Reason != sourceDecodeFailedReasonV3 {
		t.Fatalf("exhausted rotation terminal = %#v, want reason %q", response.Terminal, sourceDecodeFailedReasonV3)
	}
	if response.Terminal.Retryable {
		t.Fatalf("exhausted decode terminal is retryable: %#v", response.Terminal)
	}
	exclusions := f.recordedExclusions()
	if !containsExclusion(exclusions, "A") || !containsExclusion(exclusions, "A", "B") {
		t.Fatalf("rotation did not accumulate exclusions: %v", exclusions)
	}
	if calls := atomic.LoadInt32(&f.transcodeCalls); calls > 3 {
		t.Fatalf("transcode started %d times, want at most maxVirtualFailoverAttempts=3", calls)
	}
}

// TestVirtualReplanDecodeErrorRotates drives a decode_error failure_recovery on
// a healthy virtual session. The replan must exclude the session-bound candidate
// and commit the next release, without demoting the HLS delivery.
func TestVirtualReplanDecodeErrorRotates(t *testing.T) {
	f := newDecodeRotationFixture(t, []string{"A", "B"}, "", 0)
	start := f.request()

	code, started := f.start(t, start)
	if code != http.StatusCreated || started.PlaybackPlan == nil {
		t.Fatalf("start status=%d response=%+v", code, started)
	}
	defer f.handler.tm.CloseTranscodeSession(started.SessionID, "")
	if got := f.sessionVirtualURI(t, started.SessionID); got != "virtual://movie/tt-rot?result=A" {
		t.Fatalf("start candidate = %q, want A", got)
	}

	recovery := playback.ReplanRequestV3{
		ProtocolVersion: playback.ProtocolV3, ClientFeatures: start.ClientFeatures,
		Operation: playback.ReplanOperationFailureRecoveryV3, PlaybackAttemptID: start.PlaybackAttemptID,
		ReplanRequestID: "decode-rotation-replan-0001", FailedPlanID: started.PlaybackPlan.PlanID,
		PlanAttemptID: "decode-rotation-attempt-0001", PlanAttemptKey: started.PlaybackPlan.PlanAttemptKey,
		AttemptedPlanKeys: []string{started.PlaybackPlan.PlanAttemptKey}, AttemptCount: 1,
		PositionSeconds: 10, SelectedTracks: started.PlaybackPlan.SelectedTracks,
		Failure:               playback.FailureV3{Classification: "decode_error"},
		Capabilities:          start.Capabilities,
		ClientPlaybackContext: start.ClientPlaybackContext,
	}
	recovered := postPlaybackReplanV3(t, f.handler, started.SessionID, recovery)
	if recovered.Terminal != nil || recovered.PlaybackPlan == nil {
		t.Fatalf("decode replan terminal=%+v", recovered.Terminal)
	}
	if recovered.PlaybackPlan.Delivery != playback.DeliveryTranscodeHLSV3 {
		t.Fatalf("rotated plan delivery = %s, want HLS", recovered.PlaybackPlan.Delivery)
	}
	if !containsExclusion(f.recordedExclusions(), "A") {
		t.Fatalf("replan did not exclude the rejected session candidate; exclusions = %v", f.recordedExclusions())
	}
	if got := f.sessionVirtualURI(t, started.SessionID); got != "virtual://movie/tt-rot?result=B" {
		t.Fatalf("replanned effective virtual uri = %q, want B", got)
	}
	record, err := f.handler.PlanStoreV3.GetAttempt(t.Context(), started.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	caps := record.NormalizedRequest.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3]
	if !caps.Enabled || caps.FailureReason == demoteDeliveryReasonV3 {
		t.Fatalf("HLS delivery was demoted while rotation was pending: %+v", caps)
	}
}

// TestVirtualDecodeRotationGPUOnlyRetiresOnNoAlternate proves gpu_only never
// spawns a software decode and, when no sibling candidate exists, the delivery
// is retired and the replan terminalls with source_decode_failed.
func TestVirtualDecodeRotationGPUOnlyRetiresOnNoAlternate(t *testing.T) {
	f := newDecodeRotationFixture(t, []string{"A"}, "gpu_only", 0)
	start := f.request()

	code, started := f.start(t, start)
	if code != http.StatusCreated || started.PlaybackPlan == nil {
		t.Fatalf("start status=%d response=%+v", code, started)
	}
	defer f.handler.tm.CloseTranscodeSession(started.SessionID, "")

	recovery := playback.ReplanRequestV3{
		ProtocolVersion: playback.ProtocolV3, ClientFeatures: start.ClientFeatures,
		Operation: playback.ReplanOperationFailureRecoveryV3, PlaybackAttemptID: start.PlaybackAttemptID,
		ReplanRequestID: "gpu-only-decode-replan-0001", FailedPlanID: started.PlaybackPlan.PlanID,
		PlanAttemptID: "gpu-only-decode-attempt-0001", PlanAttemptKey: started.PlaybackPlan.PlanAttemptKey,
		AttemptedPlanKeys: []string{started.PlaybackPlan.PlanAttemptKey}, AttemptCount: 1,
		PositionSeconds: 10, SelectedTracks: started.PlaybackPlan.SelectedTracks,
		Failure:               playback.FailureV3{Classification: "decode_error"},
		Capabilities:          start.Capabilities,
		ClientPlaybackContext: start.ClientPlaybackContext,
	}
	recovered := postPlaybackReplanV3(t, f.handler, started.SessionID, recovery)
	if recovered.Terminal == nil || recovered.Terminal.Reason != sourceDecodeFailedReasonV3 {
		t.Fatalf("gpu_only no-alternate terminal = %#v, want %q", recovered.Terminal, sourceDecodeFailedReasonV3)
	}
	f.mu.Lock()
	softwareSpawns := f.softwareSpawns
	f.mu.Unlock()
	if softwareSpawns != 0 {
		t.Fatalf("gpu_only spawned %d software-decode transcodes, want 0", softwareSpawns)
	}
	record, err := f.handler.PlanStoreV3.GetAttempt(t.Context(), started.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	caps := record.NormalizedRequest.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3]
	if caps.Enabled || caps.FailureReason != demoteDeliveryReasonV3 {
		t.Fatalf("gpu_only kept the exhausted HLS delivery eligible: %+v", caps)
	}
}

// TestVirtualDecodeRotationReplanIsIdempotent proves replaying the same
// replan_request_id returns the first response without rotating again.
func TestVirtualDecodeRotationReplanIsIdempotent(t *testing.T) {
	f := newDecodeRotationFixture(t, []string{"A", "B"}, "", 0)
	start := f.request()

	code, started := f.start(t, start)
	if code != http.StatusCreated || started.PlaybackPlan == nil {
		t.Fatalf("start status=%d response=%+v", code, started)
	}
	defer f.handler.tm.CloseTranscodeSession(started.SessionID, "")

	recovery := playback.ReplanRequestV3{
		ProtocolVersion: playback.ProtocolV3, ClientFeatures: start.ClientFeatures,
		Operation: playback.ReplanOperationFailureRecoveryV3, PlaybackAttemptID: start.PlaybackAttemptID,
		ReplanRequestID: "idempotent-decode-replan-0001", FailedPlanID: started.PlaybackPlan.PlanID,
		PlanAttemptID: "idempotent-decode-attempt-0001", PlanAttemptKey: started.PlaybackPlan.PlanAttemptKey,
		AttemptedPlanKeys: []string{started.PlaybackPlan.PlanAttemptKey}, AttemptCount: 1,
		PositionSeconds: 10, SelectedTracks: started.PlaybackPlan.SelectedTracks,
		Failure:               playback.FailureV3{Classification: "decode_error"},
		Capabilities:          start.Capabilities,
		ClientPlaybackContext: start.ClientPlaybackContext,
	}
	first := postPlaybackReplanV3(t, f.handler, started.SessionID, recovery)
	firstResolves := len(f.recordedExclusions())
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}

	replay := recovery
	replay.FailedPlanID = first.PlaybackPlan.PlanID
	second := postPlaybackReplanV3(t, f.handler, started.SessionID, replay)
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("idempotent replay diverged:\n first=%s\nsecond=%s", firstJSON, secondJSON)
	}
	if got := len(f.recordedExclusions()); got != firstResolves {
		t.Fatalf("idempotent replay re-ran rotation: resolves %d -> %d", firstResolves, got)
	}
}

// TestTranscodeManifestDoesNotRotateAfterSourceRejected proves the manifest
// route keeps answering the permanent decode verdict for the generation already
// on screen and never swaps the live transcode session for a different one.
func TestTranscodeManifestDoesNotRotateAfterSourceRejected(t *testing.T) {
	handler, _ := hardwareDecodeFailureHandler(t)

	start := v3HandlerStartRequest()
	start.QualityPreference = "auto"
	start.ClientFeatures = append(start.ClientFeatures, playback.FeatureHeaderAuthenticatedMediaV3)
	start.ClientPlaybackContext.Deliveries[playback.DeliveryClassHLSV3] = playback.DeliveryCapabilityV3{
		Enabled: true, SupportedOnDevice: true,
		Containers:        []string{"hls"},
		VideoCodecs:       []string{"h264"},
		AudioDecodeCodecs: []string{"aac"},
	}
	rr := httptest.NewRecorder()
	handler.HandleStartPlayback(rr, httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", strings.NewReader(marshalV3StartRequest(t, start))).WithContext(newAuthorizedPlaybackContext()))
	var started playback.DecisionResponseV3
	if rr.Code != http.StatusCreated || json.Unmarshal(rr.Body.Bytes(), &started) != nil || started.PlaybackPlan == nil {
		t.Fatalf("start status=%d body=%s", rr.Code, rr.Body.String())
	}
	defer handler.tm.CloseTranscodeSession(started.SessionID, "")

	live := handler.tm.GetTranscodeSession(started.SessionID)
	if live == nil {
		t.Fatal("start registered no live transcode session")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !live.IsSourceRejected() {
		if time.Now().After(deadline) {
			t.Fatal("the decoder rejection was never observed on the live session")
		}
		time.Sleep(5 * time.Millisecond)
	}

	recorder := httptest.NewRecorder()
	handler.HandleGetTranscodeManifest(recorder, playbackTestRequest(
		http.MethodGet,
		"/api/v1/playback/transcode/"+started.SessionID+"/master.m3u8",
		nil,
		map[string]string{"session_id": started.SessionID},
	))
	assertDecodeFailureResponse(t, recorder)
	if after := handler.tm.GetTranscodeSession(started.SessionID); after != live {
		t.Fatal("manifest route swapped the transcode generation after a decode rejection")
	}
}
