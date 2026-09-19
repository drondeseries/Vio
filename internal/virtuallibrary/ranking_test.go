package virtuallibrary_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/quality"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

// --- fixtures ---------------------------------------------------------------

type classifyByURL map[string]struct{ failed, confirmed bool }

func (c classifyByURL) ClassifyCandidates(candidates []resolver.StreamCandidate) {
	for i := range candidates {
		state := c[candidates[i].URL]
		if state.failed {
			candidates[i].SourceFailed = true
		}
		if state.confirmed {
			candidates[i].SourceConfirmed = true
		}
	}
}

type fakeGate struct {
	released bool
	air      *time.Time
}

func (g fakeGate) IsReleased(string, string, int, int) (bool, *time.Time) {
	return g.released, g.air
}

func streamEntry(name, rawURL string) map[string]any {
	return map[string]any{"name": name, "title": name, "url": rawURL}
}

func tokenFormat(name, pattern string, score int, reject bool) quality.CustomFormat {
	return quality.CustomFormat{
		Name: name, Pattern: pattern, PatternType: "token",
		Score: score, Reject: reject, Enabled: true,
	}
}

// newProviderService starts a Stremio provider stub that serves streams and
// builds the core service pointed at it.
func newProviderService(t *testing.T, logger *slog.Logger, cfg virtuallibrary.Config, streams ...map[string]any) *virtuallibrary.Service {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"streams": streams})
	if err != nil {
		t.Fatalf("marshal provider streams: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/manifest.json" {
			_, _ = w.Write([]byte(`{"id":"org.stremio.test","resources":["stream"],"types":["movie","series"]}`))
			return
		}
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)

	cfg.Enabled = true
	cfg.ManifestURL = server.URL + "/manifest.json"
	cfg.AllowInsecureHTTP = true
	svc := virtuallibrary.New(cfg, nil, logger)
	if svc == nil {
		t.Fatal("service is nil")
	}
	return svc
}

// --- ranking ----------------------------------------------------------------

// TestListStreamsAppliesCustomFormatRanking proves the custom-format score,
// not the provider's order or raw resolution, decides the version list.
func TestListStreamsAppliesCustomFormatRanking(t *testing.T) {
	cfg := virtuallibrary.Config{Quality: quality.QualityConfig{
		CustomFormats: []quality.CustomFormat{tokenFormat("Prefer 720p", "720p", 500, false)},
	}}
	svc := newProviderService(t, nil, cfg,
		streamEntry("1080p", "http://192.168.1.10/1080.mkv"),
		streamEntry("720p", "http://192.168.1.10/720.mkv"),
	)

	streams, err := svc.ListStreams(context.Background(), "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("ListStreams: %v", err)
	}
	if len(streams) != 2 {
		t.Fatalf("streams = %d, want 2", len(streams))
	}
	if streams[0].Resolution != "720p" {
		t.Fatalf("ranked order = %q, %q; want the higher-scored 720p first", streams[0].Resolution, streams[1].Resolution)
	}
	if streams[0].QualityScore == 0 {
		t.Fatal("QualityScore was not populated on the ranked stream")
	}
}

// TestListStreamsKeepsRejectedBehindAccepted proves a rejected candidate is
// rank-last but still returned, never dropped.
func TestListStreamsKeepsRejectedBehindAccepted(t *testing.T) {
	cfg := virtuallibrary.Config{Quality: quality.QualityConfig{
		CustomFormats: []quality.CustomFormat{tokenFormat("No 1080p", "1080p", 0, true)},
	}}
	svc := newProviderService(t, nil, cfg,
		streamEntry("1080p", "http://192.168.1.10/1080.mkv"),
		streamEntry("720p", "http://192.168.1.10/720.mkv"),
	)

	streams, err := svc.ListStreams(context.Background(), "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("ListStreams: %v", err)
	}
	if len(streams) != 2 {
		t.Fatalf("streams = %d, want 2 (reject is rank-last, not a drop)", len(streams))
	}
	if streams[0].Rejected || !streams[1].Rejected {
		t.Fatalf("rejected ordering = accepted:%t rejected:%t, want an accepted candidate first", streams[0].Rejected, streams[1].Rejected)
	}
}

