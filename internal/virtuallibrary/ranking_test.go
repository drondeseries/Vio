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

// collapsedPinFixture builds a listing with one multi-file release X (variants
// file-0 keeper and file-1 dropped) plus a distinct, higher-ranked release C.
// It returns the service, the dropped variant's id (the pin), the keeper's id,
// and C's id. The higher-ranked C is what makes the collapse test discriminating:
// an unfixed resolver returns C, a fixed one returns the keeper.
func collapsedPinFixture(t *testing.T) (svc *virtuallibrary.Service, droppedID, keeperID, otherID string) {
	t.Helper()
	entries := []map[string]any{
		{
			"name": "Movie.2024.1080p.WEB-DL-0", "title": "Movie.2024.1080p.WEB-DL",
			"url": "http://192.168.1.10/file-0.mkv", "behaviorHints": map[string]any{"videoHash": "release-x"},
		},
		{
			"name": "Movie.2024.1080p.WEB-DL-1", "title": "Movie.2024.1080p.WEB-DL",
			"url": "http://192.168.1.10/file-1.mkv", "behaviorHints": map[string]any{"videoHash": "release-x"},
		},
		{
			"name": "Other.2024.2160p.WEB-DL", "title": "Other.2024.2160p.WEB-DL",
			"url": "http://192.168.1.10/other.mkv",
		},
	}
	svc = newProviderService(t, nil, virtuallibrary.Config{}, entries...)
	streams, err := svc.ListStreams(context.Background(), "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("ListStreams: %v", err)
	}
	if len(streams) != 2 {
		t.Fatalf("ranked candidates = %d, want 2 (release X keeper + release C)", len(streams))
	}
	for _, s := range streams {
		switch s.Resolution {
		case "1080p":
			keeperID = s.ID
		case "2160p":
			otherID = s.ID
		}
	}
	if keeperID == "" || otherID == "" {
		t.Fatalf("failed to identify candidates: keeper=%q other=%q", keeperID, otherID)
	}
	if streams[0].ID != otherID {
		t.Fatalf("higher-ranked release is not first: %+v", streams)
	}

	// Recompute the dropped variant's stable id the same way the resolver does.
	dropped := stream.StreamCandidate{
		Name: "Movie.2024.1080p.WEB-DL-1", Title: "Movie.2024.1080p.WEB-DL",
		URL: "http://192.168.1.10/file-1.mkv",
	}
	dropped.BehaviorHints.VideoHash = "release-x"
	stream.ParseStreamDetails(&dropped)
	droppedID = stream.CandidateVariantID(dropped)
	if droppedID == "" || droppedID == keeperID {
		t.Fatalf("dropped variant id = %q, keeper = %q", droppedID, keeperID)
	}
	return svc, droppedID, keeperID, otherID
}

// TestResolveDetailedPinCollapsedByDedupResolvesToKeeper proves a pin whose
// variant dedup collapsed resolves to the surviving keeper of its release even
// when a different, higher-ranked release is present: a file swap inside a
// release, never a release swap and never a hard failure.
func TestResolveDetailedPinCollapsedByDedupResolvesToKeeper(t *testing.T) {
	svc, droppedID, keeperID, otherID := collapsedPinFixture(t)

	resolved, err := svc.ResolveDetailed(context.Background(), "virtual://movie/tt100?result="+droppedID, false, nil, "")
	if err != nil {
		t.Fatalf("ResolveDetailed(collapsed pin): %v", err)
	}
	if resolved.CandidateID != keeperID {
		t.Fatalf("collapsed pin resolved to %q, want its release keeper %q (not the higher-ranked %q)", resolved.CandidateID, keeperID, otherID)
	}
}

// TestResolveDetailedCollapsedPinHonoursExclusion proves the translated pin is
// still governed by the caller's exclusion list: excluding the collapsed
// variant refuses without substitution, and an explicit rotation substitutes a
// different release rather than the excluded release's keeper.
func TestResolveDetailedCollapsedPinHonoursExclusion(t *testing.T) {
	svc, droppedID, keeperID, otherID := collapsedPinFixture(t)
	ctx := context.Background()

	if _, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+droppedID, false, []string{droppedID}, "", false); err == nil {
		t.Fatal("expected refusal when the collapsed pin's release is excluded without rotation")
	}
	resolved, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+droppedID, false, []string{droppedID}, "", true)
	if err != nil {
		t.Fatalf("rotation resolve: %v", err)
	}
	if resolved.CandidateID == keeperID {
		t.Fatal("rotation returned the excluded release's keeper")
	}
	if resolved.CandidateID != otherID {
		t.Fatalf("rotation resolved to %q, want the alternative release %q", resolved.CandidateID, otherID)
	}
}

