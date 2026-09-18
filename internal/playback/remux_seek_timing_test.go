package playback

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestIsRemoteRemuxInput(t *testing.T) {
	cases := map[string]bool{
		"http://127.0.0.1:1234/source/token/stream.mkv": true,
		"HTTPS://provider.example/media.mkv":            true,
		"virtual://movie/123":                           true,
		"/media/movies/movie.mkv":                       false,
		"":                                              false,
	}
	for input, want := range cases {
		if got := isRemoteRemuxInput(input); got != want {
			t.Fatalf("isRemoteRemuxInput(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestLogRemuxSeekTimingEmitsOnlyForSeeks(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	start := time.Unix(1_700_000_000, 0)
	spawnStart := start.Add(20 * time.Millisecond)
	spawnDone := start.Add(35 * time.Millisecond)
	firstByte := start.Add(900 * time.Millisecond)

	// A start (seek 0) is ordinary playback and must not add a per-request line.
	logRemuxSeekTiming(context.Background(), 0, "https://provider.example/media.mkv", "mp4", start, spawnStart, spawnDone, firstByte)
	if logs.Len() != 0 {
		t.Fatalf("seek 0 emitted a timing line: %s", logs.String())
	}

	logRemuxSeekTiming(context.Background(), 60.25, "https://provider.example/media.mkv", "mp4", start, spawnStart, spawnDone, firstByte)
	out := logs.String()
	for _, want := range []string{
		`"seek_seconds":60.25`,
		`"remote_input":true`,
		`"spawn_ms":15`,
		`"handler_ms":20`,
		`"first_byte_ms":865`,
		`"total_ms":880`,
		`"request_to_first_byte_ms":900`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("timing line missing %s: %s", want, out)
		}
	}
}
