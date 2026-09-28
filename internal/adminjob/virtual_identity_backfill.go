package adminjob

import (
	"context"
	"time"
)

// JobTypeVirtualIdentityBackfill is the one-shot resumable backfill of durable
// provider identity onto legacy virtual rows. Those rows predate identity
// persistence, so they carry neither a provider video hash nor a provider GUID
// and cannot re-match a release across a provider renumbering.
const JobTypeVirtualIdentityBackfill = "virtual_identity_backfill"

// virtualIdentityBackfillTimeout bounds one execution. The backfill re-lists
// providers once per candidate group and the volume can be in the thousands, so
// the budget is deliberately long; the durable cursor in the job's result
// payload lets a later claim resume instead of restarting.
const virtualIdentityBackfillTimeout = 12 * time.Hour

// VirtualIdentityBackfillRequest is empty: the backfill derives its own scope
// from the catalog. It exists as a typed payload so the runner can decode a job
// without special-casing nil.
type VirtualIdentityBackfillRequest struct{}

// VirtualIdentityBackfillResume is the durable cursor a claimed job resumes
// from. It is refreshed with every page so a process that dies mid-run picks up
// after the last fully-processed page instead of re-listing providers from the
// start. Cursor is the highest media_files id the pass has finished.
type VirtualIdentityBackfillResume struct {
	Cursor int `json:"cursor,omitempty"`
}

// VirtualIdentityBackfillResult summarizes one completed pass.
type VirtualIdentityBackfillResult struct {
	Cursor        int            `json:"cursor"`
	GroupsScanned int            `json:"groups_scanned"`
	GroupsFailed  int            `json:"groups_failed"`
	RowsTotal     int            `json:"rows_total"`
	RowsScanned   int            `json:"rows_scanned"`
	RowsMatched   int            `json:"rows_matched"`
	RowsUnmatched int            `json:"rows_unmatched"`
	RowsSkipped   int            `json:"rows_skipped"`
	Tiers         map[string]int `json:"tiers,omitempty"`
}

// VirtualIdentityBackfillExecutor runs one backfill page loop. It is
// implemented by the HTTP layer, which owns the provider listing and the
// catalog write, and injected into the runner, mirroring the item-, library-
// and virtual-refresh executors.
//
// progress reports the rows scanned so far, the total legacy rows at the start,
// the durable cursor after the last fully-processed page, and a message. The
// runner persists the cursor so a requeued claim resumes from it.
type VirtualIdentityBackfillExecutor interface {
	Execute(ctx context.Context, req VirtualIdentityBackfillRequest, resume VirtualIdentityBackfillResume, progress func(current, total, cursor int, message string)) (*VirtualIdentityBackfillResult, error)
}

// SetVirtualIdentityBackfillExecutor installs the backfill executor. It is
// optional: a runner without one fails the job with a clear message instead of
// hanging.
func (r *Runner) SetVirtualIdentityBackfillExecutor(executor VirtualIdentityBackfillExecutor) {
	if r != nil {
		r.virtualIdentityBackfill = executor
	}
}
