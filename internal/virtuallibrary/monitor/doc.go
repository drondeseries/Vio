// Package monitor runs the virtual-library monitoring queue: items requested
// via Fulfill are persisted to a JSON queue file and re-evaluated on each
// Run until their release metadata and indexer state mark them ready, at
// which point they are registered with the catalog registrar.
//
// # Behavioral review (Oracle Phase 3)
//
// Line-reviewed: monitor (monitor.go, types.go), prowlarr, altmount.
// Build success was not treated as behavioral evidence; the findings below
// come from reading the control flow.
//
// Run is bounded scheduled work, not a daemon. The package spawns no
// goroutines, owns no ticker/cron/sleep, and performs no background work
// (verified: no `go` statements, no time.Ticker/Sleep in this package;
// Configure only loads the queue file). Each Run snapshots the item map
// under mutex, sorts it by key, and iterates it once, starting at the key
// after the persisted cursor and wrapping to the front. There is no loop
// and no requeue timer; repetition is the caller's job.
//
// A pass is resumable rather than all-or-nothing. The cursor (the key of
// the last item whose evaluation completed) is persisted in the queue file
// and cleared only after a full pass, so a pass cut off by its deadline
// records progress and the next pass continues from there. Exhausting the
// deadline is normal operation: the pass logs at Info, returns a successful
// response, and leaves the remainder for the next call. Because the cursor
// only moves forward through the sorted keys and only wraps once per cycle,
// a poison item is attempted at most once per cycle and cannot starve items
// behind it.
//
// Cancellation is honored. Run checks ctx.Err() at the top of each item
// iteration and stops cleanly when the pass deadline fires. Each item's
// evaluate/register work (and each source reconciliation) additionally runs
// under a per-item timeout (monitorPerItemTimeout), so one hung provider
// cannot consume the whole budget; a per-item timeout is a deferral, not a
// pass failure, and its source is barred from reconciliation that pass. All
// outbound HTTP (Cinemeta, TVMaze, TMDB, Prowlarr, AltMount) is built with
// NewRequestWithContext, the metadata client has a 20s timeout, and
// TestConnection imposes its own 8s deadline. Run still relies on the
// caller's deadline for the overall pass budget (the code assumes the ~2
// minute scheduled-task host deadline); a caller that invokes Run without a
// deadline invites an unbounded wall clock when many items each sequentially
// fetch metadata.
//
// Retries are across invocations, not within a call. No retry loop or
// backoff exists in Run, evaluate, the metadata fetchers, or
// RefreshIfStale: a per-item failure (evaluate error, register error,
// stale-refresh error) is logged, counted as pending, and left queued for
// a later Run. An item's source is reconciled only after every item in it
// was evaluated successfully this pass, so a persistently failing provider
// never turns an incomplete keep set into destructive reconciliation. On top
// of the per-source condition, reconciliation now requires full-cycle
// evidence: the pass must have begun at the first queue item, reached the
// end, and not been cut short by its budget or context. The monitor attests
// that with ReconcileEvidence (FullCycle plus the source and queue counts)
// and the catalog refuses destructive reconciliation without it, so a
// truncated or replaced queue cannot authorise deleting live media. Stale
// Prowlarr/AltMount refresh failures are warn-and-continue, never fatal.
//
// Duplicate submissions converge by key but with gaps. Queue keys are
// deterministic (type:streamID), load rejects duplicate keys, and remember()
// overwrites by key, so double-Fulfill of the same request is idempotent at
// the queue level. Run merges registrar ListVirtual results into a
// key-keyed map, deduping queue-vs-catalog overlap. The in-memory
// `registered` set suppresses re-registration for movies
// (isRegistered skip) but series re-register on every Run while ready
// (upsert semantics assumed of the registrar), and concurrent overlapping
// Fulfill/Run calls have no mutual exclusion beyond the per-structure
// mutexes, so a double-submit racing a Run can register twice.
//
// Restart state is partially persisted. The queue (items, episodes,
// readiness) is durable: atomic tmp-file-plus-rename writes, 32MB / 10000
// item / 10000 episode caps, sorted deterministic output. Prowlarr and
// AltMount caches have their own persisted index/state files. The
// `registered` set, however, is memory-only: after a restart it starts
// empty and is repopulated only by a Run whose registrar implements
// ListVirtual. A restart followed by Fulfill/Run against a non-listing
// registrar re-registers already-registered movies once (benign if the
// registrar upserts, duplicate work otherwise).
//
// Multi-node ownership is guarded at the Run boundary by a PostgreSQL session
// advisory lock in monitor.go. The lock is held on a dedicated pgxpool
// connection for the entire pass and released safely on return; a crashed
// process releases it when PostgreSQL closes the session. An in-process runMu
// also prevents concurrent Run calls on one monitor. Queue/index/state files
// remain local disk paths, so all replicas must share the database and the
// configured monitor state must be treated as node-local input. The database
// lock prevents concurrent catalog side effects, but it does not merge or
// replicate separate local queue files.
//
// Verdict: Run is safe to call on a timer when every replica shares the
// database, invocations are bounded by a caller-supplied deadline, and the
// monitor state file is managed as node-local input.
package monitor
