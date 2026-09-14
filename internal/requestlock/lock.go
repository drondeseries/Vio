package requestlock

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// LockItem serializes catalog item identity work. It acquires both the
// historical unprefixed advisory lock and the namespaced lock so old and new
// workers coordinate during rollout. Callers must hold no conflicting row
// locks before acquiring advisory locks.
func LockItem(ctx context.Context, tx pgx.Tx, key string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, key); err != nil {
		return fmt.Errorf("acquire legacy catalog item lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('catalog:item:' || $1))`, key); err != nil {
		return fmt.Errorf("acquire catalog item lock: %w", err)
	}
	return nil
}

// LockVirtual serializes virtual-media source/installation/URI work with the
// same dual-lock rollout compatibility as LockItem.
func LockVirtual(ctx context.Context, tx pgx.Tx, key string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, key); err != nil {
		return fmt.Errorf("acquire legacy catalog virtual lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('catalog:virtual:' || $1))`, key); err != nil {
		return fmt.Errorf("acquire catalog virtual lock: %w", err)
	}
	return nil
}
