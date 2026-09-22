package handlers

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/scanner"
)

// p0PinSeams records the seams the P0 pre-validation gate exercises. The
// detailed resolver and prober spies prove whether the repeat-play fast path
// skipped the provider; the clear spy proves the failed-verdict recovery fired.
type p0PinSeams struct {
	detailedCalls     int
	probeCalls        int
	clearCalls        int
	lastClearFilePath string
	lastClearFailedAt *time.Time
	onClear           func()
}

// newP0PinHandler wires a detailed-resolver spy, a synchronous prober spy, and
// the failed-verdict clear spy onto the minimum seams resolveVirtualPlaybackSource
// needs. The legacy resolver is present only because the entry point requires one.
func newP0PinHandler(seams *p0PinSeams) *PlaybackHandler {
	h := &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(
			func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
				return "http://provider.example/legacy?path=" + path, nil
			}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(
			func(_ context.Context, virtualURI string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
				seams.detailedCalls++
				return ResolvedVirtualMedia{URL: "http://provider.example/stream.mkv", URI: virtualURI}, nil
			}),
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			seams.probeCalls++
			f.VideoTracks = []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080, FrameRate: "24", BitDepth: 8, Bitrate: 10_000}}
			f.AudioTracks = []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng"}}
			f.CodecVideo, f.CodecAudio, f.Resolution, f.Container = "h264", "aac", "1080p", "mkv"
			return f, nil
		},
		VirtualCandidateClearFailedMarker: func(_ context.Context, _ int, expectedFilePath string, observedFailedAt *time.Time) error {
			seams.clearCalls++
			seams.lastClearFilePath = expectedFilePath
			seams.lastClearFailedAt = observedFailedAt
			if seams.onClear != nil {
				seams.onClear()
			}
			return nil
		},
	}
	return h
}

// p0PinFile is a virtual row with complete probed evidence and a probe stamp of
// the requested age: the state the P0 repeat-play fast path recognizes.
func p0PinFile(path string, probeAge time.Duration) *models.MediaFile {
	probedAt := time.Now().Add(-probeAge)
	return &models.MediaFile{
		ID:                         700,
		ContentID:                  "movie-p0",
		FilePath:                   path,
		Container:                  "mkv",
		CodecVideo:                 "h264",
		CodecAudio:                 "aac",
		Resolution:                 "1080p",
		Bitrate:                    10_000,
		ProbeUpdatedAt:             &probedAt,
		VirtualOwnerInstallationID: 5,
		VideoTracks:                []models.VideoTrack{{Codec: "h264", Width: 1920, Height: 1080, FrameRate: "24", BitDepth: 8, Bitrate: 10_000}},
		AudioTracks:                []models.AudioTrack{{Codec: "aac", Channels: 2, Language: "eng"}},
	}
}

func p0PinStartRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
}

// The staleness bound is a fixed multiple of the background refresh cadence.
// Pin it so a later change to either constant is a deliberate one. The reason
// classifier must flag nil, failed, and stale rows and clear a fresh one.
func TestVirtualP0ProbeStalenessBoundAndReason(t *testing.T) {
	if virtualP0ProbeStalenessBound != 12*time.Hour {
		t.Fatalf("virtualP0ProbeStalenessBound = %s, want 12h (24 * %s)", virtualP0ProbeStalenessBound, virtualCandidateRefreshInterval)
	}
	now := time.Now()
	if !p0PinSuspect(nil, now) {
		t.Fatal("nil row must be suspect")
	}
	if got := p0PinSuspectReason(nil, now); got != "missing_row" {
		t.Fatalf("nil row reason = %q, want missing_row", got)
	}

	fresh := p0PinFile("virtual://movie/tt-p0-reason?result=cand-1", time.Hour)
	if p0PinSuspect(fresh, now) {
		t.Fatal("fresh, non-failed row must not be suspect")
	}

	stale := p0PinFile("virtual://movie/tt-p0-reason?result=cand-1", virtualP0ProbeStalenessBound+time.Minute)
	if !p0PinSuspect(stale, now) {
		t.Fatal("row past the staleness bound must be suspect")
	}
	if got := p0PinSuspectReason(stale, now); got != "stale_probe" {
		t.Fatalf("stale row reason = %q, want stale_probe", got)
	}

	failedFile := p0PinFile("virtual://movie/tt-p0-reason?result=cand-1", time.Hour)
	failedAt := now.Add(-time.Hour)
	failedFile.FailedAt = &failedAt
	if got := p0PinSuspectReason(failedFile, now); got != "failed_verdict" {
		t.Fatalf("failed row reason = %q, want failed_verdict", got)
	}
}

