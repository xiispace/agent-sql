package shared

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/pflag"

	"github.com/shhac/agent-sql/internal/driver"
	"github.com/shhac/agent-sql/internal/errors"
	"github.com/shhac/agent-sql/internal/resolve"
	"github.com/shhac/agent-sql/internal/schemadiff"
)

// CompareFlags are the flags `schema diff` and `copy` share: the two
// connections, the schema pair, table selection and the comparison options.
type CompareFlags struct {
	From, To         string
	Schema, ToSchema string
	Tables           []string
	ExcludeTables    []string
	Objects          string

	CompareAutoIncrement  bool
	CompareColumnPosition bool
	IgnoreComments        bool
	IgnoreCharset         bool
	IgnoreDefiner         bool
	CompareEngine         bool
	ComparePartitions     bool
	DropExtra             bool
	AllowDestructive      bool
}

// Register binds the flags onto fs.
func (f *CompareFlags) Register(fs *pflag.FlagSet) {
	defaults := schemadiff.DefaultOptions()
	fs.StringVar(&f.From, "from", "", "Source connection: alias, file path or URL (required; no default fallback)")
	fs.StringVar(&f.To, "to", "", "Target connection: alias, file path or URL (required; no default fallback)")
	fs.StringVar(&f.Schema, "schema", "", "Source schema (database on MySQL) (required)")
	fs.StringVar(&f.ToSchema, "to-schema", "", "Target schema (default: same as --schema)")
	fs.StringSliceVar(&f.Tables, "tables", nil, "Only these tables (comma-separated)")
	fs.StringSliceVar(&f.ExcludeTables, "exclude-tables", nil, "Skip these tables (comma-separated)")
	fs.StringVar(&f.Objects, "objects", "tables", "Object kinds: tables,views,routines,triggers,events")
	fs.BoolVar(&f.CompareAutoIncrement, "compare-auto-increment", false, "Compare table AUTO_INCREMENT values")
	fs.BoolVar(&f.CompareColumnPosition, "compare-column-position", false, "Compare column order")
	fs.BoolVar(&f.IgnoreComments, "ignore-comments", false, "Ignore table, column and index comments")
	fs.BoolVar(&f.IgnoreCharset, "ignore-charset", false, "Ignore character sets and collations")
	fs.BoolVar(&f.IgnoreDefiner, "ignore-definer", false, "Ignore DEFINER on views, routines, triggers and events (and omit it from emitted DDL)")
	fs.BoolVar(&f.CompareEngine, "compare-engine", defaults.CompareEngine, "Compare storage engines")
	fs.BoolVar(&f.ComparePartitions, "compare-partitions", defaults.ComparePartitions, "Compare partitioning")
	fs.BoolVar(&f.DropExtra, "drop-extra", false, "Drop objects that exist only in the target (destructive)")
	fs.BoolVar(&f.AllowDestructive, "allow-destructive", false, "Allow changes that can lose data on the target")
}

// Validate checks the required flags.
func (f *CompareFlags) Validate() error {
	var missing []string
	if strings.TrimSpace(f.From) == "" {
		missing = append(missing, "--from")
	}
	if strings.TrimSpace(f.To) == "" {
		missing = append(missing, "--to")
	}
	if strings.TrimSpace(f.Schema) == "" {
		missing = append(missing, "--schema")
	}
	if len(missing) > 0 {
		return errors.New("missing required flags: "+strings.Join(missing, ", "), errors.FixableByAgent).
			WithHint("--from and --to take an alias, file path or URL and never fall back to -c or the default connection; --schema names the source schema")
	}
	if f.ToSchema == "" {
		f.ToSchema = f.Schema
	}
	return nil
}

// Options returns the comparison options the flags select.
func (f *CompareFlags) Options() (schemadiff.Options, error) {
	o := schemadiff.Options{
		CompareAutoIncrement:  f.CompareAutoIncrement,
		CompareColumnPosition: f.CompareColumnPosition,
		IgnoreComments:        f.IgnoreComments,
		IgnoreCharset:         f.IgnoreCharset,
		IgnoreDefiner:         f.IgnoreDefiner,
		CompareEngine:         f.CompareEngine,
		ComparePartitions:     f.ComparePartitions,
		DropExtra:             f.DropExtra,
	}
	for _, kind := range strings.Split(f.Objects, ",") {
		switch strings.TrimSpace(strings.ToLower(kind)) {
		case "", "tables", "table":
		case "views", "view":
			o.Objects.Views = true
		case "routines", "routine":
			o.Objects.Routines = true
		case "triggers", "trigger":
			o.Objects.Triggers = true
		case "events", "event":
			o.Objects.Events = true
		default:
			return o, errors.New(fmt.Sprintf("invalid --objects kind %q", kind), errors.FixableByAgent).
				WithHint("valid kinds: tables, views, routines, triggers, events")
		}
	}
	if (o.Objects.Routines || o.Objects.Events) && len(f.Tables) > 0 {
		return o, errors.New("--tables cannot select routines or events: they belong to no table", errors.FixableByAgent).
			WithHint("compare routines and events in a run without --tables (--exclude-tables is fine)")
	}
	return o, nil
}

// ResolvePair resolves --from read-only and --to (read-only unless
// writeTarget, which goes through the same write gate as `run --write`).
// database (-D) overrides the database on both sides. The caller closes both.
func (f *CompareFlags) ResolvePair(ctx context.Context, database string, timeout int, writeTarget bool) (driver.Connection, driver.Connection, error) {
	src, err := resolve.Resolve(ctx, resolve.Opts{Connection: f.From, Database: database, Timeout: timeout})
	if err != nil {
		return nil, nil, errors.Prefix("--from", err)
	}
	dst, err := resolve.Resolve(ctx, resolve.Opts{Connection: f.To, Database: database, Write: writeTarget, Timeout: timeout})
	if err != nil {
		_ = src.Close()
		return nil, nil, errors.Prefix("--to", err)
	}
	return src, dst, nil
}
