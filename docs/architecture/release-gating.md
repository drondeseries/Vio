# Verified Release Gating: Eligibility and Lock Protocol

Verified release overrides (`verified_release_override_history`, admin API in
`docs/admin-api.md`) gate virtual materialization: whether new virtual files
may be created for a movie or episode. They never delete physical files and
never revoke existing playback by themselves.

## Eligibility precedence

Every decision point applies the same order:

1. Resolve **all known aliases** for the item: incoming external IDs plus the
   stored scalar (`tmdb_id`, `tvdb_id`, `imdb_id`) and provider-table
   (`media_item_provider_ids`) aliases. Alias-read failures abort the
   decision; an incomplete set is never treated as complete. Episode gating
   uses the registration's incoming series aliases even when metadata
   ownership prevents persisting them.
2. If **any alias carries an active override**, it decides: a future instant
   blocks new virtual files, a past instant permits. One future override
   blocks regardless of past overrides on other aliases.
3. Only with **no active override** do fallbacks apply: local possession
   (local metadata or physical files, which legacy rows often have instead
   of dates), then provider/air-date evidence.
4. Physical files are preserved independently of the verdict for new files.

Decisions are made from captured override entries at a single evaluation
time (`decideReleaseOverrides`), not from repeated live reads. The provider
evidence helper performs no override evaluation of its own.

Snapshots record alias membership as well as revisions. If the current alias
set differs from the prepared snapshot, acceptance and registration return a
retryable revision conflict instead of deciding on a stale union; removed
aliases never retain authority.

## Transaction lock protocol

All locks below are transaction-scoped. The required global order is:

1. Content lock (`catalog:release:content:<contentID>`).
2. Collection, canonical-item, and row locks.
3. Per-identity shared/exclusive advisory locks.
4. Refresh-debt row writes.

Alias writers (provider-ID attach/replace, metadata item persistence,
registration that owns item metadata) hold the content lock **exclusively**,
acquired before any other locked resource in their transaction. Readers
(registration preparation, collection preparation/acceptance, episode
materialization) hold it **shared**, likewise first. Override mutation takes
per-identity exclusive locks before its debt write, matching the readers'
locks-before-debt order.

Multi-item acceptance defers every refresh-debt write until all members'
release locks and eligibility decisions complete, then emits debt rows in
sorted content-ID order — no new release identity is acquired after the
first debt write in an acceptance transaction.

Reconciliation discovery, materialization, and stale-file cleanup share one
rule: an active future override beats local evidence on every path, and
local fallback applies only where no active override decides. A series whose
episodes are all override-blocked converges to no new files rather than
churning.
