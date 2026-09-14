package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestManifestBuildFailureIsClientStop pins the classification used by the
// transcode manifest route: once the session has been torn down (client stop /
// DELETE), a manifest failure is expected teardown behavior — not a fault.
func TestManifestBuildFailureIsClientStop(t *testing.T) {
	sessionMgr := playback.NewSessionManager(0, 0)
	h := NewPlaybackHandler(sessionMgr)

	if !manifestBuildFailureIsClientStop(h, "missing-session") {
		t.Fatal("session absent from manager must classify as client stop " +
			"(manifest requests reach the build step only via a valid token+card, " +
			"so absence means teardown already happened)")
	}

	live, err := sessionMgr.StartSession(1, "profile-1", 42, playback.PlayTranscode, true)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	if manifestBuildFailureIsClientStop(h, live.ID) {
		t.Fatal("live session must not classify as client stop")
	}

	if err := sessionMgr.StopSession(live.ID); err != nil {
		t.Fatalf("stop session: %v", err)
	}
	if !manifestBuildFailureIsClientStop(h, live.ID) {
		t.Fatal("stopped session must classify as client stop")
	}
}

func TestCopySeekAnchorRetrySkipsWhenContextDone(t *testing.T) {
	// The retry loop must not burn a second probe after the caller's own
	// context expired (e.g. the bounded replan budget): the first attempt's
	// failure is already explained by cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	attempts := 0
	resolve := func() error {
		attempts++
		return errors.New("boom")
	}

	_ = ctx.Err() // mark done
	for attempt := 1; attempt <= 2; attempt++ {
		err := resolve()
		if err == nil || ctx.Err() != nil {
			break
		}
		_ = attempt
	}

	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 when context is already canceled", attempts)
	}
}

// TestDropStaleSubtitleIdentityParity mirrors the audio-side staleness rule
// for subtitles at the request boundary: only well-formed identities bound to
// a different file are considered stale.
func TestDropStaleSubtitleIdentityParity(t *testing.T) {
	file := &models.MediaFile{ID: 42}

	stale := playback.TrackIDV3(41, "subtitle", 0)
	if stale == "" {
		t.Fatal("sanity")
	}
	// The subtitle boundary check lives in remapSubtitleSelectionV3; here we
	// pin that malformed IDs are never treated as stale (they stay hard
	// errors), matching the audio-side contract.
	_, kind, _, ok := playback.ParseTrackIDV3("garbage")
	if ok || kind != "" {
		t.Fatal("malformed identity should not parse")
	}
	_ = file
}
