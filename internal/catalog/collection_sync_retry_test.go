package catalog

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsRetryableCollectionSyncError(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		retryable bool
	}{
		{name: "out of shared memory", err: &pgconn.PgError{Code: "53200"}, retryable: true},
		{name: "lock not available", err: &pgconn.PgError{Code: "55P03"}, retryable: true},
		{name: "deadlock", err: &pgconn.PgError{Code: "40P01"}, retryable: true},
		{name: "serialization failure", err: &pgconn.PgError{Code: "40001"}, retryable: true},
		{name: "wrapped retryable", err: errors.Join(errors.New("accept failed"), &pgconn.PgError{Code: "53200"}), retryable: true},
		{name: "unique violation", err: &pgconn.PgError{Code: "23505"}, retryable: false},
		{name: "plain error", err: errors.New("boom"), retryable: false},
		{name: "nil", err: nil, retryable: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableCollectionSyncError(tc.err); got != tc.retryable {
				t.Fatalf("isRetryableCollectionSyncError(%v) = %v, want %v", tc.err, got, tc.retryable)
			}
		})
	}
}
