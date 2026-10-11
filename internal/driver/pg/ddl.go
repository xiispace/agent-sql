package pg

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/shhac/agent-sql/internal/schemadiff"
)

// RenderDDL implements driver.DDLRenderer for PostgreSQL. PostgreSQL has no
// SHOW CREATE TABLE, so tables are rendered from the model. Statements come
// in dependency order: foreign key drops, new tables (sequences first,
// referenced tables first), table alterations, foreign key adds, table
// drops, then views, routines and triggers. Every name is qualified by the
// target schema; definitions the server rendered with the source schema are
// re-pointed at it.
func (c *pgConn) RenderDDL(diff *schemadiff.Diff, opts schemadiff.RenderOpts) ([]schemadiff.Statement, error) {
	r := &pgRenderer{diff: diff, schema: opts.TargetSchema}
	if r.schema == "" {
		r.schema = diff.Target.Schema
	}
	if opts.Mode == schemadiff.StructureRecreate {
		r.recreate = true
		r.recreateTables()
	} else {
		r.syncTables()
	}
	r.views()
	r.routines()
	r.triggers()
	return r.out, nil
}

type pgRenderer struct {
	diff     *schemadiff.Diff
	schema   string
	recreate bool
	out      []schemadiff.Statement
}

func (r *pgRenderer) emit(sql string, destructive bool, object, table, name string) {
	r.out = append(r.out, schemadiff.Statement{SQL: sql, Destructive: destructive, Object: object, Table: table, Name: name})
}

func (r *pgRenderer) q(name string) string { return qualify(r.schema, name) }

// rq re-points source-schema qualifiers in server-rendered SQL at the target.
func (r *pgRenderer) rq(sql string) string {
	return schemadiff.RequalifySchema(sql, r.diff.Source.Schema, r.schema, quoteIdent)
}

func (r *pgRenderer) recreateTables() {
	src := r.diff.Source
	order := schemadiff.TableOrder(src, src.TableNames())
	// PostgreSQL refuses to drop a table a view reads. Views in scope are
	// dropped first and recreated after the tables: from the source, or for
	// a target-only view from its own definition, unless --drop-extra.
	for _, v := range r.diff.Target.Views {
		lost := r.diff.Source.View(v.Name) == nil && r.diff.Options.DropExtra
		r.emit("DROP VIEW IF EXISTS "+r.q(v.Name)+" CASCADE", lost, schemadiff.ObjectView, "", v.Name)
	}
	for i := len(order) - 1; i >= 0; i-- {
		exists := r.diff.Target.Table(order[i]) != nil
		r.emit("DROP TABLE IF EXISTS "+r.q(order[i]), exists, schemadiff.ObjectTable, "", order[i])
	}
	for _, name := range order {
		r.createTable(src.Table(name))
	}
	for _, name := range order {
		for _, fk := range src.Table(name).ForeignKeys {
			r.addForeignKey(name, fk)
		}
	}
	if r.diff.Options.DropExtra {
		r.dropExtraTables()
	}
}

func (r *pgRenderer) syncTables() {
	for _, it := range r.diff.Changes(schemadiff.ObjectForeignKey, schemadiff.Different, schemadiff.OnlyInTarget) {
		r.emit(fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s", r.q(it.Table), quoteIdent(it.Name)),
			it.Destructive, schemadiff.ObjectForeignKey, it.Table, it.Name)
	}
	var created []string
	for _, it := range r.diff.Changes(schemadiff.ObjectTable, schemadiff.OnlyInSource) {
		created = append(created, it.Name)
	}
	order := schemadiff.TableOrder(r.diff.Source, created)
	for _, name := range order {
		r.createTable(r.diff.Source.Table(name))
	}
	for _, name := range r.diff.AlteredTables() {
		r.alterTable(name)
	}
	for _, name := range order {
		for _, fk := range r.diff.Source.Table(name).ForeignKeys {
			r.addForeignKey(name, fk)
		}
	}
	for _, it := range r.diff.Changes(schemadiff.ObjectForeignKey, schemadiff.OnlyInSource, schemadiff.Different) {
		r.addForeignKey(it.Table, r.diff.Source.Table(it.Table).ForeignKey(it.Name))
	}
	r.dropExtraTables()
}

func (r *pgRenderer) dropExtraTables() {
	for _, name := range r.diff.DroppedTables() {
		r.emit("DROP TABLE "+r.q(name), true, schemadiff.ObjectTable, "", name)
	}
}

