package release

import (
	"testing"
	"time"
)

// TestReleaseStoreIsReleasedFailsOpenAndBlocksOnlyTheFuture pins the gate
// contract the resolver relies on: unknown or partial schedule data never
// blocks playback, a concrete future date does, and a past date allows it.
func TestReleaseStoreIsReleasedFailsOpenAndBlocksOnlyTheFuture(t *testing.T) {
	store := NewReleaseStore()

	if released, air := store.IsReleased("movie", "tt0000001", 0, 0); !released || air != nil {
		t.Fatalf("unknown movie = released %t air %v, want true/nil", released, air)
	}
	if released, air := store.IsReleased("series", "tt0000002", 1, 1); !released || air != nil {
		t.Fatalf("unknown episode = released %t air %v, want true/nil", released, air)
	}

	future := time.Now().Add(72 * time.Hour)
	store.SetMovie("tt0000010", future)
	if released, air := store.IsReleased("movie", "tt0000010", 0, 0); released || air == nil {
		t.Fatalf("future movie = released %t air %v, want false/non-nil", released, air)
	}

	past := time.Now().Add(-72 * time.Hour)
	store.SetMovie("tt0000011", past)
	if released, _ := store.IsReleased("movie", "tt0000011", 0, 0); !released {
		t.Fatal("past movie was blocked")
	}

	store.SetShow("tt0000012", &ShowSchedule{
		Status: "Running",
		Episodes: map[string]EpisodeInfo{
			"1:1": {Season: 1, Episode: 1, AirDate: future},
			"1:2": {Season: 1, Episode: 2, AirDate: past},
		},
	})
	if released, _ := store.IsReleased("series", "tt0000012", 1, 1); released {
		t.Fatal("future episode was not blocked")
	}
	if released, _ := store.IsReleased("series", "tt0000012", 1, 2); !released {
		t.Fatal("past episode was blocked")
	}
}
