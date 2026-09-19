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