var nextvalSequence = regexp.MustCompile(`nextval\('((?:[^']|'')+)'::regclass\)`)

// sequenceOf returns the (target-qualified) sequence a serial column's
// default draws from, or "".
func (r *pgRenderer) sequenceOf(c *schemadiff.Column) string {
	if c.Default == nil {
		return ""
	}
	m := nextvalSequence.FindStringSubmatch(r.rq(*c.Default))
	if m == nil {
		return ""
	}
	return strings.ReplaceAll(m[1], "''", "'")
}

func (r *pgRenderer) createTable(t *schemadiff.Table) {
	var defs []string
	var owned []string
	for _, c := range t.Columns {
		if seq := r.sequenceOf(c); seq != "" {
			r.emit("CREATE SEQUENCE IF NOT EXISTS "+seq, false, schemadiff.ObjectTable, "", t.Name)
			owned = append(owned, fmt.Sprintf("ALTER SEQUENCE %s OWNED BY %s.%s", seq, r.q(t.Name), quoteIdent(c.Name)))
		}
		defs = append(defs, r.columnDef(c))
	}
	for _, ix := range t.SortedIndexes() {
		if ix.Constraint {
			defs = append(defs, "CONSTRAINT "+quoteIdent(ix.Name)+" "+r.constraintDef(ix))
		}
	}
	for _, ck := range t.Checks {
		defs = append(defs, "CONSTRAINT "+quoteIdent(ck.Name)+" "+r.rq(ck.Definition))
	}
	sql := "CREATE TABLE " + r.q(t.Name) + " (\n  " + strings.Join(defs, ",\n  ") + "\n)"
	if t.Partitioning != "" {
		sql += " " + r.rq(t.Partitioning)
	}
	r.emit(sql, false, schemadiff.ObjectTable, "", t.Name)
	for _, stmt := range owned {
		r.emit(stmt, false, schemadiff.ObjectTable, "", t.Name)
	}
	for _, ix := range t.SortedIndexes() {
		if !ix.Constraint {
			r.emit(r.rq(ix.Definition), false, schemadiff.ObjectIndex, t.Name, ix.Name)
		}
	}
	r.comments(t)
}

func (r *pgRenderer) constraintDef(ix *schemadiff.Index) string {
	if ix.ConstraintDefinition != "" {
		return r.rq(ix.ConstraintDefinition)
	}
	cols := make([]string, len(ix.Parts))
	for i, p := range ix.Parts {
		cols[i] = quoteIdent(p.Column)
	}
	kind := "UNIQUE"
	if ix.Primary {
		kind = "PRIMARY KEY"
	}
	return kind + " (" + strings.Join(cols, ", ") + ")"
}

func (r *pgRenderer) comments(t *schemadiff.Table) {
	if t.Comment != "" {
		r.emit(fmt.Sprintf("COMMENT ON TABLE %s IS %s", r.q(t.Name), quoteLiteral(t.Comment)), false, schemadiff.ObjectTable, "", t.Name)
	}
	for _, c := range t.Columns {
		if c.Comment != "" {
			r.emit(fmt.Sprintf("COMMENT ON COLUMN %s.%s IS %s", r.q(t.Name), quoteIdent(c.Name), quoteLiteral(c.Comment)), false, schemadiff.ObjectColumn, t.Name, c.Name)
		}
	}
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func (r *pgRenderer) columnDef(c *schemadiff.Column) string {
	var b strings.Builder
	b.WriteString(quoteIdent(c.Name) + " " + r.rq(c.Type))
	if c.Collation != "" {
		b.WriteString(" COLLATE " + quoteIdent(c.Collation))
	}
	switch {
	case c.Generated != "":
		b.WriteString(" GENERATED ALWAYS AS (" + r.rq(c.Generated) + ") STORED")
	case c.Identity != "":
		b.WriteString(" GENERATED " + c.Identity + " AS IDENTITY")
	case c.Default != nil:
		b.WriteString(" DEFAULT " + r.rq(*c.Default))
	}
	if !c.Nullable {
		b.WriteString(" NOT NULL")
	}
	return b.String()
}

func (r *pgRenderer) addForeignKey(table string, fk *schemadiff.ForeignKey) {
	notValid := ""
	if fk.NotValid {
		notValid = " NOT VALID"
	}
	// The server's definition carries what the model fields do not
	// (MATCH, DEFERRABLE, SET NULL (cols)).
	if fk.Definition != "" {
		r.emit(fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s %s%s", r.q(table), quoteIdent(fk.Name), r.rq(fk.Definition), notValid),
			false, schemadiff.ObjectForeignKey, table, fk.Name)
		return
	}
	quoteAll := func(names []string) string {
		out := make([]string, len(names))
		for i, n := range names {
			out[i] = quoteIdent(n)
		}
		return strings.Join(out, ", ")
	}
	ref := r.q(fk.RefTable)
	if fk.RefSchema != "" && fk.RefSchema != r.diff.Source.Schema {
		ref = quoteIdent(fk.RefSchema) + "." + quoteIdent(fk.RefTable)
	}
	sql := fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s)",
		r.q(table), quoteIdent(fk.Name), quoteAll(fk.Columns), ref, quoteAll(fk.RefColumns))
	if fk.OnDelete != "" && !strings.EqualFold(fk.OnDelete, "NO ACTION") {
		sql += " ON DELETE " + fk.OnDelete
	}
	if fk.OnUpdate != "" && !strings.EqualFold(fk.OnUpdate, "NO ACTION") {
		sql += " ON UPDATE " + fk.OnUpdate
	}
	sql += notValid
	r.emit(sql, false, schemadiff.ObjectForeignKey, table, fk.Name)
}

