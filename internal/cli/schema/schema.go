package schema

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/shhac/agent-sql/internal/cli/shared"
	"github.com/shhac/agent-sql/internal/driver"
	"github.com/shhac/agent-sql/internal/output"
	"github.com/shhac/agent-sql/internal/resolve"
	libcli "github.com/shhac/lib-agent-cli/cli"
)

const usageText = `SCHEMA COMMANDS
===============

List databases on the server:
  agent-sql schema databases                       Databases this credential can see
  agent-sql schema databases --pattern 'shop_*'    Case-insensitive glob filter
  agent-sql schema databases --include-system      Include information_schema, mysql, templates, ...
  Ignores -D. On PostgreSQL/CockroachDB, when the connection's own database
  no longer exists it falls back to the server's default (postgres,
  defaultdb), so it still lists what is there. An empty list (with a stderr notice) means the account has no
  visible databases.

List tables:
  agent-sql schema tables                          All user tables
  agent-sql schema tables --include-system         Include system/internal tables

Describe a table:
  agent-sql schema describe <table>                Columns, types, nullability
  agent-sql schema describe <table> --detailed     Include constraints, indexes, and comments
  agent-sql schema describe public.users           PG namespace (schema.table)
  agent-sql schema describe shop.orders            MySQL/MariaDB database (db.table)

Indexes:
  agent-sql schema indexes                         All indexes across all tables
  agent-sql schema indexes <table>                 Indexes for a specific table

Constraints:
  agent-sql schema constraints                     All constraints across all tables
  agent-sql schema constraints <table>             Constraints for a specific table
  agent-sql schema constraints <table> --type pk   Filter by type (pk, fk, unique, check)

Search:
  agent-sql schema search <pattern>                Search table and column names by pattern

Dump full schema:
  agent-sql schema dump                            DDL-style dump of all tables
  agent-sql schema dump --tables users,orders      Dump specific tables only
  agent-sql schema dump --format sql               CREATE TABLE statements (driver-dependent)
  agent-sql schema dump --include-system           Include system tables

Compare a schema across two connections (read-only on both):
  agent-sql schema diff --from stage --to dev --schema shop
  agent-sql schema diff --from stage --to dev --schema shop --to-schema shop_dev
  agent-sql schema diff ... --format sql           DDL script that brings --to in line
  agent-sql schema diff ... --objects tables,views,routines,triggers,events
  --from/--to take an alias, file path or URL and never fall back to -c or the
  default connection. One NDJSON record per differing object, then a receipt:
    {"object":"column","table":"t","name":"status","status":"different",
     "destructive":true,"action":"modify","changes":{"type":{"from":"varchar(64)","to":"varchar(32)"}},
     "hint":"narrowing varchar(64) to varchar(32) can truncate data"}
    {"from":"stage","to":"dev","schema":"shop","to_schema":"shop","only_in_source":9,
     "only_in_target":0,"different":3,"identical":9,"destructive":1}
  status: only_in_source | only_in_target | different | identical (--show-identical).
  "from" is the target's current value, "to" the source's. Objects only in the
  target are reported with action "none" unless --drop-extra (columns of a shared
  table are always dropped, destructively). Destructive = dropping anything,
  narrowing a type, NULL -> NOT NULL, changing a column's character set. With
  --format sql, destructive statements are commented out unless --allow-destructive.
  Comparison options: --compare-auto-increment, --compare-column-position (both
  off), --ignore-comments, --ignore-charset, --ignore-definer, --compare-engine=false,
  --compare-partitions=false; --tables/--exclude-tables select tables (with
  their triggers; --tables cannot be combined with routines/events, which belong
  to no table). -D overrides the database of both --from and --to.
  Column renames are not detected; a drop + add pair carries a rename hint.
  Supported for mysql, mariadb and pg (on PostgreSQL --schema is a namespace in the
  connection's database; there are no events). To apply the changes: 'agent-sql copy usage'.

OPTIONS
  -c, --connection <alias>    Connection to use (default: configured default)
  -D, --database <db>         Database to use instead of the connection's
  --detailed                  Include constraints, indexes, and comments (describe only)
  --include-system            Include system/internal tables (tables, dump)
  --type <type>               Filter constraint type: pk, fk, unique, check
  --tables <list>             Comma-separated table names (dump, diff)
  --pattern <glob>            Database name filter (databases only)
  --compact                   Typed NDJSON output for schema commands

OUTPUT FORMAT
  Lists (tables, indexes, constraints) return NDJSON records by default —
  one JSON object per line; --format json/yaml wraps them in {"data": [...]}.
  Single resources (describe, search, dump) return one JSON line by default,
  pretty JSON with --format json.
  --compact: {"type":"tables","values":{...}}
  --format sql: CREATE TABLE statements (schema dump only)
  Errors: { "error": "...", "fixable_by": "agent"|"human" } to stderr.

WORKFLOW
  0. schema databases            Which databases exist (then -D <db>)
  1. schema tables               List what's available
  2. schema describe <table>     Inspect columns and types
  3. schema indexes <table>      Check index coverage
  4. schema constraints <table>  Understand relationships
  5. schema search <pattern>     Find tables/columns by name
`

