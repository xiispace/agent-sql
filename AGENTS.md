# agent-sql

Read-only-by-default SQL CLI for AI agents. Go, compiled to standalone binaries.

## Design docs

Design docs live in `design-docs/` (gitignored, local-only). If present:

- `design-docs/go-rewrite.md` — Go rewrite plan with phased migration strategy
- `design-docs/subprocess-drivers.md` — subprocess driver pattern design
- `design-docs/credential-form.md` — `credential add --form` LLM-safe secret entry via native OS dialogs
- `design-docs/schema-diff-and-copy.md` — `-D`/`schema databases` database addressing, `schema diff`, and cross-connection `copy`
- `design-docs/TASKS.md` — implementation task tracker

## Runtime

- **Go** — single compiled binary, no runtime dependencies
- **lib-agent-cli / lib-agent-output / lib-agent-mcp** — the family's shared CLI runtime (root scaffolding, --format/--color/--timeout/--debug globals, keychain/dialog), wire contract (NDJSON, `{error, fixable_by, hint}`, `@pagination`, colour funnel), and MCP server. Keep these pinned to the latest tags; family alignment lives in these libs.
- **pgx** — native PostgreSQL driver (also used by CockroachDB)
- **go-sql-driver/mysql** — native MySQL driver (also used by MariaDB)
- **modernc.org/sqlite** — pure Go SQLite driver (no CGo)
- **go-mssqldb** — native MSSQL driver (pure Go, SQL auth only — no Azure SDK)
- **DuckDB** — subprocess driver, spawns `duckdb` CLI with NDJSON output (requires CLI installed separately)
- **Snowflake** — HTTP REST API v2 with PAT authentication (lightweight, no gosnowflake)
- **ncruces/zenity** — pure-Go cross-platform native dialog library (Win32, osascript, zenity/kdialog) for `credential add --form`

## Key design decisions

