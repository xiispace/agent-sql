package resolve

import (
	"context"
	"strings"
	"testing"

	"github.com/shhac/agent-sql/internal/config"
	"github.com/shhac/agent-sql/internal/errors"
)

// TestDatabaseOverrideRejectedForFileDrivers covers -D on sqlite and duckdb,
// for ad-hoc paths, ad-hoc URLs and stored connections alike.
func TestDatabaseOverrideRejectedForFileDrivers(t *testing.T) {
	dir := t.TempDir()
	config.SetConfigDir(dir)
	cfg := config.Read()
	cfg.Connections = map[string]config.Connection{
		"lite": {Driver: "sqlite", Path: dir + "/x.db"},
	}
	if err := config.Write(cfg); err != nil {
		t.Fatal(err)
	}
	for _, conn := range []string{dir + "/x.db", "sqlite://" + dir + "/x.db", dir + "/x.duckdb", "lite"} {
		_, err := Resolve(context.Background(), Opts{Connection: conn, Database: "other"})
		var qerr *errors.QueryError
		if !errors.As(err, &qerr) {
			t.Fatalf("%s: expected *QueryError, got %v", conn, err)
		}
		if qerr.FixableBy != errors.FixableByAgent || !strings.Contains(qerr.Message, "-D is not supported") {
			t.Errorf("%s: err = %+v", conn, qerr)
		}
	}
}

func TestWithURLDatabase(t *testing.T) {
	cases := map[string]string{
		"mysql://u:p%40ss@h:3306/old?parseTime=true": "mysql://u:p%40ss@h:3306/new?parseTime=true",
		"postgres://u@h":          "postgres://u@h/new",
		"sqlserver://u:p@h:1433/": "sqlserver://u:p@h:1433/new",
	}
	for in, want := range cases {
		got, err := withURLDatabase(in, "new")
		if err != nil || got != want {
			t.Errorf("withURLDatabase(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// TestDatabaseOverrideDoesNotMutateConfig checks -D is per-process: the
// stored connection's database is untouched after a resolve that overrode it.
func TestDatabaseOverrideDoesNotMutateConfig(t *testing.T) {
	dir := t.TempDir()
	config.SetConfigDir(dir)
	cfg := config.Read()
	cfg.Connections = map[string]config.Connection{
		// No credential: resolution fails after the override, before any
		// network I/O, which is all this test needs.
		"pgx": {Driver: "pg", Host: "127.0.0.1", Port: 1, Database: "orig"},
	}
	if err := config.Write(cfg); err != nil {
		t.Fatal(err)
	}
	_, _ = Resolve(context.Background(), Opts{Connection: "pgx", Database: "other"})
	if got := config.GetConnection("pgx").Database; got != "orig" {
		t.Errorf("stored database = %q, want orig", got)
	}
}

func TestURLErrorsHidePassword(t *testing.T) {
	for _, bad := range []string{
		"mysql://root:s3cret@host:3306/%zz",
		"postgres://u:s3cret/x@host:5432/db%",
	} {
		_, err1 := parseGenericURL(bad)
		_, err2 := withURLDatabase(bad, "shop")
		for _, err := range []error{err1, err2} {
			if err == nil {
				t.Fatalf("%q parsed", bad)
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Errorf("error leaks the password: %v", err)
			}
		}
	}
}
