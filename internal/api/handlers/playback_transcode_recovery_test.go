package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// TestTranscodeSegmentDeliveryClearsVirtualCandidateFailure drives a real
// (fake-ffmpeg) HLS transcode session serving a full segment and verifies the
// delivery clears the virtual candidate's failed mark through the recovered
// marker, fenced on the delivered candidate path and the failure stamp observed
// before the serving request. This is the HLS counterpart of the direct-play
// recovery: without it a candidate rejected on hardware decode and retried
// successfully stays excluded from the auto-pick.
func TestTranscodeSegmentDeliveryClearsVirtualCandidateFailure(t *testing.T) {
	outputDir := t.TempDir()
	ffmpegPath := writePlaybackTestFFmpegSleep(t, "30")

	const sessionID = "hls-recovery-session"
	const deliveredPath = "virtual://movie/tt1?result=recovered"
	transcodeSession, err := playback.StartTranscode(context.Background(), playback.TranscodeOpts{
		SessionID:          sessionID,
		InputPath:          deliveredPath,
		OutputDir:          outputDir,
		FFmpegPath:         ffmpegPath,
		TargetCodecVideo:   "h264",
		TargetCodecAudio:   "aac",
		SegmentDuration:    2,
		AudioTrackIndex:    -1,
		SubtitleTrackIndex: -1,
	})
	if err != nil {
		t.Fatalf("StartTranscode: %v", err)
	}
	t.Cleanup(func() { _ = transcodeSession.Close() })

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, statErr := os.Stat(filepath.Join(outputDir, "seg_0.m4s")); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake transcode never produced seg_0.m4s")
		}
		time.Sleep(5 * time.Millisecond)
	}

	sessions := playback.NewSessionManager(0, 0)
	sessions.RegisterReconstructed(&playback.Session{
		ID:               sessionID,
		UserID:           1,
		MediaFileID:      91,
		PlayMethod:       playback.PlayTranscode,
		VirtualSourceURI: deliveredPath,
	})
	handler := NewPlaybackHandler(sessions)
	handler.TranscodeManager().RegisterTranscodeSession(sessionID, transcodeSession)

	observed := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	handler.fileResolver = mapPlaybackFileResolver{files: map[int]*models.MediaFile{
		91: {ID: 91, FilePath: deliveredPath, Container: "virtual", FailedAt: &observed},
	}}

	type call struct {
		fileID       int
		delivered    string
		observedFail *time.Time
	}
	var calls []call
	handler.VirtualCandidateRecoveredMarker = func(_ context.Context, fileID int, delivered string, observedFailedAt *time.Time) error {
		calls = append(calls, call{fileID: fileID, delivered: delivered, observedFail: observedFailedAt})
		return nil
	}

	rec := httptest.NewRecorder()
	handler.HandleGetTranscodeSegment(rec, playbackTestRequest(
		http.MethodGet,
		"/api/v1/playback/transcode/"+sessionID+"/segment/seg_0.m4s",
		nil,
		map[string]string{"session_id": sessionID, "name": "seg_0.m4s"},
	))
	if rec.Code != http.StatusOK {
		t.Fatalf("segment status = %d body=%q, want 200", rec.Code, rec.Body.String())
	}
	if len(calls) != 1 {
		t.Fatalf("recovery marker calls = %d, want exactly 1 (a delivered segment clears once)", len(calls))
	}
	if calls[0].fileID != 91 || calls[0].delivered != deliveredPath {
		t.Fatalf("recovery call = (file=%d path=%q), want (91, %q)", calls[0].fileID, calls[0].delivered, deliveredPath)
	}
	if calls[0].observedFail == nil || !calls[0].observedFail.Equal(observed) {
		t.Fatalf("recovery observed failure = %v, want the stamp read before the serve (%v)", calls[0].observedFail, observed)
	}
}

// TestVirtualTranscodeDeliveryIdentity pins how the served candidate identity
// is derived: a virtual session reports its effective row and pinned URI, while
// a non-virtual or unidentified session reports no recovery.
func TestVirtualTranscodeDeliveryIdentity(t *testing.T) {
	fileID, path, ok := virtualTranscodeDelivery(&playback.Session{MediaFileID: 7, VirtualSourceURI: "virtual://movie/tt1?result=a"}, nil)
	if !ok || fileID != 7 || path != "virtual://movie/tt1?result=a" {
		t.Fatalf("virtual session delivery = (%d,%q,%v)", fileID, path, ok)
	}

	fileID, path, ok = virtualTranscodeDelivery(
		&playback.Session{MediaFileID: 0, VirtualSourceURI: ""},
		&playback.RecipeCard{MediaFileID: 8, InputPath: "virtual://movie/tt2?result=b"},
	)
	if !ok || fileID != 8 || path != "virtual://movie/tt2?result=b" {
		t.Fatalf("card fallback delivery = (%d,%q,%v)", fileID, path, ok)
	}

	if _, _, ok := virtualTranscodeDelivery(&playback.Session{MediaFileID: 9, VirtualSourceURI: "/media/local.mkv"}, nil); ok {
		t.Fatal("a local source must not report a virtual delivery")
	}
	if _, _, ok := virtualTranscodeDelivery(nil, nil); ok {
		t.Fatal("a nil session/card must not report a virtual delivery")
	}
}
