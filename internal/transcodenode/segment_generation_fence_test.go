package transcodenode

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// A segment URL naming a generation this node no longer serves must be refused
// before the missing-segment wait/restart machinery runs. OpenSegment checks
// the file before the token, so a stale token whose segment is absent surfaces
// as ErrSegmentNotFound; without the guard the node would wait on and restart
// the live generation for a request the final lease fence refuses anyway.
func TestNodeSegmentStaleTokenDoesNotEnterRecovery(t *testing.T) {
	const sessionID = "stale-generation-session"
	server := newTestServer(t)

	session, err := playback.NewReadyTranscodeSessionForTesting(t.TempDir(), playback.TranscodeOpts{
		SessionID:        sessionID,
		TargetCodecVideo: "copy",
		SegmentDuration:  2,
	})
	if err != nil {
		t.Fatalf("ready transcode session: %v", err)
	}
	server.sessions[sessionID] = session
	server.lastAccess[sessionID] = time.Now()

	// A token that names no generation this session owns, paired with a segment
	// that does not exist on disk: both conditions route around the token check
	// in OpenSegmentForGeneration.
	staleToken := session.GenerationToken() + "-stale"
	const segmentName = "seg_99999.ts"
	req := withNodeRouteParams(
		httptest.NewRequest(http.MethodGet,
			"/transcode/"+sessionID+"/segment/"+segmentName+"?sgen="+url.QueryEscape(staleToken), nil),
		map[string]string{"session_id": sessionID, "name": segmentName},
	)
	rr := httptest.NewRecorder()

	start := time.Now()
	server.handleSegment(rr, req)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("stale generation entered wait/restart recovery: handleSegment took %v", elapsed)
	}
	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want %d (stale generation); body = %q", rr.Code, http.StatusPreconditionFailed, rr.Body.String())
	}
}