// TestResolveDetailedDeadPinStillFallsBack proves a genuinely dead pin (absent
// with no keeper) still resolves to an alternative under substitution, so a
// genuinely unavailable provider recovers.
func TestResolveDetailedDeadPinStillFallsBack(t *testing.T) {
	svc, _, _, otherID := collapsedPinFixture(t)
	resolved, err := svc.ResolveDetailed(context.Background(), "virtual://movie/tt100?result=ffffffffffffffffffffffff", false, nil, "", true)
	if err != nil {
		t.Fatalf("dead-pin resolve: %v", err)
	}
	if resolved.CandidateID != otherID {
		t.Fatalf("dead pin resolved to %q, want the top alternative %q", resolved.CandidateID, otherID)
	}
}

// TestResolveDetailedRefusesDifferentReleaseForSessionPin reproduces the
// handler's per-candidate shape: the session pinned variant A of release X
// (dedup collapsed A -> keeper B) and the request's resultID names a
// higher-ranked different release C while substitution is refused. The resolver
// must refuse C rather than serve it under the session binding; with
// substitution allowed the explicit resultID still wins; and probing the
// session's own keeper is always allowed. The ordinary non-pinned path is
// unaffected.
func TestResolveDetailedRefusesDifferentReleaseForSessionPin(t *testing.T) {
	svc, droppedID, keeperID, otherID := collapsedPinFixture(t)
	ctx := context.Background()

	// Substitution refused, probing the different release: refuse instead of
	// swapping the release under the session binding.
	if _, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+otherID, false, nil, droppedID, false); err == nil {
		t.Fatalf("expected refusal when probing %q for session pin %q with substitution refused", otherID, droppedID)
	}
	// Substitution allowed: the explicit resultID wins.
	resolved, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+otherID, false, nil, droppedID, true)
	if err != nil {
		t.Fatalf("substitution-allowed resolve: %v", err)
	}
	if resolved.CandidateID != otherID {
		t.Fatalf("substitution-allowed resolve = %q, want the explicit candidate %q", resolved.CandidateID, otherID)
	}
	// Probing the session's own resolved keeper is allowed even when
	// substitution is refused.
	resolved, err = svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+keeperID, false, nil, droppedID, false)
	if err != nil {
		t.Fatalf("session keeper resolve: %v", err)
	}
	if resolved.CandidateID != keeperID {
		t.Fatalf("session keeper resolve = %q, want %q", resolved.CandidateID, keeperID)
	}
	// The ordinary non-pinned path is unaffected.
	resolved, err = svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+otherID, false, nil, "", false)
	if err != nil {
		t.Fatalf("non-pinned resolve: %v", err)
	}
	if resolved.CandidateID != otherID {
		t.Fatalf("non-pinned resolve = %q, want %q", resolved.CandidateID, otherID)
	}
}

