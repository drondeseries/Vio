package altmount

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

// recordingObserver collects the release keys an AltMount client reports.
type recordingObserver struct {
	mu   sync.Mutex
	keys []string
}

func (o *recordingObserver) observe(key string) {
	o.mu.Lock()
	o.keys = append(o.keys, key)
	o.mu.Unlock()
}

func (o *recordingObserver) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.keys...)
}

// TestClassifyCandidatesNotifiesConfirmedReleaseOnce proves the uncached ->
// cached flip from AltMount's completed history is reported exactly once, even
// though classification runs on every serve.
func TestClassifyCandidatesNotifiesConfirmedReleaseOnce(t *testing.T) {
	client := New(nil)
	observer := &recordingObserver{}
	client.SetConfirmObserver(observer.observe)

	releaseName := "Movie.2024.1080p.WEB-DL.x264-GRP"
	client.mu.Lock()
	client.state = altmountStateSnapshot{
		Completed: map[string]altmountReleaseRecord{
			ReleaseKey(releaseName): {Size: 1000, Identity: ReleaseKey(releaseName)},
		},
		Failed: map[string]altmountReleaseRecord{},
	}
	client.mu.Unlock()

	candidates := []stream.StreamCandidate{{Name: releaseName, FileSize: 1000}}
	client.ClassifyCandidates(candidates)
	if !candidates[0].SourceConfirmed {
		t.Fatal("candidate was not marked SourceConfirmed")
	}
	// A second classification of the same, already-confirmed release must not
	// re-announce it.
	client.ClassifyCandidates(candidates)

	keys := observer.snapshot()
	if len(keys) != 1 {
		t.Fatalf("observer notifications = %v, want exactly one", keys)
	}
	if keys[0] != ReleaseKey(releaseName) {
		t.Fatalf("notification key = %q, want %q", keys[0], ReleaseKey(releaseName))
	}
}

// TestClassifyCandidatesNotifiesBadgeConfirmation proves the free "⚡ cached"
// badge is observed even when no history API is wired.
func TestClassifyCandidatesNotifiesBadgeConfirmation(t *testing.T) {
	client := New(nil)
	observer := &recordingObserver{}
	client.SetConfirmObserver(observer.observe)
	client.mu.Lock()
	client.state = altmountStateSnapshot{
		Completed: map[string]altmountReleaseRecord{},
		Failed:    map[string]altmountReleaseRecord{},
	}
	client.mu.Unlock()

	candidates := []stream.StreamCandidate{{Name: "⚡ cached Movie.2024.2160p.REMUX"}}
	client.ClassifyCandidates(candidates)
	if !candidates[0].SourceConfirmed {
		t.Fatal("badge candidate was not marked SourceConfirmed")
	}
	keys := observer.snapshot()
	if len(keys) != 1 {
		t.Fatalf("observer notifications = %v, want exactly one", keys)
	}
	// The badge is display decoration, not identity: a badge-only confirmation
	// must key the same release as the unbadged name, or it can never match the
	// identity the handoff waiter registered from the persisted row.
	want := ReleaseKey("Movie.2024.2160p.REMUX")
	if keys[0] != want {
		t.Fatalf("badge notification key = %q, want the unbadged key %q", keys[0], want)
	}
	if got := candidateReleaseName(stream.StreamCandidate{Name: "Movie.2024.2160p.REMUX"}); got != want {
		t.Fatalf("unbadged key = %q, want %q", got, want)
	}
}

// TestClassifyCandidatesDoesNotNotifyWithoutObserver proves classification is
// unchanged when no listener is installed: no badge marking is forced and the
// observer path is inert.
func TestClassifyCandidatesDoesNotNotifyWithoutObserver(t *testing.T) {
	client := New(nil)
	candidates := []stream.StreamCandidate{{Name: "⚡ cached Movie.2024.2160p.REMUX"}}
	client.ClassifyCandidates(candidates)
	if candidates[0].SourceConfirmed {
		t.Fatal("badge marking must not run without a listener")
	}
}

// TestRefreshNotifiesNewlyCompletedRelease proves a refresh that discovers a
// completed release reports it once, and a later refresh of the same state does
// not repeat it.
func TestRefreshNotifiesNewlyCompletedRelease(t *testing.T) {
	releaseName := "Movie.2024.1080p.WEB-DL.x264-GRP"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"history":{"slots":[{"name":%q,"nzb_name":%q,"status":"Completed","storage":"/downloads/%s","bytes":1000,"completetime":%d}]}}`, releaseName, releaseName, releaseName, time.Now().Unix())
	}))
	defer srv.Close()

	client := New(nil)
	client.Configure(srv.URL, "", 15)
	observer := &recordingObserver{}
	client.SetConfirmObserver(observer.observe)

	if err := client.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	afterFirst := observer.snapshot()
	if !containsKey(afterFirst, ReleaseKey(releaseName)) {
		t.Fatalf("refresh notifications = %v, want the completed release %q", afterFirst, ReleaseKey(releaseName))
	}
	if err := client.Refresh(context.Background()); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if afterSecond := observer.snapshot(); len(afterSecond) != len(afterFirst) {
		t.Fatalf("second refresh re-notified: first=%v second=%v", afterFirst, afterSecond)
	}
}

func containsKey(keys []string, want string) bool {
	for _, key := range keys {
		if key == want {
			return true
		}
	}
	return false
}

// TestReleaseKeyNormalizesLikeClassification pins the exported key to the
// normalization classification matches on, so a listener keys on the same
// identity.
func TestReleaseKeyNormalizesLikeClassification(t *testing.T) {
	got := ReleaseKey("Movie.2024.1080p.WEB-DL.x264-GRP.mkv")
	want := releaseNameKey("Movie.2024.1080p.WEB-DL.x264-GRP.mkv")
	if got != want || got == "" {
		t.Fatalf("ReleaseKey = %q, want %q", got, want)
	}
}
