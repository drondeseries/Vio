package api

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/api/handlers"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

type fakeDetailedResolverSource struct {
	allowSubstitution []bool
	sessionBound      bool
}

func (f *fakeDetailedResolverSource) ResolveDetailed(_ context.Context, _ string, _ bool, _ []string, _ string, sessionBound bool, allowCandidateSubstitution ...bool) (virtuallibrary.ResolvedVirtualStream, error) {
	f.allowSubstitution = append([]bool(nil), allowCandidateSubstitution...)
	f.sessionBound = sessionBound
	return virtuallibrary.ResolvedVirtualStream{URL: "http://127.0.0.1:8080/stream", URI: "virtual://movie/1?result=one", CandidateID: "one"}, nil
}

// TestNewVirtualMediaDetailedResolverForwardsIdentityRematch proves the adapter
// carries the same-release re-identification flag to the handler, so the caller
// adopts the new result id instead of treating it as a substitution.
func TestNewVirtualMediaDetailedResolverForwardsIdentityRematch(t *testing.T) {
	source := detailedResolverSourceFunc(func(_ context.Context, _ string, _ bool, _ []string, _ string, _ bool, _ ...bool) (virtuallibrary.ResolvedVirtualStream, error) {
		return virtuallibrary.ResolvedVirtualStream{
			URL:               "http://127.0.0.1:8080/stream",
			URI:               "virtual://movie/1?result=renumbered",
			CandidateID:       "renumbered",
			IdentityRematched: true,
		}, nil
	})
	resolver := newVirtualMediaDetailedResolver(source)

	resolved, err := resolver.ResolveVirtualMediaDetailed(context.Background(), "virtual://movie/1?result=old", 1, 2, "profile", false, nil, "")
	if err != nil {
		t.Fatalf("ResolveVirtualMediaDetailed() error = %v", err)
	}
	if !resolved.IdentityRematched {
		t.Fatal("adapter dropped the same-release re-identification flag")
	}
	if resolved.CandidateID != "renumbered" || resolved.URI != "virtual://movie/1?result=renumbered" {
		t.Fatalf("adapter returned %+v, want the re-identified candidate", resolved)
	}
}

type detailedResolverSourceFunc func(context.Context, string, bool, []string, string, bool, ...bool) (virtuallibrary.ResolvedVirtualStream, error)

func (f detailedResolverSourceFunc) ResolveDetailed(ctx context.Context, virtualPath string, forceRefresh bool, excludedCandidateIDs []string, preferredCandidateID string, sessionBound bool, allowCandidateSubstitution ...bool) (virtuallibrary.ResolvedVirtualStream, error) {
	return f(ctx, virtualPath, forceRefresh, excludedCandidateIDs, preferredCandidateID, sessionBound, allowCandidateSubstitution...)
}

func TestNewVirtualMediaDetailedResolverForwardsIntents(t *testing.T) {
	source := &fakeDetailedResolverSource{}
	resolver := newVirtualMediaDetailedResolver(source)

	// An absent context intent keeps the pre-existing substitution behavior and
	// defaults to session-bound, the conservative choice that refuses a
	// profile-removed substitution.
	if _, err := resolver.ResolveVirtualMediaDetailed(context.Background(), "virtual://movie/1?result=one", 1, 2, "profile", false, []string{"one"}, "one"); err != nil {
		t.Fatalf("ResolveVirtualMediaDetailed() error = %v", err)
	}
	if len(source.allowSubstitution) != 1 || !source.allowSubstitution[0] {
		t.Fatalf("absent rotation intent forwarded %v, want [true]", source.allowSubstitution)
	}
	if !source.sessionBound {
		t.Fatalf("absent binding intent forwarded %v, want true", source.sessionBound)
	}

	// An explicit denial and a fresh-selection declaration must reach the
	// service.
	ctx := handlers.WithVirtualCandidateRotation(context.Background(), false)
	ctx = handlers.WithVirtualSessionBinding(ctx, false)
	if _, err := resolver.ResolveVirtualMediaDetailed(ctx, "virtual://movie/1?result=one", 1, 2, "profile", false, []string{"one"}, "one"); err != nil {
		t.Fatalf("ResolveVirtualMediaDetailed() error = %v", err)
	}
	if len(source.allowSubstitution) != 1 || source.allowSubstitution[0] {
		t.Fatalf("denied rotation intent forwarded %v, want [false]", source.allowSubstitution)
	}
	if source.sessionBound {
		t.Fatalf("fresh-selection intent forwarded %v, want false", source.sessionBound)
	}
}
