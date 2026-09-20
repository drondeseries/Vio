package playback

import (
	"fmt"
	"testing"
	"time"
)

// Under churn of permanently failing identities the failed-warm cooldown map
// must stay capped, while a still-live identity keeps suppressing its warm.
// Once every cooldown has lapsed, an access at the cap prunes the map.
func TestWarmGuardStaysBoundedUnderChurn(t *testing.T) {
	c := NewSubtitleCache(func() string { return t.TempDir() })
	base := time.Now()

	churn := subtitleWarmGuardMaxEntries + 64
	for i := 0; i < churn; i++ {
		c.noteWarmOutcome(fmt.Sprintf("identity-%d", i), false, base.Add(time.Duration(i)*time.Millisecond))
	}

	c.warmMu.Lock()
	got := len(c.warmGuard)
	c.warmMu.Unlock()
	if got > subtitleWarmGuardMaxEntries {
		t.Fatalf("warmGuard entries = %d, want <= %d", got, subtitleWarmGuardMaxEntries)
	}

	last := fmt.Sprintf("identity-%d", churn-1)
	lastNow := base.Add(time.Duration(churn-1) * time.Millisecond)
	if c.warmAdmitted(last, lastNow) {
		t.Fatal("live failed identity was evicted by churn")
	}

	// Advance past every cooldown; an admission check at the cap drops the
	// lapsed entries rather than retaining them forever.
	later := base.Add(time.Duration(churn)*time.Millisecond + subtitleWarmFailureMaxCooldown + time.Minute)
	if !c.warmAdmitted(last, later) {
		t.Fatal("lapsed cooldown must admit the next warm")
	}
	c.warmMu.Lock()
	after := len(c.warmGuard)
	c.warmMu.Unlock()
	if after != 0 {
		t.Fatalf("warmGuard entries after all cooldowns lapsed = %d, want 0", after)
	}
}
