package testdb

import (
	"bytes"
	"log"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRewriteDSN(t *testing.T) {
	t.Setenv("PGDATABASE", "environment_test")
	for _, dsn := range []string{
		"postgres://tester:secret@localhost:5433/silo_test?sslmode=disable&application_name=testdb",
		"postgresql://tester:p%40ss%2Fword@localhost/silo%5Ftest?sslmode=disable",
		"postgres://tester:secret@localhost/path_test?dbname=query_test&dbname=other_test&sslmode=disable",
		"postgres://tester@localhost?sslmode=disable",
		"host=localhost port=5433 user=tester password='a b\\'c\\\\d' dbname=silo_test sslmode=disable",
		"host=localhost dbname=first_test dbname='second test' application_name='test runner'",
		"host=localhost user=tester",
	} {
		t.Run("rewrite", func(t *testing.T) {
			before, err := pgxpool.ParseConfig(dsn)
			if err != nil {
				t.Fatal("invalid fixture")
			}
			for _, target := range []string{"silo_test_scope_suite_0123456789abcdef", "test /?#%'\\name"} {
				rewritten, err := rewriteDSN(dsn, target)
				if err != nil {
					t.Fatal(err)
				}
				after, err := pgxpool.ParseConfig(rewritten)
				if err != nil {
					t.Fatal("rewritten DSN did not parse")
				}
				if after.ConnConfig.Database != target {
					t.Fatal("wrong database target")
				}
				a, b := before.ConnConfig, after.ConnConfig
				if a.Host != b.Host || a.Port != b.Port || a.User != b.User || a.Password != b.Password {
					t.Fatal("connection settings changed")
				}
				if (a.TLSConfig == nil) != (b.TLSConfig == nil) || a.RuntimeParams["application_name"] != b.RuntimeParams["application_name"] {
					t.Fatal("connection options changed")
				}
			}
		})
	}
}

func TestRewriteDSNRejectsInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		dsn  string
		name string
	}{
		{"postgres://tester:secret@localhost/%zz", "silo_test"},
		{"host=localhost password='secret", "silo_test"},
		{"postgres://localhost/silo_test?application_name=%zz", "silo_test"},
		{"host=localhost dbname=silo_test", ""},
		{"host=localhost dbname=silo_test", strings.Repeat("a", 64)},
		{"host=localhost dbname=silo_test", "test\x00other"},
	} {
		if rewritten, err := rewriteDSN(tc.dsn, tc.name); err == nil || rewritten != "" {
			t.Fatal("unsafe rewrite accepted")
		} else if strings.Contains(err.Error(), "secret") {
			t.Fatal("error exposed credentials")
		}
	}
}

func TestDisposableName(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z0-9_]+_[0-9a-f]{32}$`)
	seen := make(map[string]bool)
	for _, base := range []string{"silo_test", strings.Repeat("long_test", 30), "测试_test\";DROP DATABASE postgres;"} {
		for range 100 {
			name, err := disposableName(base, strings.Repeat("A / unusual 标签", 20))
			if err != nil {
				t.Fatal(err)
			}
			if len(name) > 63 || !valid.MatchString(name) || name == base {
				t.Fatalf("invalid disposable name: %q", name)
			}
			if seen[name] {
				t.Fatal("duplicate disposable name")
			}
			seen[name] = true
		}
	}
}

func TestIsDisposableBaseName(t *testing.T) {
	for _, name := range []string{"silo_test", "TEST", "silo_purge"} {
		if !isDisposableBaseName(name) {
			t.Fatalf("rejected test database %q", name)
		}
	}
	for _, name := range []string{"", "postgres", "silo", "production"} {
		if isDisposableBaseName(name) {
			t.Fatalf("accepted non-test database %q", name)
		}
	}
}

func TestSetupPackageFailsClosedBeforeConnecting(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	for _, dsn := range []string{
		"postgres://tester:credential_marker@localhost/%zz",
		"host=localhost password='credential_marker",
		"postgres://tester:credential_marker@localhost/production",
		"host=localhost user=tester password=credential_marker dbname=postgres",
	} {
		t.Setenv("SILO_TEST_DATABASE_URL", dsn)
		if code := SetupPackage(nil, "closed"); code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
	}
	if strings.Contains(output.String(), "credential_marker") {
		t.Fatal("logs exposed credentials")
	}
}
