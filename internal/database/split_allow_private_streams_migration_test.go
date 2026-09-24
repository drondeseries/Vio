package database

import (
	"os"
	"strings"
	"testing"
)

func TestSplitAllowPrivateStreamsMigration(t *testing.T) {
	migration, err := os.ReadFile("../../migrations/sql/20260924205925_split_allow_private_streams.sql")
	if err != nil {
		t.Fatalf("read split migration: %v", err)
	}
	normalized := strings.Join(strings.Fields(string(migration)), " ")
	for _, fragment := range []string{
		"INSERT INTO server_settings (key, value)",
		"SELECT 'virtual_library.allow_private_streams', value",
		"WHERE key = 'virtual_library.allow_insecure_http'",
		"ON CONFLICT (key) DO NOTHING",
		"DELETE FROM server_settings WHERE key = 'virtual_library.allow_private_streams'",
	} {
		if !strings.Contains(normalized, fragment) {
			t.Fatalf("split migration missing %q:\n%s", fragment, migration)
		}
	}

	defaults, err := os.ReadFile("../../internal/config/admin_settings.go")
	if err != nil {
		t.Fatalf("read admin settings defaults: %v", err)
	}
	if !strings.Contains(string(defaults), `"virtual_library.allow_private_streams":`) {
		t.Fatal("admin defaults do not seed virtual_library.allow_private_streams")
	}

	loader, err := os.ReadFile("../../internal/virtuallibrary/service.go")
	if err != nil {
		t.Fatalf("read virtual library service: %v", err)
	}
	if !strings.Contains(string(loader), `"virtual_library.allow_private_streams"`) {
		t.Fatal("virtual library config does not load virtual_library.allow_private_streams")
	}
}
