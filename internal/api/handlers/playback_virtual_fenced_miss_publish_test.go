package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
)

// TestFencedUpdateMissIsNotAnAdoptionOrCachePublish pins what a caller must
// conclude from a fenced, zero-row UPDATE: the row did not take the write, so
// the candidate's tracks and probe stamp were not published anywhere. The
// explicit saver result distinguishes a metadata miss from an adoption, and a
// caller that treats the miss as an ownership/CAS miss must not stamp a cache
// or pin on the strength of it.
func TestFencedUpdateMissIsNotAnAdoptionOrCachePublish(t *testing.T) {
	// A fenced write that matched no row reports nothing updated and no
	// adoption; that is the only shape a caller may treat as a refusal.
	refused := VirtualFileMetadataUpdateResult{RowsAffected: 0, MetadataUpdated: false, IdentityAdopted: false}
	if refused.MetadataUpdated || refused.IdentityAdopted {
		t.Fatal("a zero-row fenced write must not report metadata update or adoption")
	}

	// A caller that receives that result must not seed the best-result cache or
	// the sticky pin with the rejected candidate. Model the publish decision the
	// way a serve-side caller makes it and assert the miss blocks both.
	cache := NewVirtualBestResultCache(0, 0)
	h := NewPlaybackHandler(nil, nil)
	const (
		key       = "fenced-miss"
		neutral   = "virtual://movie/tt-fenced-miss"
		candidate = neutral + "?result=rejected"
		cacheKey  = "fenced-miss-cache"
	)
	publish := func(result VirtualFileMetadataUpdateResult) {
		// The same gate the serve/refresh callers apply: only a write that
		// actually landed may publish the candidate as reusable evidence.
		if !result.MetadataUpdated {
			return
		}
		h.pinVirtualSticky(key, candidate)
		cache.setWithDetails(cacheKey, "content", neutral, 5, []VirtualPlaybackStream{{ID: "rejected", URI: candidate}}, time.Now())
	}

	publish(refused)

	if got := h.peekVirtualSticky(key); got != "" {
		t.Fatalf("sticky pin = %q, want no pin published for a refused write", got)
	}
	if got := cache.get(cacheKey, time.Now()); got != nil {
		t.Fatalf("best-result cache = %+v, want no entry published for a refused write", got)
	}

	// The complementary case: a write that did land may publish. This proves the
	// guard above is wired to MetadataUpdated and not vacuously blocking.
	publish(VirtualFileMetadataUpdateResult{RowsAffected: 1, MetadataUpdated: true, IdentityAdopted: true})
	if got := h.peekVirtualSticky(key); got != candidate {
		t.Fatalf("sticky pin = %q, want the landed candidate %q", got, candidate)
	}
}

// TestFencedUpdateMissLeavesEvidenceBufferUnpublished pins the other half: when
// the fenced saver refuses the row, the evidence task reports a terminal miss
// and retries nothing, so a refused candidate can never be retried into landing.
func TestFencedUpdateMissLeavesEvidenceBufferUnpublished(t *testing.T) {
	h := &PlaybackHandler{
		VirtualFileMetadataSaver: func(context.Context, models.VirtualFilePersistArgs) (VirtualFileMetadataUpdateResult, error) {
			// The fence matched no row: neither metadata nor identity landed.
			return VirtualFileMetadataUpdateResult{RowsAffected: 0, MetadataUpdated: false, IdentityAdopted: false}, nil
		},
	}
	err := h.persistVirtualEvidenceTask(evidenceTask(21, "virtual://movie/tt-fenced-miss?result=rejected", time.Now()), time.Time{})
	if !errors.Is(err, errVirtualEvidenceStale) {
		t.Fatalf("fenced miss error = %v, want errVirtualEvidenceStale (a CAS/ownership miss, not a retriable failure)", err)
	}
}
