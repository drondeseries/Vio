package userstore

import (
	"context"
	"errors"
	"time"
)

// ErrNextUpStateInvalidLimit is returned by ListNextUpStatePage for a
// non-positive page limit. Next Up state paging rejects an invalid limit rather
// than silently substituting a default: a caller asking for zero rows has a
// bug, and a silent clamp would hide it behind a page that looks exhausted.
var ErrNextUpStateInvalidLimit = errors.New("userstore: next up state page limit must be positive")

// NextUpStateCursor is a keyset position in the (updated_at DESC,
// media_item_id DESC) order. The pair is a total order because the progress
// key is (profile, media_item_id) and updated_at ties are broken by
// media_item_id, so no row is skipped or repeated across pages.
type NextUpStateCursor struct {
	UpdatedAt   time.Time
	MediaItemID string
}

// NextUpStateEntry is one progress row contributing to Next Up.
type NextUpStateEntry struct {
	MediaItemID string
	Completed   bool
	Position    float64
	UpdatedAt   time.Time
}

// NextUpStatePage is one provider page; Next is nil when Exhausted.
type NextUpStatePage struct {
	Entries   []NextUpStateEntry
	Next      *NextUpStateCursor
	Exhausted bool
}

// NextUpStateStore is implemented by stores that can page Next Up state.
//
// It is an optional capability (not part of UserStore) so stores and test
// doubles that do not back Next Up state are unaffected; consumers type-assert
// it and fall back to their previous path when it is absent.
//
// # Semantics
//
// Scope: the store's account (Postgres user_id; the per-user SQLite file is
// already account-scoped) AND the given profileID. No other profile's rows are
// ever returned.
//
// Rows: progress rows with completed = TRUE OR position_seconds > 0. A row
// hidden by hidden_history_items/user_history_hidden_items (hidden_before >=
// updated_at, inclusive) is excluded from the page.
//
// Order: updated_at DESC, media_item_id DESC. Cursor: strictly after the
// cursor, i.e. (updated_at, media_item_id) < (cursor.updated_at,
// cursor.media_item_id); a nil cursor starts at the newest row.
//
// Exhaustion: explicit. Implementations read limit+1 rows so Exhausted is set
// exactly when no further rows exist; otherwise Next is the last returned
// entry's keyset. A non-exhausted page is never empty.
//
// Cancellation: returns the context error rather than a silently truncated
// page. limit <= 0 is rejected with ErrNextUpStateInvalidLimit.
type NextUpStateStore interface {
	ListNextUpStatePage(ctx context.Context, profileID string, cursor *NextUpStateCursor, limit int) (NextUpStatePage, error)

	// ListNextUpStateForItems returns every state row (completed or in-progress)
	// for the given content ids, in the store's account/profile scope, excluding
	// hidden activity (hidden_before >= updated_at, inclusive). Order is
	// unspecified.
	//
	// It is the exact counterpart to ListNextUpStatePage. A caller that has
	// already resolved which catalog items matter uses it instead of depending
	// on how far a recency-ordered page walk happened to get: the page walk can
	// stop early, but a state row older than the stop is still state.
	//
	// Empty mediaItemIDs returns an empty result without querying; cancellation
	// returns the context error.
	ListNextUpStateForItems(ctx context.Context, profileID string, mediaItemIDs []string) ([]NextUpStateEntry, error)
}

// NextUpStatePageFromEntries builds a page from the rows an implementation read
// with a limit+1 bound. It centralizes the exhaustion rule so both backends
// agree: more than limit rows means there is a further page, and the last
// returned entry's keyset becomes Next. Otherwise the page is Exhausted and
// Next is nil, which also guarantees an empty page reports Exhausted rather
// than an empty non-exhausted page.
func NextUpStatePageFromEntries(entries []NextUpStateEntry, limit int) NextUpStatePage {
	if len(entries) <= limit {
		return NextUpStatePage{Entries: entries, Exhausted: true}
	}
	trimmed := entries[:limit:limit]
	last := trimmed[limit-1]
	return NextUpStatePage{
		Entries: trimmed,
		Next: &NextUpStateCursor{
			UpdatedAt:   last.UpdatedAt,
			MediaItemID: last.MediaItemID,
		},
	}
}
