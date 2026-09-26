package handlers

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/cache"
	evt "github.com/Silo-Server/silo-server/internal/events"
	"github.com/Silo-Server/silo-server/internal/sections"
)

// TestSubscribePurgeInvalidationsReactsToHubEvent verifies that a
// virtual_purge_complete event published on the catalog channel triggers the
// hard purge invalidation locally, so other API replicas clear their Home
// caches without waiting for TTL expiry.
func TestSubscribePurgeInvalidationsReactsToHubEvent(t *testing.T) {
	sections.ResetResolvedListCacheForTestForPurge()
	defer sections.ResetResolvedListCacheForTestForPurge()

	h := &LibraryCollectionHandler{
		EventsHub: evt.NewHub("test", &cache.NoopEventBus{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.SubscribePurgeInvalidations(ctx)

	before := sections.ResolvedListPurgeEpochForTest()
	if err := h.EventsHub.PublishJSON(ctx, evt.ChannelCatalog, "virtual_purge_complete",
		map[string]any{"library_id": 0, "installation_id": 0}, evt.PublishOptions{}); err != nil {
		t.Fatalf("publish virtual_purge_complete: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for sections.ResolvedListPurgeEpochForTest() == before {
		if time.Now().After(deadline) {
			t.Fatal("subscriber did not invalidate after virtual_purge_complete event")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestSubscribePurgeInvalidationsIgnoresOtherChannels verifies the subscriber
// only reacts to catalog-channel purge events, not unrelated hub traffic.
func TestSubscribePurgeInvalidationsIgnoresOtherChannels(t *testing.T) {
	sections.ResetResolvedListCacheForTestForPurge()
	defer sections.ResetResolvedListCacheForTestForPurge()

	h := &LibraryCollectionHandler{
		EventsHub: evt.NewHub("test", &cache.NoopEventBus{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.SubscribePurgeInvalidations(ctx)

	before := sections.ResolvedListPurgeEpochForTest()
	if err := h.EventsHub.PublishJSON(ctx, evt.ChannelCatalog, "some_other_event",
		map[string]any{}, evt.PublishOptions{}); err != nil {
		t.Fatalf("publish other event: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if sections.ResolvedListPurgeEpochForTest() != before {
		t.Fatal("subscriber invalidated on a non-purge event")
	}
}

// TestSubscribePurgeInvalidationsNilSafe verifies the subscriber tolerates a
// nil hub or nil context without panicking.
func TestSubscribePurgeInvalidationsNilSafe(t *testing.T) {
	h := &LibraryCollectionHandler{}
	h.SubscribePurgeInvalidations(context.Background())
	h.SubscribePurgeInvalidations(nil)
}

// TestReconcilePurgeRevisionRecoversMissedEvent verifies the blocking case:
// a direct handler invalidation advances the cache epoch without recording
// the revision, the own-event echo records it, then a later purge commits a
// higher revision whose event is lost. The poller path must still recover.
func TestReconcilePurgeRevisionRecoversMissedEvent(t *testing.T) {
	sections.ResetResolvedListCacheForTestForPurge()
	defer sections.ResetResolvedListCacheForTestForPurge()

	before := sections.ResolvedListPurgeEpochForTest()
	// First observation after a populated cache must invalidate (caches may
	// already be warm before the first successful poll).
	if !sections.ReconcilePurgeRevision(5) {
		t.Fatal("first observed revision must invalidate")
	}
	if got := sections.LastSeenPurgeRevisionForTest(); got != 5 {
		t.Fatalf("watermark = %d, want 5", got)
	}
	// Real handler sequence: direct local invalidation (epoch only), then
	// the own-event echo carries the committed revision.
	sections.InvalidateAllResolvedListCachesForPurge()
	epochAfterDirect := sections.ResolvedListPurgeEpochForTest()
	if epochAfterDirect == before {
		t.Fatal("direct invalidation did not advance the epoch")
	}
	if !sections.ReconcilePurgeRevision(6) {
		t.Fatal("own-event echo with newer revision must reconcile")
	}
	if sections.ReconcilePurgeRevision(6) {
		t.Fatal("duplicate revision must not re-invalidate")
	}
	// A later purge commits 7 but its event is lost; polling observes it.
	if !sections.ReconcilePurgeRevision(7) {
		t.Fatal("revision jump 6->7 must reconcile in one step")
	}
	if got := sections.LastSeenPurgeRevisionForTest(); got != 7 {
		t.Fatalf("watermark = %d, want 7 after jump", got)
	}
	// Exactly three reconciled-or-direct invalidations: first observation,
	// direct handler call, echo, jump = epoch advanced 3 past baseline... the
	// direct call is included, so expect before+4 total (5-reconcile, direct,
	// 6-reconcile, 7-reconcile).
	if got := sections.ResolvedListPurgeEpochForTest(); got != before+4 {
		t.Fatalf("epoch = %d, want %d (first + direct + echo + jump)", got, before+4)
	}
}

// TestReconcilePurgeRevisionConcurrentDoesNotSuppressRecovery verifies that
// overlapping poll and event reconciliations serialize without losing a
// newer revision.
func TestReconcilePurgeRevisionConcurrentDoesNotSuppressRecovery(t *testing.T) {
	sections.ResetResolvedListCacheForTestForPurge()
	defer sections.ResetResolvedListCacheForTestForPurge()

	const workers = 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(rev int64) {
			defer wg.Done()
			sections.ReconcilePurgeRevision(rev)
		}(int64(i) + 1)
	}
	wg.Wait()
	if got := sections.LastSeenPurgeRevisionForTest(); got != int64(workers) {
		t.Fatalf("watermark = %d, want %d", got, workers)
	}
	if !sections.ReconcilePurgeRevision(int64(workers) + 1) {
		t.Fatal("newer revision after concurrent storm must reconcile")
	}
}

// TestReconcilePurgeRevisionLegacyEventForcesInvalidation verifies that an
// event without a revision payload still invalidates locally without moving
// the watermark.
func TestReconcilePurgeRevisionLegacyEventForcesInvalidation(t *testing.T) {
	sections.ResetResolvedListCacheForTestForPurge()
	defer sections.ResetResolvedListCacheForTestForPurge()

	before := sections.ResolvedListPurgeEpochForTest()
	if !sections.ReconcilePurgeRevision(-1) {
		t.Fatal("legacy event must invalidate")
	}
	if sections.ResolvedListPurgeEpochForTest() == before {
		t.Fatal("legacy reconcile did not invalidate")
	}
	if got := sections.LastSeenPurgeRevisionForTest(); got != -1 {
		t.Fatalf("watermark = %d, want -1 (unchanged) after legacy event", got)
	}
}
