package copier

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shhac/agent-sql/internal/driver"
	"github.com/shhac/agent-sql/internal/errors"
	"github.com/shhac/agent-sql/internal/schemadiff"
)

// Data modes of `copy --data`.
const (
	DataReplace = "replace"
	DataAppend  = "append"
	DataUpsert  = "upsert"
	DataSync    = "sync"
	DataNone    = "none"
)

// DefaultBatchRows is the default number of rows per INSERT batch.
const DefaultBatchRows = 1000

// maxBatchBytes caps one batch's payload so a statement of wide rows stays
// well inside the server's max_allowed_packet.
const maxBatchBytes = 8 << 20

// progressEvery is how often (in rows) long tables report progress.
const progressEvery = 100_000

// Options configures one copy run.
type Options struct {
	// From and To label the connections in the receipt.
	From, To         string
	Schema, ToSchema string
	Structure        string
	Data             string
	Tables           []string
	ExcludeTables    []string
	// Where maps a table name to a filter condition for its source rows.
	Where            map[string]string
	NoDelete         bool
	AllowDestructive bool
	Compare          schemadiff.Options
	BatchRows        int
	ContinueOnError  bool
	DryRun           bool
	// OnTable receives each table's record as the table completes.
	OnTable func(TableResult)
	// OnProgress receives periodic row counts for long tables.
	OnProgress func(table string, rows int64)
}

