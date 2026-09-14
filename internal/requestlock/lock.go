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

// LockPurgeBarrierExclusive acquires an exclusive purge barrier. Used by the
// purge sweep so all participating writers wait until the purge commits or
// rolls back. Acquired before any domain advisory locks or row locks.
func LockPurgeBarrierExclusive(ctx context.Context, tx pgx.Tx) error {
	const (
		class int32 = 0x53494C4F // "SILO"
		id    int32 = 1
	)
	// Legacy hashtext compatibility: coordinate with old binaries that use
	// LockVirtual("virtual-purge") during rolling deploys.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "virtual-purge"); err != nil {
		return fmt.Errorf("acquire legacy purge barrier: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('catalog:virtual:' || $1))`, "virtual-purge"); err != nil {
		return fmt.Errorf("acquire purge barrier: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::int4, $2::int4)`, class, id); err != nil {
		return fmt.Errorf("acquire purge barrier (v2): %w", err)
	}
	return nil
}

// LockPurgeBarrierShared acquires a shared purge barrier. Used by participating
// writers (upsert, materialization, registration, collection replacement) so
// they can run concurrently but not during a purge. Acquired before any domain
// advisory locks or row locks.
func LockPurgeBarrierShared(ctx context.Context, tx pgx.Tx) error {
	const (
		class int32 = 0x53494C4F // "SILO"
		id    int32 = 1
	)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtext($1))`, "virtual-purge"); err != nil {
		return fmt.Errorf("acquire legacy purge barrier (shared): %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtext('catalog:virtual:' || $1))`, "virtual-purge"); err != nil {
		return fmt.Errorf("acquire purge barrier (shared): %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared($1::int4, $2::int4)`, class, id); err != nil {
		return fmt.Errorf("acquire purge barrier (shared, v2): %w", err)
	}
	return nil
}
