# Schema Diff and Cross-Connection Copy (`schema diff`, `copy`) Design Document

## Problem

A team runs a shared dev sandbox whose MySQL schemas drift behind stage. Bringing one service's database (`meowth`) in line with stage took the following with today's agent-sql:

- stage had 18 tables, dev had 9. Finding that out meant running the same `information_schema` query on both connections and diffing the NDJSON by hand.
- The `stage` connection has no default database, so every query failed with `1044 Access denied … to database 'mysql'`. The workaround was to borrow a sibling alias (`stage-risk`, default DB `newrails_risk`) and fully qualify `meowth.*`. All MySQL schema methods (`GetTables`, `DescribeTable`, `GetIndexes`, …) are bound to `DATABASE()`, so `schema` commands could not address `meowth` at all.
- `mysqldump` could not be used: the credential lives in the Keychain, and the agent cannot (and should not) extract it to hand to another tool.
- So the copy was a throwaway script: `SHOW CREATE TABLE` + `SELECT *` per table through `agent-sql run`, Python-side literal escaping, then 71 `run --write` calls against dev. One `agent-sql` subprocess per statement made a 7.5 MB export take over five minutes, and the script crashed on an empty table because `run` emits no `columns` record when there are no rows.
- The copy dropped and recreated every table, so dev lost its own rows. That was the only mode the script had, even though the underlying need was "dev's schema is behind" — a structure-only sync would have kept dev's data and its cross-service references.

GUI tools have solved this for years. Navicat splits it into three tools — **Structure Synchronization**, **Data Transfer** and **Data Synchronization** — each with a small set of well-chosen options. This document brings that model to agent-sql as two commands: a read-only `schema diff`, and a `copy` that applies structure and data changes from one connection to another, with the credentials staying inside the agent-sql process.

## Constraints

