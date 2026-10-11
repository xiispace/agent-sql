// Package copy implements `agent-sql copy`: structure and data from one
// connection to another, with the credentials staying inside the process.
package copy

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/shhac/agent-sql/internal/cli/shared"
	"github.com/shhac/agent-sql/internal/copier"
	"github.com/shhac/agent-sql/internal/errors"
	"github.com/shhac/agent-sql/internal/output"
	libcli "github.com/shhac/lib-agent-cli/cli"
	agentmcp "github.com/shhac/lib-agent-mcp"
)

// Register adds the copy command to root.
func Register(root *cobra.Command, globals func() *shared.GlobalFlags) {
	var (
		flags           shared.CompareFlags
		structure       string
		data            string
		where           []string
		noDelete        bool
		batchRows       int
		continueOnError bool
		dryRun          bool
	)

	cmd := &cobra.Command{
		Use:   "copy",
		Short: "Copy structure and/or data from one connection to another",
		Long:  "Copy structure and/or data from --from to --to. Writes only to --to, through the same write gate as `run --write`. Run `agent-sql copy usage` for the full reference.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := copier.ValidateModes(structure, data); err != nil {
				return err
			}
			if err := flags.Validate(); err != nil {
				return err
			}
			compare, err := flags.Options()
			if err != nil {
				return err
			}
			filters, err := parseWhere(where)
			if err != nil {
				return err
			}
			g := globals()
			if g.Format == "sql" && !dryRun {
				return errors.New("--format sql needs --dry-run", errors.FixableByAgent).
					WithHint("copy --dry-run --format sql prints the DDL that would run")
			}
			ctx, cancel := shared.MakeContext(g.TimeoutMS)
			defer cancel()

			// A dry run only reads the target: no write permission needed.
			src, dst, err := flags.ResolvePair(ctx, g.Database, g.TimeoutMS, !dryRun)
			if err != nil {
				return err
			}
			defer func() { _ = src.Close(); _ = dst.Close() }()

			ndjson := output.ResolveFormat(g.Format) == output.FormatNDJSON
			var records []any
			plan, err := copier.Prepare(ctx, src, dst, copier.Options{
				From: flags.From, To: flags.To, Schema: flags.Schema, ToSchema: flags.ToSchema,
				Structure: structure, Data: data,
				Tables: flags.Tables, ExcludeTables: flags.ExcludeTables, Where: filters,
				NoDelete: noDelete, AllowDestructive: flags.AllowDestructive, Compare: compare,
				BatchRows: batchRows, ContinueOnError: continueOnError, DryRun: dryRun,
				OnTable: func(r copier.TableResult) {
					if g.Format == "sql" {
						return
					}
					if ndjson {
						output.PrintResult(g.Format, r, false)
						return
					}
					records = append(records, r)
				},
				OnProgress: func(table string, rows int64) {
					output.Notice(fmt.Sprintf("%s: %d rows copied so far", table, rows), "")
				},
			})
			if err != nil {
				return err
			}
			defer plan.Close()

			if g.Format == "sql" {
				output.WriteSQLScript(plan.Statements, flags.AllowDestructive)
				return nil
			}
			receipt, err := plan.Execute(ctx)
			if err != nil {
				return err
			}
			output.PrintListWithSummary(g.Format, records, receipt) // NDJSON records already streamed
			if len(receipt.Failures) > 0 {
				return errors.New(fmt.Sprintf("copy finished with %d failure(s); see the receipt's failures", len(receipt.Failures)), errors.FixableByAgent)
			}
			return nil
		},
	}
	f := cmd.Flags()
	flags.Register(f)
	f.StringVar(&structure, "structure", "", "Structure mode: recreate, sync or none (required)")
	f.StringVar(&data, "data", "", "Data mode: replace, append, upsert, sync or none (required)")
	f.StringArrayVar(&where, "where", nil, "Filter one table's source rows: 'table=<condition>' (repeatable)")
	f.BoolVar(&noDelete, "no-delete", false, "With --data sync, keep target rows missing from the source")
	f.IntVar(&batchRows, "batch-rows", copier.DefaultBatchRows, "Rows per INSERT batch (capped by the placeholder limit)")
	f.BoolVar(&continueOnError, "continue-on-error", false, "Record a failed table or DDL statement and move on")
	f.BoolVar(&dryRun, "dry-run", false, "Plan and count without writing anything")
	libcli.AllowFormats(cmd, "sql")

	shared.RegisterUsage(cmd, "copy", usageText)
	// copy is a leaf command with a `usage` child. Hide the child from the
	// MCP surface so copy is exposed as one tool taking flags rather than as
	// a group dispatching subcommands; it writes, so hosts confirm first.
	for _, c := range cmd.Commands() {
		if c.Name() == "usage" {
			agentmcp.Skip(c)
		}
	}
	agentmcp.Destructive(cmd)
	root.AddCommand(cmd)
}

// parseWhere parses repeated 'table=<condition>' flags.
func parseWhere(raw []string) (map[string]string, error) {
	out := map[string]string{}
	for _, w := range raw {
		i := strings.IndexByte(w, '=')
		if i <= 0 || strings.TrimSpace(w[i+1:]) == "" {
			return nil, errors.New(fmt.Sprintf("invalid --where %q", w), errors.FixableByAgent).
				WithHint("use --where 'table=<condition>', e.g. --where \"orders=created_at >= '2026-01-01'\"")
		}
		table := strings.TrimSpace(w[:i])
		if _, dup := out[table]; dup {
			return nil, errors.New(fmt.Sprintf("--where given twice for table %q", table), errors.FixableByAgent).
				WithHint("combine the conditions with AND in one --where")
		}
		out[table] = strings.TrimSpace(w[i+1:])
	}
	return out, nil
}
