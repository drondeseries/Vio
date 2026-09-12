package main

import (
	"context"
	"errors"
	"testing"

	"github.com/Silo-Server/silo-server/internal/catalog"
)

type stubReleaseOverrideReader struct {
	entries []catalog.ReleaseOverride
	err     error
	calls   int
}

func (s *stubReleaseOverrideReader) LookupReleaseOverrides(_ context.Context, ids []catalog.ReleaseIdentity) ([]catalog.ReleaseOverride, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.entries, nil
}

func TestVirtualCatalogHostAdapterForwardsReleaseOverrides(t *testing.T) {
	ctx := context.Background()
	ids := []catalog.ReleaseIdentity{{MediaType: "movie", Provider: "tmdb", ProviderID: "42"}}
	stub := &stubReleaseOverrideReader{entries: []catalog.ReleaseOverride{{ReleaseIdentity: ids[0], Revision: 3}}}
	adapter := virtualCatalogHostAdapter{overrides: stub}

	got, err := adapter.LookupReleaseOverrides(ctx, ids)
	if err != nil {
		t.Fatalf("forwarded lookup failed: %v", err)
	}
	if stub.calls != 1 || len(got) != 1 || got[0].Revision != 3 {
		t.Fatalf("lookup not forwarded: calls=%d entries=%+v", stub.calls, got)
	}

	if _, err := (virtualCatalogHostAdapter{}).LookupReleaseOverrides(ctx, ids); err == nil {
		t.Fatal("adapter without an override reader must fail instead of serving empty results")
	}

	failing := virtualCatalogHostAdapter{overrides: &stubReleaseOverrideReader{err: errors.New("store down")}}
	if _, err := failing.LookupReleaseOverrides(ctx, ids); err == nil {
		t.Fatal("reader errors must propagate to the plugin caller")
	}
}
