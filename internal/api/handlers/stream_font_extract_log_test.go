package handlers

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestFontExtractFailLogThrottlesRepeats pins the font-extraction log throttle:
// the first failure per file+track warns, repeats drop to debug, and a
// successful extraction clears the key so a later regression warns again. The
// extraction itself never fails playback — the endpoint 500 stays the signal.
func TestFontExtractFailLogThrottlesRepeats(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	var l fontExtractFailLog
	ctx := context.Background()
	key := fontExtractFailureKey(7, 1)

	l.failed(ctx, key, "file_id", 7, "track", 1)
	if got := strings.Count(logs.String(), `"level":"WARN"`); got != 1 {
		t.Fatalf("warn count after first failure = %d, want 1\n%s", got, logs.String())
	}

	l.failed(ctx, key, "file_id", 7, "track", 1)
	if got := strings.Count(logs.String(), `"level":"WARN"`); got != 1 {
		t.Fatalf("warn count after repeat = %d, want the repeat throttled\n%s", got, logs.String())
	}
	if got := strings.Count(logs.String(), `"level":"DEBUG"`); got != 1 {
		t.Fatalf("debug count after repeat = %d, want 1\n%s", got, logs.String())
	}

	l.recovered(key)
	l.failed(ctx, key, "file_id", 7, "track", 1)
	if got := strings.Count(logs.String(), `"level":"WARN"`); got != 2 {
		t.Fatalf("warn count after recovery = %d, want the key to warn again\n%s", got, logs.String())
	}
}
