package schema

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/shhac/agent-sql/internal/cli/shared"
	"github.com/shhac/agent-sql/internal/copier"
	"github.com/shhac/agent-sql/internal/driver"
	"github.com/shhac/agent-sql/internal/errors"
	"github.com/shhac/agent-sql/internal/output"
	"github.com/shhac/agent-sql/internal/schemadiff"
	libcli "github.com/shhac/lib-agent-cli/cli"
)

// diffReceipt is the summary line that follows the diff records.
type diffReceipt struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Schema   string `json:"schema"`
	ToSchema string `json:"to_schema"`
	schemadiff.Counts
}

func registerDiff(parent *cobra.Command, globals func() SchemaGlobals) {
	var flags shared.CompareFlags
	var showIdentical bool

	diff := &cobra.Command{
		Use:   "diff",
		Short: "Compare a schema between two connections (read-only on both)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := flags.Validate(); err != nil {
				return err
			}
			opts, err := flags.Options()
			if err != nil {
				return err
			}
			g := globals()
			ctx, cancel := shared.MakeContext(g.TimeoutMS)
			defer cancel()

			src, dst, err := flags.ResolvePair(ctx, g.Database, g.TimeoutMS, false)
			if err != nil {
				return err
			}
			defer func() { _ = src.Close(); _ = dst.Close() }()

			d, err := computeDiff(ctx, src, dst, &flags, opts)
			if err != nil {
				return err
			}
			if g.Format == "sql" {
				return printDiffSQL(dst, d, flags.ToSchema, flags.AllowDestructive)
			}
			items := make([]any, 0, len(d.Items))
			for _, it := range d.Items {
				if it.Status == schemadiff.Identical && !showIdentical {
					continue
				}
				items = append(items, it)
			}
			receipt := diffReceipt{From: flags.From, To: flags.To, Schema: flags.Schema, ToSchema: flags.ToSchema, Counts: d.Counts()}
			output.PrintListWithSummary(g.Format, items, receipt)
			return nil
		},
	}
	flags.Register(diff.Flags())
	diff.Flags().BoolVar(&showIdentical, "show-identical", false, "Also emit identical objects")
	libcli.AllowFormats(diff, "sql")
	parent.AddCommand(diff)
}

// computeDiff introspects both sides and classifies the selected objects.
func computeDiff(ctx context.Context, src, dst driver.Connection, flags *shared.CompareFlags, opts schemadiff.Options) (*schemadiff.Diff, error) {
	si, ti, err := copier.Introspectors(src, dst)
	if err != nil {
		return nil, err
	}
	iopts := driver.IntrospectOpts{Objects: opts.Objects}
	source, err := si.IntrospectSchema(ctx, flags.Schema, iopts)
	if err != nil {
		return nil, errors.Prefix("source", err)
	}
	target, err := ti.IntrospectSchema(ctx, flags.ToSchema, iopts)
	if err != nil {
		return nil, errors.Prefix("target", err)
	}
	var missing []string
	for _, n := range flags.Tables {
		if source.Table(n) == nil && target.Table(n) == nil {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return nil, errors.New(fmt.Sprintf("--tables names tables on neither side: %s", strings.Join(missing, ", ")), errors.FixableByAgent)
	}
	return schemadiff.Compare(source.Filter(flags.Tables, flags.ExcludeTables),
		target.Filter(flags.Tables, flags.ExcludeTables), opts), nil
}

// printDiffSQL prints the DDL script that brings the target in line.
func printDiffSQL(dst driver.Connection, d *schemadiff.Diff, toSchema string, allowDestructive bool) error {
	renderer, ok := dst.(driver.DDLRenderer)
	if !ok {
		return errors.New("--format sql is not supported for this connection", errors.FixableByAgent).
			WithHint(errors.HintSchemaCompare)
	}
	stmts, err := renderer.RenderDDL(d, schemadiff.RenderOpts{Mode: schemadiff.StructureSync, TargetSchema: toSchema})
	if err != nil {
		return err
	}
	output.WriteSQLScript(stmts, allowDestructive)
	return nil
}
