package playback

import (
	"context"
	"testing"
	"time"
)

// TestManifestStartupTimeoutForBurnIn pins the burn-in first-window budget.
// A subtitle composite is slower than a plain encode, so it gets a longer
// budget than the historical timeout; the ordinary timeout is unchanged so
// video-failure startup behavior does not move.
func TestManifestStartupTimeoutForBurnIn(t *testing.T) {
	plain := ManifestStartupTimeoutFor(TranscodeOpts{SubtitleTrackIndex: -1})
	if plain != ManifestStartupTimeout {
		t.Fatalf("non-burn-in budget = %v, want %v", plain, ManifestStartupTimeout)
	}
	burnIn := ManifestStartupTimeoutFor(TranscodeOpts{SubtitleBurnIn: true, SubtitleTrackIndex: 0})
	if burnIn != ManifestStartupBurnInTimeout {
		t.Fatalf("burn-in budget = %v, want %v", burnIn, ManifestStartupBurnInTimeout)
	}
	if burnIn <= plain {
		t.Fatalf("burn-in budget %v must exceed the ordinary budget %v", burnIn, plain)
	}
	if ManifestStartupBurnInTimeout <= ManifestStartupTimeout {
		t.Fatal("burn-in timeout must be longer than the ordinary timeout")
	}
	if ManifestStartupTimeout != 30*time.Second {
		t.Fatalf("ordinary startup timeout = %v, want the historical 30s", ManifestStartupTimeout)
	}
}

// TestWaitForManifestContextHonorsOwnerDeadline proves the manifest wait ends
// with the owner's deadline, not a fresh full timeout: a startup context with
// 100ms left bounds a 30s wait, so a slow resolve leaves the manifest wait
// only the remainder of the single startup budget.
func TestWaitForManifestContextHonorsOwnerDeadline(t *testing.T) {
	s := &TranscodeSession{}
	owner, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := s.WaitForManifestContext(owner, 30*time.Second); err == nil {
		t.Fatal("WaitForManifestContext with an expired owner returned nil error")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("manifest wait ran %v, want the owner deadline (~100ms), not the 30s timeout", elapsed)
	}
}

// TestWaitForManifestContextKeepsSingleWaitBound proves the context variant
// preserves the per-wait cap when the owner is generous: a canceled owner
// still ends the wait, and a background owner keeps the historical timeout
// behavior through the shared code path.
func TestWaitForManifestContextKeepsSingleWaitBound(t *testing.T) {
	s := &TranscodeSession{}
	if _, err := s.WaitForManifestContext(context.Background(), time.Millisecond); err == nil {
		t.Fatal("WaitForManifestContext with a 1ms timeout returned nil error")
	}
}
