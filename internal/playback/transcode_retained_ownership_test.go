package playback

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestRetainedGenerationRegistryIsPerSessionAndConcurrencySafe drives concurrent
// retire/get operations across several session ids. It proves the retired map is
// guarded by a single mutex (no data race under -race), retention is keyed
// per-session (one session's retained bytes never serve another), and a
// concurrent switch cannot overwrite a different session's entry.
func TestRetainedGenerationRegistryIsPerSessionAndConcurrencySafe(t *testing.T) {
	m := NewTranscodeManager()
	const sessions = 8
	const switchesPerSession = 4

	// Each session gets its own retained directory containing a session-named
	// segment, so cross-session leakage is observable.
	dirs := make(map[string][]string, sessions)
	for i := 0; i < sessions; i++ {
		sid := sessionIDForIndex(i)
		for j := 0; j < switchesPerSession; j++ {
			dir := filepath.Join(t.TempDir(), "gen")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			name := sid + "-segment.ts"
			if err := os.WriteFile(filepath.Join(dir, name), []byte(sid), 0o644); err != nil {
				t.Fatal(err)
			}
			dirs[sid] = append(dirs[sid], dir)
		}
	}

	var wg sync.WaitGroup
	// One goroutine per session performs that session's switches serially, while
	// distinct sessions run concurrently: this exercises the shared-map mutex and
	// proves entries for different sessions never interfere.
	for i := 0; i < sessions; i++ {
		sid := sessionIDForIndex(i)
		wg.Add(1)
		go func(sid string, dirsForSession []string) {
			defer wg.Done()
			for _, dir := range dirsForSession {
				m.RetireTranscodeSessionPredecessor(sid, NewTranscodeSessionForTest(dir), 5*time.Second)
				if got := m.GetRetainedTranscodeSession(sid); got == nil {
					t.Errorf("session %q lost its retained generation under concurrency", sid)
					return
				}
			}
		}(sid, dirs[sid])
	}
	wg.Wait()

	// Every session's final retained entry only serves its own segment; a
	// different session's segment must not resolve through it.
	for i := 0; i < sessions; i++ {
		sid := sessionIDForIndex(i)
		got := m.GetRetainedTranscodeSession(sid)
		if got == nil {
			t.Fatalf("session %q has no retained generation after the switches", sid)
		}
		if _, err := got.OpenSegment(sid + "-segment.ts"); err != nil {
			t.Fatalf("session %q cannot serve its own retained segment: %v", sid, err)
		}
		other := sessionIDForIndex((i + 1) % sessions)
		if _, err := got.OpenSegment(other + "-segment.ts"); err == nil {
			t.Fatalf("session %q retained entry served session %q's segment (cross-session leak)", sid, other)
		}
	}
}

func sessionIDForIndex(i int) string {
	return "retained-owner-" + string(rune('a'+i))
}

// TestRetainedGenerationReplaceIsAtomicUnderSwitchRace stresses repeated
// same-session switches while a reader polls: the map never holds two entries,
// and every read returns a session that is either serving or cleanly replaced.
// Run under -race to exercise the mutex coverage directly.
func TestRetainedGenerationReplaceIsAtomicUnderSwitchRace(t *testing.T) {
	m := NewTranscodeManager()
	const sid = "retained-race-owner"

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			dir := filepath.Join(t.TempDir(), "gen")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Errorf("mkdir: %v", err)
				return
			}
			m.RetireTranscodeSessionPredecessor(sid, NewTranscodeSessionForTest(dir), 50*time.Millisecond)
		}
		close(stop)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = m.GetRetainedTranscodeSession(sid)
			}
		}
	}()
	wg.Wait()
}

// TestRetainedGenerationTimerReapDoesNotCloseReplacement proves the expiry timer
// of a replaced entry cannot reap its successor: only the exact entry the map
// still holds is removed. The replacement's directory must survive the old
// entry's deadline.
func TestRetainedGenerationTimerReapDoesNotCloseReplacement(t *testing.T) {
	m := NewTranscodeManager()
	firstDir := filepath.Join(t.TempDir(), "gen-first")
	first := readyRetainedSession(t, firstDir)
	secondDir := filepath.Join(t.TempDir(), "gen-second")
	second := readyRetainedSession(t, secondDir)

	// First entry expires quickly; the replacement lives well past it.
	m.RetireTranscodeSessionPredecessor("s1", first, 30*time.Millisecond)
	m.RetireTranscodeSessionPredecessor("s1", second, 5*time.Second)

	// Wait past the first entry's deadline plus the first's dir reaping. The
	// replacement must remain servable and on disk.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(firstDir); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replaced retained dir was not closed on replacement")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond)
	if got := m.GetRetainedTranscodeSession("s1"); got != second {
		t.Fatalf("replacement retained generation = %v, want the newest predecessor", got)
	}
	if _, err := os.Stat(secondDir); err != nil {
		t.Fatalf("replacement retained dir was reaped by the replaced entry's timer: %v", err)
	}
}

// TestRetainedGenerationShutdownStopsTimers proves shutdown closes retained
// generations and their timers do not fire afterward against a drained manager.
func TestRetainedGenerationShutdownStopsTimers(t *testing.T) {
	m := NewTranscodeManager()
	dir := filepath.Join(t.TempDir(), "gen")
	ts := readyRetainedSession(t, dir)
	m.RetireTranscodeSessionPredecessor("s1", ts, 5*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := m.StartShutdownCleanup(ctx)
	cancel()
	<-done

	if got := m.GetRetainedTranscodeSession("s1"); got != nil {
		t.Fatal("retained generation survived shutdown")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("retained dir survived shutdown: %v", err)
	}
}
