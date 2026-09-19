package api

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/api/handlers"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

type fakeDetailedResolverSource struct {
	allowSubstitution []bool
}

func (f *fakeDetailedResolverSource) ResolveDetailed(_ context.Context, _ string, _ bool, _ []string, _ string, allowCandidateSubstitution ...bool) (virtuallibrary.ResolvedVirtualStream, error) {
	f.allowSubstitution = append([]bool(nil), allowCandidateSubstitution...)
	return virtuallibrary.ResolvedVirtualStream{URL: "http://127.0.0.1:8080/stream", URI: "virtual://movie/1?result=one", CandidateID: "one"}, nil
}

func TestNewVirtualMediaDetailedResolverForwardsRotationIntent(t *testing.T) {
	source := &fakeDetailedResolverSource{}
	resolver := newVirtualMediaDetailedResolver(source)

	// An absent intent keeps the pre-existing substitution behavior.
	if _, err := resolver.ResolveVirtualMediaDetailed(context.Background(), "virtual://movie/1?result=one", 1, 2, "profile", false, []string{"one"}, "one"); err != nil {
		t.Fatalf("ResolveVirtualMediaDetailed() error = %v", err)
	}
	if len(source.allowSubstitution) != 1 || !source.allowSubstitution[0] {
		t.Fatalf("absent intent forwarded %v, want [true]", source.allowSubstitution)
	}

	// An explicit denial must reach the service so a display-driven fallback
	// cannot substitute a sibling for the pinned candidate.
	ctx := handlers.WithVirtualCandidateRotation(context.Background(), false)
	if _, err := resolver.ResolveVirtualMediaDetailed(ctx, "virtual://movie/1?result=one", 1, 2, "profile", false, []string{"one"}, "one"); err != nil {
		t.Fatalf("ResolveVirtualMediaDetailed() error = %v", err)
	}
	if len(source.allowSubstitution) != 1 || source.allowSubstitution[0] {
		t.Fatalf("denied intent forwarded %v, want [false]", source.allowSubstitution)
	}
}
