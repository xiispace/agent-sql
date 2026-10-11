// Package resolve maps connection aliases, URLs, and file paths to
// concrete driver connections. Separated from the driver package to
// avoid import cycles.
//
// File layout:
//   - resolve.go: top-level Resolve dispatch (alias / ad-hoc / config).
//   - policy.go: write permission, credential validation helpers.
//   - urlparse.go: generic host:port URL parser used by ad-hoc paths.
//   - connect_pg.go / connect_mysql.go / connect_snowflake.go /
//     connect_mssql.go / connect_file.go: per-driver Connect wiring.
//
// Adding a new driver: add an entry to driver.Registry and a new
// connect_<driver>.go here.
package resolve

import (
	"context"
	stderrors "errors"
	"fmt"
	"os"
	"strings"

	"github.com/shhac/agent-sql/internal/config"
	"github.com/shhac/agent-sql/internal/driver"
	"github.com/shhac/agent-sql/internal/driver/cockroachdb"
	"github.com/shhac/agent-sql/internal/driver/duckdb"
	"github.com/shhac/agent-sql/internal/driver/pg"
	"github.com/shhac/agent-sql/internal/driver/sqlite"
	"github.com/shhac/agent-sql/internal/errors"
)

// Opts configures driver resolution.
type Opts struct {
	Connection string
	Write      bool
	Timeout    int
	// Database overrides the connection's database (-D). It applies to stored
	// aliases and ad-hoc URLs alike; file-based drivers reject it.
	Database string
	// FallbackDatabase retries once on the driver's default database
	// (driver.Registry DefaultDB: postgres, defaultdb) when the connection's
	// own database does not exist. For commands that need the server, not
	// a database: schema databases.
	FallbackDatabase bool
}

// Resolve resolves a connection alias, URL, or file path to a driver.Connection.
func Resolve(ctx context.Context, opts Opts) (driver.Connection, error) {
	alias := resolveAlias(opts.Connection)
	conn, err := resolveOnce(ctx, alias, opts)
	if err != nil && opts.FallbackDatabase && opts.Database == "" && stderrors.Is(err, driver.ErrUnknownDatabase) {
		if db := driver.Lookup(driverOf(alias)).DefaultDB; db != "" {
			opts.Database = db
			return resolveOnce(ctx, alias, opts)
		}
	}
	return conn, err
}

// driverOf names the driver an alias or URL resolves to, "" when unknown.
func driverOf(alias string) driver.Driver {
	if driver.IsConnectionURL(alias) {
		return driver.DetectDriverFromURL(alias)
	}
	conn := config.GetConnection(alias)
	if conn == nil {
		return ""
	}
	if conn.Driver == "" && conn.URL != "" {
		return driver.DetectDriverFromURL(conn.URL)
	}
	return driver.Driver(conn.Driver)
}

func resolveOnce(ctx context.Context, alias string, opts Opts) (driver.Connection, error) {
	conn, err := resolveAdHoc(ctx, alias, opts.Write, opts.Database)
	if err != nil {
		return nil, err
	}
	if conn != nil {
		return conn, nil
	}

	return resolveFromConfig(ctx, alias, opts.Write, opts.Database)
}

// rejectDatabaseOverride returns the -D error for drivers whose connection is
// a single database file, or nil when the driver honours Connection.Database.
func rejectDatabaseOverride(d driver.Driver, database string) error {
	if database == "" {
		return nil
	}
	if info := driver.Lookup(d); info.SingleFile {
		return errors.New(
			fmt.Sprintf("-D is not supported for %s connections", d),
			errors.FixableByAgent,
		).WithHint(fmt.Sprintf("%s connections are a single database file; drop -D", info.DisplayLabel))
	}
	return nil
}

func resolveAlias(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if env := strings.TrimSpace(os.Getenv("AGENT_SQL_CONNECTION")); env != "" {
		return env
	}
	return config.GetDefaultAlias()
}

func resolveAdHoc(ctx context.Context, connStr string, write bool, database string) (driver.Connection, error) {
	if connStr == "" {
		return nil, errors.New(
			fmt.Sprintf("No connection specified and no default configured. Available connections: %s. %s", listAliases(), errors.ConnTip),
			errors.FixableByAgent,
		)
	}

	if driver.IsConnectionURL(connStr) {
		d := driver.DetectDriverFromURL(connStr)
		if d == "" {
			return nil, nil
		}
		if write {
			return nil, rejectAdHocWrite()
		}
		if err := rejectDatabaseOverride(d, database); err != nil {
			return nil, err
		}
		return connectFromURL(ctx, d, connStr, database)
	}

	if driver.IsFilePath(connStr) {
		d := driver.DetectDriverFromURL(connStr)
		if err := rejectDatabaseOverride(d, database); err != nil {
			return nil, err
		}
		switch d {
		case driver.DriverDuckDB:
			if write {
				return nil, rejectAdHocWrite()
			}
			return connectDuckDbAdHoc(ctx, connStr)
		case driver.DriverSQLite:
			return connectSqliteAdHoc(ctx, connStr, write)
		default:
			return nil, errors.New(
				fmt.Sprintf("Cannot infer driver for file path %q. Recognized extensions: .db, .sqlite, .sqlite3, .db3 (sqlite); .duckdb (duckdb). Use a connection URL or --driver to disambiguate.", connStr),
				errors.FixableByAgent,
			)
		}
	}

	return nil, nil
}

