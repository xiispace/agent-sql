package schemadiff

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Status classifies one object in a diff.
type Status string

const (
	OnlyInSource Status = "only_in_source"
	OnlyInTarget Status = "only_in_target"
	Different    Status = "different"
	Identical    Status = "identical"
)

// Actions a diff item implies for the target.
const (
	ActionCreate   = "create"
	ActionAlter    = "alter"
	ActionAdd      = "add"
	ActionModify   = "modify"
	ActionDrop     = "drop"
	ActionRecreate = "recreate"
	ActionReplace  = "replace"
	ActionNone     = "none"
)

// Options are the comparison options of `schema diff` and `copy`, with
// Navicat's defaults (see DefaultOptions).
type Options struct {
	CompareAutoIncrement  bool
	CompareColumnPosition bool
	IgnoreComments        bool
	IgnoreCharset         bool
	IgnoreDefiner         bool
	CompareEngine         bool
	ComparePartitions     bool
	// DropExtra turns objects that exist only in the target into drops.
	// Columns of a table present on both sides are always dropped (the
	// drop is destructive); everything else needs DropExtra.
	DropExtra bool
	Objects   ObjectSet
}

// DefaultOptions returns the default comparison options.
func DefaultOptions() Options {
	return Options{CompareEngine: true, ComparePartitions: true}
}

// Change is one differing attribute: From is the target's current value, To
// the source's (the value applying the change sets).
type Change struct {
	From any `json:"from"`
	To   any `json:"to"`
}

// Item is one diff record.
type Item struct {
	Object      string            `json:"object"`
	Table       string            `json:"table,omitempty"`
	Name        string            `json:"name"`
	Status      Status            `json:"status"`
	Destructive bool              `json:"destructive"`
	Action      string            `json:"action"`
	Changes     map[string]Change `json:"changes,omitempty"`
	Hint        string            `json:"hint,omitempty"`
}

// Diff is the classified comparison of two schema models.
type Diff struct {
	Source  *Model
	Target  *Model
	Options Options
	Items   []Item
}

// Counts summarises a diff for the receipt. Identical objects count once per
// top-level object (table, view, routine, trigger, event); differences count
// once per record.
type Counts struct {
	OnlyInSource int `json:"only_in_source"`
	OnlyInTarget int `json:"only_in_target"`
	Different    int `json:"different"`
	Identical    int `json:"identical"`
	Destructive  int `json:"destructive"`
}

// Counts tallies the diff's items by status.
func (d *Diff) Counts() Counts {
	var c Counts
	for _, it := range d.Items {
		switch it.Status {
		case OnlyInSource:
			c.OnlyInSource++
		case OnlyInTarget:
			c.OnlyInTarget++
		case Different:
			c.Different++
		case Identical:
			c.Identical++
		}
		if it.Destructive {
			c.Destructive++
		}
	}
	return c
}

// Destructive returns the items whose application can lose data.
func (d *Diff) Destructive() []Item {
	var out []Item
	for _, it := range d.Items {
		if it.Destructive {
			out = append(out, it)
		}
	}
	return out
}

// HasChanges reports whether applying the diff would change the target.
func (d *Diff) HasChanges() bool {
	for _, it := range d.Items {
		if it.Status != Identical && it.Action != ActionNone {
			return true
		}
	}
	return false
}

// TableItems returns the items that belong to the named table (the table
// record itself and its columns, indexes and foreign keys).
func (d *Diff) TableItems(name string) []Item {
	var out []Item
	for _, it := range d.Items {
		if (it.Object == ObjectTable && it.Name == name) || (it.Table == name && it.Object != ObjectTrigger) {
			out = append(out, it)
		}
	}
	return out
}

// Changes returns the items of the given object that would change the
// target (Action not ActionNone) and have one of the given statuses.
func (d *Diff) Changes(object string, statuses ...Status) []Item {
	var out []Item
	for _, it := range d.Items {
		if it.Object != object || it.Action == ActionNone {
			continue
		}
		for _, s := range statuses {
			if it.Status == s {
				out = append(out, it)
				break
			}
		}
	}
	return out
}