// TestListStreamsWarnsWhenAllRejected proves the last-resort path is loud:
// every candidate is returned and a Warn names the profile, count and the
// rejecting formats.
func TestListStreamsWarnsWhenAllRejected(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	cfg := virtuallibrary.Config{Quality: quality.QualityConfig{
		CustomFormats: []quality.CustomFormat{tokenFormat("No 1080p", "1080p", 0, true)},
	}}
	svc := newProviderService(t, logger, cfg,
		streamEntry("1080p", "http://192.168.1.10/a.mkv"),
		streamEntry("1080p Remux", "http://192.168.1.10/b.mkv"),
	)

	streams, err := svc.ListStreams(context.Background(), "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("ListStreams: %v", err)
	}
	if len(streams) != 2 {
		t.Fatalf("streams = %d, want 2", len(streams))
	}
	for _, s := range streams {
		if !s.Rejected {
			t.Fatalf("candidate %q not marked rejected", s.Label)
		}
	}
	if !strings.Contains(logs.String(), "every virtual candidate is rejected") {
		t.Fatalf("all-rejected warning not logged: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "No 1080p") {
		t.Fatalf("warning did not name the rejecting format: %s", logs.String())
	}
}

// TestResolveDetailedPinOverridesReject proves an explicit ?result= pin wins
// over both the ranking and the custom-format reject verdict.
func TestResolveDetailedPinOverridesReject(t *testing.T) {
	cfg := virtuallibrary.Config{Quality: quality.QualityConfig{
		CustomFormats: []quality.CustomFormat{tokenFormat("No 1080p", "1080p", 0, true)},
	}}
	svc := newProviderService(t, nil, cfg,
		streamEntry("1080p", "http://192.168.1.10/1080.mkv"),
		streamEntry("720p", "http://192.168.1.10/720.mkv"),
	)

	streams, err := svc.ListStreams(context.Background(), "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("ListStreams: %v", err)
	}
	var pinnedID string
	for _, s := range streams {
		if s.Resolution == "1080p" {
			pinnedID = s.ID
		}
	}
	if pinnedID == "" {
		t.Fatal("no 1080p candidate to pin")
	}

	resolved, err := svc.ResolveDetailed(context.Background(), "virtual://movie/tt100?result="+pinnedID, false, nil, "")
	if err != nil {
		t.Fatalf("ResolveDetailed: %v", err)
	}
	if resolved.CandidateID != pinnedID {
		t.Fatalf("resolved candidate = %q, want the rejected pin %q", resolved.CandidateID, pinnedID)
	}
}

// TestResolveDetailedPinnedProfileRemovedRefusesWithoutSubstitution proves a
// pin the profile removes is treated like an excluded pin: refused without a
// rotation request, substitutable with one.
func TestResolveDetailedPinnedProfileRemovedRefusesWithoutSubstitution(t *testing.T) {
	cfg := virtuallibrary.Config{Quality: quality.QualityConfig{
		EnableProfiles: true,
		Profiles:       []quality.QualityProfile{{Label: "fhd", Resolution: "1080p"}},
	}}
	svc := newProviderService(t, nil, cfg,
		streamEntry("1080p", "http://192.168.1.10/1080.mkv"),
		streamEntry("720p", "http://192.168.1.10/720.mkv"),
	)

	streams, err := svc.ListStreams(context.Background(), "virtual://movie/tt100?profile=fhd")
	if err != nil {
		t.Fatalf("ListStreams: %v", err)
	}
	var pinned720 string
	for _, s := range streams {
		if s.Resolution == "720p" {
			pinned720 = s.ID
		}
	}
	if pinned720 == "" {
		t.Fatal("no 720p candidate to pin")
	}
	pinURI := "virtual://movie/tt100?profile=fhd&result=" + pinned720

	if _, err := svc.ResolveDetailed(context.Background(), pinURI, false, nil, "", false); err == nil {
		t.Fatal("expected refusal for a profile-removed pin without substitution")
	}
	resolved, err := svc.ResolveDetailed(context.Background(), pinURI, false, nil, "", true)
	if err != nil {
		t.Fatalf("rotation resolve: %v", err)
	}
	if resolved.CandidateID == pinned720 {
		t.Fatal("substitution returned the profile-removed pin")
	}
}

// TestServeRerunsClassificationAndNeverResurrectsFailed proves the classifier
// is applied on a cache hit (no TTL wait), a SourceFailed candidate is dropped
// from the version list, and rotation cannot bring it back.
func TestServeRerunsClassificationAndNeverResurrectsFailed(t *testing.T) {
	svc := newProviderService(t, nil, virtuallibrary.Config{},
		streamEntry("1080p", "http://192.168.1.10/a.mkv"),
		streamEntry("720p", "http://192.168.1.10/b.mkv"),
	)
	ctx := context.Background()

	streams, err := svc.ListStreams(ctx, "virtual://movie/tt100")
	if err != nil || len(streams) != 2 {
		t.Fatalf("initial ListStreams: count=%d err=%v", len(streams), err)
	}
	var failedID, liveID string
	for _, s := range streams {
		if s.Resolution == "1080p" {
			failedID = s.ID
		} else {
			liveID = s.ID
		}
	}

	svc.Resolver.SetCandidateClassifier(classifyByURL{
		"http://192.168.1.10/a.mkv": {failed: true},
	})

	// The cache entry is still fresh; the serve-time classifier must drop the
	// failed release without waiting for the TTL.
	streams, err = svc.ListStreams(ctx, "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("reclassified ListStreams: %v", err)
	}
	if len(streams) != 1 || streams[0].ID != liveID {
		t.Fatalf("failed candidate survived reclassification: %+v", streams)
	}

	// Rotation that excludes the (now absent) failed candidate must not
	// resurrect it.
	resolved, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+failedID, false, []string{failedID}, "", true)
	if err != nil {
		t.Fatalf("rotation resolve: %v", err)
	}
	if resolved.CandidateID == failedID {
		t.Fatal("rotation resurrected a SourceFailed candidate")
	}
}