func resolveFromConfig(ctx context.Context, alias string, write bool, database string) (driver.Connection, error) {
	if alias == "" {
		return nil, errors.New(
			fmt.Sprintf("No connection specified and no default configured. Available connections: %s.", listAliases()),
			errors.FixableByAgent,
		)
	}

	conn := config.GetConnection(alias)
	if conn == nil {
		return nil, errors.NotFound(alias, configAliases())
	}

	d := driver.Driver(conn.Driver)
	if d == "" && conn.URL != "" {
		d = driver.DetectDriverFromURL(conn.URL)
	}
	if d == "" {
		return nil, errors.New("Cannot determine driver type.", errors.FixableByAgent)
	}

	if write {
		cred := credFor(conn)
		if err := checkWritePermission(d, cred, alias); err != nil {
			return nil, err
		}
	}

	if database != "" {
		if err := rejectDatabaseOverride(d, database); err != nil {
			return nil, err
		}
		// Copy before overriding: config.GetConnection may hand back shared
		// state, and -D is per-process, never persisted.
		overridden := *conn
		overridden.Database = database
		conn = &overridden
	}

	return connectFromConfig(ctx, d, conn, !write)
}

// connectFromURL dispatches an ad-hoc URL to the right driver. A non-empty
// database (-D) replaces the URL's database path.
func connectFromURL(ctx context.Context, d driver.Driver, connStr, database string) (driver.Connection, error) {
	if database != "" && d != driver.DriverSnowflake {
		rewritten, err := withURLDatabase(connStr, database)
		if err != nil {
			return nil, err
		}
		connStr = rewritten
	}
	switch d {
	case driver.DriverSQLite:
		return connectSqliteAdHoc(ctx, strings.TrimPrefix(connStr, "sqlite://"), false)
	case driver.DriverDuckDB:
		return connectDuckDbAdHoc(ctx, strings.TrimPrefix(connStr, "duckdb://"))
	case driver.DriverPG:
		return pg.ConnectURL(ctx, connStr, true)
	case driver.DriverCockroachDB:
		return cockroachdb.ConnectURL(ctx, connStr, true)
	case driver.DriverMySQL, driver.DriverMariaDB:
		return connectMysqlLikeURL(d, connStr)
	case driver.DriverSnowflake:
		return connectSnowflakeURL(connStr, database)
	case driver.DriverMSSQL:
		return connectMssqlURL(connStr)
	}
	return nil, errors.New(fmt.Sprintf("Unsupported driver: %s", d), errors.FixableByAgent)
}

// connectFromConfig dispatches a stored connection to the right driver.
func connectFromConfig(ctx context.Context, d driver.Driver, conn *config.Connection, readonly bool) (driver.Connection, error) {
	cred := credFor(conn)
	switch d {
	case driver.DriverSQLite:
		path := resolveFilePath(conn, "sqlite://")
		if path == "" {
			return nil, errors.New("SQLite connection requires a path.", errors.FixableByAgent)
		}
		return sqlite.Connect(sqlite.Opts{Path: path, Readonly: readonly, Options: conn.Options})
	case driver.DriverDuckDB:
		path := resolveFilePath(conn, "duckdb://")
		if path == "" {
			return nil, errors.New("DuckDB connection requires a path.", errors.FixableByAgent)
		}
		return duckdb.Connect(ctx, duckdb.Opts{Path: path, Readonly: readonly, Options: conn.Options})
	case driver.DriverPG, driver.DriverCockroachDB:
		return connectPgLikeConfig(ctx, d, conn, cred, readonly)
	case driver.DriverMySQL, driver.DriverMariaDB:
		return connectMysqlLikeConfig(d, conn, cred, readonly)
	case driver.DriverSnowflake:
		return connectSnowflakeConfig(conn, cred, readonly)
	case driver.DriverMSSQL:
		return connectMssqlConfig(conn, cred, readonly)
	}
	return nil, errors.New(fmt.Sprintf("Unknown driver '%s'.", d), errors.FixableByAgent)
}

// listAliases returns the saved connection aliases as a comma-separated
// string for error messages, or "(none configured)" if empty.
func listAliases() string {
	a := configAliases()
	if len(a) == 0 {
		return "(none configured)"
	}
	return strings.Join(a, ", ")
}

func configAliases() []string {
	conns := config.GetConnections()
	out := make([]string, 0, len(conns))
	for k := range conns {
		out = append(out, k)
	}
	return out
}

// resolveFilePath returns the file path for a SQLite or DuckDB stored
// connection, preferring the Path field but falling back to URL with
// the scheme stripped (urlPrefix is "sqlite://" or "duckdb://").
func resolveFilePath(conn *config.Connection, urlPrefix string) string {
	if conn.Path != "" {
		return conn.Path
	}
	if conn.URL != "" {
		return strings.TrimPrefix(conn.URL, urlPrefix)
	}
	return ""
}

func orStr(val, def string) string {
	if val == "" {
		return def
	}
	return val
}

func orInt(val, def int) int {
	if val == 0 {
		return def
	}
	return val
}