// A fresh, non-failed row with complete probed evidence keeps the P0 fast path:
// no provider resolve and no listing. This is the back-compat guarantee the
// pre-validation gate must not disturb.
func TestResolveVirtualP0FreshPinStillFast(t *testing.T) {
	seams := &p0PinSeams{}
	h := newP0PinHandler(seams)
	file := p0PinFile("virtual://movie/tt-p0-fresh?result=cand-1", time.Hour)

	resolved, err := h.resolveVirtualPlaybackSource(p0PinStartRequest(), file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if seams.detailedCalls != 0 {
		t.Fatalf("detailed resolver called %d times, want 0 for a fresh pin", seams.detailedCalls)
	}
	if resolved.URL != "" || resolved.URI != file.FilePath {
		t.Fatalf("resolved URL=%q URI=%q, want empty URL and the persisted pin", resolved.URL, resolved.URI)
	}
	if resolved.Provenance != ProbeProvenancePending || resolved.ProbeSucceeded {
		t.Fatalf("provenance=%q succeeded=%v, want pending/false P0 fast path", resolved.Provenance, resolved.ProbeSucceeded)
	}
}

// An explicitly retried row whose verdict damper is still active must not take
// P0: it re-resolves through the provider, clears the stale verdict after the
// same-identity resolve, and returns to the fast path on the next replay. This
// is the core audit fix.
func TestResolveVirtualP0FailedPinRevalidatesAndRecovers(t *testing.T) {
	seams := &p0PinSeams{}
	file := p0PinFile("virtual://movie/tt-p0-failed?result=cand-1", time.Hour)
	failedAt := time.Now().Add(-time.Hour)
	file.FailedAt = &failedAt
	// Simulate the fenced DB clear landing so the second call observes recovery.
	seams.onClear = func() { file.FailedAt = nil }
	h := newP0PinHandler(seams)
	req := p0PinStartRequest()

	resolved, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false, virtualResolveOptionsV3{allowFailedCandidate: true})
	if err != nil {
		t.Fatalf("first resolve error: %v", err)
	}
	if seams.detailedCalls != 1 {
		t.Fatalf("detailed resolver called %d times, want 1: a failed pin must not take P0", seams.detailedCalls)
	}
	if seams.clearCalls != 1 {
		t.Fatalf("verdict clear called %d times, want 1 after the same-identity resolve", seams.clearCalls)
	}
	if seams.lastClearFilePath != file.FilePath {
		t.Fatalf("clear path = %q, want the resolved pin %q", seams.lastClearFilePath, file.FilePath)
	}
	if seams.lastClearFailedAt == nil || !seams.lastClearFailedAt.Equal(failedAt) {
		t.Fatalf("clear observed failed_at = %v, want the observed verdict %v", seams.lastClearFailedAt, failedAt)
	}
	// The suspect fall-through forces a real probe so probe_updated_at advances
	// and the row cannot stay suspect forever.
	if seams.probeCalls != 1 {
		t.Fatalf("prober called %d times, want 1 for a suspect fall-through", seams.probeCalls)
	}
	if resolved.Provenance != ProbeProvenanceVerified || !resolved.ProbeSucceeded {
		t.Fatalf("first resolve provenance=%q succeeded=%v, want verified/true", resolved.Provenance, resolved.ProbeSucceeded)
	}

	// Next replay: the verdict is cleared and the probe stamp is fresh, so P0
	// returns without another provider round-trip.
	second, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("second resolve error: %v", err)
	}
	if seams.detailedCalls != 1 {
		t.Fatalf("detailed resolver called %d times after recovery, want 1 (P0 on replay)", seams.detailedCalls)
	}
	if second.Provenance != ProbeProvenancePending || second.URI != file.FilePath {
		t.Fatalf("second resolve provenance=%q URI=%q, want pending P0 and the persisted pin", second.Provenance, second.URI)
	}
}