// AlteredTables lists, in name order, the tables on both sides with at
// least one table, column, index or check change.
func (d *Diff) AlteredTables() []string {
	seen := map[string]bool{}
	var out []string
	for _, it := range d.Items {
		if it.Status == Identical || it.Action == ActionNone {
			continue
		}
		name := ""
		switch it.Object {
		case ObjectTable:
			if it.Status == Different {
				name = it.Name
			}
		case ObjectColumn, ObjectIndex, ObjectCheck:
			name = it.Table
		}
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// DroppedTables lists the target-only tables the diff drops, dependents
// first (reverse TableOrder), so each drop precedes the tables it references.
func (d *Diff) DroppedTables() []string {
	var dropped []string
	for _, it := range d.Changes(ObjectTable, OnlyInTarget) {
		if it.Action == ActionDrop {
			dropped = append(dropped, it.Name)
		}
	}
	order := TableOrder(d.Target, dropped)
	for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
		order[i], order[j] = order[j], order[i]
	}
	return order
}

// Compare classifies every object of src against dst.
func Compare(src, dst *Model, opts Options) *Diff {
	d := &Diff{Source: src, Target: dst, Options: opts}
	d.compareTables()
	if opts.Objects.Views {
		d.compareDefinitions(ObjectView, viewDefs(src), viewDefs(dst))
	}
	if opts.Objects.Routines {
		d.compareDefinitions(ObjectRoutine, routineDefs(src), routineDefs(dst))
	}
	if opts.Objects.Triggers {
		d.compareDefinitions(ObjectTrigger, triggerDefs(src), triggerDefs(dst))
	}
	if opts.Objects.Events {
		d.compareDefinitions(ObjectEvent, eventDefs(src), eventDefs(dst))
	}
	return d
}

func (d *Diff) add(it Item) { d.Items = append(d.Items, it) }

func (d *Diff) extra(object, table, name string) Item {
	it := Item{Object: object, Table: table, Name: name, Status: OnlyInTarget, Action: ActionNone,
		Hint: "pass --drop-extra to drop"}
	if d.Options.DropExtra {
		it.Action = ActionDrop
		it.Destructive = true
		it.Hint = fmt.Sprintf("dropping %s %s loses it from the target", object, name)
	}
	return it
}

func (d *Diff) compareTables() {
	for _, st := range d.Source.SortedTables() {
		tt := d.Target.Table(st.Name)
		if tt == nil {
			d.add(Item{Object: ObjectTable, Name: st.Name, Status: OnlyInSource, Action: ActionCreate})
			continue
		}
		before := len(d.Items)
		d.compareTable(st, tt)
		if len(d.Items) == before {
			d.add(Item{Object: ObjectTable, Name: st.Name, Status: Identical, Action: ActionNone})
		}
	}
	for _, tt := range d.Target.SortedTables() {
		if d.Source.Table(tt.Name) == nil {
			d.add(d.extra(ObjectTable, "", tt.Name))
		}
	}
}

func (d *Diff) compareTable(st, tt *Table) {
	o := d.Options
	changes := map[string]Change{}
	if o.CompareEngine && !strings.EqualFold(st.Engine, tt.Engine) {
		changes["engine"] = Change{tt.Engine, st.Engine}
	}
	if !o.IgnoreCharset {
		if !strings.EqualFold(st.Charset, tt.Charset) {
			changes["charset"] = Change{tt.Charset, st.Charset}
		}
		if !strings.EqualFold(st.Collation, tt.Collation) {
			changes["collation"] = Change{tt.Collation, st.Collation}
		}
	}
	if !o.IgnoreComments && st.Comment != tt.Comment {
		changes["comment"] = Change{tt.Comment, st.Comment}
	}
	if o.CompareAutoIncrement && st.AutoIncrement != tt.AutoIncrement {
		changes["auto_increment"] = Change{tt.AutoIncrement, st.AutoIncrement}
	}
	if o.ComparePartitions && normalizeSpace(st.Partitioning) != normalizeSpace(tt.Partitioning) {
		changes["partitioning"] = Change{tt.Partitioning, st.Partitioning}
	}
	if len(changes) > 0 {
		d.add(Item{Object: ObjectTable, Name: st.Name, Status: Different, Action: ActionAlter, Changes: changes})
	}

	d.compareColumns(st, tt)
	d.compareIndexes(st, tt)
	d.compareForeignKeys(st, tt)
	d.compareChecks(st, tt)
}

func (d *Diff) compareColumns(st, tt *Table) {
	var added []*Column
	for _, sc := range st.Columns {
		tc := tt.Column(sc.Name)
		if tc == nil {
			added = append(added, sc)
			d.add(Item{Object: ObjectColumn, Table: st.Name, Name: sc.Name, Status: OnlyInSource, Action: ActionAdd})
			continue
		}
		if it, ok := d.compareColumn(st, tt, sc, tc); ok {
			d.add(it)
		}
	}
	for _, tc := range tt.Columns {
		if st.Column(tc.Name) != nil {
			continue
		}
		it := Item{Object: ObjectColumn, Table: st.Name, Name: tc.Name, Status: OnlyInTarget,
			Action: ActionDrop, Destructive: true,
			Hint: fmt.Sprintf("dropping column %s loses its data", tc.Name)}
		if r := renameCandidate(tc, added); r != nil {
			it.Hint = fmt.Sprintf("dropping column %s loses its data; it may be a rename of %s (same type, adjacent position), which agent-sql does not detect — rename it on the target first to keep the data", tc.Name, r.Name)
		}
		d.add(it)
	}
}

// renameCandidate returns a source-only column that looks like a rename of
// the target-only column dropped: same type, ordinal position within one.
func renameCandidate(dropped *Column, added []*Column) *Column {
	for _, a := range added {
		diff := a.Position - dropped.Position
		if normalizeType(a.Type) == normalizeType(dropped.Type) && diff >= -1 && diff <= 1 {
			return a
		}
	}
	return nil
}

func (d *Diff) compareColumn(st, tt *Table, sc, tc *Column) (Item, bool) {
	o := d.Options
	changes := map[string]Change{}
	var reasons []string
	destructive := false

	if normalizeType(unqualify(sc.Type, d.Source.Schema)) != normalizeType(unqualify(tc.Type, d.Target.Schema)) {
		changes["type"] = Change{tc.Type, sc.Type}
		if lossy, why := typeChangeLoss(tc.Type, sc.Type); lossy {
			destructive = true
			reasons = append(reasons, why)
		}
	}
	if sc.Nullable != tc.Nullable {
		changes["nullable"] = Change{tc.Nullable, sc.Nullable}
		if !sc.Nullable {
			destructive = true
			reasons = append(reasons, "NULL to NOT NULL fails or rewrites existing NULLs")
		}
	}
	if !d.sameDefault(sc, tc) {
		changes["default"] = Change{defaultText(tc), defaultText(sc)}
	}
	if sc.AutoIncrement != tc.AutoIncrement {
		changes["auto_increment"] = Change{tc.AutoIncrement, sc.AutoIncrement}
	}
	if !strings.EqualFold(normalizeExpr(sc.OnUpdate), normalizeExpr(tc.OnUpdate)) {
		changes["on_update"] = Change{tc.OnUpdate, sc.OnUpdate}
	}
	if normalizeExpr(sc.Generated) != normalizeExpr(tc.Generated) || sc.GeneratedStored != tc.GeneratedStored {
		changes["generated"] = Change{generatedText(tc), generatedText(sc)}
	}
	if sc.Invisible != tc.Invisible {
		changes["invisible"] = Change{tc.Invisible, sc.Invisible}
	}
	if !strings.EqualFold(sc.Identity, tc.Identity) {
		changes["identity"] = Change{tc.Identity, sc.Identity}
	}
	if !o.IgnoreCharset {
		if !strings.EqualFold(sc.Charset, tc.Charset) {
			changes["charset"] = Change{tc.Charset, sc.Charset}
			if tc.Charset != "" {
				destructive = true
				reasons = append(reasons, fmt.Sprintf("converting %s to %s can lose characters", tc.Charset, sc.Charset))
			}
		}
		if !strings.EqualFold(sc.Collation, tc.Collation) {
			changes["collation"] = Change{tc.Collation, sc.Collation}
		}
	}
	if !o.IgnoreComments && sc.Comment != tc.Comment {
		changes["comment"] = Change{tc.Comment, sc.Comment}
	}
	if o.CompareColumnPosition {
		if sp, tp := PreviousColumn(st, sc), PreviousColumn(tt, tc); sp != tp {
			changes["after"] = Change{tp, sp}
		}
	}
	if len(changes) == 0 {
		return Item{}, false
	}
	return Item{Object: ObjectColumn, Table: st.Name, Name: sc.Name, Status: Different,
		Action: ActionModify, Destructive: destructive, Changes: changes,
		Hint: strings.Join(reasons, "; ")}, true
}

// PreviousColumn returns the name of the column before c ("" when first),
// which is how column position is compared (absolute ordinals shift whenever
// either side has extra columns) and how DDL places a column (AFTER prev).
func PreviousColumn(t *Table, c *Column) string {
	prev := ""
	for _, x := range t.Columns {
		if x.Name == c.Name {
			return prev
		}
		prev = x.Name
	}
	return prev
}

func (d *Diff) sameDefault(a, b *Column) bool {
	if (a.Default == nil) != (b.Default == nil) {
		return false
	}
	if a.Default == nil {
		return true
	}
	if a.DefaultIsExpr || b.DefaultIsExpr {
		// Expressions may name schema objects (a PostgreSQL serial's
		// sequence), so each side's own schema qualifier is removed.
		return normalizeExpr(unqualify(*a.Default, d.Source.Schema)) == normalizeExpr(unqualify(*b.Default, d.Target.Schema))
	}
	return *a.Default == *b.Default
}

func defaultText(c *Column) any {
	if c.Default == nil {
		return nil
	}
	return *c.Default
}

func generatedText(c *Column) any {
	if c.Generated == "" {
		return nil
	}
	kind := "VIRTUAL"
	if c.GeneratedStored {
		kind = "STORED"
	}
	return c.Generated + " " + kind
}

var trailingCallParens = regexp.MustCompile(`\(\)`)

// normalizeExpr makes server-rendered expressions comparable across
// versions: case, whitespace, redundant outer parentheses, and MariaDB's
// "current_timestamp()" spelling.
func normalizeExpr(s string) string {
	s = strings.ToLower(normalizeSpace(s))
	s = trailingCallParens.ReplaceAllString(s, "")
	for len(s) >= 2 && s[0] == '(' && matchingParen(s) == len(s)-1 {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

var intDisplayWidth = regexp.MustCompile(`^(tinyint|smallint|mediumint|int|integer|bigint)\(\d+\)`)

// normalizeType makes column types comparable across servers: case, and the
// integer display width MySQL 8 no longer reports (MariaDB does), which is
// cosmetic unless ZEROFILL pads with it.
func normalizeType(t string) string {
	t = strings.ToLower(normalizeSpace(t))
	if !strings.Contains(t, "zerofill") {
		t = intDisplayWidth.ReplaceAllString(t, "$1")
	}
	return t
}

// normalizeRule makes a foreign key rule comparable: case, spacing, and the
// model's RuleAliases. Equivalences are dialect-specific (InnoDB enforces NO
// ACTION like RESTRICT, PostgreSQL does not), so the driver supplies them.
func normalizeRule(r string, m *Model) string {
	r = strings.ToUpper(strings.TrimSpace(r))
	if a, ok := m.RuleAliases[r]; ok {
		return a
	}
	return r
}

func normalizeSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

func (d *Diff) compareIndexes(st, tt *Table) {
	for _, si := range st.SortedIndexes() {
		ti := tt.Index(si.Name)
		if ti == nil {
			d.add(Item{Object: ObjectIndex, Table: st.Name, Name: si.Name, Status: OnlyInSource, Action: ActionAdd})
			continue
		}
		if changes := d.indexChanges(si, ti); len(changes) > 0 {
			d.add(Item{Object: ObjectIndex, Table: st.Name, Name: si.Name, Status: Different,
				Action: ActionRecreate, Changes: changes})
		}
	}
	for _, ti := range tt.SortedIndexes() {
		if st.Index(ti.Name) == nil {
			d.add(d.extra(ObjectIndex, st.Name, ti.Name))
		}
	}
}

func (d *Diff) indexChanges(si, ti *Index) map[string]Change {
	changes := map[string]Change{}
	if si.Definition != "" || ti.Definition != "" {
		sd := normalizeSpace(unqualify(si.Definition, d.Source.Schema))
		td := normalizeSpace(unqualify(ti.Definition, d.Target.Schema))
		if sd != td {
			changes["definition"] = Change{ti.Definition, si.Definition}
		}
		if !d.Options.IgnoreComments && si.Comment != ti.Comment {
			changes["comment"] = Change{ti.Comment, si.Comment}
		}
		return changes
	}
	if si.Unique != ti.Unique {
		changes["unique"] = Change{ti.Unique, si.Unique}
	}
	if !strings.EqualFold(si.Type, ti.Type) {
		changes["type"] = Change{ti.Type, si.Type}
	}
	if sp, tp := partsText(si.Parts), partsText(ti.Parts); sp != tp {
		changes["columns"] = Change{tp, sp}
	}
	if si.Visible != ti.Visible {
		changes["visible"] = Change{ti.Visible, si.Visible}
	}
	if !d.Options.IgnoreComments && si.Comment != ti.Comment {
		changes["comment"] = Change{ti.Comment, si.Comment}
	}
	return changes
}

func partsText(parts []IndexPart) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		s := p.Column
		if p.Expression != "" {
			s = "(" + normalizeExpr(p.Expression) + ")"
		}
		if p.SubPart > 0 {
			s += fmt.Sprintf("(%d)", p.SubPart)
		}
		if p.Desc {
			s += " DESC"
		}
		out = append(out, s)
	}
	return strings.Join(out, ", ")
}

func (d *Diff) compareChecks(st, tt *Table) {
	sorted := func(t *Table) []*Check {
		out := append([]*Check(nil), t.Checks...)
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out
	}
	for _, sc := range sorted(st) {
		tc := tt.Check(sc.Name)
		if tc == nil {
			d.add(Item{Object: ObjectCheck, Table: st.Name, Name: sc.Name, Status: OnlyInSource, Action: ActionAdd})
			continue
		}
		if normalizeSpace(unqualify(sc.Definition, d.Source.Schema)) != normalizeSpace(unqualify(tc.Definition, d.Target.Schema)) {
			d.add(Item{Object: ObjectCheck, Table: st.Name, Name: sc.Name, Status: Different, Action: ActionRecreate,
				Changes: map[string]Change{"definition": {tc.Definition, sc.Definition}}})
		}
	}
	for _, tc := range sorted(tt) {
		if st.Check(tc.Name) == nil {
			d.add(d.extra(ObjectCheck, st.Name, tc.Name))
		}
	}
}

func (d *Diff) compareForeignKeys(st, tt *Table) {
	for _, sf := range sortedFKs(st) {
		tf := tt.ForeignKey(sf.Name)
		if tf == nil {
			d.add(Item{Object: ObjectForeignKey, Table: st.Name, Name: sf.Name, Status: OnlyInSource, Action: ActionAdd})
			continue
		}
		if changes := d.fkChanges(sf, tf); len(changes) > 0 {
			d.add(Item{Object: ObjectForeignKey, Table: st.Name, Name: sf.Name, Status: Different,
				Action: ActionRecreate, Changes: changes})
		}
	}
	for _, tf := range sortedFKs(tt) {
		if st.ForeignKey(tf.Name) == nil {
			d.add(d.extra(ObjectForeignKey, st.Name, tf.Name))
		}
	}
}

func sortedFKs(t *Table) []*ForeignKey {
	return sortedByName(t.ForeignKeys, func(fk *ForeignKey) string { return fk.Name })
}

func (d *Diff) fkChanges(sf, tf *ForeignKey) map[string]Change {
	changes := map[string]Change{}
	// A server definition covers columns, references and rules too, so it
	// is the whole comparison, as for indexes.
	if sf.Definition != "" || tf.Definition != "" {
		if normalizeSpace(unqualify(sf.Definition, d.Source.Schema)) != normalizeSpace(unqualify(tf.Definition, d.Target.Schema)) {
			changes["definition"] = Change{tf.Definition, sf.Definition}
		}
		return changes
	}
	if a, b := strings.Join(sf.Columns, ","), strings.Join(tf.Columns, ","); a != b {
		changes["columns"] = Change{b, a}
	}
	if a, b := d.refName(sf, d.Source), d.refName(tf, d.Target); a != b {
		changes["references"] = Change{b, a}
	}
	if a, b := strings.Join(sf.RefColumns, ","), strings.Join(tf.RefColumns, ","); a != b {
		changes["referenced_columns"] = Change{b, a}
	}
	if normalizeRule(sf.OnUpdate, d.Source) != normalizeRule(tf.OnUpdate, d.Target) {
		changes["on_update"] = Change{tf.OnUpdate, sf.OnUpdate}
	}
	if normalizeRule(sf.OnDelete, d.Source) != normalizeRule(tf.OnDelete, d.Target) {
		changes["on_delete"] = Change{tf.OnDelete, sf.OnDelete}
	}
	return changes
}

// refName renders a foreign key's referenced table relative to its model's
// schema, so a same-schema reference compares equal across differently
// named source and target schemas.
func (d *Diff) refName(fk *ForeignKey, m *Model) string {
	if fk.RefSchema == "" || fk.RefSchema == m.Schema {
		return fk.RefTable
	}
	return fk.RefSchema + "." + fk.RefTable
}

// definition is the comparable form of a view, routine, trigger or event.
type definition struct {
	key     string // unique name within the kind ("PROCEDURE p" for routines)
	name    string
	table   string
	definer string
	sql     string
}

func viewDefs(m *Model) []definition {
	out := make([]definition, 0, len(m.Views))
	for _, v := range m.Views {
		out = append(out, definition{key: v.Name, name: v.Name, definer: v.Definer, sql: v.CreateSQL})
	}
	return out
}

func routineDefs(m *Model) []definition {
	out := make([]definition, 0, len(m.Routines))
	for _, r := range m.Routines {
		out = append(out, definition{key: r.Kind + " " + r.Name, name: r.Name, definer: r.Definer, sql: r.CreateSQL})
	}
	return out
}

func triggerDefs(m *Model) []definition {
	out := make([]definition, 0, len(m.Triggers))
	for _, t := range m.Triggers {
		out = append(out, definition{key: t.Name, name: t.Name, table: t.Table, definer: t.Definer, sql: t.CreateSQL})
	}
	return out
}

func eventDefs(m *Model) []definition {
	out := make([]definition, 0, len(m.Events))
	for _, e := range m.Events {
		out = append(out, definition{key: e.Name, name: e.Name, definer: e.Definer, sql: e.CreateSQL})
	}
	return out
}

var definerClause = regexp.MustCompile("(?i)\\s*DEFINER\\s*=\\s*(`[^`]*`|'[^']*'|[^\\s@]+)@(`[^`]*`|'[^']*'|[^\\s]+)")

// StripDefiner removes the DEFINER=user@host clause from a CREATE statement.
func StripDefiner(sql string) string { return definerClause.ReplaceAllString(sql, "") }

// normalizeDefinition makes a definition comparable across schemas: the
// model's own schema qualifier is removed, and DEFINER too when ignored.
func (d *Diff) normalizeDefinition(sql, schema string) string {
	if d.Options.IgnoreDefiner {
		sql = StripDefiner(sql)
	}
	return normalizeSpace(unqualify(sql, schema))
}

// unqualify removes a schema qualifier from SQL text in any of its quoted
// forms: `s`. (MySQL), "s". and bare s. (PostgreSQL). The bare form only
// matches at an identifier boundary.
func unqualify(sql, schema string) string {
	if schema == "" || sql == "" {
		return sql
	}
	sql = strings.ReplaceAll(sql, "`"+schema+"`.", "")
	sql = strings.ReplaceAll(sql, `"`+schema+`".`, "")
	return bareQualifier(schema).ReplaceAllString(sql, "$1")
}

// bareQualifiers caches bareQualifier's patterns: a run only ever sees
// a couple of schema names, but unqualify runs per column and definition.
var bareQualifiers sync.Map

// bareQualifier matches a bare schema qualifier at an identifier
// boundary; group 1 is the preceding character (or empty at the start).
func bareQualifier(schema string) *regexp.Regexp {
	if re, ok := bareQualifiers.Load(schema); ok {
		return re.(*regexp.Regexp)
	}
	re := regexp.MustCompile(`(^|[^A-Za-z0-9_$."` + "`" + `])` + regexp.QuoteMeta(schema) + `\.`)
	bareQualifiers.Store(schema, re)
	return re
}

// RequalifySchema rewrites every qualifier naming schema `from` to `to`,
// for definitions copied from one schema into another. quote renders an
// identifier in the dialect's quoting.
func RequalifySchema(sql, from, to string, quote func(string) string) string {
	if from == "" || from == to {
		return sql
	}
	sql = strings.ReplaceAll(sql, "`"+from+"`.", quote(to)+".")
	sql = strings.ReplaceAll(sql, `"`+from+`".`, quote(to)+".")
	return bareQualifier(from).ReplaceAllString(sql, "${1}"+quote(to)+".")
}

func (d *Diff) compareDefinitions(object string, src, dst []definition) {
	sort.Slice(src, func(i, j int) bool { return src[i].key < src[j].key })
	sort.Slice(dst, func(i, j int) bool { return dst[i].key < dst[j].key })
	byKey := make(map[string]definition, len(dst))
	for _, x := range dst {
		byKey[x.key] = x
	}
	inSrc := make(map[string]bool, len(src))
	for _, s := range src {
		inSrc[s.key] = true
		t, ok := byKey[s.key]
		if !ok {
			d.add(Item{Object: object, Table: s.table, Name: s.name, Status: OnlyInSource, Action: ActionCreate})
			continue
		}
		changes := map[string]Change{}
		if !d.Options.IgnoreDefiner && s.definer != t.definer {
			changes["definer"] = Change{t.definer, s.definer}
		}
		if d.normalizeDefinition(StripDefiner(s.sql), d.Source.Schema) != d.normalizeDefinition(StripDefiner(t.sql), d.Target.Schema) {
			changes["definition"] = Change{"(target definition)", "(source definition)"}
		}
		if len(changes) > 0 {
			d.add(Item{Object: object, Table: s.table, Name: s.name, Status: Different, Action: ActionReplace, Changes: changes})
		} else {
			d.add(Item{Object: object, Table: s.table, Name: s.name, Status: Identical, Action: ActionNone})
		}
	}
	for _, t := range dst {
		if !inSrc[t.key] {
			d.add(d.extra(object, t.table, t.name))
		}
	}
}
