package migrations

import (
	"strings"
	"testing"
)

// TestVirtualCandidateResolutionMigrationShape pins the schema shape of the
// local-ready virtual candidate persistence migration without a database. The
// columns are nullable and purely additive to the neutral `?result=` listing
// identity; the down migration must drop exactly what the up added.
func TestVirtualCandidateResolutionMigrationShape(t *testing.T) {
	const file = "20260920171029_virtual_candidate_resolution.sql"
	raw, err := FS.ReadFile("sql/" + file)
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)

	up, down, ok := strings.Cut(sql, "-- +goose Down")
	if !ok {
		t.Fatal("migration has no down section")
	}
	if !strings.Contains(up, "ALTER TABLE public.media_files") {
		t.Fatal("up does not alter media_files")
	}

	columns := []struct {
		name string
		typ  string
	}{
		{"resolved_url", "text"},
		{"resolved_url_expires_at", "timestamptz"},
		{"provider_video_hash", "text"},
		{"provider_guid", "text"},
		{"provider_release_name", "text"},
		{"provider_release_size", "bigint"},
	}
	for _, column := range columns {
		want := "ADD COLUMN IF NOT EXISTS " + column.name + " " + column.typ
		if !strings.Contains(up, want) {
			t.Errorf("up is missing %q", want)
		}
		if !strings.Contains(down, "DROP COLUMN IF EXISTS "+column.name) {
			t.Errorf("down is missing a drop for %s", column.name)
		}
	}
	// Every column is nullable: no NOT NULL or DEFAULT may be introduced.
	if strings.Contains(up, "NOT NULL") {
		t.Fatal("migration adds a NOT NULL column; the columns must be nullable")
	}
	// The listing identity is untouched.
	for _, forbidden := range []string{"DROP COLUMN", "RENAME COLUMN", "ALTER COLUMN"} {
		if strings.Contains(up, forbidden) {
			t.Errorf("up contains %q; the migration must only add columns", forbidden)
		}
	}
}