// alterTable renders one table's changes: index drops, then a destructive
// ALTER TABLE (column drops, lossy changes), then the rest, then index
// creation and comments.
func (r *pgRenderer) alterTable(name string) {
	st := r.diff.Source.Table(name)
	var before, after []schemadiff.Statement
	var destructive, regular []string
	stmt := func(sql string, destr bool, object, item string) schemadiff.Statement {
		return schemadiff.Statement{SQL: sql, Destructive: destr, Object: object, Table: name, Name: item}
	}

	for _, it := range r.diff.TableItems(name) {
		if it.Action == schemadiff.ActionNone || it.Status == schemadiff.Identical {
			continue
		}
		switch it.Object {
		case schemadiff.ObjectTable:
			if _, ok := it.Changes["comment"]; ok {
				after = append(after, stmt(fmt.Sprintf("COMMENT ON TABLE %s IS %s", r.q(name), quoteLiteral(st.Comment)), false, schemadiff.ObjectTable, name))
			}
		case schemadiff.ObjectColumn:
			r.alterColumn(st, it, &destructive, &regular, &before, &after)
		case schemadiff.ObjectIndex:
			var src *schemadiff.Index
			if st != nil {
				src = st.Index(it.Name)
			}
			tgt := r.diff.Target.Table(name).Index(it.Name)
			if it.Action == schemadiff.ActionDrop || it.Action == schemadiff.ActionRecreate {
				if tgt != nil && tgt.Constraint {
					clause := "DROP CONSTRAINT " + quoteIdent(it.Name)
					if it.Destructive {
						destructive = append(destructive, clause)
					} else {
						regular = append(regular, clause)
					}
				} else {
					before = append(before, stmt("DROP INDEX "+r.q(it.Name), it.Destructive, schemadiff.ObjectIndex, it.Name))
				}
			}
			if (it.Action == schemadiff.ActionAdd || it.Action == schemadiff.ActionRecreate) && src != nil {
				if src.Constraint {
					regular = append(regular, "ADD CONSTRAINT "+quoteIdent(src.Name)+" "+r.constraintDef(src))
				} else {
					after = append(after, stmt(r.rq(src.Definition), false, schemadiff.ObjectIndex, src.Name))
				}
			}
		case schemadiff.ObjectCheck:
			if it.Action == schemadiff.ActionDrop || it.Action == schemadiff.ActionRecreate {
				clause := "DROP CONSTRAINT " + quoteIdent(it.Name)
				if it.Destructive {
					destructive = append(destructive, clause)
				} else {
					regular = append(regular, clause)
				}
			}
			if it.Action == schemadiff.ActionAdd || it.Action == schemadiff.ActionRecreate {
				ck := st.Check(it.Name)
				regular = append(regular, "ADD CONSTRAINT "+quoteIdent(ck.Name)+" "+r.rq(ck.Definition))
			}
		}
	}
	r.out = append(r.out, before...)
	if len(destructive) > 0 {
		r.emit("ALTER TABLE "+r.q(name)+"\n  "+strings.Join(destructive, ",\n  "), true, schemadiff.ObjectTable, "", name)
	}
	if len(regular) > 0 {
		r.emit("ALTER TABLE "+r.q(name)+"\n  "+strings.Join(regular, ",\n  "), false, schemadiff.ObjectTable, "", name)
	}
	r.out = append(r.out, after...)
}

