package userstore

import "context"

// UserStoreProvider returns a UserStore scoped to a specific user.
// For SQLite, this returns a store wrapping the per-user SQLite DB from the pool.
// For Postgres, this returns a store scoped to the user_id in shared tables.
type UserStoreProvider interface {
	ForUser(ctx context.Context, userID int) (UserStore, error)
	Close() error
}