// SchemaGlobals holds the global flags relevant to schema commands.
type SchemaGlobals struct {
	Connection string
	Database   string
	TimeoutMS  int
	Format     string
	Compact    bool
}

// resolveOpts returns the read-mode resolve options for schema commands.
func (g SchemaGlobals) resolveOpts() resolve.Opts {
	return resolve.Opts{Connection: g.Connection, Database: g.Database, Timeout: g.TimeoutMS}
}

// printResult outputs a single schema resource (describe, search, dump)
// honoring the resolved --format. When compact is true, the data is wrapped in
// a typed NDJSON message using the provided schemaType (e.g. "describe").
func printResult(data any, g SchemaGlobals, prune bool, schemaType string) {
	if g.Compact {
		printCompact(data, schemaType)
		return
	}
	output.PrintResult(g.Format, data, prune)
}

// printList outputs list-shaped schema data (tables, indexes, constraints) in
// the family list contract: NDJSON records by default, a {"data": [...]}
// envelope for json/yaml. Compact keeps its wrapped one-line shape.
func printList[T any](items []T, g SchemaGlobals, schemaType string) {
	if g.Compact {
		printCompact(map[string]any{schemaType: items}, schemaType)
		return
	}
	widened := make([]any, len(items))
	for i, it := range items {
		widened[i] = it
	}
	output.PrintList(g.Format, widened, nil, true)
}

// printCompact writes a single typed NDJSON line for schema output, through
// the shared funnel so it colorizes on a terminal.
func printCompact(data any, schemaType string) {
	_ = output.WriteTypedLine(os.Stdout, schemaType, data)
}

// Register adds the schema command group to root.
func Register(root *cobra.Command, globals func() SchemaGlobals) {
	schema := &cobra.Command{
		Use:   "schema",
		Short: "Explore database schema",
	}
	libcli.HandleUnknownCommand(schema, "run 'agent-sql schema usage' to see the available commands")

	registerDatabases(schema, globals)
	registerTables(schema, globals)
	registerDescribe(schema, globals)
	registerIndexes(schema, globals)
	registerConstraints(schema, globals)
	registerSearch(schema, globals)
	registerDump(schema, globals)
	registerDiff(schema, globals)

	shared.RegisterUsage(schema, "schema", usageText)

	root.AddCommand(schema)
}

func registerTables(parent *cobra.Command, globals func() SchemaGlobals) {
	var includeSystem bool

	tables := &cobra.Command{
		Use:   "tables",
		Short: "List all tables",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			g := globals()
			return shared.WithConnection(g.resolveOpts(), func(ctx context.Context, drv driver.Connection) error {
				result, err := drv.GetTables(ctx, includeSystem)
				if err != nil {
					return err
				}
				printList(result, g, "tables")
				return nil
			})
		},
	}
	tables.Flags().BoolVar(&includeSystem, "include-system", false, "Include system tables")
	parent.AddCommand(tables)
}