- **Read-only by default** — credentials stored in macOS Keychain, not in config file. Config has zero sensitive data.
- **PG/CockroachDB read-only** — keyword-based guard + `SET default_transaction_read_only = on` + `BEGIN READ ONLY` per query. Defense in depth.
- **SQLite readonly** — `SQLITE_OPEN_READONLY` via `?mode=ro` DSN. OS-level enforcement.
- **MySQL/MariaDB readonly** — `START TRANSACTION READ ONLY` per query + session-level enforcement. MariaDB uses `max_statement_time` for timeout (vs MySQL's `MAX_EXECUTION_TIME`).
- **DuckDB readonly** — `-readonly` CLI flag, engine-level enforcement (like SQLite). No guard needed.
- **Snowflake readonly** — client-side keyword allowlist + `MULTI_STATEMENT_COUNT=1`.
- **MSSQL readonly** — keyword-based guard. Server-side `db_datareader` role recommended for production.
- **Subprocess drivers** — pattern for databases without lightweight native drivers. Spawns CLI tool with NDJSON output. See `design-docs/subprocess-drivers.md`.
- **Streaming output** — NDJSON written row-by-row via `ResultWriter` interface. Never buffers full result sets for streaming formats.
- **Output** — NDJSON to stdout (default), errors to stderr as JSON. NULLs preserved. `@truncated` per row. `fixable_by` on errors.
- **Skill boundary** — query, schema (incl. `schema diff`), config, connection list/test, usage exposed to LLMs. `copy` is exposed too (as one MCP tool, `destructiveHint` set): like `run --write` it can only write through a stored connection whose credential a human granted `writePermission`. Credential and connection mutation are human-only.
- **Database addressing** — global `-D/--database` overrides `Connection.Database` in `resolve` for aliases and URLs (sqlite/duckdb reject it). MySQL/MariaDB have no `DefaultDB`: a connection without a database has none, and the MySQL schema methods bind `table_schema = ?` (the `db.table` qualifier, else the connection's database) instead of `DATABASE()`. `schema databases` (`driver.DatabaseLister`) lists what the credential can see.
- **Schema diff / copy** — `internal/schemadiff` is dialect-neutral (model, compare/classify, destructive detection, dependency order; imports nothing internal so `driver` can depend on it). Drivers opt in via `driver.SchemaIntrospector`, `driver.DDLRenderer`, `driver.CopySessioner` (MySQL/MariaDB in `driver/mysql`, PostgreSQL in `driver/pg`). `internal/copier` plans (gates: write permission via `resolve`, same-server/schema, keys, `--structure none` compatibility, `--allow-destructive`) and executes: DDL in dependency order, then per-table data in one target transaction. Source reads run in one `START TRANSACTION WITH CONSISTENT SNAPSHOT, READ ONLY` session over the text protocol with parseTime off, so values round-trip as raw bytes; the target writer session has FK checks off (unique checks stay on: append/upsert/sync write into populated tables), the target session's own sql_mode minus `NO_ZERO_DATE`/`NO_ZERO_IN_DATE`/`NO_BACKSLASH_ESCAPES` and its parse modes, plus the source's parse modes (`ANSI_QUOTES`, `PIPES_AS_CONCAT`, `IGNORE_SPACE`, `REAL_AS_FLOAT`, `HIGH_NOT_PRECEDENCE`: replayed DDL is the source's text) and `NO_AUTO_VALUE_ON_ZERO` (never the source's mode wholesale: MySQL 8 refuses MariaDB's `NO_AUTO_CREATE_USER`). `--data upsert`/`sync` merge-join and write plain INSERT plus `BulkWriter.Update` (UPDATE ... WHERE key; never ON DUPLICATE KEY / ON CONFLICT, which match any unique index), sync deleting target-only rows in a pass of its own first; `RowKey` never uses generated columns. `--dry-run` resolves `--to` read-only and reads it through `OpenSnapshotReader` instead of a writer. Same-server check: `ServerIdentity` (MySQL `@@server_uuid`, MariaDB hostname:port) or a `driver.SessionMarker` (MySQL `GET_LOCK`/`IS_USED_LOCK`, PG advisory locks). `schema databases` sets `resolve.Opts.FallbackDatabase`: a connect error carrying `driver.ErrUnknownDatabase` (PG 3D000) retries on the Registry `DefaultDB`. `--data sync` merge-joins both sides in key order (string keys ordered `CAST(... AS BINARY)` on MySQL, `::text COLLATE "C"` on PG, so Go's byte compare matches). PostgreSQL: introspection runs with `search_path = pg_catalog` so every server-rendered definition is schema-qualified and `schemadiff.RequalifySchema` can re-point it (and, on CockroachDB, `stripDatabase` drops the three-part database qualifier); tables are rendered from the model, but foreign keys (like checks and indexes) carry `pg_get_constraintdef` as `Definition`, which is the whole comparison and is re-rendered verbatim so MATCH/DEFERRABLE/`SET NULL (cols)` survive; NOT VALID is split off into `ForeignKey.NotValid` (rendered on create, never compared: validation is state, not structure); foreign key rule equivalences are per dialect via `Model.RuleAliases` (MySQL folds NO ACTION into RESTRICT at compare time, the model keeps the server's spelling); rows move in text format via `pgconn`; FK checks off needs `session_replication_role = replica` (superuser), otherwise the data phase relies on parents-first `TableOrder`, and sync deletes children-first in two passes around it (the first keeps rows a foreign key still references, via `driver.ReferencedKeeper`).
- **LLM-safe credential entry** — `credential add --form` opens a native OS dialog. The user types secrets directly into the OS; the LLM driving the CLI sees only a redacted JSON receipt on stdout. Headless detection emits `fixable_by="human"` with a hint to the non-interactive fallback. See `design-docs/credential-form.md`.
- **Pure Go** — no CGo dependencies. Keyword guard instead of `pg_query_go`. Cross-compilation is trivial.

## Dev tools

- **Build**: `make build`
- **Test**: `make test` (full) / `make test-short` (skip integration)
- **Lint**: `make lint` (golangci-lint)
- **Format**: `make fmt` (gofmt + goimports)
- **Dev runner**: `make dev ARGS="run -c ./data.db 'SELECT 1'"`
- **Vet**: `make vet`

## Architecture

```
cmd/
  agent-sql/                  # CLI entry point (main.go)
internal/
  cli/                        # cobra commands; root has SilenceErrors=true so RunE errors propagate
                              # as exit codes without cobra reprinting them.
    root.go                   # global flags (-c, -D, --format, --expand, --full, --timeout)
    run.go                    # top-level `run` alias
    usage.go                  # LLM reference card
    shared/                   # WithConnection helper, GlobalFlags, RegisterUsage,
                              # CompareFlags (--from/--to/--schema + comparison options)
    query/                    # query run/sample/explain/count/usage
    schema/                   # schema databases/tables/describe/indexes/constraints/search/dump/diff/usage
    copy/                     # copy command + usage.go
    connection/               # split per command:
                              #   register.go      — Register + usage const
                              #   add.go           — registerAdd
                              #   update.go        — registerUpdate
                              #   list.go          — registerList + renderConnection
                              #   simple.go        — registerRemove + registerSetDefault
                              #   test_cmd.go      — registerTest (only one using resolve)
                              #   build.go         — buildConnectionFromAddArgs,
                              #                       buildConnectionUpdates,
                              #                       validateCredentialRef, applyURLUpdate,
                              #                       applyOptionUpdates
                              #   parse.go         — parsedConnString, parseGenericURL,
                              #                       parseSnowflakeURL, rejectEmbeddedCreds,
                              #                       parseOptionFlags
    credential/               # credential add/remove/list/usage
    config/                   # config get/set/reset/list-keys/usage
  driver/
    driver.go                 # Connection interface, QueryResult, schema types, DatabaseLister
    copy.go                   # SchemaIntrospector, DDLRenderer, CopySessioner (+ SnapshotReader,
                              # BulkWriter, RawRows) optional interfaces for schema diff / copy
    registry.go               # SINGLE source of truth for per-driver metadata: Scheme,
                              # DefaultPort, DefaultDB, HostPort, CredentialKind, DisplayLabel.
                              # display.go and resolve/ both read from this.
    detect.go                 # URL scheme and file extension detection
    guard.go                  # Keyword-based read-only guard (shared by PG, MSSQL)
    helpers.go / sqlrows.go   # ScanAllRows, SQLRowsIterator, SplitSchemaTable
    pg/                       # PostgreSQL (pgx) — also used by CockroachDB;
                              #   introspect.go, ddl.go, copy.go (schema diff / copy)
    cockroachdb/              # Thin wrapper over pg (port 26257, db defaultdb)
    mysql/                    # MySQL (go-sql-driver/mysql) — also used by MariaDB;
                              #   introspect.go (SchemaIntrospector), ddl.go (DDLRenderer),
                              #   copy.go (CopySessioner: snapshot reader, bulk writer)
    mariadb/                  # Thin wrapper over mysql (max_statement_time)
    sqlite/                   # SQLite (modernc.org/sqlite, pure Go)
    duckdb/                   # DuckDB (subprocess — spawns duckdb CLI with NDJSON output)
    snowflake/                # Snowflake (HTTP REST API v2)
    mssql/                    # MSSQL (go-mssqldb)

    # Canonical per-driver layout (every driver follows this):
    #   <name>.go    Connect + Connection-interface methods only
    #   dsn.go       DSN/URL builder (when non-trivial)
    #   schema.go    Schema methods (GetTables, DescribeTable, …)
    #   errors.go    classifyError + driver-specific error helpers
    #   options.go   Option-merging helpers (currently only duckdb)

  resolve/                    # alias / URL / file → driver.Connection (the dispatch hub)
    resolve.go                # top-level Resolve dispatch
    policy.go                 # write-permission, credential-required helpers
    urlparse.go               # genericURL struct, parseGenericURL, parsePort
    connect_pg.go             # pg/cockroachdb stored connect
    connect_mysql.go          # mysql/mariadb URL + stored connect
    connect_snowflake.go      # snowflake URL + stored connect
    connect_mssql.go          # mssql URL + stored connect
    connect_file.go           # sqlite + duckdb ad-hoc

  schemadiff/                 # dialect-neutral schema model, Compare (four-way classification,
                              # destructive detection, comparison options), TableOrder
  copier/                     # copy orchestration: Prepare (plan + gates) / Execute (DDL, data
                              # phase: replace/append, key-ordered merge for upsert/sync)
  config/                     # Config file I/O + display rendering
    config.go                 # types, JSON I/O, settings get/set
    display.go                # DisplayURL, AsReceipt, EffectiveHost/Port; reads driver.Registry
  credential/                 # Credential storage (Keychain on macOS, file fallback)
  output/                     # ResultWriter interface; NDJSON/JSON/YAML/CSV formatters
                              # (routed through lib-agent-output's funnel: colour,
                              # HTML-escaping off); PrintResult/PrintList (family
                              # receipt/list contract), WriteError, Notice helpers
  truncation/                 # @truncated decorator (ResultWriter wrapper)
  errors/                     # QueryError (alias of lib-agent-output Error),
                              # per-driver error classification
```

## Key patterns

- **Connection interface**: Every driver implements `driver.Connection`. CLI commands call interface methods — zero driver-specific branching.
- **Driver registry**: `internal/driver/registry.go` is the single source of truth for per-driver metadata (scheme, default port/db, credential kind, display label). `config/display.go` and `resolve/` both read from it. Adding a driver is a single Registry entry plus the driver-package implementation.
- **context.Context**: All driver methods take `context.Context` for timeout/cancellation (except `QuoteIdent` and `Close`).
- **ResultWriter**: Streaming output interface. NDJSON writes rows as they arrive. JSON/YAML buffer internally. Truncation is a decorator wrapping the inner writer.
- **Error classification**: `errors.QueryError` is a type alias of `lib-agent-output`'s `Error` (`{error, fixable_by, hint}` with `agent`/`human`/`retry`), so errors bubbled from RunE keep their classification and hint when `libcli.Run` writes them to stderr. Every driver's `classifyError` includes an "already classified, pass through" guard so re-wrapping doesn't lose the original FixableBy.
- **Persisted format defaults are boundary-owned**: the root ConfigDefaults hook backfills g.Format before --format validation — `query.format` (may be csv) applies to the csv-capable command class via `libcli.FormatAllowed`, `defaults.format` (universal formats only) elsewhere; flag beats both. The output layer resolves the post-boundary flag purely and never reads the config store.
- **Family output contract**: pagination is snake_case (`{"@pagination": {"has_more", "row_count", "hint"}}`); lists (schema tables/indexes/constraints, connection/credential list, config list-keys) emit NDJSON records by default and a `{"data": [...]}` envelope for json/yaml (`output.PrintList` → `WriteList`); receipts/single resources are one JSON line by default (`output.PrintResult`); stderr advisories are structured `{"notice", "hint"}` lines (`output.Notice`). `--color auto|always|never` colorizes via lib-agent-output; csv (query commands) and sql (schema dump) are domain formats opted in per command via `libcli.AllowFormats`.
- **Hard-exit on errors**: every CLI command's RunE returns non-nil on failure (with cobra's `SilenceErrors: true` on root, this propagates as a non-zero exit code without double-printing). Shell `&&` chains reflect actual outcomes.
- **Connection resolution**: `-c` accepts aliases, file paths (`.db`, `.duckdb`), or URLs (postgres://, cockroachdb://, mysql://, mariadb://, duckdb://, snowflake://, mssql://, sqlserver://). Chain: `-c` flag > `AGENT_SQL_CONNECTION` env > config default.
- **Connection options**: per-driver knobs (sslmode, parseTime, encrypt, _journal_mode, query_tag, …) flow from URL query strings or repeated `--option k=v` flags into `Connection.Options`, then through to the driver lib via its native option-handling (pgx URL, gomysql.ParseDSN, sqlite URI, go-mssqldb URL, snowflake session params, duckdb SET prelude). Pass-through: the underlying driver lib is the source of truth for valid keys.
- **Embedded credentials**: stored connection URLs reject `user:pass@` at add/update time (config is plaintext on disk). Ad-hoc `-c <url>` preserves embedded creds because they're per-process and never written.
- **Keyword guard**: Shared read-only guard for PG and MSSQL. Blocks statements starting with INSERT, UPDATE, DELETE, CREATE, ALTER, DROP, TRUNCATE, MERGE, GRANT, REVOKE. PG also uses server-side `BEGIN READ ONLY`.
- **Thin wrappers**: CockroachDB wraps PG (different defaults). MariaDB wraps MySQL (different timeout syntax).

## Releasing

Use `/release <patch|minor|major>` to build, tag, and publish.
The Homebrew tap lives at `../homebrew-tap`. Always `cd` back after updating it.

## After making changes

When changing CLI behavior, flags, output shape, or commands, also update the applicable docs:
- `internal/cli/usage.go` — top-level LLM reference card
- `internal/cli/*/usage.go` — per-command usage text
- `skills/agent-sql/SKILL.md` — Claude Code skill definition
- `skills/agent-sql/references/commands.md` — full command reference
- `skills/agent-sql/references/output.md` — output format reference
- `README.md` — user-facing documentation

### Adding a new driver — checklist

Use the existing drivers as a model. The driver registry centralizes most
metadata; the per-driver wiring follows the canonical file shape.

**Core (driver package + registry):**
- [ ] `internal/driver/driver.go` — add to `Driver` constants and `AllDrivers`
- [ ] `internal/driver/registry.go` — add a `Registry` entry (Scheme, DefaultPort, DefaultDB, HostPort, Credential, DisplayLabel). Most downstream code reads from here automatically.
- [ ] `internal/driver/detect.go` — URL pattern and/or file extension detection
- [ ] `internal/driver/<name>/` — driver implementation following the canonical layout:
  - [ ] `<name>.go` — Connect + Connection-interface methods
  - [ ] `dsn.go` — DSN/URL builder if non-trivial
  - [ ] `schema.go` — schema methods (GetTables, DescribeTable, …)
  - [ ] `errors.go` — classifyError with the "already-classified pass-through" guard at top
  - [ ] `options.go` — option helpers if non-trivial
  - [ ] optional: `DatabaseLister` (`schema databases`), and `SchemaIntrospector` + `DDLRenderer` + `CopySessioner` in `introspect.go` / `ddl.go` / `copy.go` for `schema diff` / `copy` (model on `driver/mysql` or `driver/pg`; `Dialect()` decides which drivers may pair)

**Resolve dispatch:**
- [ ] `internal/resolve/connect_<name>.go` — connect-time wiring (one file per driver)
- [ ] `internal/resolve/resolve.go` — add a case in connectFromURL and connectFromConfig
- [ ] `internal/resolve/connect_<name>.go` calls into `requireUserPass` / `requirePassword` (policy.go)

**CLI text (every place that lists drivers):**
- [ ] `internal/cli/connection/add.go` — `--driver` flag description
- [ ] `internal/cli/connection/update.go` — `--driver` flag description
- [ ] `internal/cli/connection/register.go` — `usageText` const: examples, AD-HOC section, driver list
- [ ] `internal/cli/credential/credential.go` — `usageText` hint text
- [ ] `internal/cli/usage.go` — top-level driver list, ad-hoc examples, CONNECTION section

**Documentation:**
- [ ] `skills/agent-sql/SKILL.md` — description, triggers, driver list, quick start, safety section
- [ ] `skills/agent-sql/references/commands.md` — `-c` flag description
- [ ] `README.md` — tagline, quick start, safety table, connection resolution
- [ ] `AGENTS.md` — this file (runtime, design decisions, architecture)

**Tests:**
- [ ] `internal/driver/<name>/<name>_test.go` — query, schema, readonly, data types, error classification
- [ ] `internal/driver/<name>/`-test for the DSN/URL builder if `dsn.go` exists
- [ ] `internal/driver/detect_test.go` — URL detection, file extension detection
- [ ] `internal/resolve/resolve_test.go` — `TestCheckWritePermission` table
- [ ] `internal/cli/connection/build_test.go` — `TestOptionsURLBridge` row for the driver's URL grammar

