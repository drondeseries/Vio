package virtuallibrary_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
)

// TestRefusalLogsCarryIdentityPresenceAndRequestID pins #147: the
// session-bound dead-pin refusal must name whether the row carried any durable
// identity, which tier (if any) was compared, and the threaded request id, so a
// renumber can be correlated from one grep instead of manual log archaeology.
func TestRefusalLogsCarryIdentityPresenceAndRequestID(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := newProviderService(t, logger, virtuallibrary.Config{},
		streamEntry("Alpha.Movie.2024.1080p.WEB-DL.x264", "http://192.168.1.10/alpha.mkv"),
	)

	ctx := virtuallibrary.WithRequestID(context.Background(), "req-abc-123")
	ctx = virtuallibrary.WithPersistedCandidateIdentity(ctx, virtuallibrary.PersistedCandidateIdentity{
		ReleaseName: "alpha.movie.2024.1080p.web.dl.x264",
		ReleaseSize: 8_000_000_000,
	})

	_, err := svc.ResolveDetailed(
		ctx,
		"virtual://movie/tt100?result="+absentResultID,
		false, nil, "", true, false,
	)
	if err == nil {
		t.Fatal("expected the session-bound dead-pin refusal")
	}
	if !strings.Contains(err.Error(), "no longer listed") {
		t.Fatalf("error = %v, want the dead-pin refusal", err)
	}

	logged := logs.String()
	if !strings.Contains(logged, "has_identity=true") {
		t.Fatalf("refusal log did not report identity presence: %s", logged)
	}
	if !strings.Contains(logged, "request_id=req-abc-123") {
		t.Fatalf("refusal log did not carry the threaded request id: %s", logged)
	}
	if !strings.Contains(logged, "identity_tier=") {
		t.Fatalf("refusal log did not report the compared tier: %s", logged)
	}
}

// TestRefusalLogsReportNoIdentityForLegacyRow proves the other half of #147: a
// legacy row with no durable identity logs has_identity=false and names all
// three empty tiers, so "no identity" is distinguishable from a real mismatch.
func TestRefusalLogsReportNoIdentityForLegacyRow(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := newProviderService(t, logger, virtuallibrary.Config{},
		streamEntry("Alpha.Movie.2024.1080p.WEB-DL.x264", "http://192.168.1.10/alpha.mkv"),
	)

	_, err := svc.ResolveDetailed(
		context.Background(),
		"virtual://movie/tt100?result="+absentResultID,
		false, nil, "", true, false,
	)
	if err == nil {
		t.Fatal("expected the session-bound dead-pin refusal for a legacy row")
	}

	logged := logs.String()
	if !strings.Contains(logged, "has_identity=false") {
		t.Fatalf("refusal log for a legacy row did not report has_identity=false: %s", logged)
	}
	if !strings.Contains(logged, "video_hash") || !strings.Contains(logged, "release_name") {
		t.Fatalf("refusal log did not name the empty tiers: %s", logged)
	}
}

// TestWithRequestIDRoundTrips proves the request-id seam is a no-op for an
// empty id and readable through the exported accessor, so the handlers lane can
// thread chimw.GetReqID without the resolver importing the middleware.
func TestWithRequestIDRoundTrips(t *testing.T) {
	if got := virtuallibrary.RequestIDFromContext(nil); got != "" {
		t.Fatalf("nil ctx id = %q, want empty", got)
	}
	base := context.Background()
	if got := virtuallibrary.RequestIDFromContext(virtuallibrary.WithRequestID(base, "  ")); got != "" {
		t.Fatalf("blank id = %q, want empty", got)
	}
	ctx := virtuallibrary.WithRequestID(base, " req-1 ")
	if got := virtuallibrary.RequestIDFromContext(ctx); got != "req-1" {
		t.Fatalf("id = %q, want the trimmed request id", got)
	}
}