// TestKeeperMapDropsKeepersBeyondTheCandidateCap proves the dropped -> keeper
// map is filtered to the surviving candidates: when a release's keeper ranks
// beyond the selectable cap, its entry is removed so a pin on the collapsed
// variant is treated as a genuinely dead release and takes the existing
// dead-pin fallback, instead of being translated to a keeper that is not in the
// list.
func TestKeeperMapDropsKeepersBeyondTheCandidateCap(t *testing.T) {
	const others = 51 // more than the cap, so the low-ranked keeper is cut
	entries := make([]map[string]any, 0, others+2)
	for i := 0; i < others; i++ {
		entries = append(entries, map[string]any{
			"name":  fmt.Sprintf("Other.%02d.2160p.WEB-DL", i),
			"title": fmt.Sprintf("Other.%02d.2160p.WEB-DL", i),
			"url":   fmt.Sprintf("http://192.168.1.10/other-%d.mkv", i),
		})
	}
	// Release X has two per-file variants sharing a content hash and is 1080p,
	// so its keeper sorts below every 2160p release and falls beyond the cap.
	entries = append(entries,
		map[string]any{
			"name": "Pinned.2024.1080p.WEB-DL-0", "title": "Pinned.2024.1080p.WEB-DL",
			"url": "http://192.168.1.10/pinned-0.mkv", "behaviorHints": map[string]any{"videoHash": "release-pinned"},
		},
		map[string]any{
			"name": "Pinned.2024.1080p.WEB-DL-1", "title": "Pinned.2024.1080p.WEB-DL",
			"url": "http://192.168.1.10/pinned-1.mkv", "behaviorHints": map[string]any{"videoHash": "release-pinned"},
		},
	)
	svc := newProviderService(t, nil, virtuallibrary.Config{}, entries...)
	ctx := context.Background()

	candidates, keepers, _, _, err := svc.Resolver.GetCandidatesWithKeepers(ctx, "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("GetCandidatesWithKeepers: %v", err)
	}
	if len(candidates) != 50 {
		t.Fatalf("surviving candidates = %d, want the 50-candidate cap", len(candidates))
	}

	variantID := func(name, rawURL string) string {
		candidate := stream.StreamCandidate{
			Name: name, Title: "Pinned.2024.1080p.WEB-DL", URL: rawURL,
		}
		candidate.BehaviorHints.VideoHash = "release-pinned"
		stream.ParseStreamDetails(&candidate)
		return stream.CandidateVariantID(candidate)
	}
	keeperID := variantID("Pinned.2024.1080p.WEB-DL-0", "http://192.168.1.10/pinned-0.mkv")
	droppedID := variantID("Pinned.2024.1080p.WEB-DL-1", "http://192.168.1.10/pinned-1.mkv")
	if keeperID == "" || droppedID == "" || keeperID == droppedID {
		t.Fatalf("bad variant ids: keeper=%q dropped=%q", keeperID, droppedID)
	}

	for _, c := range candidates {
		if stream.CandidateVariantID(c) == keeperID {
			t.Fatal("test setup: the low-ranked keeper unexpectedly survived the cap")
		}
	}
	if _, ok := keepers[droppedID]; ok {
		t.Fatalf("keeper map retained an entry for %q whose keeper was truncated", droppedID)
	}

	// A pin on the collapsed variant therefore takes the dead-pin fallback and
	// resolves to a surviving candidate, never the truncated keeper.
	resolved, err := svc.ResolveDetailed(ctx, "virtual://movie/tt100?result="+droppedID, false, nil, "", true)
	if err != nil {
		t.Fatalf("dead-pin fallback resolve: %v", err)
	}
	if resolved.CandidateID == keeperID {
		t.Fatal("resolve returned the truncated keeper")
	}
	if resolved.CandidateID == "" {
		t.Fatal("dead-pin fallback returned no candidate")
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

// TestAllFailedListingWarnsAndRecoversWithoutNegativeCache proves a non-empty
// provider answer the classifier drops entirely is not a two-minute negative
// cache entry: it warns with the count and recovers as soon as the classifier
// state changes, without waiting for a TTL.
func TestAllFailedListingWarnsAndRecoversWithoutNegativeCache(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := newProviderService(t, logger, virtuallibrary.Config{},
		streamEntry("1080p", "http://192.168.1.10/a.mkv"),
		streamEntry("720p", "http://192.168.1.10/b.mkv"),
	)
	ctx := context.Background()

	svc.Resolver.SetCandidateClassifier(classifyByURL{
		"http://192.168.1.10/a.mkv": {failed: true},
		"http://192.168.1.10/b.mkv": {failed: true},
	})
	streams, err := svc.ListStreams(ctx, "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("all-failed ListStreams: %v", err)
	}
	if len(streams) != 0 {
		t.Fatalf("all-failed listing returned %d candidates, want 0", len(streams))
	}
	if !strings.Contains(logs.String(), "every provider candidate was dropped as failed") {
		t.Fatalf("all-failed warning not logged: %s", logs.String())
	}

	// The same cached answer must recover immediately when the classifier
	// changes: a non-empty all-failed answer is cached as data, not as a
	// negative entry.
	svc.Resolver.SetCandidateClassifier(classifyByURL{})
	streams, err = svc.ListStreams(ctx, "virtual://movie/tt100")
	if err != nil {
		t.Fatalf("recovered ListStreams: %v", err)
	}
	if len(streams) != 2 {
		t.Fatalf("recovered listing = %d candidates, want 2 (all-failed must not negative-cache)", len(streams))
	}
}
