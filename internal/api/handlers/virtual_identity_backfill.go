package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/Silo-Server/silo-server/internal/adminjob"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/scanner"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/resolver"
)

// virtualIdentityBackfillBatch is the number of legacy rows one page scans.
// The page is also the unit of cursor advance, so a crash redoes at most one
// page and a provider is re-listed at most once per candidate group per page.
const virtualIdentityBackfillBatch = 200

// VirtualIdentityBackfillLister lists a virtual source group's provider
// candidates. It is the core virtual library's ListStreams, which uses the
// bounded provider cache so the backfill never storms a provider.
type VirtualIdentityBackfillLister func(ctx context.Context, virtualPath string) ([]virtuallibrary.PlaybackStream, error)

// VirtualIdentityBackfillStore is the catalog slice the backfill needs:
// select the legacy rows, count them, and fill a confidently matched identity.
// It is implemented by *scanner.FileRepository.
type VirtualIdentityBackfillStore interface {
	ListVirtualIdentityBackfillRows(ctx context.Context, afterID, limit int) ([]*models.MediaFile, error)
	CountVirtualIdentityBackfillRows(ctx context.Context) (int, error)
	FillVirtualProviderIdentity(ctx context.Context, fileID int, expectedFilePath string, identity scanner.VirtualProviderIdentity) (bool, error)
}

// VirtualIdentityBackfillJobs is the admin-job slice the one-shot gate needs.
// It is a narrow interface so the gate is unit-testable without a database;
// *adminjob.Repository implements it.
type VirtualIdentityBackfillJobs interface {
	HasFinishedJobOfType(ctx context.Context, jobType string) (bool, error)
	Create(ctx context.Context, input adminjob.CreateJobInput) (*models.AdminJob, error)
}

// VirtualIdentityBackfillExecutor implements the one-shot resumable backfill
// of durable provider identity onto legacy virtual rows.
//
// It walks the legacy set in id order, groups each page by candidate source,
// re-lists each group once, and writes the fresh candidate's identity onto a
// row only when the existing dedup-key tiers prove the same release. A row
// that already has identity is never selected, so the pass is idempotent and a
// resumed page redoes no work that already landed.
type VirtualIdentityBackfillExecutor struct {
	// Lister re-lists one source group's provider candidates. Nil disables the
	// backfill (Execute fails with a clear message).
	Lister VirtualIdentityBackfillLister
	// Store reads the legacy rows and writes the matched identity.
	Store VirtualIdentityBackfillStore
	// Jobs queues the one-shot job at boot. It is only needed by
	// EnsureBackfillJob; Execute does not use it.
	Jobs   VirtualIdentityBackfillJobs
	Logger *slog.Logger

	batchSize int
}

