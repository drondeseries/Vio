package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
)

// TestSameVirtualReleaseIdentityReconcilesProfile pins item 2: a profile variant
// and the neutral provider candidate naming the same provider result id are one
// release, while a different result id or a different title is not.
func TestSameVirtualReleaseIdentityReconcilesProfile(t *testing.T) {
	const (
		base      = "virtual://movie/tt-profile"
		profileA  = base + "?profile=4K+HDR&result=X"
		neutral   = base + "?result=X"
		otherID   = base + "?result=Y"
		otherPath = "virtual://movie/tt-other?result=X"
	)
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"profile variant and neutral same id", profileA, neutral, true},
		{"neutral and profile variant same id", neutral, profileA, true},
		{"same profile same id", profileA, profileA, true},
		{"profile variant different id", profileA, otherID, false},
		{"same id different title", profileA, otherPath, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameVirtualReleaseIdentity(tc.a, tc.b); got != tc.want {
				t.Fatalf("sameVirtualReleaseIdentity(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestVirtualCandidateRowVerifiedReconcilesProfile pins the adoption/evidence
// half of item 2: a row owns its release regardless of a profile-only suffix,
// including a profile-neutral row whose candidate carries the profile.
func TestVirtualCandidateRowVerifiedReconcilesProfile(t *testing.T) {
	const (
		base        = "virtual://movie/tt-verified"
		profilePin  = base + "?profile=4K+HDR&result=X"
		neutralCand = base + "?result=X"
		otherCand   = base + "?result=Y"
		profileCand = base + "?profile=4K+HDR&result=X"
		neutralRow  = base + "?profile=4K+HDR"
	)
	profileRow := &models.MediaFile{ID: 1, FilePath: profilePin}
	cases := []struct {
		name string
		row  *models.MediaFile
		cand string
		want bool
	}{
		{"profile row owns neutral same id", profileRow, neutralCand, true},
		{"profile row owns same profile same id", profileRow, profileCand, true},
		{"profile row does not own different id", profileRow, otherCand, false},
		{"profile-neutral row owns same-profile neutral candidate", &models.MediaFile{ID: 2, FilePath: neutralRow}, neutralRow, true},
		{"profile-neutral row does not own a different-profile candidate", &models.MediaFile{ID: 3, FilePath: neutralRow}, base + "?profile=1080p&result=X", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := virtualCandidateRowVerified(tc.row, tc.cand); got != tc.want {
				t.Fatalf("virtualCandidateRowVerified(%q, %q) = %v, want %v", tc.row.FilePath, tc.cand, got, tc.want)
			}
		})
	}
}

// TestResolveVirtualRepeatPlayCollectionRequiresDelivery pins item 3: a
// collection-owned row that has complete evidence and a probe stamp but has
// never delivered must not take the P0 fast path (which would bind a dead pin
// without re-listing); once it has delivered, the replay fast path applies.
func TestResolveVirtualRepeatPlayCollectionRequiresDelivery(t *testing.T) {
	t.Run("never delivered falls through to the resolver", func(t *testing.T) {
		detailedCalls, legacyCalls := 0, 0
		h := virtualRepeatPlayHandler(&detailedCalls, &legacyCalls)
		file := virtualRepeatPlayFile("virtual://movie/tt-collection?result=cand-1")
		file.ProbeSource = "virtual_collection"

		req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
		if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false); err != nil {
			t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
		}
		if detailedCalls == 0 {
			t.Fatal("a never-delivered collection row took the P0 fast path; want a provider resolve")
		}
	})

	t.Run("delivered keeps the replay fast path", func(t *testing.T) {
		detailedCalls, legacyCalls := 0, 0
		h := virtualRepeatPlayHandler(&detailedCalls, &legacyCalls)
		file := virtualRepeatPlayFile("virtual://movie/tt-collection?result=cand-1")
		file.ProbeSource = "virtual_collection"
		delivered := time.Now().Add(-time.Hour)
		file.LastDeliveredAt = &delivered

		req := httptest.NewRequest(http.MethodPost, "/api/v1/playback/start", nil)
		if _, err := h.resolveVirtualPlaybackSource(req, file, "profile-1", true, nil, "", "", 0, false); err != nil {
			t.Fatalf("resolveVirtualPlaybackSource error: %v", err)
		}
		if detailedCalls != 0 || legacyCalls != 0 {
			t.Fatalf("resolvers called detailed=%d legacy=%d, want 0/0 for a delivered collection replay", detailedCalls, legacyCalls)
		}
	})
}

// collectionReconcileHandler wires a lister, a verified prober and a capturing
// saver so a fallback reconcile can be observed end to end.
func collectionReconcileHandler(t *testing.T, streams []VirtualPlaybackStream, absent map[string]bool, saved *[]models.VirtualFilePersistArgs) *PlaybackHandler {
	t.Helper()
	h := &PlaybackHandler{
		PlaybackConfig: func() config.PlaybackConfig {
			return config.PlaybackConfig{MaxVirtualFailoverAttempts: 3}
		},
		VirtualPlaybackStreamLister: VirtualPlaybackStreamListerFunc(func(_ context.Context, _ string, _ int, _ string, _ int) ([]VirtualPlaybackStream, error) {
			return streams, nil
		}),
		VirtualMediaDetailedResolver: VirtualMediaDetailedResolverFunc(func(_ context.Context, uri string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
			id := virtualResultCandidateID(uri)
			if absent[id] {
				return ResolvedVirtualMedia{}, errors.New("virtual stream provider returned no matching candidate")
			}
			return ResolvedVirtualMedia{
				URL: "http://localhost:8080/" + id + ".mp4",
				URI: uri, CandidateID: id,
				ProviderVideoHash: "hash-new", ProviderReleaseName: "Movie.New.2160p",
			}, nil
		}),
		VirtualPlaybackSourceProber: func(_ context.Context, _ string, base *models.MediaFile) (*models.MediaFile, error) {
			probed := *base
			probed.VideoTracks = []models.VideoTrack{{Codec: "hevc", Width: 3840, Height: 2160, BitDepth: 10}}
			probed.AudioTracks = []models.AudioTrack{{Codec: "eac3", Channels: 6, Language: "eng"}}
			probed.Resolution = "2160p"
			probed.CodecVideo = "hevc"
			probed.CodecAudio = "eac3"
			probed.Container = "mkv"
			return &probed, nil
		},
	}
	h.VirtualFileSaver = func(_ context.Context, args models.VirtualFilePersistArgs) (int64, error) {
		*saved = append(*saved, args)
		return 1, nil
	}
	return h
}

func collectionVariantFile(pinURI string) *models.MediaFile {
	return &models.MediaFile{
		ID: 26340, ContentID: "movie-tmdb-302699", FilePath: pinURI,
		ProbeSource: "virtual_collection", VirtualOwnerInstallationID: 5,
		ProviderVideoHash: "hash-old", ProviderReleaseName: "Movie.Old.2160p",
	}
}

// TestFallbackReconcilesVanishedCollectionVariant pins item 1: a collection
// variant row whose pinned release is genuinely gone (result id and durable
// identity both absent from the fresh listing) reconciles to the profile's live
// candidate through the recorded-verdict path, instead of refusing forever.
func TestFallbackReconcilesVanishedCollectionVariant(t *testing.T) {
	const (
		base    = "virtual://movie/tt-reconcile?profile=4K+HDR"
		pinURI  = base + "&result=old"
		freshID = base + "&result=new"
	)
	fresh := VirtualPlaybackStream{
		ID: "new", URI: freshID, Resolution: "2160p",
		ProviderVideoHash: "hash-new", ProviderReleaseName: "Movie.New.2160p",
	}
	var saved []models.VirtualFilePersistArgs
	file := collectionVariantFile(pinURI)
	h := collectionReconcileHandler(t, []VirtualPlaybackStream{fresh}, map[string]bool{"old": true}, &saved)

	result := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1", virtualFallbackEligibility{sessionBound: true, rotationAllowed: false})
	if result == nil {
		t.Fatal("fallback returned nil, want the reconciled live candidate")
	}
	if got := virtualResultCandidateID(result.URI); got != "new" {
		t.Fatalf("reconciled candidate = %q, want the fresh candidate", got)
	}
	if len(saved) != 1 {
		t.Fatalf("persist calls = %d, want 1 reconcile adoption", len(saved))
	}
	if !saved[0].ReconcileCollectionVariant {
		t.Fatal("reconcile persistence was not marked as a collection-variant reconcile")
	}
	if !saved[0].RequireAdopt || saved[0].AdoptPath != result.URI {
		t.Fatalf("reconcile persist args = %#v, want a required adoption of %q", saved[0], result.URI)
	}
}

// TestFallbackDoesNotReconcileStillListedRelease pins the no-silent-swap safety:
// when the pinned result id is still listed, the collection variant is not
// reconciled even though a different release is offered.
func TestFallbackDoesNotReconcileStillListedRelease(t *testing.T) {
	const (
		base   = "virtual://movie/tt-still-listed?profile=4K+HDR"
		pinURI = base + "&result=old"
		newURI = base + "&result=new"
	)
	streams := []VirtualPlaybackStream{
		{ID: "old", URI: pinURI, Resolution: "2160p"},
		{ID: "new", URI: newURI, Resolution: "2160p", ProviderVideoHash: "hash-new"},
	}
	var saved []models.VirtualFilePersistArgs
	file := collectionVariantFile(pinURI)
	h := collectionReconcileHandler(t, streams, nil, &saved)

	result := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1", virtualFallbackEligibility{sessionBound: true, rotationAllowed: false})
	if result == nil {
		t.Fatal("fallback returned nil, want the still-listed pinned release reused")
	}
	if got := virtualResultCandidateID(result.URI); got != "old" {
		t.Fatalf("fallback exchanged a still-listed release for %q", got)
	}
	for _, args := range saved {
		if args.ReconcileCollectionVariant {
			t.Fatalf("a still-listed release was reconciled: %#v", args)
		}
	}
}

// TestFallbackDoesNotReconcileRenumberedSameIdentity pins item 2 at the fallback
// boundary: a fresh result id that carries the row's durable identity is the
// same release, not a vanished one, so it is never treated as a cross-release
// reconcile.
func TestFallbackDoesNotReconcileRenumberedSameIdentity(t *testing.T) {
	const (
		base   = "virtual://movie/tt-renumbered?profile=4K+HDR"
		pinURI = base + "&result=old"
		newURI = base + "&result=new"
	)
	streams := []VirtualPlaybackStream{{
		ID: "new", URI: newURI, Resolution: "2160p",
		ProviderVideoHash: "hash-old", ProviderReleaseName: "Movie.Old.2160p",
	}}
	var saved []models.VirtualFilePersistArgs
	file := collectionVariantFile(pinURI)
	h := collectionReconcileHandler(t, streams, nil, &saved)

	if got := collectionVariantPinVanished(file, streams, "old"); got {
		t.Fatal("a renumbered same-identity listing was treated as a vanished release")
	}
	// Through the fallback the pin is also not reconciled: the same release is
	// still offered under a new id, so no cross-release verdict is recorded.
	result := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1", virtualFallbackEligibility{sessionBound: true, rotationAllowed: false})
	for _, args := range saved {
		if args.ReconcileCollectionVariant {
			t.Fatalf("a renumbered same-identity listing was reconciled: %#v", args)
		}
	}
	_ = result
}

// TestFallbackReconcileRefusedWithoutIdentity pins the identity requirement: a
// collection variant row with no durable identity can never prove the pin
// vanished, so it is not reconciled (a renumbered listing is indistinguishable
// from a gone release).
func TestFallbackReconcileRefusedWithoutIdentity(t *testing.T) {
	const (
		base   = "virtual://movie/tt-no-identity?profile=4K+HDR"
		pinURI = base + "&result=old"
		newURI = base + "&result=new"
	)
	streams := []VirtualPlaybackStream{{
		ID: "new", URI: newURI, Resolution: "2160p",
		ProviderVideoHash: "hash-new", ProviderReleaseName: "Movie.New.2160p",
	}}
	var saved []models.VirtualFilePersistArgs
	file := collectionVariantFile(pinURI)
	file.ProviderVideoHash = ""
	file.ProviderReleaseName = ""
	file.ProviderGUID = ""
	file.ProviderReleaseSize = 0
	h := collectionReconcileHandler(t, streams, map[string]bool{"old": true}, &saved)

	if got := collectionVariantPinVanished(file, streams, "old"); got {
		t.Fatal("an identity-less row must never be treated as a vanished release")
	}
	result := h.fallbackResolveStaleVirtualSource(context.Background(), file, 1, "profile-1", virtualFallbackEligibility{sessionBound: true, rotationAllowed: false})
	if result != nil {
		t.Fatalf("fallback returned %#v, want a refusal for an identity-less collection row", result)
	}
	for _, args := range saved {
		if args.ReconcileCollectionVariant {
			t.Fatalf("identity-less row was reconciled: %#v", args)
		}
	}
}
