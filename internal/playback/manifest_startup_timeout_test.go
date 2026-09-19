package playback

import (
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
