package schema

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/spf13/cobra"

	"github.com/shhac/agent-sql/internal/cli/shared"
	"github.com/shhac/agent-sql/internal/driver"
	"github.com/shhac/agent-sql/internal/errors"
	"github.com/shhac/agent-sql/internal/output"
)

func registerDatabases(parent *cobra.Command, globals func() SchemaGlobals) {
	var includeSystem bool
	var pattern string

	databases := &cobra.Command{
		Use:   "databases",
		Short: "List the databases the credential can see on the server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if pattern != "" {
				if _, err := path.Match(pattern, ""); err != nil {
					return errors.New(fmt.Sprintf("invalid --pattern %q: %v", pattern, err), errors.FixableByAgent).
						WithHint("use a glob such as 'newrails_*'")
				}
			}
			g := globals()
			// The listing ignores -D. It connects through the connection's
			// own database, and on PostgreSQL/CockroachDB, when that one no
			// longer exists, falls back to the driver's default (postgres,
			// defaultdb), so it still lists what is there.
			opts := g.resolveOpts()
			opts.Database = ""
			opts.FallbackDatabase = true
			return shared.WithConnection(opts, func(ctx context.Context, drv driver.Connection) error {
				lister, ok := drv.(driver.DatabaseLister)
				if !ok {
					return errors.New("schema databases is not supported for this connection", errors.FixableByAgent).
						WithHint("sqlite and duckdb connections are a single database file")
				}
				all, err := lister.ListDatabases(ctx, includeSystem)
				if err != nil {
					return err
				}
				result := filterDatabases(all, pattern)
				if len(all) == 0 {
					output.Notice("no databases visible to this credential",
						"the server lists only databases the account has privileges on")
				}
				printList(result, g, "databases")
				return nil
			})
		},
	}
	databases.Flags().BoolVar(&includeSystem, "include-system", false, "Include system databases")
	databases.Flags().StringVar(&pattern, "pattern", "", "Case-insensitive glob filter on database names (e.g. 'newrails_*')")
	parent.AddCommand(databases)
}

// filterDatabases keeps the databases whose names match the case-insensitive
// glob; an empty pattern keeps all of them.
func filterDatabases(all []driver.DatabaseInfo, pattern string) []driver.DatabaseInfo {
	if pattern == "" {
		return all
	}
	p := strings.ToLower(pattern)
	out := make([]driver.DatabaseInfo, 0, len(all))
	for _, d := range all {
		if ok, _ := path.Match(p, strings.ToLower(d.Name)); ok {
			out = append(out, d)
		}
	}
	return out
}