// A pin whose probe evidence is older than the staleness bound falls through to
// the provider instead of being served blind, and the skip is logged distinctly
// so a deployment can see why a replay cost a provider round-trip.
func TestResolveVirtualP0StalePinFallsThrough(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	seams := &p0PinSeams{}
	h := newP0PinHandler(seams)
	file := p0PinFile("virtual://movie/tt-p0-stale?result=cand-1", virtualP0ProbeStalenessBound+time.Hour)

	resolved, err := h.resolveVirtualPlaybackSource(p0PinStartRequest(), file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if seams.detailedCalls != 1 {
		t.Fatalf("detailed resolver called %d times, want 1 for a stale pin", seams.detailedCalls)
	}
	if seams.probeCalls != 1 {
		t.Fatalf("prober called %d times, want 1: a stale fall-through must refresh the probe stamp", seams.probeCalls)
	}
	if resolved.URL == "" || resolved.Provenance != ProbeProvenanceVerified {
		t.Fatalf("resolved URL=%q provenance=%q, want a validated fall-through", resolved.URL, resolved.Provenance)
	}
	if !strings.Contains(logs.String(), "fast_path_skipped_suspect_pin") || !strings.Contains(logs.String(), "stale_probe") {
		t.Fatalf("stale-pin skip was not logged distinctly: %s", logs.String())
	}
}

// A delivered collection row keeps the P0 fast path: the #84 exemption covers
// collection rows that actually delivered bytes.
func TestResolveVirtualP0DeliveredCollectionPinStillFast(t *testing.T) {
	seams := &p0PinSeams{}
	h := newP0PinHandler(seams)
	file := p0PinFile("virtual://movie/tt-p0-collection?result=cand-1", time.Hour)
	file.ProbeSource = virtualCollectionProbeSource
	deliveredAt := time.Now().Add(-time.Hour)
	file.LastDeliveredAt = &deliveredAt

	resolved, err := h.resolveVirtualPlaybackSource(p0PinStartRequest(), file, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if seams.detailedCalls != 0 {
		t.Fatalf("detailed resolver called %d times, want 0 for a delivered collection pin", seams.detailedCalls)
	}
	if resolved.URL != "" || resolved.Provenance != ProbeProvenancePending {
		t.Fatalf("resolved URL=%q provenance=%q, want empty/pending P0", resolved.URL, resolved.Provenance)
	}
}

// A never-delivered collection row still falls through: the staleness gate must
// not weaken the existing #84 delivery exemption.
func TestResolveVirtualP0UndeliveredCollectionPinFallsThrough(t *testing.T) {
	seams := &p0PinSeams{}
	h := newP0PinHandler(seams)
	file := p0PinFile("virtual://movie/tt-p0-collection-undelivered?result=cand-1", time.Hour)
	file.ProbeSource = virtualCollectionProbeSource

	if _, err := h.resolveVirtualPlaybackSource(p0PinStartRequest(), file, "profile-1", true, nil, "", "", 0, false); err != nil {
		t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
	}
	if seams.detailedCalls != 1 {
		t.Fatalf("detailed resolver called %d times, want 1 for a never-delivered collection pin", seams.detailedCalls)
	}
}

// TestResolveVirtualP0FailedPinClearsVerdictInDatabase is the end-to-end
// recovery proof: a failed row that falls through the P0 gate re-resolves
// against the provider, clears its failed_at through the same fenced scanner
// clear the versions check uses, and the reloaded row takes the fast path on the
// next play. It skips without SILO_TEST_DATABASE_URL.
func TestResolveVirtualP0FailedPinClearsVerdictInDatabase(t *testing.T) {
	pool := virtualMetadataUpdateTestPool(t)
	ctx := context.Background()
	const folderID, ownerID = 994451, 7151
	seedVirtualMetadataUpdateFolder(t, pool, folderID, ownerID)

	candidatePath := "virtual://movie/tt-p0-db?result=cand-db"
	id, _, _ := seedResolvedCandidateRow(t, pool, folderID, ownerID, "movie-p0-db", candidatePath, models.MediaFile{})
	probedAt := time.Now().Add(-time.Hour).UTC()
	// Complete probed evidence plus an active verdict: the exact row the P0 gate
	// must refuse to serve blind.
	if _, err := pool.Exec(ctx, `
		UPDATE media_files SET
			file_size = 0,
			codec_video = 'h264', codec_audio = 'aac', resolution = '1080p', container = 'mkv',
			bitrate = 10000,
			video_tracks = '[{"codec":"h264","width":1920,"height":1080,"frame_rate":"24","bit_depth":8,"bitrate":10000}]'::jsonb,
			audio_tracks = '[{"codec":"aac","channels":2,"language":"eng"}]'::jsonb,
			probe_updated_at = $2,
			failed_at = NOW()
		WHERE id = $1`, id, probedAt); err != nil {
		t.Fatalf("seed failed complete-evidence row: %v", err)
	}

	repo := scanner.NewFileRepository(pool)
	row, err := repo.GetByPath(ctx, candidatePath)
	if err != nil || row == nil {
		t.Fatalf("load seeded row: %v", err)
	}
	if row.FailedAt == nil {
		t.Fatal("seeded row did not carry a verdict")
	}

	detailedCalls := 0
	probeCalls := 0
	h := &PlaybackHandler{
		VirtualPlaybackResolver: VirtualPlaybackResolverFunc(func(_ context.Context, path string, _ int, _ string, _ int) (string, error) {
			return "https://93.184.216.34/legacy?path=" + path, nil
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			detailedCalls++
			return ResolvedVirtualMedia{URL: "https://93.184.216.34/stream/token=fresh", URI: candidatePath, CandidateID: "cand-db"}, nil
		}),
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, f *models.MediaFile) (*models.MediaFile, error) {
			probeCalls++
			return f, nil
		},
		VirtualFileLookup: func(ctx context.Context, path string) (*models.MediaFile, error) {
			loaded, lookupErr := repo.GetByPath(ctx, path)
			if lookupErr != nil {
				return nil, ErrVirtualCandidateNotFound
			}
			return loaded, nil
		},
		// The production wiring: the fenced scanner clear the versions check uses.
		VirtualCandidateClearFailedMarker: repo.ClearVirtualCandidateFailed,
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
	if _, err := h.resolveVirtualPlaybackSource(req, row, "profile-1", true, nil, "", "", 0, false, virtualResolveOptionsV3{allowFailedCandidate: true}); err != nil {
		t.Fatalf("first resolve error: %v", err)
	}
	if detailedCalls != 1 {
		t.Fatalf("detailed resolver calls = %d, want 1: a failed pin must revalidate", detailedCalls)
	}
	if probeCalls != 1 {
		t.Fatalf("prober calls = %d, want 1: the suspect fall-through must refresh the probe", probeCalls)
	}

	reloaded, err := repo.GetByPath(ctx, candidatePath)
	if err != nil || reloaded == nil {
		t.Fatalf("reload row: %v", err)
	}
	if reloaded.FailedAt != nil {
		t.Fatalf("failed_at = %v, want cleared after the same-identity re-resolve", reloaded.FailedAt)
	}

	// The reloaded, fresh, non-failed row takes P0 without another provider call.
	detailedCalls = 0
	second, err := h.resolveVirtualPlaybackSource(req, reloaded, "profile-1", true, nil, "", "", 0, false)
	if err != nil {
		t.Fatalf("second resolve error: %v", err)
	}
	if detailedCalls != 0 {
		t.Fatalf("detailed resolver calls on replay = %d, want 0 (P0)", detailedCalls)
	}
	if second.Provenance != ProbeProvenancePending || second.URI != candidatePath {
		t.Fatalf("replay provenance=%q URI=%q, want pending P0 at %q", second.Provenance, second.URI, candidatePath)
	}
}
