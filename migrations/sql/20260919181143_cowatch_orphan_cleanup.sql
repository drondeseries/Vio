-- +goose Up
-- +goose StatementBegin
-- item_cowatch has no foreign key to media_items (unlike media_item_embeddings
-- since 044). Co-watch rows for an item deleted or relinked after the matrix
-- was computed therefore survived and leaked through
-- GET /api/v2/recommendations/similar/{item_id} as ids that resolve to 404.
--
-- Delete the stranded rows. This is deliberately cleanup only: adding the
-- matching foreign keys is a follow-up that first needs the co-watch writer
-- hardened so a pair built from a stale watcher snapshot cannot violate the
-- constraint and abort the matrix build. Until then the read path joins
-- media_items and the engine drops unresolved ids, so a newly orphaned row
-- still cannot reach a client.
DELETE FROM public.item_cowatch c
WHERE NOT EXISTS (SELECT 1 FROM public.media_items mi WHERE mi.content_id = c.item_id)
   OR NOT EXISTS (SELECT 1 FROM public.media_items mi WHERE mi.content_id = c.similar_item_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Deleted co-watch rows are derived data with no source of truth, so there is
-- nothing meaningful to restore.
SELECT 1;
-- +goose StatementEnd