// TestIngestionDedupesPerFileVariantsBeforeCap proves one release offered as
// many per-file variants cannot flood the version list: the shared content
// hash collapses them to a single candidate.
func TestIngestionDedupesPerFileVariantsBeforeCap(t *testing.T) {
	entries := make([]map[string]any, 0, 60)
	for i := 0; i < 60; i++ {
		entries = append(entries, map[string]any{
			"name":          fmt.Sprintf("Movie.2024.1080p.WEB-DL-%d", i),
			"title":         "Movie.2024.1080p.WEB-DL",
			"url":           fmt.Sprintf("http://192.168.1.10/file-%d.mkv", i),
			"behaviorHints": map[string]any{"videoHash": "same-content-hash"},
		})
	}
	svc := newProviderService(t, nil, virtuallibrary.Config{}, entries...)

	streams, err := svc.ListStreams(context.Background(), "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("ListStreams: %v", err)
	}
	if len(streams) != 1 {
		t.Fatalf("dedup left %d candidates, want 1", len(streams))
	}
}

// TestResolveDetailedPinCollapsedByDedupResolvesToKeeper proves a pin that one
// multi-file release's dedup collapsed resolves to the surviving keeper of that
// release: a file swap inside a release, never a different release and never a
// hard failure.
func TestResolveDetailedPinCollapsedByDedupResolvesToKeeper(t *testing.T) {
	entries := []map[string]any{
		{
			"name": "Movie.2024.1080p.WEB-DL-0", "title": "Movie.2024.1080p.WEB-DL",
			"url": "http://192.168.1.10/file-0.mkv", "behaviorHints": map[string]any{"videoHash": "same-release-hash"},
		},
		{
			"name": "Movie.2024.1080p.WEB-DL-1", "title": "Movie.2024.1080p.WEB-DL",
			"url": "http://192.168.1.10/file-1.mkv", "behaviorHints": map[string]any{"videoHash": "same-release-hash"},
		},
	}
	svc := newProviderService(t, nil, virtuallibrary.Config{}, entries...)
	ctx := context.Background()

	streams, err := svc.ListStreams(ctx, "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("ListStreams: %v", err)
	}
	if len(streams) != 1 {
		t.Fatalf("dedup left %d candidates, want 1", len(streams))
	}
	keeperID := streams[0].ID

	// Recompute the dropped variant's stable id the same way the resolver does.
	dropped := stream.StreamCandidate{
		Name: "Movie.2024.1080p.WEB-DL-1", Title: "Movie.2024.1080p.WEB-DL",
		URL: "http://192.168.1.10/file-1.mkv",
	}
	dropped.BehaviorHints.VideoHash = "same-release-hash"
	stream.ParseStreamDetails(&dropped)
	droppedID := stream.CandidateVariantID(dropped)
	if droppedID == "" || droppedID == keeperID {
		t.Fatalf("dropped variant id = %q, keeper = %q", droppedID, keeperID)
	}

	resolved, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+droppedID, false, nil, "")
	if err != nil {
		t.Fatalf("ResolveDetailed(collapsed pin): %v", err)
	}
	if resolved.CandidateID != keeperID {
		t.Fatalf("collapsed pin resolved to %q, want its release keeper %q", resolved.CandidateID, keeperID)
	}
}

