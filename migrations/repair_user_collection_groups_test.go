package migrations

import (
	"fmt"
	"testing"
	"time"
)

const repairUserCollectionGroupsMigration = "20260918080712_repair_user_collection_groups"

func TestRepairUserCollectionGroupsPostgres(t *testing.T) {
	tx, schema := adminMigrationFixture(t)
	migrationExec(t, tx, `
CREATE TABLE media_folders (id bigint PRIMARY KEY);
CREATE TABLE library_collection_groups (
    library_id bigint NOT NULL REFERENCES media_folders(id) ON DELETE CASCADE,
    label text NOT NULL,
    title text NOT NULL,
    sort_order integer NOT NULL DEFAULT 0,
    id text NOT NULL UNIQUE,
    name text NOT NULL,
    slug text NOT NULL,
    kind text NOT NULL,
    default_sort_mode text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (library_id, label),
    UNIQUE (library_id, slug)
);
CREATE UNIQUE INDEX idx_library_collection_groups_user_unique
    ON library_collection_groups (library_id)
    WHERE kind = 'user_collections';
INSERT INTO media_folders VALUES (1), (2);
INSERT INTO library_collection_groups (
    library_id, label, title, sort_order, id, name, slug, kind, default_sort_mode, created_at, updated_at
) VALUES (2, 'user-collections', 'My collections', 9998, 'lcg_user_2', 'My collections', 'user-collections', 'user_collections', 'manual', '2020-01-01', '2020-01-01');`)

	up := adminMigrationSQL(t, repairUserCollectionGroupsMigration, schema, false)
	migrationExec(t, tx, up)
	migrationExec(t, tx, up)

	for _, libraryID := range []int{1, 2} {
		var count int
		if err := tx.QueryRow(t.Context(), `
			SELECT count(*)
			FROM library_collection_groups
			WHERE library_id = $1 AND kind = 'user_collections'`, libraryID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("library %d user collection groups = %d, want 1", libraryID, count)
		}
		var id, label, title, name, slug, kind, sortMode string
		var sortOrder int
		if err := tx.QueryRow(t.Context(), `
			SELECT id, label, title, name, slug, kind, default_sort_mode, sort_order
			FROM library_collection_groups
			WHERE library_id = $1 AND kind = 'user_collections'`, libraryID).Scan(
			&id, &label, &title, &name, &slug, &kind, &sortMode, &sortOrder,
		); err != nil {
			t.Fatal(err)
		}
		if id != fmt.Sprintf("lcg_user_%d", libraryID) || label != "user-collections" || title != "My collections" ||
			name != "My collections" || slug != "user-collections" || kind != "user_collections" ||
			sortMode != "manual" || sortOrder != 9998 {
			t.Fatalf("library %d canonical group = id=%q label=%q title=%q name=%q slug=%q kind=%q mode=%q order=%d",
				libraryID, id, label, title, name, slug, kind, sortMode, sortOrder)
		}
	}

	var createdAt, updatedAt time.Time
	if err := tx.QueryRow(t.Context(), `
		SELECT created_at, updated_at
		FROM library_collection_groups
		WHERE library_id = 2 AND kind = 'user_collections'`).Scan(&createdAt, &updatedAt); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if !createdAt.Equal(want) || !updatedAt.Equal(want) {
		t.Fatalf("existing valid group timestamps changed: created=%s updated=%s", createdAt, updatedAt)
	}
}
