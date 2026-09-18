package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// levelRecordingHandler captures slog records so tests can assert on both the
// level and the message of a log line.
type levelRecordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *levelRecordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *levelRecordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r.Clone())
	h.mu.Unlock()
	return nil
}

func (h *levelRecordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *levelRecordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *levelRecordingHandler) has(level slog.Level, msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, record := range h.records {
		if record.Level == level && record.Message == msg {
			return true
		}
	}
	return false
}

func TestIsClientCancellation(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"direct canceled", context.Background(), context.Canceled, true},
		{"wrapped canceled", context.Background(), fmt.Errorf("resolve: %w", context.Canceled), true},
		{"canceled ctx masks error", canceledCtx, errors.New("provider blew up"), true},
		{"deadline is not cancellation", context.Background(), context.DeadlineExceeded, false},
		{"provider error is not cancellation", context.Background(), errors.New("provider returned HTTP 500"), false},
		{"nil error and ctx", context.Background(), nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isClientCancellation(tc.ctx, tc.err); got != tc.want {
				t.Fatalf("isClientCancellation(%v, %v) = %v, want %v", tc.ctx, tc.err, got, tc.want)
			}
		})
	}
}

func TestLogVirtualStreamFailureDowngradesClientCancellation(t *testing.T) {
	capture := &levelRecordingHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(previous) })

	file := &models.MediaFile{ID: 7, FilePath: "virtual://movie/tt14538850"}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	logVirtualStreamFailure(canceledCtx, "session-1", file, fmt.Errorf("resolve virtual input: %w", context.Canceled))

	if capture.has(slog.LevelWarn, "virtual stream transport failed") {
		t.Fatal("client cancellation logged as a WARN transport failure")
	}
	if !capture.has(slog.LevelDebug, "virtual stream transport canceled by client") {
		t.Fatal("client cancellation did not log the debug cancellation line")
	}

	// A genuine timeout must still warn.
	logVirtualStreamFailure(context.Background(), "session-1", file, fmt.Errorf("provider fetch: %w", context.DeadlineExceeded))
	if !capture.has(slog.LevelWarn, "virtual stream transport failed") {
		t.Fatal("timeout did not log as a WARN transport failure")
	}
}

func TestHandleTransportStartFailureDowngradesClientCancellation(t *testing.T) {
	capture := &levelRecordingHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(previous) })

	filePath := writePlaybackTestMediaFile(t, "movie.mkv")
	file := &models.MediaFile{
		ID:        42,
		ContentID: "movie-1",
		FilePath:  filePath,
		Duration:  3600,
	}
	baseMgr := playback.NewSessionManager(0, 0)
	session, err := baseMgr.StartSession(1, "profile-1", 42, playback.PlayDirect, false)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	handler := NewStreamHandler(baseMgr, testPlaybackFileResolver{file: file})

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	handler.handleTransportStartFailure(canceledCtx, session, file, context.Canceled)
	if capture.has(slog.LevelWarn, "stream transport startup failed") {
		t.Fatal("client cancellation logged as a WARN transport failure")
	}
	if !capture.has(slog.LevelDebug, "stream transport canceled by client") {
		t.Fatal("client cancellation did not log the debug cancellation line")
	}

	handler.handleTransportStartFailure(context.Background(), session, file, errors.New("ffmpeg unavailable"))
	if !capture.has(slog.LevelWarn, "stream transport startup failed") {
		t.Fatal("a genuine transport error did not log as WARN")
	}
}
