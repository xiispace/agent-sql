package copy

const usageText = `COPY
====

Copy structure and/or rows from one connection to another. agent-sql opens
both connections itself, so credentials never leave the process. The source is
always read-only, inside one consistent snapshot. Only --to is written, through
the same gate as 'run --write': it must be a stored connection whose credential
has writePermission. No --write flag is needed. --dry-run only reads --to, so
it needs no write permission (like schema diff) and --to may be an ad-hoc URL.

  agent-sql copy --from <conn> --to <conn> --schema <name> [--to-schema <name>]
      --structure recreate|sync|none --data replace|append|upsert|sync|none
      [options]

--structure and --data are both required.

STRUCTURE MODES
  recreate   DROP TABLE IF EXISTS + the source's CREATE TABLE for every selected
             table (AUTO_INCREMENT included). Destructive when a target table
             exists.
  sync       Apply the 'schema diff' DDL. Same comparison options, --drop-extra
             and --allow-destructive gates as schema diff.
  none       Leave structure alone. Target tables must already have every source
             column (and the same key for upsert/sync).

DATA MODES
  replace    TRUNCATE the target table, then insert every source row (destructive)
  append     Insert source rows; duplicate keys are errors
  upsert     Compare rows by key: insert missing, update changed; target-only
             rows are kept (needs a key)
  sync       As upsert, and first delete target-only rows (destructive unless
             --no-delete; needs a key)
  none       Structure only

  A key is the primary key, or else a unique index whose columns are all
  plain, non-generated and NOT NULL. Tables without one fail upsert/sync;
  exclude them or use replace/append.

  upsert and sync write plain INSERTs and UPDATE ... WHERE <key>, never
  ON DUPLICATE KEY / ON CONFLICT: a row that collides with a different target
  row on another unique index (say the same email) is an error that rolls the
  table back, not a silent overwrite. sync deletes target-only rows before
  writing, which frees their unique values. Keys that differ only by case or
  trailing spaces under the target's collation ('Alice' vs 'alice') are
  different rows to the comparison: sync replaces one with the other, upsert
  and sync --no-delete fail on the duplicate.

EXAMPLES
  agent-sql schema diff --from stage --to dev --schema meowth
  agent-sql copy --from stage --to dev --schema meowth --structure sync --data none
  agent-sql copy --from stage --to dev --schema meowth --structure sync --data sync --no-delete
  agent-sql copy --from stage --to dev --schema meowth --structure none --data upsert \
      --tables orders --where "orders=created_at >= '2026-09-01'"
  agent-sql copy --from stage --to dev --schema meowth --structure recreate --data replace \
      --allow-destructive --dry-run

OPTIONS
  --from, --to <conn>        Alias, file path or URL. Required; never fall back to
                             -c, AGENT_SQL_CONNECTION or the default connection
  --schema <name>            Source schema (database on MySQL). Required
  --to-schema <name>         Target schema (default: --schema)
  -D <db>                    Override the database of both --from and --to
  --tables a,b               Only these tables (and their triggers; not with
                             --objects routines/events, which belong to no table)
  --exclude-tables c,d       Skip these tables
  --where 'table=<cond>'     Filter one table's source rows (repeatable). Rows
                             are matched against every target row (the one a
                             source row updates may fall outside <cond>); sync
                             deletes only target-only rows that match <cond>.
                             Unqualified names resolve in each side's own schema
  --no-delete                With --data sync, keep target-only rows
  --drop-extra               Also drop objects that exist only in the target
  --allow-destructive        Allow anything that can lose target data
  --objects tables,views,routines,triggers,events   Object kinds (default tables)
  --batch-rows N             Rows per INSERT (default 1000, capped at 65535/columns)
  --continue-on-error        Record a failed table or statement and move on
  --dry-run                  Plan and count without writing (read-only --to is
                             fine); --format sql prints the DDL that would run
  Comparison options (as schema diff): --compare-auto-increment,
  --compare-column-position, --ignore-comments, --ignore-charset,
  --ignore-definer, --compare-engine=false, --compare-partitions=false

DESTRUCTIVE WORK
  Refused without --allow-destructive: --data replace, --data sync without
  --no-delete, --structure recreate over existing tables, and any structure
  change that can lose data (dropping a column/table/object, narrowing a type,
  NULL -> NOT NULL, changing a column's character set). The error lists every
  item. Preview with --dry-run first.

OUTPUT
  One NDJSON record per table as it completes:
    {"table":"cus_contract","structure":"created","rows_read":484,"inserted":484,
     "updated":0,"deleted":0,"duration_ms":312}
  then a receipt with totals, from/to/schema, the modes, and "failures" when
  --continue-on-error recorded any. --dry-run records carry "dry_run":true and
  planned counts. "deleted" counts rows sync deleted; tables emptied wholesale
  (replace's TRUNCATE, recreate's DROP) carry "cleared":true and are counted
  in the receipt's "cleared", their rows uncounted (--dry-run counts them in
  "deleted"). upsert reports "upserted" (rows inserted or updated; unchanged
  rows are not rewritten) instead of inserted/updated. Progress for long tables goes to stderr as notices.
  json/yaml: {"data": [records], "summary": receipt}.

EXECUTION (MySQL / MariaDB)
  Target session: FOREIGN_KEY_CHECKS=0, time_zone '+00:00', utf8mb4, and the
  target's own sql_mode (accepting zero dates, with backslash escapes on)
  plus the source's parse modes (ANSI_QUOTES, PIPES_AS_CONCAT, IGNORE_SPACE,
  REAL_AS_FLOAT, HIGH_NOT_PRECEDENCE), so replayed DDL reads as it did. Rows
  travel as raw bytes, so DATETIME(3), JSON, DECIMAL, binary and zero dates
  round-trip unchanged; generated columns are skipped. Each table's rows are written in one transaction (TRUNCATE runs
  before it). DDL is not transactional in MySQL, which is why --dry-run exists.
  After the data phase each table's AUTO_INCREMENT is raised to the source's.

POSTGRESQL
  --schema/--to-schema name namespaces inside each connection's database. Tables
  are rendered from the catalog (sequences, identity and generated columns,
  CHECK/UNIQUE/PRIMARY KEY constraints, indexes, comments); views, functions,
  procedures and triggers come from pg_get_*def. Rows travel in PostgreSQL's
  text format under fixed session settings (UTC, ISO dates, hex bytea). Foreign
  key checks are switched off with session_replication_role = replica when the
  role may (superuser); otherwise tables are written parents first with checks
  on, and sync deletes children first: rows still referenced are kept until
  the data pass has re-pointed their children, then deleted. Partitioned tables are created with their PARTITION BY clause but their
  partitions are not copied. There are no events.

SUPPORTED
  mysql and mariadb (and between them), and pg.
`