func registerDescribe(parent *cobra.Command, globals func() SchemaGlobals) {
	var detailed bool

	describe := &cobra.Command{
		Use:   "describe <table>",
		Short: "Describe a table's columns, types, and constraints",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			g := globals()
			return shared.WithConnection(g.resolveOpts(), func(ctx context.Context, drv driver.Connection) error {
				table := args[0]
				columns, err := drv.DescribeTable(ctx, table)
				if err != nil {
					return err
				}

				result := map[string]any{"table": table, "columns": columns}

				if detailed {
					constraints, cErr := drv.GetConstraints(ctx, table)
					if cErr != nil {
						return cErr
					}
					indexes, iErr := drv.GetIndexes(ctx, table)
					if iErr != nil {
						return iErr
					}
					result["constraints"] = constraints
					result["indexes"] = indexes
				}

				printResult(result, g, true, "describe")
				return nil
			})
		},
	}
	describe.Flags().BoolVar(&detailed, "detailed", false, "Include constraints, indexes, and comments")
	parent.AddCommand(describe)
}

func registerIndexes(parent *cobra.Command, globals func() SchemaGlobals) {
	indexes := &cobra.Command{
		Use:   "indexes [table]",
		Short: "Show indexes for a table or all tables",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			g := globals()
			return shared.WithConnection(g.resolveOpts(), func(ctx context.Context, drv driver.Connection) error {
				table := ""
				if len(args) > 0 {
					table = args[0]
				}
				result, err := drv.GetIndexes(ctx, table)
				if err != nil {
					return err
				}
				printList(result, g, "indexes")
				return nil
			})
		},
	}
	parent.AddCommand(indexes)
}

func registerConstraints(parent *cobra.Command, globals func() SchemaGlobals) {
	var constraintType string

	typeMap := map[string]driver.ConstraintType{
		"pk":     driver.ConstraintPrimaryKey,
		"fk":     driver.ConstraintForeignKey,
		"unique": driver.ConstraintUnique,
		"check":  driver.ConstraintCheck,
	}

	constraints := &cobra.Command{
		Use:   "constraints [table]",
		Short: "Show constraints for a table or all tables",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if constraintType != "" {
				if _, ok := typeMap[constraintType]; !ok {
					return fmt.Errorf(
						"invalid constraint type: %q; valid types: pk, fk, unique, check", constraintType,
					)
				}
			}

			g := globals()
			return shared.WithConnection(g.resolveOpts(), func(ctx context.Context, drv driver.Connection) error {
				table := ""
				if len(args) > 0 {
					table = args[0]
				}

				result, err := drv.GetConstraints(ctx, table)
				if err != nil {
					return err
				}

				if constraintType != "" {
					filtered := make([]driver.ConstraintInfo, 0, len(result))
					target := typeMap[constraintType]
					for _, c := range result {
						if c.Type == target {
							filtered = append(filtered, c)
						}
					}
					result = filtered
				}

				printList(result, g, "constraints")
				return nil
			})
		},
	}
	constraints.Flags().StringVar(&constraintType, "type", "", "Filter by type: pk, fk, unique, check")
	parent.AddCommand(constraints)
}

func registerSearch(parent *cobra.Command, globals func() SchemaGlobals) {
	search := &cobra.Command{
		Use:   "search <pattern>",
		Short: "Search table and column names by pattern",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			g := globals()
			return shared.WithConnection(g.resolveOpts(), func(ctx context.Context, drv driver.Connection) error {
				result, err := drv.SearchSchema(ctx, args[0])
				if err != nil {
					return err
				}
				printResult(result, g, true, "search")
				return nil
			})
		},
	}
	parent.AddCommand(search)
}
