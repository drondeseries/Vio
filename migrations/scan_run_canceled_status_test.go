package migrations

import (
	"strings"
	"testing"
)

// TestScanRunCanceledStatusMigrationPostgres proves migration
// 20260924140423_allow_canceled_scan_runs widens scan_runs_status_check to accept
// both the 'cancelled' spelling 085 constrained and the 'canceled' spelling
// scanqueue persists, while still rejecting any other value.
func TestScanRunCanceledStatusMigrationPostgres(t *testing.T) {
	tx, schema := adminMigrationFixture(t)
	// 085 creates the table with the original single-spelling constraint; it
	// references media_folders, so stub that dependency.
	migrationExec(t, tx, `CREATE TABLE media_folders (id integer PRIMARY KEY);`)
	migrationExec(t, tx, `INSERT INTO media_folders (id) VALUES (1);`)
	migrationExec(t, tx, adminMigrationSQL(t, "085_scan_runs", schema, false))

	// An accepted row seeded before the widening proves the migration does not
	// rewrite existing runs.
	migrationExec(t, tx, `INSERT INTO scan_runs (id, media_folder_id, mode, status) VALUES ('seed', 1, 'library', 'accepted');`)
	requireMigrationSQLState(t, tx, `UPDATE scan_runs SET status='canceled' WHERE id='seed'`, "23514")

	const repair = "20260924140423_allow_canceled_scan_runs"
	migrationExec(t, tx, adminMigrationSQL(t, repair, schema, false))

	// Both spellings are now accepted on the same row.
	migrationExec(t, tx, `UPDATE scan_runs SET status='canceled' WHERE id='seed';`)
	var status string
	if err := tx.QueryRow(t.Context(), `SELECT status FROM scan_runs WHERE id='seed'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "canceled" {
		t.Fatalf("status = %q, want canceled", status)
	}
	migrationExec(t, tx, `INSERT INTO scan_runs (id, media_folder_id, mode, status) VALUES ('legacy', 1, 'library', 'cancelled');`)

	// An unknown value is still rejected.
	requireMigrationSQLState(t, tx, `INSERT INTO scan_runs (id, media_folder_id, mode, status) VALUES ('bad', 1, 'library', 'invalid')`, "23514")
}

// TestScanRunCanceledStatusMigrationShape guards the migration's statement
// shape: the replacement constraint is added NOT VALID then validated, matching
// the historyimport precedent so a pre-existing bad row cannot roll the widening
// back with the validation step.
func TestScanRunCanceledStatusMigrationShape(t *testing.T) {
	data, err := FS.ReadFile("sql/20260924140423_allow_canceled_scan_runs.sql")
	if err != nil {
		t.Fatal(err)
	}
	up := strings.SplitN(string(data), "-- +goose Down", 2)[0]
	flat := strings.Join(strings.Fields(up), " ")

	add := "ADD CONSTRAINT scan_runs_status_check CHECK (status = ANY (ARRAY[ 'accepted'::text, 'running'::text, 'completed'::text, 'failed'::text, 'cancelled'::text, 'canceled'::text ])) NOT VALID;"
	addAt, validateAt := strings.Index(flat, add), strings.Index(flat, "VALIDATE CONSTRAINT scan_runs_status_check;")
	if addAt < 0 {
		t.Fatalf("migration does not add the widened constraint NOT VALID")
	}
	if validateAt < 0 {
		t.Fatalf("migration does not validate the widened constraint")
	}
	if addAt > validateAt {
		t.Fatal("constraint must be added before it is validated")
	}
	if !strings.Contains(flat, "DROP CONSTRAINT IF EXISTS scan_runs_status_check") {
		t.Fatal("migration does not drop the old constraint first")
	}
}
