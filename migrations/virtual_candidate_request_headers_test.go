package migrations

import (
	"strings"
	"testing"
)

// TestVirtualCandidateRequestHeadersMigrationShape pins the schema shape of the
// request-header persistence migration without a database. The column is
// nullable and purely additive; the down migration must drop exactly what the
// up added.
func TestVirtualCandidateRequestHeadersMigrationShape(t *testing.T) {
	const file = "20260920184223_add_virtual_candidate_request_headers.sql"
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
	if !strings.Contains(up, "ADD COLUMN IF NOT EXISTS provider_request_headers jsonb") {
		t.Fatal("up does not add the nullable jsonb provider_request_headers column")
	}
	if !strings.Contains(down, "DROP COLUMN IF EXISTS provider_request_headers") {
		t.Fatal("down does not drop provider_request_headers")
	}
	if strings.Contains(up, "NOT NULL") {
		t.Fatal("migration adds a NOT NULL column; it must be nullable")
	}
	for _, forbidden := range []string{"DROP COLUMN", "RENAME COLUMN", "ALTER COLUMN"} {
		if strings.Contains(up, forbidden) {
			t.Errorf("up contains %q; the migration must only add a column", forbidden)
		}
	}
}