// TestIngestionMovesConfirmedFirst proves the classifier's confirmation is
// authoritative for order at ingestion.
func TestIngestionMovesConfirmedFirst(t *testing.T) {
	svc := newProviderService(t, nil, virtuallibrary.Config{},
		streamEntry("1080p", "http://192.168.1.10/a.mkv"),
		streamEntry("720p", "http://192.168.1.10/b.mkv"),
	)
	svc.Resolver.SetCandidateClassifier(classifyByURL{
		"http://192.168.1.10/b.mkv": {confirmed: true},
	})

	streams, err := svc.ListStreams(context.Background(), "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("ListStreams: %v", err)
	}
	if len(streams) != 2 || streams[0].Resolution != "720p" {
		t.Fatalf("confirmed candidate not first: %+v", streams)
	}
}

// TestReleaseGateBlocksFutureAndFailsOpen proves the wired gate blocks only a
// concrete future air date.
func TestReleaseGateBlocksFutureAndFailsOpen(t *testing.T) {
	ctx := context.Background()

	future := time.Now().Add(72 * time.Hour)
	blocked := newProviderService(t, nil, virtuallibrary.Config{}, streamEntry("1080p", "http://192.168.1.10/a.mkv"))
	blocked.Resolver.SetReleaseGate(fakeGate{released: false, air: &future})
	if _, err := blocked.ListStreams(ctx, "virtual://movie/tt100"); err == nil {
		t.Fatal("future release was not blocked")
	}

	past := time.Now().Add(-72 * time.Hour)
	allowed := newProviderService(t, nil, virtuallibrary.Config{}, streamEntry("1080p", "http://192.168.1.10/a.mkv"))
	allowed.Resolver.SetReleaseGate(fakeGate{released: true, air: &past})
	if _, err := allowed.ListStreams(ctx, "virtual://movie/tt100"); err != nil {
		t.Fatalf("past release was blocked: %v", err)
	}

	unknown := newProviderService(t, nil, virtuallibrary.Config{}, streamEntry("1080p", "http://192.168.1.10/a.mkv"))
	unknown.Resolver.SetReleaseGate(fakeGate{released: true})
	if _, err := unknown.ListStreams(ctx, "virtual://movie/tt100"); err != nil {
		t.Fatalf("unknown release was blocked: %v", err)
	}
}