// Execute runs the backfill page loop, resuming from resume.Cursor. A provider
// that cannot be listed for one candidate group is counted and skipped rather
// than failing the pass: the remaining groups are independent, and the one-shot
// job still completes so its cursor never regresses. It returns an error only
// for a catalog read/write failure that a retry might fix.
func (e *VirtualIdentityBackfillExecutor) Execute(
	ctx context.Context,
	_ adminjob.VirtualIdentityBackfillRequest,
	resume adminjob.VirtualIdentityBackfillResume,
	progress func(current, total, cursor int, message string),
) (*adminjob.VirtualIdentityBackfillResult, error) {
	if e == nil || e.Lister == nil || e.Store == nil {
		return nil, errors.New("virtual identity backfill executor is not configured")
	}
	result := &adminjob.VirtualIdentityBackfillResult{Cursor: resume.Cursor, Tiers: map[string]int{}}

	total, err := e.Store.CountVirtualIdentityBackfillRows(ctx)
	if err != nil {
		return nil, fmt.Errorf("count legacy virtual rows: %w", err)
	}
	result.RowsTotal = total
	report := func(message string) {
		if progress != nil {
			progress(result.RowsScanned, total, result.Cursor, message)
		}
	}
	report("Backfilling virtual provider identity")

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rows, err := e.Store.ListVirtualIdentityBackfillRows(ctx, result.Cursor, e.batch())
		if err != nil {
			return nil, fmt.Errorf("list legacy virtual rows: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		result.RowsScanned += len(rows)
		if err := e.processPage(ctx, rows, result); err != nil {
			return nil, err
		}
		report(fmt.Sprintf("Backfilled %d of %d legacy virtual rows", result.RowsScanned, total))
		if len(rows) < e.batch() {
			break
		}
	}
	return result, nil
}

// processPage handles one id-ordered page: it groups the rows by candidate
// source, re-lists each group once, matches each row, and advances the cursor
// to the page's highest id. The cursor only advances after every row in the
// page has been considered, so a crash mid-page resumes before it and no row
// is silently skipped.
func (e *VirtualIdentityBackfillExecutor) processPage(ctx context.Context, rows []*models.MediaFile, result *adminjob.VirtualIdentityBackfillResult) error {
	type group struct {
		path string
		rows []*models.MediaFile
	}
	groupsByKey := make(map[string]*group)
	order := make([]string, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		path, ok := virtualCandidateGroupURI(row.FilePath)
		if !ok {
			// Not a virtual candidate group: nothing to re-list, but the row
			// still counts as scanned and advances the cursor.
			result.RowsUnmatched++
			if row.ID > result.Cursor {
				result.Cursor = row.ID
			}
			continue
		}
		key := fmt.Sprintf("%s\x00%s\x00%d\x00%d", path, row.EpisodeID, row.MediaFolderID, row.VirtualOwnerInstallationID)
		existing, ok := groupsByKey[key]
		if !ok {
			existing = &group{path: path}
			groupsByKey[key] = existing
			order = append(order, key)
		}
		existing.rows = append(existing.rows, row)
	}
	// Every row in the page advances the cursor, whether or not it could be
	// grouped, so a crash after this point never re-lists work already done.
	for _, row := range rows {
		if row != nil && row.ID > result.Cursor {
			result.Cursor = row.ID
		}
	}
	sort.Strings(order)

	for _, key := range order {
		group := groupsByKey[key]
		result.GroupsScanned++
		streams, err := e.Lister(ctx, group.path)
		if err != nil {
			// A provider outage is not fatal to the pass: count the group's
			// rows unmatched and move on. The one-shot job still completes so a
			// later operator re-run can pick these up.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			result.GroupsFailed++
			result.RowsUnmatched += len(group.rows)
			e.logger().WarnContext(ctx, "virtual identity backfill: provider re-list failed",
				"component", "api", "virtual_path", group.path, "rows", len(group.rows), "error", err)
			continue
		}
		for _, row := range group.rows {
			candidate, tier, ok := matchBackfillCandidate(row, streams)
			if !ok {
				result.RowsUnmatched++
				continue
			}
			identity := scanner.VirtualProviderIdentity{
				VideoHash:   candidate.ProviderVideoHash,
				GUID:        candidate.ProviderGUID,
				ReleaseName: candidate.ProviderReleaseName,
				ReleaseSize: candidate.FileSize,
			}
			wrote, err := e.Store.FillVirtualProviderIdentity(ctx, row.ID, row.FilePath, identity)
			if err != nil {
				return fmt.Errorf("fill virtual provider identity: %w", err)
			}
			if !wrote {
				// The row gained identity between the page read and this write
				// (an opportunistic adoption), or the candidate carried no
				// usable tier. Neither is an error and neither overwrites.
				result.RowsSkipped++
				continue
			}
			result.RowsMatched++
			result.Tiers[tier]++
		}
	}
	return nil
}

// matchBackfillCandidate returns the fresh candidate a legacy row can be
// confidently bound to, and the tier that proved it.
//
// The row's listing id is checked first: if the provider still lists the exact
// result the row was persisted from, the bytes are the same listing entry and
// the candidate's identity can be adopted outright. Otherwise the durable tiers
// are compared with MatchCandidateIdentityForBackfill, which requires size
// corroboration on the name tier so a bare release-name coincidence never
// invents identity.
func matchBackfillCandidate(row *models.MediaFile, streams []virtuallibrary.PlaybackStream) (virtuallibrary.PlaybackStream, string, bool) {
	if row == nil {
		return virtuallibrary.PlaybackStream{}, "", false
	}
	if resultID := virtualResultCandidateID(row.FilePath); resultID != "" {
		for _, stream := range streams {
			if stream.ID == resultID {
				return stream, "listing", true
			}
		}
	}
	persisted := resolver.NewPersistedIdentityTiers(
		row.ProviderVideoHash, row.ProviderGUID, row.ProviderReleaseName, row.ProviderReleaseSize,
	)
	for _, stream := range streams {
		candidate := resolver.NewPersistedIdentityTiers(
			stream.ProviderVideoHash, stream.ProviderGUID, stream.ProviderReleaseName, stream.FileSize,
		)
		if tier, ok := resolver.MatchCandidateIdentityForBackfill(persisted, candidate); ok {
			return stream, tier, true
		}
	}
	return virtuallibrary.PlaybackStream{}, "", false
}

// EnsureBackfillJob queues the one-shot backfill at boot when legacy rows exist
// and no backfill has finished. A completed (or explicitly canceled) job is the
// one-shot marker: rows that could not be matched stay legacy on purpose, and
// re-listing every provider on every restart would be a permanent tax. An
// already-active job is a no-op, so a restart during a run resumes the existing
// job instead of queueing a duplicate. A server with no admin account cannot
// own a job and is skipped.
func (e *VirtualIdentityBackfillExecutor) EnsureBackfillJob(ctx context.Context, ownerUserID int) error {
	if e == nil || e.Store == nil || e.Jobs == nil || ownerUserID <= 0 {
		return nil
	}
	done, err := e.Jobs.HasFinishedJobOfType(ctx, adminjob.JobTypeVirtualIdentityBackfill)
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	rows, err := e.Store.ListVirtualIdentityBackfillRows(ctx, 0, 1)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	_, err = e.Jobs.Create(ctx, adminjob.CreateJobInput{
		JobType:         adminjob.JobTypeVirtualIdentityBackfill,
		CreatedByUserID: ownerUserID,
		Message:         "Queued virtual identity backfill",
	})
	if err != nil {
		var conflict *adminjob.ActiveJobConflictError
		if errors.As(err, &conflict) {
			return nil
		}
		return err
	}
	return nil
}

func (e *VirtualIdentityBackfillExecutor) batch() int {
	if e != nil && e.batchSize > 0 {
		return e.batchSize
	}
	return virtualIdentityBackfillBatch
}

func (e *VirtualIdentityBackfillExecutor) logger() *slog.Logger {
	if e == nil || e.Logger == nil {
		return slog.Default()
	}
	return e.Logger
}
