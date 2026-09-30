package playback

import (
	"context"
	"encoding/json"
	"testing"
)

type captureRealtimeConn struct {
	written []any
}

func (c *captureRealtimeConn) WriteJSON(v any) error {
	c.written = append(c.written, v)
	return nil
}

// TestNewDownloadProgressEventDerivesPercentAndClamps pins the payload shape a
// client reads: a zero percent with byte counts is derived, completed is 100,
// and an empty session id is rejected.
func TestNewDownloadProgressEventDerivesPercentAndClamps(t *testing.T) {
	event, err := NewDownloadProgressEvent("sess-1", DownloadProgressPayload{
		State: DownloadProgressStateDownloading, Bytes: 25, TotalBytes: 100,
	})
	if err != nil {
		t.Fatalf("NewDownloadProgressEvent: %v", err)
	}
	var payload DownloadProgressPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Percent != 25 {
		t.Fatalf("percent = %v, want 25", payload.Percent)
	}
	if payload.SessionID != "sess-1" {
		t.Fatalf("session id = %q, want sess-1", payload.SessionID)
	}

	completed, err := NewDownloadProgressEvent("sess-1", DownloadProgressPayload{State: DownloadProgressStateCompleted})
	if err != nil {
		t.Fatalf("completed event: %v", err)
	}
	if err := json.Unmarshal(completed.Payload, &payload); err != nil {
		t.Fatalf("decode completed payload: %v", err)
	}
	if payload.Percent != 100 {
		t.Fatalf("completed percent = %v, want 100", payload.Percent)
	}

	if _, err := NewDownloadProgressEvent("", DownloadProgressPayload{}); err == nil {
		t.Fatal("empty session id must be rejected")
	}
}

// TestRealtimeHubPublishDownloadProgressDeliversAndReportsMissing proves the hub
// send is best-effort: a session with no connection reports false and a live
// one receives the validated event.
func TestRealtimeHubPublishDownloadProgressDeliversAndReportsMissing(t *testing.T) {
	hub := NewRealtimeHub()
	if hub.PublishDownloadProgress("sess-missing", DownloadProgressPayload{}) {
		t.Fatal("publish without a connection reported true")
	}

	conn := &captureRealtimeConn{}
	registration := hub.Register("sess-1", conn)
	defer hub.Unregister(registration)

	if !hub.PublishDownloadProgress("sess-1", DownloadProgressPayload{State: DownloadProgressStateQueued}) {
		t.Fatal("publish with a live connection reported false")
	}
	if len(conn.written) != 1 {
		t.Fatalf("writes = %d, want 1", len(conn.written))
	}
	event, ok := conn.written[0].(EventEnvelope)
	if !ok {
		t.Fatalf("written value = %T, want EventEnvelope", conn.written[0])
	}
	if event.Name != RealtimeEventDownloadProgress {
		t.Fatalf("event name = %q, want %q", event.Name, RealtimeEventDownloadProgress)
	}
}

// TestSetVirtualSourceIfGenerationFencesStaleHandoff proves the CAS the cache
// handoff uses: a binding move that landed after the handoff captured its
// generation refuses the stale write, while the current generation applies.
func TestSetVirtualSourceIfGenerationFencesStaleHandoff(t *testing.T) {
	manager := NewSessionManager(0, 0)
	session, err := manager.StartSession(1, "profile-1", 10, PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	const releaseURI = "virtual://movie/tt1?result=cand-a"
	if err := manager.SetVirtualSource(session.ID, releaseURI, 5); err != nil {
		t.Fatalf("SetVirtualSource: %v", err)
	}
	stale, err := manager.VirtualSourceGeneration(session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceGeneration: %v", err)
	}
	// A concurrent binding move (a rotation) advances the generation.
	if err := manager.SetVirtualSource(session.ID, "virtual://movie/tt1?result=cand-b", 5); err != nil {
		t.Fatalf("second SetVirtualSource: %v", err)
	}

	if _, applied, err := manager.SetVirtualSourceIfGeneration(session.ID, stale, "virtual://movie/tt1?result=cached", 5); err != nil || applied {
		t.Fatalf("stale handoff applied=%v err=%v, want false/nil", applied, err)
	}
	current, _ := manager.GetSession(session.ID)
	if current.VirtualSourceURI != "virtual://movie/tt1?result=cand-b" {
		t.Fatalf("binding = %q, want the newer binding", current.VirtualSourceURI)
	}

	generation, err := manager.VirtualSourceGeneration(session.ID)
	if err != nil {
		t.Fatalf("VirtualSourceGeneration: %v", err)
	}
	if _, applied, err := manager.SetVirtualSourceIfGeneration(session.ID, generation, "virtual://movie/tt1?result=cached", 5); err != nil || !applied {
		t.Fatalf("current handoff applied=%v err=%v, want true/nil", applied, err)
	}
	current, _ = manager.GetSession(session.ID)
	if current.VirtualSourceURI != "virtual://movie/tt1?result=cached" {
		t.Fatalf("binding = %q, want the cached binding", current.VirtualSourceURI)
	}
}

// TestVirtualSourceGenerationMissingSession proves the fence reports an unknown
// session rather than silently succeeding.
func TestVirtualSourceGenerationMissingSession(t *testing.T) {
	manager := NewSessionManager(0, 0)
	if _, err := manager.VirtualSourceGeneration("missing"); err == nil {
		t.Fatal("missing session must report an error")
	}
	if _, applied, err := manager.SetVirtualSourceIfGeneration("missing", 0, "virtual://x", 1); err == nil || applied {
		t.Fatal("missing session must report an error and not apply")
	}
	_ = context.Background()
}
