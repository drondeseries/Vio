package playback

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A segment URL minted against one generation must be refused once the session
// has advanced to another, rather than served same-numbered bytes from the
// replacement generation.
func TestOpenSegmentForGenerationRejectsStaleToken(t *testing.T) {
	dir := t.TempDir()
	const name = "seg_00007.ts"
	writePrunerTestFile(t, filepath.Join(dir, name), []byte("generation bytes"), time.Now())

	session := &TranscodeSession{outputDir: dir, opts: TranscodeOpts{SessionID: "session-1"}}
	token := session.GenerationToken()
	if token == "" {
		t.Fatal("GenerationToken returned empty for a live session")
	}
	if !session.MatchesGenerationToken(token) {
		t.Fatal("session does not match its own generation token")
	}

	lease, err := session.OpenSegmentForGeneration(name, token)
	if err != nil {
		t.Fatalf("OpenSegmentForGeneration with the current token: %v", err)
	}
	_ = lease.Close()

	// A restart advances the numeric timeline while the incarnation is stable.
	session.mu.Lock()
	session.segmentGeneration++
	session.mu.Unlock()
	if session.MatchesGenerationToken(token) {
		t.Fatal("the prior generation token still matches after a restart")
	}
	if _, err := session.OpenSegmentForGeneration(name, token); !errors.Is(err, ErrStaleSegmentGeneration) {
		t.Fatalf("stale token error = %v, want ErrStaleSegmentGeneration", err)
	}

	// The replacement generation's own token still serves.
	newToken := session.GenerationToken()
	if newToken == token {
		t.Fatal("generation token did not change across the restart")
	}
	lease, err = session.OpenSegmentForGeneration(name, newToken)
	if err != nil {
		t.Fatalf("OpenSegmentForGeneration with the replacement token: %v", err)
	}
	_ = lease.Close()
}

// An empty token keeps the pre-fencing behavior for clients minted before
// segment URLs carried a generation.
func TestOpenSegmentForGenerationEmptyTokenKeepsBackCompat(t *testing.T) {
	dir := t.TempDir()
	const name = "seg_00000.ts"
	writePrunerTestFile(t, filepath.Join(dir, name), []byte("bytes"), time.Now())
	session := &TranscodeSession{outputDir: dir}

	lease, err := session.OpenSegmentForGeneration(name, "")
	if err != nil {
		t.Fatalf("OpenSegmentForGeneration with an empty token: %v", err)
	}
	_ = lease.Close()
}

// A lease that a wait or restart produced for a different generation must be
// refused, and the rejected lease closed, before it reaches the client.
func TestFenceSegmentLeaseRejectsMismatchedGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seg.ts")
	writePrunerTestFile(t, path, []byte("bytes"), time.Now())

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	lease := &SegmentLease{File: f, Info: info, GenerationToken: "session:inc:3"}

	if _, err := FenceSegmentLease(lease, "session:inc:4"); !errors.Is(err, ErrStaleSegmentGeneration) {
		t.Fatalf("mismatched fence error = %v, want ErrStaleSegmentGeneration", err)
	}
	if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("rejected lease was not closed: stat error = %v", err)
	}

	// A nil lease or an empty token passes through unchanged.
	if lease, err := FenceSegmentLease(nil, "session:inc:4"); lease != nil || err != nil {
		t.Fatalf("nil lease = (%v, %v), want (nil, nil)", lease, err)
	}
	matching := &SegmentLease{GenerationToken: "session:inc:4"}
	if got, err := FenceSegmentLease(matching, "session:inc:4"); got != matching || err != nil {
		t.Fatalf("matching lease = (%v, %v), want the lease back", got, err)
	}
	untokened := &SegmentLease{GenerationToken: "anything"}
	if got, err := FenceSegmentLease(untokened, ""); got != untokened || err != nil {
		t.Fatalf("empty token = (%v, %v), want passthrough", got, err)
	}
}

// The generation token binds the session, the incarnation (attempt), and the
// numeric timeline, and the manifest query carries it verbatim.
func TestGenerationTokenBindsSessionAttemptAndTimeline(t *testing.T) {
	session := &TranscodeSession{
		outputDir:          t.TempDir(),
		segmentIncarnation: "inc",
		opts:               TranscodeOpts{SessionID: "session-1"},
	}
	want := "session-1:inc:0"
	if got := session.GenerationToken(); got != want {
		t.Fatalf("GenerationToken = %q, want %q", got, want)
	}
	if got := session.generationScopedQuery("token=test"); got != "token=test&sgen=session-1%3Ainc%3A0" {
		t.Fatalf("scoped query = %q", got)
	}
	if got := session.generationScopedQuery(""); got != "sgen=session-1%3Ainc%3A0" {
		t.Fatalf("scoped empty query = %q", got)
	}

	session.mu.Lock()
	session.segmentGeneration = 4
	session.mu.Unlock()
	if got := session.GenerationToken(); got != "session-1:inc:4" {
		t.Fatalf("GenerationToken after advance = %q", got)
	}
}

// A segment URI built for a live generation parses back to a token the session
// accepts, so the manifest and the fence agree on the format.
func TestManifestGenerationTokenRoundTrips(t *testing.T) {
	session := &TranscodeSession{
		outputDir:          t.TempDir(),
		segmentIncarnation: "inc",
		opts:               TranscodeOpts{SessionID: "s"},
	}
	query := session.generationScopedQuery("st=jwt")
	const prefix = "sgen="
	idx := strings.Index(query, prefix)
	if idx < 0 {
		t.Fatalf("query %q carries no generation", query)
	}
	// The token is the last query parameter and percent-encoded.
	encoded := query[idx+len(prefix):]
	if strings.Contains(encoded, "&") {
		t.Fatalf("generation parameter %q is not last", encoded)
	}
	token, err := url.QueryUnescape(encoded)
	if err != nil {
		t.Fatalf("unescape generation: %v", err)
	}
	if !session.MatchesGenerationToken(token) {
		t.Fatalf("manifest token %q does not match the session", token)
	}
}