func (r *pgRenderer) alterColumn(st *schemadiff.Table, it schemadiff.Item, destructive, regular *[]string, before, after *[]schemadiff.Statement) {
	col := quoteIdent(it.Name)
	switch it.Action {
	case schemadiff.ActionDrop:
		*destructive = append(*destructive, "DROP COLUMN "+col)
		return
	case schemadiff.ActionAdd:
		c := st.Column(it.Name)
		if seq := r.sequenceOf(c); seq != "" {
			*before = append(*before, schemadiff.Statement{SQL: "CREATE SEQUENCE IF NOT EXISTS " + seq, Object: schemadiff.ObjectColumn, Table: st.Name, Name: c.Name})
		}
		*regular = append(*regular, "ADD COLUMN "+r.columnDef(c))
		if c.Comment != "" {
			*after = append(*after, schemadiff.Statement{SQL: fmt.Sprintf("COMMENT ON COLUMN %s.%s IS %s", r.q(st.Name), col, quoteLiteral(c.Comment)), Object: schemadiff.ObjectColumn, Table: st.Name, Name: c.Name})
		}
		return
	}
	c := st.Column(it.Name)
	var clauses []string
	if _, ok := it.Changes["generated"]; ok {
		// A generation expression cannot be altered in place before
		// PostgreSQL 17; the column is computed, so drop + add loses nothing.
		clauses = append(clauses, "DROP COLUMN "+col, "ADD COLUMN "+r.columnDef(c))
	} else {
		_, typ := it.Changes["type"]
		_, coll := it.Changes["collation"]
		if typ || coll {
			t := r.rq(c.Type)
			clause := "ALTER COLUMN " + col + " TYPE " + t
			if c.Collation != "" {
				clause += " COLLATE " + quoteIdent(c.Collation)
			}
			clauses = append(clauses, clause+" USING "+col+"::"+t)
		}
		if _, ok := it.Changes["nullable"]; ok {
			if c.Nullable {
				clauses = append(clauses, "ALTER COLUMN "+col+" DROP NOT NULL")
			} else {
				clauses = append(clauses, "ALTER COLUMN "+col+" SET NOT NULL")
			}
		}
		if ch, ok := it.Changes["identity"]; ok {
			switch {
			case c.Identity == "":
				clauses = append(clauses, "ALTER COLUMN "+col+" DROP IDENTITY IF EXISTS")
			case ch.From == "":
				clauses = append(clauses, "ALTER COLUMN "+col+" ADD GENERATED "+c.Identity+" AS IDENTITY")
			default:
				clauses = append(clauses, "ALTER COLUMN "+col+" SET GENERATED "+c.Identity)
			}
		}
		if _, ok := it.Changes["default"]; ok && c.Identity == "" {
			if c.Default == nil {
				clauses = append(clauses, "ALTER COLUMN "+col+" DROP DEFAULT")
			} else {
				if seq := r.sequenceOf(c); seq != "" {
					*before = append(*before, schemadiff.Statement{SQL: "CREATE SEQUENCE IF NOT EXISTS " + seq, Object: schemadiff.ObjectColumn, Table: st.Name, Name: c.Name})
				}
				clauses = append(clauses, "ALTER COLUMN "+col+" SET DEFAULT "+r.rq(*c.Default))
			}
		}
	}
	if _, ok := it.Changes["comment"]; ok {
		*after = append(*after, schemadiff.Statement{SQL: fmt.Sprintf("COMMENT ON COLUMN %s.%s IS %s", r.q(st.Name), col, quoteLiteral(c.Comment)), Object: schemadiff.ObjectColumn, Table: st.Name, Name: c.Name})
	}
	if it.Destructive {
		*destructive = append(*destructive, clauses...)
	} else {
		*regular = append(*regular, clauses...)
	}
}

