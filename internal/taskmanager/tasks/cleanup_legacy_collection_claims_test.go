package tasks

import (
	"context"
	"testing"
)

type fakeLegacyCollectionClaimCleaner struct {
	limit int
}

func (f *fakeLegacyCollectionClaimCleaner) CleanupLegacyUnscopedCollectionClaims(_ context.Context, limit int) (int64, error) {
	f.limit = limit
	return 2, nil
}

func TestCleanupLegacyCollectionClaimsTask(t *testing.T) {
	cleaner := &fakeLegacyCollectionClaimCleaner{}
	task := NewCleanupLegacyCollectionClaimsTask(cleaner)
	if task.Key() != "cleanup_legacy_collection_claims" {
		t.Fatalf("key = %q", task.Key())
	}
	if err := task.Execute(context.Background(), noopProgressReporter{}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if cleaner.limit != cleanupLegacyCollectionClaimsLimit {
		t.Fatalf("limit = %d, want %d", cleaner.limit, cleanupLegacyCollectionClaimsLimit)
	}
}
