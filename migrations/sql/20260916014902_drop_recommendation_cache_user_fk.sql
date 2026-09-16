-- +goose Up
-- +goose StatementBegin
-- The FK added in 20260912231023_user_fk_integrity rejects the reserved global
-- cache owner (user_id=0, profile_id='__global__'), which predates the FK and
-- has no matching users row. Every global cache upsert fails deterministically,
-- leaving the global sections empty. Drop only this constraint; the remaining
-- integrity fixes from that migration are untouched.
ALTER TABLE public.recommendation_cache
    DROP CONSTRAINT IF EXISTS recommendation_cache_user_id_fkey;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Restores the FK. Global rows (user_id=0) must be deleted first or the ADD
-- CONSTRAINT will fail validation; the next worker cycle repopulates them,
-- and they will then fail again until the sentinel ownership model is fixed.
DELETE FROM public.recommendation_cache WHERE user_id = 0;
ALTER TABLE public.recommendation_cache
    ADD CONSTRAINT recommendation_cache_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
-- +goose StatementEnd