func (r *pgRenderer) views() {
	// sql is the original definition, in schema; mentions are looked up in it.
	type pending struct {
		name, sql, schema string
		target            bool
	}
	var creates []pending
	source := func(v *schemadiff.View) {
		creates = append(creates, pending{name: v.Name, sql: v.CreateSQL, schema: r.diff.Source.Schema})
	}
	if r.recreate {
		// recreate dropped every target view in scope; all come back but
		// the target-only ones --drop-extra drops.
		for _, v := range r.diff.Source.Views {
			source(v)
		}
		if !r.diff.Options.DropExtra {
			for _, v := range r.diff.Target.Views {
				if r.diff.Source.View(v.Name) == nil {
					creates = append(creates, pending{name: v.Name, sql: v.CreateSQL, schema: r.diff.Target.Schema, target: true})
				}
			}
		}
	}
	for _, it := range r.diff.Items {
		if r.recreate {
			break
		}
		if it.Object != schemadiff.ObjectView || it.Action == schemadiff.ActionNone || it.Status == schemadiff.Identical {
			continue
		}
		if it.Status == schemadiff.OnlyInTarget {
			r.emit("DROP VIEW IF EXISTS "+r.q(it.Name), it.Destructive, schemadiff.ObjectView, "", it.Name)
			continue
		}
		if v := r.diff.Source.View(it.Name); v != nil {
			source(v)
		}
	}
	// A view that reads another pending view goes after it.
	order := schemadiff.DependencyOrder(len(creates), func(i, j int) bool {
		return mentions(creates[i].sql, creates[i].schema, creates[j].name)
	})
	ordered := make([]pending, len(creates))
	for k, i := range order {
		ordered[k] = creates[i]
	}
	for _, v := range ordered {
		sql := v.sql
		if !v.target {
			sql = r.rq(sql)
		}
		r.emit(sql, false, schemadiff.ObjectView, "", v.name)
	}
}

func mentions(sql, schema, name string) bool {
	return strings.Contains(sql, schema+"."+name) || strings.Contains(sql, quoteIdent(name))
}

func (r *pgRenderer) routines() {
	for _, it := range r.diff.Items {
		if it.Object != schemadiff.ObjectRoutine || it.Action == schemadiff.ActionNone || it.Status == schemadiff.Identical {
			continue
		}
		if it.Status == schemadiff.OnlyInTarget {
			kind := "FUNCTION"
			for _, x := range r.diff.Target.Routines {
				if x.Name == it.Name {
					kind = x.Kind
				}
			}
			r.emit(fmt.Sprintf("DROP %s IF EXISTS %s", kind, r.routineName(it.Name)), it.Destructive, schemadiff.ObjectRoutine, "", it.Name)
			continue
		}
		for _, x := range r.diff.Source.Routines {
			if x.Name == it.Name {
				r.emit(r.rq(x.CreateSQL), false, schemadiff.ObjectRoutine, "", it.Name)
			}
		}
	}
}

// routineName qualifies "name(args)" with the target schema.
func (r *pgRenderer) routineName(sig string) string {
	i := strings.IndexByte(sig, '(')
	if i < 0 {
		return r.q(sig)
	}
	return r.q(sig[:i]) + r.rq(sig[i:])
}

func (r *pgRenderer) triggers() {
	if r.recreate && r.diff.Options.Objects.Triggers {
		// Dropping a table drops its triggers: recreate them all.
		for _, x := range r.diff.Source.Triggers {
			r.emit(r.rq(x.CreateSQL), false, schemadiff.ObjectTrigger, x.Table, x.Name)
		}
	}
	for _, it := range r.diff.Items {
		if it.Object != schemadiff.ObjectTrigger || it.Action == schemadiff.ActionNone || it.Status == schemadiff.Identical {
			continue
		}
		if r.recreate && it.Status != schemadiff.OnlyInTarget {
			continue
		}
		if it.Status != schemadiff.OnlyInSource {
			r.emit(fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON %s", quoteIdent(it.Name), r.q(it.Table)),
				it.Status == schemadiff.OnlyInTarget && it.Destructive, schemadiff.ObjectTrigger, it.Table, it.Name)
		}
		if it.Status == schemadiff.OnlyInTarget {
			continue
		}
		for _, x := range r.diff.Source.Triggers {
			if x.Name == it.Name && x.Table == it.Table {
				r.emit(r.rq(x.CreateSQL), false, schemadiff.ObjectTrigger, it.Table, it.Name)
			}
		}
	}
}
