package userstore

import "context"

// UserStoreProvider returns a UserStore scoped to a specific user.
// For SQLite, this returns a store wrapping the per-user SQLite DB from the pool.
// For Postgres, this returns a store scoped to the user_id in shared tables.
type UserStoreProvider interface {
	ForUser(ctx context.Context, userID int) (UserStore, error)
	Close() error
}

// PostgresAnchorStore is an optional marker implemented by stores whose
// progress rows live in the same Postgres tables the catalog can query
// directly. Callers that derive episode anchors from progress rows use it to
// decide whether a single Postgres query can join user_watch_progress (true) or
// whether progress must first be read through the store and fed into the query
// as a snapshot (false, or the interface unimplemented — e.g. SQLite).
type PostgresAnchorStore interface {
	// NextUpAnchorsBackedByPostgres reports whether watch-progress rows for
	// this store are readable by the catalog's Postgres connection.
	NextUpAnchorsBackedByPostgres() bool
}