// TableResult is the per-table record copy emits.
type TableResult struct {
	Table     string `json:"table"`
	Structure string `json:"structure"`
	RowsRead  int64  `json:"rows_read"`
	Inserted  int64  `json:"inserted"`
	Updated   int64  `json:"updated"`
	Deleted   int64  `json:"deleted"`
	Upserted  int64  `json:"upserted,omitempty"`
	// Cleared is set when the target's existing rows are discarded
	// wholesale (TRUNCATE for replace, DROP for recreate). A real run does
	// not count them, which would scan every table; --dry-run counts them
	// in Deleted.
	Cleared    bool   `json:"cleared,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	DryRun     bool   `json:"dry_run,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Failure is one failed statement or table under --continue-on-error.
type Failure struct {
	Table     string `json:"table,omitempty"`
	Statement string `json:"statement,omitempty"`
	Error     string `json:"error"`
}

// Receipt is the final summary of a copy run.
type Receipt struct {
	From        string    `json:"from"`
	To          string    `json:"to"`
	Schema      string    `json:"schema"`
	ToSchema    string    `json:"to_schema"`
	Structure   string    `json:"structure"`
	Data        string    `json:"data"`
	DryRun      bool      `json:"dry_run,omitempty"`
	Tables      int       `json:"tables"`
	Statements  int       `json:"statements"`
	RowsRead    int64     `json:"rows_read"`
	Inserted    int64     `json:"inserted"`
	Updated     int64     `json:"updated"`
	Deleted     int64     `json:"deleted"`
	Upserted    int64     `json:"upserted,omitempty"`
	Cleared     int       `json:"cleared,omitempty"` // tables whose rows were discarded wholesale
	DurationMS  int64     `json:"duration_ms"`
	Failures    []Failure `json:"failures,omitempty"`
	Destructive []string  `json:"destructive,omitempty"`
	// RequiresAllowDestructive is set on a dry run whose plan the real run
	// would refuse without --allow-destructive.
	RequiresAllowDestructive bool `json:"requires_allow_destructive,omitempty"`
}

// ValidateModes checks --structure and --data.
func ValidateModes(structure, data string) error {
	switch structure {
	case schemadiff.StructureRecreate, schemadiff.StructureSync, schemadiff.StructureNone:
	case "":
		return errors.New("--structure is required", errors.FixableByAgent).
			WithHint("pass --structure recreate|sync|none: recreate drops and recreates target tables, sync applies the schema diff, none leaves structure alone")
	default:
		return errors.New(fmt.Sprintf("invalid --structure %q", structure), errors.FixableByAgent).
			WithHint("valid modes: recreate, sync, none")
	}
	switch data {
	case DataReplace, DataAppend, DataUpsert, DataSync, DataNone:
	case "":
		return errors.New("--data is required", errors.FixableByAgent).
			WithHint("pass --data replace|append|upsert|sync|none: replace truncates then inserts, append inserts, upsert inserts or updates by key, sync also deletes target-only rows, none copies no rows")
	default:
		return errors.New(fmt.Sprintf("invalid --data %q", data), errors.FixableByAgent).
			WithHint("valid modes: replace, append, upsert, sync, none")
	}
	if structure == schemadiff.StructureNone && data == DataNone {
		return errors.New("--structure none --data none does nothing", errors.FixableByAgent).
			WithHint("choose a structure mode, a data mode, or both; use `schema diff` to only compare")
	}
	return nil
}

// Plan is a prepared copy: both sides introspected, the diff classified, the
// DDL rendered and every gate checked. Execute performs it.
type Plan struct {
	opts   Options
	reader driver.SnapshotReader
	// writer is the target writer session; nil on a dry run, which only
	// reads the target (through targetReader) and so needs no write
	// permission.
	writer       driver.BulkWriter
	targetReader driver.SnapshotReader
	// targetRows reads target rows: the writer's read session, or on a
	// dry run targetReader.
	targetRows rowSource
	source     *schemadiff.Model
	target     *schemadiff.Model
	Diff       *schemadiff.Diff
	Statements []schemadiff.Statement
	// destructive describes the work that needs --allow-destructive.
	destructive []string
	// keptReferenced names the tables whose first sync delete pass kept
	// rows a foreign key still referenced; only they need the second pass.
	keptReferenced map[string]bool
}

// Prepare opens the copy sessions and plans the run. The caller must Close
// the plan.
func Prepare(ctx context.Context, src, dst driver.Connection, opts Options) (_ *Plan, err error) {
	if err := ValidateModes(opts.Structure, opts.Data); err != nil {
		return nil, err
	}
	if opts.ToSchema == "" {
		opts.ToSchema = opts.Schema
	}
	if opts.BatchRows <= 0 {
		opts.BatchRows = DefaultBatchRows
	}
	si, ti, err := Introspectors(src, dst)
	if err != nil {
		return nil, err
	}
	srcSess, ok1 := src.(driver.CopySessioner)
	dstSess, ok2 := dst.(driver.CopySessioner)
	renderer, ok3 := dst.(driver.DDLRenderer)
	if !ok1 || !ok2 || !ok3 {
		return nil, errors.New("copy is not supported for this connection", errors.FixableByAgent).WithHint(errors.HintSchemaCompare)
	}
	if err := SameTarget(ctx, si, ti, opts.Schema, opts.ToSchema); err != nil {
		return nil, err
	}

	p := &Plan{opts: opts, keptReferenced: map[string]bool{}}
	defer func() {
		if err != nil {
			p.Close()
		}
	}()
	if p.reader, err = srcSess.OpenSnapshotReader(ctx, opts.Schema); err != nil {
		return nil, sideError("source", err)
	}
	objects := introspectOpts(opts)
	source, err := p.reader.IntrospectSchema(ctx, opts.Schema, objects)
	if err != nil {
		return nil, sideError("source", err)
	}
	if opts.DryRun {
		if p.targetReader, err = dstSess.OpenSnapshotReader(ctx, opts.ToSchema); err != nil {
			return nil, sideError("target", err)
		}
		p.targetRows = p.targetReader
	} else {
		if p.writer, err = dstSess.OpenBulkWriter(ctx, opts.ToSchema, p.reader.Settings()); err != nil {
			return nil, sideError("target", err)
		}
		p.targetRows = p.writer
	}
	target, err := ti.IntrospectSchema(ctx, opts.ToSchema, objects)
	if err != nil {
		return nil, sideError("target", err)
	}
	if err := checkTablesExist(source, opts.Tables); err != nil {
		return nil, err
	}
	p.source = source.Filter(opts.Tables, opts.ExcludeTables)
	for table := range opts.Where {
		if p.source.Table(table) == nil {
			return nil, errors.New(fmt.Sprintf("--where names table %q, which is not among the copied tables", table), errors.FixableByAgent).
				WithHint("check the table name against --tables/--exclude-tables and the source schema")
		}
	}
	p.target = target.Filter(opts.Tables, opts.ExcludeTables)
	if len(opts.Tables) == 0 && !opts.Compare.DropExtra {
		// Without --drop-extra, target-only tables are only reported; a
		// copy does not touch them.
		p.target = keepTables(p.target, p.source)
	}
	p.Diff = schemadiff.Compare(p.source, p.target, opts.Compare)

	if opts.Structure != schemadiff.StructureNone {
		p.Statements, err = renderer.RenderDDL(p.Diff, schemadiff.RenderOpts{Mode: opts.Structure, TargetSchema: opts.ToSchema})
		if err != nil {
			return nil, err
		}
	}
	if err := p.checkKeys(); err != nil {
		return nil, err
	}
	if err := p.checkCompatible(); err != nil {
		return nil, err
	}
	p.collectDestructive()
	if len(p.destructive) > 0 && !opts.AllowDestructive && !opts.DryRun {
		return nil, errors.New("copy would lose data on the target: "+strings.Join(p.destructive, "; "), errors.FixableByAgent).
			WithHint("review with --dry-run, then pass --allow-destructive to proceed, or narrow the run (--exclude-tables, --data upsert, --no-delete)")
	}
	return p, nil
}

func introspectOpts(opts Options) driver.IntrospectOpts {
	if opts.Structure == schemadiff.StructureNone {
		return driver.IntrospectOpts{}
	}
	return driver.IntrospectOpts{Objects: opts.Compare.Objects}
}

// keepTables drops from target the tables that are not in source, so they
// are neither reported nor touched. Non-table objects are kept.
func keepTables(target, source *schemadiff.Model) *schemadiff.Model {
	out := *target
	out.Tables = nil
	for _, t := range target.Tables {
		if source.Table(t.Name) != nil {
			out.Tables = append(out.Tables, t)
		}
	}
	return &out
}

func checkTablesExist(m *schemadiff.Model, names []string) error {
	var missing []string
	for _, n := range names {
		if m.Table(n) == nil {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return errors.New(fmt.Sprintf("--tables names tables not in source schema %q: %s", m.Schema, strings.Join(missing, ", ")), errors.FixableByAgent).
			WithHint("list the source's tables with `schema tables -c <from> -D " + m.Schema + "`")
	}
	return nil
}

// checkKeys requires a usable row key for upsert and sync.
func (p *Plan) checkKeys() error {
	if p.opts.Data != DataUpsert && p.opts.Data != DataSync {
		return nil
	}
	var missing []string
	for _, t := range p.source.Tables {
		if t.RowKey() == nil {
			missing = append(missing, t.Name)
		}
	}
	if len(missing) > 0 {
		return errors.New(fmt.Sprintf("--data %s needs a primary key or a unique NOT NULL index; these tables have none: %s", p.opts.Data, strings.Join(missing, ", ")), errors.FixableByAgent).
			WithHint("exclude them with --exclude-tables " + strings.Join(missing, ",") + ", or copy them separately with --data replace or append")
	}
	return nil
}

// checkCompatible verifies, for --structure none, that each target table
// can take the source's rows: it exists, has every copied column, and (for
// upsert/sync) is keyed the same way.
func (p *Plan) checkCompatible() error {
	if p.opts.Structure != schemadiff.StructureNone || p.opts.Data == DataNone {
		return nil
	}
	var problems []string
	for _, st := range p.source.Tables {
		tt := p.target.Table(st.Name)
		if tt == nil {
			problems = append(problems, st.Name+" is missing on the target")
			continue
		}
		var missing []string
		for _, c := range st.CopyColumns() {
			if tc := tt.Column(c.Name); tc == nil || tc.IsGenerated() {
				missing = append(missing, c.Name)
			}
		}
		if len(missing) > 0 {
			problems = append(problems, fmt.Sprintf("%s lacks columns %s", st.Name, strings.Join(missing, ", ")))
		}
		if p.opts.Data == DataUpsert || p.opts.Data == DataSync {
			if columnNames(st.RowKey()) != columnNames(tt.RowKey()) {
				problems = append(problems, fmt.Sprintf("%s is keyed by (%s) on the source but (%s) on the target",
					st.Name, columnNames(st.RowKey()), columnNames(tt.RowKey())))
			}
		}
	}
	if len(problems) > 0 {
		return errors.New("target tables are incompatible with --structure none: "+strings.Join(problems, "; "), errors.FixableByAgent).
			WithHint("bring the target's structure in line with --structure sync, or exclude these tables")
	}
	return nil
}

func columnNames(cols []*schemadiff.Column) string { return strings.Join(names(cols), ",") }

func (p *Plan) collectDestructive() {
	if p.opts.Structure == schemadiff.StructureSync {
		for _, it := range p.Diff.Destructive() {
			desc := it.Object + " " + qualifiedItem(it)
			if it.Hint != "" {
				desc += " (" + it.Hint + ")"
			}
			p.destructive = append(p.destructive, desc)
		}
	} else {
		for _, s := range p.Statements {
			switch {
			case !s.Destructive:
			case s.Object == schemadiff.ObjectTable && p.opts.Structure == schemadiff.StructureRecreate:
				p.destructive = append(p.destructive, "table "+s.Name+" exists on the target; recreate drops it with its rows")
			default:
				p.destructive = append(p.destructive, s.Object+" "+s.Name+": "+s.SQL)
			}
		}
	}
	switch {
	case p.opts.Data == DataReplace:
		p.destructive = append(p.destructive, "--data replace truncates every target table before copying")
	case p.opts.Data == DataSync && !p.opts.NoDelete:
		p.destructive = append(p.destructive, "--data sync deletes target rows missing from the source (pass --no-delete to keep them)")
	}
}

func qualifiedItem(it schemadiff.Item) string {
	if it.Table != "" {
		return it.Table + "." + it.Name
	}
	return it.Name
}

// Close releases both copy sessions.
func (p *Plan) Close() {
	if p.reader != nil {
		_ = p.reader.Close()
	}
	if p.writer != nil {
		_ = p.writer.Close()
	}
	if p.targetReader != nil {
		_ = p.targetReader.Close()
	}
}

// rowSource reads one side's rows; SnapshotReader and BulkWriter both are.
type rowSource interface {
	Read(ctx context.Context, req driver.ReadRequest) (driver.RawRows, error)
	Count(ctx context.Context, table, where string) (int64, error)
}

// Execute runs the plan (or, on a dry run, counts what it would do).
func (p *Plan) Execute(ctx context.Context) (*Receipt, error) {
	start := time.Now()
	o := p.opts
	rc := &Receipt{From: o.From, To: o.To, Schema: o.Schema, ToSchema: o.ToSchema,
		Structure: o.Structure, Data: o.Data, DryRun: o.DryRun}
	if o.DryRun && len(p.destructive) > 0 && !o.AllowDestructive {
		rc.RequiresAllowDestructive = true
		rc.Destructive = p.destructive
	}

	results := map[string]*TableResult{}
	var order []string
	result := func(name string) *TableResult {
		r, ok := results[name]
		if !ok {
			r = &TableResult{Table: name, Structure: "unchanged", DryRun: o.DryRun}
			results[name] = r
			order = append(order, name)
		}
		return r
	}

	if o.Structure == schemadiff.StructureRecreate {
		// The DROP discards the rows of every existing table.
		for _, st := range p.source.SortedTables() {
			if p.target.Table(st.Name) != nil {
				result(st.Name).Cleared = true
			}
		}
	}

	failedTables, err := p.applyStructure(ctx, rc, result)
	if err != nil {
		return nil, err
	}

	if o.Data == DataReplace && !o.DryRun {
		if err := p.clearTables(ctx, rc, result, failedTables); err != nil {
			return nil, err
		}
	}

	if p.deletesFirst() {
		if err := p.deleteMissing(ctx, rc, result, failedTables, true); err != nil {
			return nil, err
		}
	}

	if o.Data == DataNone {
		sort.Strings(order)
		for _, name := range order {
			p.emit(rc, results[name])
		}
	} else {
		// Referenced tables first: where the target cannot switch foreign key
		// checks off (PostgreSQL without superuser), parents must be filled
		// before their children.
		// Records wait for the second delete pass when there is one, so
		// their counts are final.
		var pending []*TableResult
		for _, st := range p.source.TablesInOrder() {
			r := result(st.Name)
			pending = append(pending, r)
			if failedTables[st.Name] {
				continue
			}
			t0 := time.Now()
			err := p.copyTable(ctx, st, r)
			r.DurationMS = time.Since(t0).Milliseconds()
			if err != nil {
				if err := p.tableFailed(rc, r, failedTables, err); err != nil {
					return nil, err
				}
			}
			if !p.deletesFirst() {
				p.emit(rc, r)
			}
		}
		if p.deletesFirst() {
			// Rows the first pass kept because children still pointed at
			// them; those children have now been updated or deleted.
			if err := p.deleteMissing(ctx, rc, result, failedTables, false); err != nil {
				return nil, err
			}
			for _, r := range pending {
				p.emit(rc, r)
			}
		}
		// Tables only dropped (--drop-extra) still get a record.
		for _, name := range order {
			if p.source.Table(name) == nil {
				p.emit(rc, results[name])
			}
		}
	}
	rc.DurationMS = time.Since(start).Milliseconds()
	return rc, nil
}

func (p *Plan) emit(rc *Receipt, r *TableResult) {
	rc.Tables++
	rc.RowsRead += r.RowsRead
	rc.Inserted += r.Inserted
	rc.Updated += r.Updated
	rc.Deleted += r.Deleted
	rc.Upserted += r.Upserted
	if r.Cleared {
		rc.Cleared++
	}
	if p.opts.OnTable != nil {
		p.opts.OnTable(*r)
	}
}

// applyStructure runs the rendered DDL in order. It returns the tables whose
// DDL failed under --continue-on-error, so their data phase is skipped.
func (p *Plan) applyStructure(ctx context.Context, rc *Receipt, result func(string) *TableResult) (map[string]bool, error) {
	failed := map[string]bool{}
	for _, s := range p.Statements {
		if s.Object == "schema" {
			continue // the writer session is already scoped to the schema
		}
		table := s.Table
		if s.Object == schemadiff.ObjectTable {
			table = s.Name
		}
		if table != "" && s.Object != schemadiff.ObjectTrigger {
			r := result(table)
			r.Structure = structureLabel(p.opts.Structure, s, r.Structure)
		}
		rc.Statements++
		if p.opts.DryRun {
			continue
		}
		if err := p.writer.Exec(ctx, s.SQL); err != nil {
			err = sideError("target", err)
			if !p.opts.ContinueOnError {
				return nil, withStatement(err, s.SQL)
			}
			rc.Failures = append(rc.Failures, Failure{Table: table, Statement: s.SQL, Error: err.Error()})
			if table != "" {
				failed[table] = true
				result(table).Error = err.Error()
			}
		}
	}
	return failed, nil
}

// tableFailed handles a table's data-phase error: fatal unless
// --continue-on-error, in which case it is recorded on the receipt and the
// table marked failed so later phases skip it.
func (p *Plan) tableFailed(rc *Receipt, r *TableResult, failed map[string]bool, err error) error {
	err = errors.Prefix("table "+r.Table, err)
	if !p.opts.ContinueOnError {
		return err
	}
	r.Error = err.Error()
	rc.Failures = append(rc.Failures, Failure{Table: r.Table, Error: err.Error()})
	failed[r.Table] = true
	return nil
}

func withStatement(err error, stmt string) error {
	var qe *errors.QueryError
	if errors.As(err, &qe) {
		out := *qe
		if out.Hint == "" {
			out.Hint = "failed statement: " + stmt + "; earlier DDL statements stay applied; pass --continue-on-error to skip failures"
		} else {
			out.Hint += " (failed statement: " + stmt + "; earlier DDL statements stay applied)"
		}
		return &out
	}
	return err
}

func structureLabel(mode string, s schemadiff.Statement, current string) string {
	upper := strings.ToUpper(s.SQL)
	switch {
	case mode == schemadiff.StructureRecreate && s.Object == schemadiff.ObjectTable &&
		(strings.HasPrefix(upper, "DROP TABLE IF EXISTS") || strings.HasPrefix(upper, "CREATE TABLE")):
		if (strings.HasPrefix(upper, "DROP") && s.Destructive) || current == "recreated" {
			return "recreated"
		}
		return "created"
	case strings.HasPrefix(upper, "CREATE TABLE"):
		return "created"
	case strings.HasPrefix(upper, "DROP TABLE"):
		return "dropped"
	case current == "created" || current == "recreated" || current == "dropped":
		return current
	}
	return "altered"
}

func (p *Plan) batchRows(ncols int) int {
	n := p.opts.BatchRows
	if ncols > 0 && p.writer != nil {
		if limit := p.writer.MaxParams() / ncols; limit < n {
			n = limit
		}
	}
	if n < 1 {
		n = 1
	}
	return n
}
