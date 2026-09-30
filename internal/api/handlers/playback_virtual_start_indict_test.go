package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/noderouting"
)

// TestStampStartVirtualCandidateFailedIndictsOnlyConfirmedDead pins the issue-3
// classification: a confirmed dead pin (no matching candidate, no usable
// stream) is stamped so a retry fails fast or rotates, while an empty provider
// listing — deliberately not a verdict about any release — is left unmarked.
func TestStampStartVirtualCandidateFailedIndictsOnlyConfirmedDead(t *testing.T) {
	row := &models.MediaFile{
		ID:                         11,
		ContentID:                  "movie-indict",
		FilePath:                   "virtual://movie/movie-indict?result=cand-a",
		ProviderVideoHash:          "hash-a",
		ProviderReleaseName:        "Movie.2024.1080p",
		VirtualOwnerInstallationID: 5,
	}
	var stamps []string
	h := &PlaybackHandler{
		VirtualCandidateFailMarker: func(_ context.Context, _ int, expectedFilePath string, _ *time.Time) error {
			stamps = append(stamps, expectedFilePath)
			return nil
		},
	}

	h.stampStartVirtualCandidateFailed(context.Background(), row, errors.New("resolve virtual playback: virtual stream provider returned no matching candidate"))
	if len(stamps) != 1 {
		t.Fatalf("confirmed-dead candidate stamps = %d, want 1", len(stamps))
	}

	h.stampStartVirtualCandidateFailed(context.Background(), row, errors.New("no streams available from provider"))
	if len(stamps) != 1 {
		t.Fatalf("empty provider listing stamped a candidate (%d stamps); it must stay unmarked", len(stamps))
	}

	// A row that already carries a verdict is never re-stamped.
	now := time.Now()
	row.FailedAt = &now
	h.stampStartVirtualCandidateFailed(context.Background(), row, errors.New("virtual stream provider returned no matching candidate"))
	if len(stamps) != 1 {
		t.Fatalf("already-failed row stamped again (%d stamps)", len(stamps))
	}
}

// TestClassifyVirtualReplanExhaustionPrefersRouteCapacity pins the issue-2 fix:
// when one candidate hit route-capacity exhaustion, that retryable verdict
// survives even though an earlier same-priority transport failure would
// otherwise win the highest-priority tie. Without the capacity scan the replan
// surfaced a masking reason and could not map to the honest 503.
func TestClassifyVirtualReplanExhaustionPrefersRouteCapacity(t *testing.T) {
	errs := []*candidateErrorV3{
		{Stage: candidateStageTransport, TransportErr: &transportErrorV3{reason: transcodeStartFailedReasonV3, retryable: true}},
		{Stage: candidateStageTransport, TransportErr: &transportErrorV3{reason: string(noderouting.OutcomeCapacityUnavailable), message: "no route", retryable: true}},
	}
	got := classifyVirtualReplanExhaustionV3(nil, errs)
	if got == nil || got.reason != string(noderouting.OutcomeCapacityUnavailable) || !got.retryable {
		t.Fatalf("classification = %#v, want retryable %s", got, noderouting.OutcomeCapacityUnavailable)
	}
}
