package api

import (
	"context"

	"github.com/Silo-Server/silo-server/internal/api/handlers"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// virtualDetailedResolverSource is the subset of *virtuallibrary.Service the
// detailed-resolver adapter uses. It is an interface so the rotation- and
// session-binding-intent forwarding is testable without standing up a live
// service.
type virtualDetailedResolverSource interface {
	ResolveDetailed(ctx context.Context, virtualPath string, forceRefresh bool, excludedCandidateIDs []string, preferredCandidateID string, sessionBound bool, allowCandidateSubstitution ...bool) (virtuallibrary.ResolvedVirtualStream, error)
}

// newVirtualMediaDetailedResolver adapts a virtual library's detailed resolver
// to the playback handler port. It forwards the intents carried on ctx: the
// rotation intent so the service can decide whether to substitute a sibling for
// an excluded pinned candidate (display-driven fallbacks must not swap the
// release; a serve-layer failure that excluded a dead pin may), and the
// session-binding intent so a session re-resolve of a profile-removed candidate
// refuses instead of silently swapping the release, while a fresh selection
// falls through to a profile-satisfying candidate.
func newVirtualMediaDetailedResolver(service virtualDetailedResolverSource) handlers.VirtualMediaDetailedResolver {
	return handlers.VirtualMediaDetailedResolverFunc(func(ctx context.Context, path string, ownerInstallationID int, userID int, profileID string, forceRefresh bool, excludedCandidateIDs []string, preferredCandidateID string) (handlers.ResolvedVirtualMedia, error) {
		res, err := service.ResolveDetailed(ctx, path, forceRefresh, excludedCandidateIDs, preferredCandidateID, handlers.VirtualSessionBinding(ctx), handlers.VirtualCandidateRotationAllowed(ctx))
		if err != nil {
			return handlers.ResolvedVirtualMedia{}, err
		}
		return handlers.ResolvedVirtualMedia{
			URL:                 res.URL,
			URI:                 res.URI,
			CandidateID:         res.CandidateID,
			RequestHeaders:      res.RequestHeaders,
			ExpiresAt:           res.ExpiresAt,
			ProviderVideoHash:   res.ProviderVideoHash,
			ProviderGUID:        res.ProviderGUID,
			ProviderReleaseName: res.ProviderReleaseName,
			ProviderReleaseSize: res.ProviderReleaseSize,
			IdentityRematched:   res.IdentityRematched,
		}, nil
	})
}