- **Credentials never leave the process.** Both connections are opened by agent-sql through `resolve`; nothing is passed to an external binary.
- **Read-only by default still holds.** `schema diff` is read-only on both sides. `copy` only writes to the target, through the existing write gate (see [Write permission](#write-permission)). The source is always opened read-only.
- **No driver branching in the CLI.** New capabilities are optional interfaces next to `DDLDumper` and `StreamingQuerier`; drivers that don't implement them return `fixable_by: agent` "not supported for <driver>".
- **Schema addressing is explicit.** Every introspection query and every emitted statement is qualified by the schema named on the command line. Nothing depends on the connection's default database.
- **Columns come from introspection, not from result-set metadata**, so empty tables behave like any other table.
- **Streaming.** Rows move source → target in bounded batches; neither side is buffered whole.
- **Same driver family only.** `mysql` ↔ `mariadb` is allowed; cross-dialect copy (MySQL → PG) is out of scope.
- **Pure Go, no CGo**, as for the rest of the binary.

## Prior art: what we take from Navicat

| Navicat tool | What it does | agent-sql equivalent |
|---|---|---|
| Structure Synchronization | Compares objects between two connections, classifies them, previews a DDL script, executes the selected statements | `schema diff` (compare + preview) and `copy --structure sync` (execute) |
| Data Transfer | Copies selected tables (structure and/or rows) between connections, with options for dropping the target first, batching and error handling | `copy --structure recreate\|none` with `--data replace\|append` |
| Data Synchronization | Compares rows by primary key and applies inserts, updates and deletes | `copy --data upsert\|sync` |

Borrowed specifically:

- **Four-way classification**: only in source (create), only in target (drop), different (alter), identical.
- **Drops are opt-in.** Objects that exist only in the target are reported but not dropped unless `--drop-extra` is given.
- **Per-object comparison options**, with Navicat's defaults (AUTO_INCREMENT off, column position off; see below).
- **Dependency-ordered DDL**: referenced tables before referencing tables, tables before views, views before triggers.
- **Preview before execute**: `copy --dry-run` prints exactly the statements and row actions it would run.
- **Continue on error** and batch size as explicit knobs.

Where we deliberately go further than Navicat: changes that can lose data are classified as `destructive` and refused unless `--allow-destructive` is given (Navicat shows them in the script but does not single them out).

## Prerequisite: database addressing on MySQL connections

The 1044 failure above is a general problem, not specific to copying, and `schema diff`/`copy` build on its fix, so it ships first.

**Today:**

- A stored MySQL/MariaDB connection without `database` silently connects to `mysql`. `connectMysqlLikeConfig` falls back to `orStr(conn.Database, info.DefaultDB)`, and the registry's `DefaultDB` for both drivers is `"mysql"`. Restricted accounts (stage RDS) have no privilege on the `mysql` system database, so the connection fails outright. Ad-hoc URLs without a path don't have this problem; they pass the empty database through.
- Every MySQL schema method (`GetTables`, `DescribeTable`, `GetIndexes`, `GetConstraints`, `SearchSchema`, and through them `schema dump`) filters on `table_schema = DATABASE()`. `run` can reach another database with `db.table`, but the `schema` commands cannot.
- So the only way to inspect another database on the same instance is another stored connection that differs only in `database`: `stage-risk`, `dev-risk`, and one more for every database the team needs. A single RDS instance holds 20+ business databases.

**Changes** (all four; they cover different cases):

1. **Global `--database` / `-D <name>`.** Overrides `Connection.Database` in `resolve` before connecting, for stored aliases and ad-hoc URLs alike, so it applies to `run`, `query *` and every `schema` subcommand with no per-command code. It works for every driver that reads `Connection.Database` (pg, cockroachdb, mysql, mariadb, mssql, snowflake); on PG it means connecting to another database, which is the right meaning there too. File-based drivers (sqlite, duckdb) reject it with `fixable_by: agent`. The credential and the write gate are unchanged: `-D` picks a database, not a permission. `-D` follows the `mysql` client's convention; `-d` is already `--debug`.
2. **`db.table` in MySQL schema commands.** `schema describe newrails_risk.device`, `schema indexes newrails_risk.device` and `schema constraints newrails_risk.device` split the name with the existing `driver.SplitSchemaTable`, and the MySQL queries change from `table_schema = DATABASE()` to a bound parameter: the qualifier if given, else the `-D`/connection database. This matches how PG already treats `schema.table`. For whole-database commands (`schema tables`, `search`, `dump`) the scope is `-D` or the connection's database. These commands don't take a separate `--schema` flag, because on MySQL it would duplicate `-D`.
3. **No implicit `mysql` fallback.** `DefaultDB` becomes `""` for mysql and mariadb, so a stored connection without `database` connects with no default database, the same as `mysql` with no `-D`. Fully qualified `run` statements work; whole-database `schema` commands with nothing to scope to fail with `fixable_by: agent` and the hint `no database selected; pass -D <db> or qualify the table as db.table` (and `schema databases` to list the candidates, see change 4). PG and CockroachDB keep their defaults, because a PG connection must name a database.

4. **`schema databases`.** Lists the databases the credential can see on the connection's server: Navicat's database layer under a connection, as a command. It ignores `-D` and the connection's own database, so it works on a connection with no database, and it is the next step the "no database selected" error points to.

   ```
   agent-sql schema databases [--include-system] [--pattern <glob>]
   ```

   Output uses the family list contract (`output.PrintList`), one NDJSON record per database:

   ```json
   {"name":"meowth","charset":"utf8mb4","collation":"utf8mb4_0900_ai_ci"}
   {"name":"newrails_risk","charset":"utf8mb4","collation":"utf8mb4_0900_ai_ci"}
   ```

   - System databases are hidden unless `--include-system`, as for `schema tables`: `information_schema`, `performance_schema`, `mysql` and `sys` on MySQL/MariaDB; templates on PG; `master`, `model`, `msdb` and `tempdb` on MSSQL.
   - `--pattern` filters names with a case-insensitive glob (`--pattern 'newrails_*'`), for servers with many databases.
   - New optional driver interface `DatabaseLister { ListDatabases(ctx, includeSystem bool) ([]DatabaseInfo, error) }`. MySQL/MariaDB read `information_schema.schemata` (name, default charset, default collation). PG reads `pg_database WHERE datallowconn`; MSSQL reads `sys.databases`; Snowflake runs `SHOW DATABASES`. SQLite and DuckDB don't implement it and return `fixable_by: agent` "<driver> connections are a single database file".
   - The visible list is whatever the server returns for this credential: MySQL filters `SHOW DATABASES`/`schemata` by privilege, so a restricted account sees only the databases it can use. If the account lacks `SHOW DATABASES` and has no per-database privileges, the list is empty. That is reported as an empty list with a stderr notice (`output.Notice`), not as an error.

**Compatibility.** Change 3 changes behaviour only for stored MySQL/MariaDB connections without `database` that relied on unqualified access to `mysql.*` tables. They now get MySQL's own "No database selected" error and can add `-D mysql`. Changes 1, 2 and 4 are additive.

**Relation to `schema diff` / `copy`.** Those commands take two connections, so a single global `-D` would be ambiguous. They name the database explicitly with `--schema` / `--to-schema` (on MySQL, schema and database are the same thing) and do not read `-D`. Both rest on change 2's parameterized `table_schema = ?` queries; `SchemaIntrospector` is the full-model version of the same queries.

## Interface

### `schema diff`

```
agent-sql schema diff --from <conn> --to <conn> --schema <name> [--to-schema <name>]
    [--tables a,b] [--exclude-tables c,d]
    [--objects tables,views,routines,triggers,events]
    [--compare-auto-increment] [--compare-column-position]
    [--ignore-comments] [--ignore-charset] [--ignore-definer]
    [--compare-engine=false] [--compare-partitions=false]
    [--drop-extra] [--show-identical]
    [--format ndjson|json|yaml|sql]
```

- `--from` / `--to` are required and take the same values as `-c` (alias, URL, file). They do **not** fall back to `-c`, `AGENT_SQL_CONNECTION` or the config default: a diff against a silently defaulted connection is the kind of mistake this command exists to prevent.
- `--schema` names the source schema; `--to-schema` defaults to the same name.
- `--objects` defaults to `tables`. Views, routines, triggers and events are opt-in because their definitions embed `DEFINER` and server-specific text that differs between environments far more often than tables do.
- The default output is NDJSON, one record per object that differs (identical objects only with `--show-identical`).
- `--format sql` prints the DDL script that would bring the target in line, in dependency order, using the existing `libcli.AllowFormats` opt-in — the same mechanism as `schema dump --format sql`. Destructive statements are printed commented out unless `--allow-destructive` is also given, so the script is safe to read and safe to copy.

Comparison options:

| Option | Default | Why |
|---|---|---|
| AUTO_INCREMENT value | off (`--compare-auto-increment` to enable) | Always differs when the two sides hold different rows; comparing it is pure noise |
| Column position | off (`--compare-column-position`) | Reordering needs a table rebuild and rarely matters |
| Character set and collation (table and column) | on (`--ignore-charset` to disable) | Real semantic difference; a classic source of stage/dev bugs |
| Comments | on (`--ignore-comments`) | Cheap to sync; often how the schema is documented |
| DEFINER (views, routines, triggers, events) | on (`--ignore-definer`) | Usually an environment artifact; switch off when comparing across environments |
| Storage engine | on | |
| Partitioning | on | |
| Indexes and foreign keys | always | Compared by name and by definition (columns, order, prefix length, uniqueness, index type, FK actions) |

Destructive changes. A change is `destructive` when applying it can lose data:

- dropping a table, column, or (with `--drop-extra`) any object;
- narrowing a type: shorter `VARCHAR`/`CHAR`/`VARBINARY`, a smaller integer or decimal, `TEXT` → `VARCHAR`, removing `ENUM`/`SET` members, removing `UNSIGNED`;
- `NULL` → `NOT NULL`;
- changing a column's character set.

Column renames are not detected — Navicat doesn't detect them either. A rename appears as a dropped column plus an added column; the drop is `destructive`, and the record carries a `hint` that the pair may be a rename (same type, adjacent position).

### `copy`

```
agent-sql copy --from <conn> --to <conn> --schema <name> [--to-schema <name>]
    --structure recreate|sync|none
    --data replace|append|upsert|sync|none
    [--tables a,b] [--exclude-tables c,d]
    [--where 'table=<condition>']...
    [--no-delete]
    [--drop-extra] [--allow-destructive]
    [<schema diff comparison options>]
    [--batch-rows N] [--continue-on-error]
    [--dry-run]
```

`--structure` and `--data` are **both required**. Running `copy --from stage --to dev --schema meowth` with no mode would have to pick a default, and every candidate either does nothing useful or overwrites the target. The caller states intent.

`--structure`:

| Mode | Behaviour |
|---|---|
| `recreate` | `DROP TABLE IF EXISTS` + the source's `CREATE TABLE` for every selected table (Navicat Data Transfer with "drop target objects before create"). Destructive by definition: requires `--allow-destructive` when any target table already exists |
| `sync` | Runs the `schema diff` engine and applies its DDL. Same comparison options, `--drop-extra` and `--allow-destructive` gates as `schema diff` |
| `none` | Leaves structure alone. Data modes then require compatible target tables (see below) |

`--data`:

| Mode | Behaviour | Needs a key |
|---|---|---|
| `replace` | `TRUNCATE` the target table, then insert every source row | no |
| `append` | Insert source rows; duplicate keys are errors (subject to `--continue-on-error`) | no |
| `upsert` | `INSERT … ON DUPLICATE KEY UPDATE` every source row; target-only rows are kept | yes |
| `sync` | Navicat Data Synchronization: compare rows by key; insert missing, update changed, delete target-only rows. `--no-delete` skips the deletes | yes |
| `none` | Structure only | — |

"Needs a key" means a primary key, or failing that a unique index whose columns are all `NOT NULL`. Tables without one fail with `fixable_by: agent` and a hint listing them, so the caller can exclude them or choose `replace`/`append`. agent-sql never guesses a key.

`replace` destroys target rows and so requires `--allow-destructive`; so does `sync` unless `--no-delete` is given.

Other options:

- `--where 'table=<condition>'` (repeatable) filters the source rows for one table. The condition is appended to the source `SELECT` inside the read-only snapshot transaction. Intended for large tables (copy the last month of orders instead of 2.4 GB).
- `--batch-rows` defaults to 1000 and is capped per table at `floor(65535 / column_count)`, MySQL's placeholder limit for one statement.
- `--continue-on-error` records a failed batch or DDL statement and moves on to the next table instead of stopping. The receipt lists every failure.
- `--dry-run` resolves both connections, runs the diff, counts the row actions (for `sync`: inserts, updates and deletes per table) and prints them without writing anything. The equivalent of Navicat's preview pane.

Compatibility with `--structure none`: before moving rows, each target table must have every column the source rows carry, with the same key. Missing columns fail with a hint to use `--structure sync`. Target-only columns are left to their defaults.

The motivating case becomes:

```
# What is different?
agent-sql schema diff --from stage --to dev --schema meowth

# Bring dev's structure in line; keep dev's data.
agent-sql copy --from stage --to dev --schema meowth --structure sync --data none

# Also merge stage's rows into dev without deleting dev-only rows.
agent-sql copy --from stage --to dev --schema meowth --structure sync --data sync --no-delete
```

## Write permission

`copy` writes only to `--to`, and goes through the same gate as `run --write`:

- `--to` must be a stored connection (`rejectAdHocWrite` for URLs and files), and its credential must have `writePermission` (`checkWritePermission`). Both errors stay `fixable_by: human`.
- `copy` needs no `--write` flag; writing is the command's purpose. The gate is the same.
- `--from` may be an alias, URL or file, and is always opened read-only, whatever its credential allows.
- `--from` and `--to` resolving to the same host, port and schema is rejected (`fixable_by: agent`).

## Execution

### Source side

One dedicated session, opened as `START TRANSACTION WITH CONSISTENT SNAPSHOT, READ ONLY` (MySQL). Introspection and every row read happen inside it, so all tables are read at one point in time, and the existing MySQL read-only model still applies.

Rows are read as raw bytes (`sql.RawBytes`) and written back as such, so `DATETIME(3)`, `JSON`, `DECIMAL`, binary columns and zero dates round-trip byte-for-byte without passing through Go types. Generated columns are excluded from both the read and the insert.

For `--data sync`, each table is read `ORDER BY <key>` on both sides, and the two streams are merge-joined in key order. Memory is bounded by the batch size, not by the table size. Rows are compared with a byte-for-byte comparison of the non-key columns.

### Target side

One dedicated session (a single `*sql.Conn`), so these session settings hold for the whole run:

```
SET SESSION FOREIGN_KEY_CHECKS = 0;
-- UNIQUE_CHECKS stays on: append/upsert/sync write into populated tables,
-- where InnoDB with unique checks off may accept secondary-index duplicates.
SET SESSION time_zone = '+00:00';   -- the source session uses the same
SET NAMES utf8mb4;
SET SESSION sql_mode = <source session's sql_mode>;
```

With foreign key checks off, the data phase can run in any order. The structure phase still emits DDL in dependency order, because `CREATE TABLE` with a foreign key needs the referenced table to exist.

Rows are written with parameterized multi-row `INSERT` statements. `upsert` adds `ON DUPLICATE KEY UPDATE col = VALUES(col), …` over the non-key columns. `sync` batches its updates as upserts and its deletes as `DELETE … WHERE (key) IN (…)`.

Each table's data phase runs in its own transaction, so a failed table rolls back cleanly and `--continue-on-error` can move on. DDL is not transactional in MySQL (implicit commit), which is why `--dry-run` exists.

AUTO_INCREMENT: `--structure recreate` inherits the source's value from `SHOW CREATE TABLE`. After `replace`, `append`, `upsert` or `sync`, the target table's AUTO_INCREMENT is raised to `max(target current, source current)`, so new target inserts never collide with copied ids. `--structure sync` never touches AUTO_INCREMENT unless `--compare-auto-increment` is set.

## Output

`schema diff` emits one NDJSON record per differing object:

```json
{"object":"table","name":"cus_contract","status":"only_in_source","destructive":false}
{"object":"column","table":"cus_account","name":"status","status":"different","destructive":true,"changes":{"type":{"from":"varchar(64)","to":"varchar(32)"}},"hint":"narrowing varchar(64) to varchar(32) can truncate data"}
{"object":"index","table":"cus_customer","name":"idx_merchant","status":"only_in_target","destructive":false,"action":"none","hint":"pass --drop-extra to drop"}
```

Followed by a receipt:

```json
{"from":"stage","to":"dev","schema":"meowth","only_in_source":9,"only_in_target":0,"different":3,"identical":9,"destructive":1}
```

`copy` emits one NDJSON record per table as it completes:

```json
{"table":"cus_contract","structure":"created","rows_read":484,"inserted":484,"updated":0,"deleted":0,"duration_ms":312}
```

Progress for long tables goes to stderr through `output.Notice`. The final receipt (`output.PrintResult`) has totals, the resolved `from`/`to`/`schema`, the modes, and a `failures` array when `--continue-on-error` was used. With `--dry-run`, the per-table records report planned counts and `"dry_run": true`, and `--format sql` prints the DDL that would run.

## Driver interface additions

All optional. The CLI type-asserts them, as it does for `DDLDumper` today.

```go
// SchemaIntrospector returns the full model of one schema, addressed by name
// rather than by the connection's default database.
type SchemaIntrospector interface {
    IntrospectSchema(ctx context.Context, schema string, opts IntrospectOpts) (*SchemaModel, error)
}

// DDLRenderer turns a classified diff into ordered, dialect-specific statements.
type DDLRenderer interface {
    RenderDDL(diff *schemadiff.Diff, opts RenderOpts) ([]Statement, error)
}

// CopySessioner opens the dedicated sessions copy needs: a consistent-snapshot
// read-only reader, and a writer that keeps session settings across statements.
type CopySessioner interface {
    OpenSnapshotReader(ctx context.Context, schema string) (SnapshotReader, error)
    OpenBulkWriter(ctx context.Context, schema string, settings SessionSettings) (BulkWriter, error)
}
```

`SchemaModel` is what `ColumnInfo`/`IndexInfo` lack today: per-table engine, charset, collation, comment, AUTO_INCREMENT and partitioning; per-column ordinal position, full column type, charset, collation, default, `EXTRA` (auto_increment, `ON UPDATE`, generated expression) and comment; per-index type, column order, prefix lengths and visibility; foreign keys with `ON UPDATE`/`ON DELETE`; and view, routine, trigger and event definitions with their `DEFINER`. The existing `schema` subcommands keep their current, smaller types.

The MySQL implementation reads `information_schema` with `table_schema = ?` instead of `DATABASE()`, and fetches definitions with `SHOW CREATE … \`schema\`.\`name\``. MariaDB reuses it through the existing thin wrapper.

## Package layout

```
internal/
  schemadiff/            # dialect-neutral: SchemaModel, compare, classify (incl. destructive), dependency order
  schemadiff/mysql/      # (or driver/mysql/diff.go) DDL rendering for MySQL/MariaDB
  copier/                # copy orchestration: plan, structure phase, data phase (replace/append/upsert/sync merge)
  cli/schema/diff.go     # `schema diff`
  cli/copy/              # `copy` command + usage.go
  driver/mysql/
    introspect.go        # SchemaIntrospector
    copy.go              # CopySessioner (snapshot reader, bulk writer)
```

## Errors

| Situation | `fixable_by` | Hint |
|---|---|---|
| `-D` on a file-based connection (sqlite, duckdb) | agent | "-D is not supported for <driver> connections" |
| Whole-database `schema` command on MySQL with no `-D` and no connection database | agent | "no database selected; pass -D <db> or qualify the table as db.table; list databases with `schema databases`" |
| `schema databases` on sqlite/duckdb | agent | "<driver> connections are a single database file" |
| Driver lacks `SchemaIntrospector` / `CopySessioner` | agent | "schema diff/copy is supported for mysql, mariadb" |
| `--from` and `--to` are different driver families | agent | name both drivers |
| `--from` and `--to` are the same schema on the same server | agent | |
| `--to` is ad hoc, or its credential lacks `writePermission` | human | existing messages from `rejectAdHocWrite` / `checkWritePermission` |
| Missing `--structure` or `--data` | agent | list the modes |
| Destructive change or mode without `--allow-destructive` | agent | list the destructive items; suggest `--dry-run` |
| `upsert`/`sync` on a table without a usable key | agent | list the tables; suggest `--exclude-tables` or `replace` |
| `--structure none` and the target table is incompatible | agent | name the missing columns; suggest `--structure sync` |
| Schema not found or not visible to the credential | agent | which side, which schema |
| A batch or DDL statement fails | passes through the driver's `classifyError` | with `--continue-on-error`, also recorded in the receipt's `failures` |

## Phasing

0. **Database addressing.** Global `-D`, `db.table` in MySQL schema commands, `DefaultDB = ""` for mysql/mariadb, `schema databases`. Small, independent, and useful on its own; can ship as its own release.
1. **`schema diff`, MySQL/MariaDB, tables only.** `SchemaIntrospector`, comparison, classification, NDJSON output and `--format sql`. Fixes the schema-addressing problem along the way.
2. **`copy --structure sync|recreate --data none`.** `DDLRenderer`, the bulk writer's DDL path, `--dry-run`, the write gate.
3. **`copy --data replace|append|upsert`.** Snapshot reader, raw-bytes batching, AUTO_INCREMENT realignment, `--where`.
4. **`copy --data sync`.** Key-ordered merge, `--no-delete`.
5. **`--objects views,routines,triggers,events`** for diff and copy.
6. **PostgreSQL** (and through it CockroachDB): introspector, renderer and sessions for the pg driver.

## Testing

- Unit tests in `schemadiff` for classification and destructive detection, on hand-built `SchemaModel` pairs, one per comparison option, plus the rename hint.
- Integration tests against the `mysql:8` and `mariadb:11` services in `docker-compose.test.yml`: two schemas on one server serve as source and target. Cover each structure × data mode, an empty table, a table without a key, `DATETIME(3)`/`JSON`/binary/zero-date round-trips, generated columns, and `--continue-on-error`.
- `resolve` tests for the `--to` write gate, and for rejecting the same schema on both sides.
- Database addressing: `-D` overriding stored and URL databases, `-D` rejected for file drivers, MySQL schema commands with `db.table` and with `-D`, a stored MySQL connection without `database` connecting as a user that has no privilege on `mysql`, the "no database selected" error, and `schema databases` with and without `--include-system`/`--pattern`, as a restricted user (only its granted databases appear), and on a connection with no database.

## Files touched

Code, for database addressing:

- `internal/cli/root.go` (`-D` persistent flag; the MCP surface picks it up from the command tree), `internal/cli/shared/` (pass it to `WithConnection`)
- `internal/resolve/resolve.go`, `connect_mysql.go`, `connect_pg.go`, `connect_mssql.go`, `connect_snowflake.go`, `connect_file.go` (apply or reject the override)
- `internal/driver/registry.go` (`DefaultDB: ""` for mysql and mariadb)
- `internal/driver/mysql/schema.go` (`table_schema = ?`, `SplitSchemaTable`, `ListDatabases`)
- `internal/driver/driver.go` (`DatabaseLister`, `DatabaseInfo`); `ListDatabases` in `driver/pg`, `driver/mssql`, `driver/snowflake`
- `internal/cli/schema/databases.go` (`schema databases`), `internal/cli/schema/schema.go` (register it and add it to the usage text)

Docs, per the "After making changes" checklist in `AGENTS.md`:

- `internal/cli/usage.go`, `internal/cli/schema/usage.go`, `internal/cli/copy/usage.go`
- `skills/agent-sql/SKILL.md`: `schema diff` alongside the read commands; `copy` in the write section
- `skills/agent-sql/references/commands.md`, `skills/agent-sql/references/output.md`
- `README.md`
- `AGENTS.md`: architecture tree and the design-docs list

## Out of scope

- Plan files with a target-state fingerprint and drift checks before apply. `--dry-run` is the preview.
- Data masking or anonymization while copying.
- Automatic backup of the target before writing.
- Automatic verification after writing (row counts, re-diff). Run `schema diff` again if needed.
- Cross-dialect copy (MySQL → PostgreSQL and similar).
- Rename detection beyond the hint on drop + add pairs.
- Server-level objects: users, grants, and database-level `CREATE DATABASE` options.
