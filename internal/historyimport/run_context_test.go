package historyimport

import (
	"context"
	"testing"
	"time"
)

func TestNewRunContext_HasNoDeadline(t *testing.T) {
	t.Parallel()

	ctx, cancel := newRunContext(context.Background())
	defer cancel()

	if _, ok := ctx.Deadline(); ok {
		t.Fatal("newRunContext should not add a deadline")
	}
}

func TestNewRunContext_UsesBackgroundWhenParentNil(t *testing.T) {
	t.Parallel()

	ctx, cancel := newRunContext(nil) //nolint:staticcheck // This test verifies the function's documented nil-parent fallback.
	defer cancel()

	if err := ctx.Err(); err != nil {
		t.Fatalf("ctx.Err() = %v, want nil before cancel", err)
	}
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("newRunContext should not add a deadline when parent is nil")
	}
}

func TestNewRunContext_PropagatesParentCancellation(t *testing.T) {
	t.Parallel()

	parent, parentCancel := context.WithCancel(context.Background())
	ctx, cancel := newRunContext(parent)
	defer cancel()

	parentCancel()

	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("newRunContext did not propagate parent cancellation")
	}
}
