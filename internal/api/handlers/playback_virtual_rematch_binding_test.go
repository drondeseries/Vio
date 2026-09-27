package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// TestRehydratedIdentityRematchRequiresThisRowsPersistedRelease pins what
// IdentityRematched is allowed to mean for the replan rehydration: a match
// against the identity this request persisted on the session anchor row, not
// just a successful lookup of some sibling. The resolver reports a rematch for
// a candidate whose durable identity equals the anchor's, so the rotation is
// accepted; the accepted candidate's identity is then confirmed against the
// anchor row's own persisted release.
func TestRehydratedIdentityRematchRequiresThisRowsPersistedRelease(t *testing.T) {
	const (
		neutralURI = "virtual://movie/tt-replan-persisted-release"
		pinnedURI  = neutralURI + "?result=pinned"
		renumbered = neutralURI + "?result=renumbered"
	)
	// The session anchor row persisted hash-a for Movie.2024; that is the
	// release this request is bound to.
	file := &models.MediaFile{
		ID: 31, ContentID: "movie-replan-persisted-release", FilePath: pinnedURI,
		VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
	}
	if _, ok := persistedVirtualIdentity(file); !ok {
		t.Fatal("fixture precondition: the anchor row must carry a durable identity")
	}

	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
		return "http://127.0.0.1:9/unused", nil
	})
	var rematchReported bool
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		if !VirtualCandidateRotationAllowed(ctx) {
			return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
		}
		rematchReported = true
		// The resolver proved the renumbered id carries the anchor's own
		// identity tiers, so this is the same release re-identified.
		return ResolvedVirtualMedia{
			URL: "http://127.0.0.1:9/renumbered", URI: renumbered, CandidateID: "renumbered",
			IdentityRematched: true, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
		}, nil
	})

	r := httptest.NewRequest(http.MethodPost, "/api/v1/playback/replan", nil).WithContext(newAuthorizedPlaybackContext())
	resolved, err := h.resolveRehydratedVirtualSourceV3(r, file, "profile-1", nil, "pinned", "auto", 0, virtualResolveOptionsV3{sessionBound: true, sessionAnchorURI: pinnedURI})
	if err != nil {
		t.Fatalf("resolveRehydratedVirtualSourceV3: %v", err)
	}
	if got := virtualResultCandidateID(resolved.URI); got != "renumbered" {
		t.Fatalf("resolved candidate = %q, want the rematched same-release candidate", got)
	}
	if !rematchReported {
		t.Fatal("the rotation was accepted without the resolver reporting a same-release rematch")
	}
	// The rematch is only acceptable because the candidate's durable identity
	// tier equals the anchor row's own persisted release.
	if !rehydratedMatchesPersistedIdentity(resolved, file) {
		t.Fatalf("resolved identity %q does not match the anchor row's persisted release",
			resolved.ProviderVideoHash+"/"+resolved.ProviderReleaseName)
	}
}

// TestResolvedMatchesPersistedIdentityComparesThisRowsRelease pins the explicit
// comparison branch that the IdentityRematched bypass falls back to: a resolve
// with no rematch flag is accepted only when its durable identity equals the
// identity persisted on this request's row. A missing identity on either side is
// not proof, so it must not count as a match.
func TestResolvedMatchesPersistedIdentityComparesThisRowsRelease(t *testing.T) {
	row := &models.MediaFile{ID: 41, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024"}
	for _, tc := range []struct {
		name     string
		resolved ResolvedVirtualMedia
		want     bool
	}{
		{
			name:     "same durable hash is this row's release",
			resolved: ResolvedVirtualMedia{ProviderVideoHash: "hash-a"},
			want:     true,
		},
		{
			name:     "same release name is this row's release",
			resolved: ResolvedVirtualMedia{ProviderReleaseName: "Movie.2024"},
			want:     false, // the row's hash outranks the name tier, so the precedence keys differ
		},
		{
			name:     "a different hash is a sibling release",
			resolved: ResolvedVirtualMedia{ProviderVideoHash: "hash-b"},
			want:     false,
		},
		{
			name:     "a different release name is a sibling release",
			resolved: ResolvedVirtualMedia{ProviderReleaseName: "Movie.2024.OTHER"},
			want:     false,
		},
		{
			name:     "a resolve with no identity tiers proves nothing",
			resolved: ResolvedVirtualMedia{},
			want:     false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvedMatchesPersistedIdentity(tc.resolved, row); got != tc.want {
				t.Fatalf("resolvedMatchesPersistedIdentity = %v, want %v", got, tc.want)
			}
		})
	}

	// A legacy row with no durable identity can never confirm a release, so the
	// comparison refuses rather than inventing a match.
	legacy := &models.MediaFile{ID: 42}
	if resolvedMatchesPersistedIdentity(ResolvedVirtualMedia{ProviderVideoHash: "hash-a"}, legacy) {
		t.Fatal("a legacy row with no persisted identity must not confirm a release")
	}
}

// TestRehydratedRotationDifferentReleaseRefusedByExplicitComparison proves the
// explicit durable-identity comparison is the branch that refuses a sibling when
// the resolver reports no rematch: the rotation resolved a different release, so
// the rehydration returns the original absent-pin cause instead of anchoring the
// plan on sibling bytes.
func TestRehydratedRotationDifferentReleaseRefusedByExplicitComparison(t *testing.T) {
	const (
		neutralURI = "virtual://movie/tt-replan-rematch-mismatch"
		pinnedURI  = neutralURI + "?result=pinned"
		siblingURI = neutralURI + "?result=sibling"
	)
	file := &models.MediaFile{
		ID: 32, ContentID: "movie-replan-rematch-mismatch", FilePath: pinnedURI,
		VirtualOwnerInstallationID: 5, ProviderVideoHash: "hash-a", ProviderReleaseName: "Movie.2024",
	}
	h := NewPlaybackHandler(playback.NewSessionManager(0, 0))
	h.VirtualPlaybackResolver = VirtualPlaybackResolverFunc(func(context.Context, string, int, string, int) (string, error) {
		return "http://127.0.0.1:9/unused", nil
	})
	h.VirtualMediaDetailedResolver = VirtualMediaDetailedResolverFunc(func(ctx context.Context, _ string, _ int, _ int, _ string, _ bool, _ []string, _ string) (ResolvedVirtualMedia, error) {
		if !VirtualCandidateRotationAllowed(ctx) {
			return ResolvedVirtualMedia{}, absentSessionPinError("pinned")
		}
		// A genuinely different release: no rematch, and a hash that does not
		// equal this row's persisted identity. The explicit comparison branch
		// must refuse it.
		return ResolvedVirtualMedia{
			URL: "http://127.0.0.1:9/sibling", URI: siblingURI, CandidateID: "sibling",
			ProviderVideoHash: "hash-b",
		}, nil
	})

	r := httptest.NewRequest(http.MethodPost, "/api/v1/playback/replan", nil).WithContext(newAuthorizedPlaybackContext())
	resolved, err := h.resolveRehydratedVirtualSourceV3(r, file, "profile-1", nil, "pinned", "auto", 0, virtualResolveOptionsV3{sessionBound: true, sessionAnchorURI: pinnedURI})
	if !errors.Is(err, virtuallibrary.ErrSessionBoundCandidateAbsent) {
		t.Fatalf("err = %v, want the original absent-pin refusal for a different release", err)
	}
	if got := virtualResultCandidateID(resolved.URI); got != "" {
		t.Fatalf("resolved candidate = %q, want no accepted rotation", got)
	}
}
