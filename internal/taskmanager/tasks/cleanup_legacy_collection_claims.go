package tasks

import (
	"context"
	"fmt"
	"time"

	"github.com/Silo-Server/silo-server/internal/taskmanager"
)

const (
	cleanupLegacyCollectionClaimsInterval = 24 * time.Hour
	cleanupLegacyCollectionClaimsLimit    = 100
)

type LegacyCollectionClaimCleaner interface {
	CleanupLegacyUnscopedCollectionClaims(ctx context.Context, limit int) (int64, error)
}

type CleanupLegacyCollectionClaimsTask struct {
	cleaner LegacyCollectionClaimCleaner
}

func NewCleanupLegacyCollectionClaimsTask(cleaner LegacyCollectionClaimCleaner) *CleanupLegacyCollectionClaimsTask {
	return &CleanupLegacyCollectionClaimsTask{cleaner: cleaner}
}

func (t *CleanupLegacyCollectionClaimsTask) Key() string {
	return "cleanup_legacy_collection_claims"
}

func (t *CleanupLegacyCollectionClaimsTask) Name() string {
	return "Clean up legacy collection claims"
}

func (t *CleanupLegacyCollectionClaimsTask) Description() string {
	return "Removes obsolete unscoped virtual collection claims"
}

func (t *CleanupLegacyCollectionClaimsTask) Category() taskmanager.TaskCategory {
	return taskmanager.TaskCategoryLibrary
}

func (t *CleanupLegacyCollectionClaimsTask) IsHidden() bool {
	return false
}

func (t *CleanupLegacyCollectionClaimsTask) DefaultTriggers() []taskmanager.TriggerConfig {
	return []taskmanager.TriggerConfig{{
		Type:       taskmanager.TriggerTypeInterval,
		IntervalMs: int64(cleanupLegacyCollectionClaimsInterval / time.Millisecond),
	}}
}

func (t *CleanupLegacyCollectionClaimsTask) Execute(ctx context.Context, progress taskmanager.ProgressReporter) error {
	progress.Report(0, "Finding legacy collection claims")
	if t.cleaner == nil {
		progress.Report(100, "Legacy collection claim cleanup unavailable")
		return nil
	}
	cleaned, err := t.cleaner.CleanupLegacyUnscopedCollectionClaims(ctx, cleanupLegacyCollectionClaimsLimit)
	if err != nil {
		return fmt.Errorf("clean up legacy collection claims: %w", err)
	}
	progress.Report(100, fmt.Sprintf("Cleaned %d legacy collection claims", cleaned))
	return nil
}
