package mysql

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/shhac/agent-sql/internal/schemadiff"
)

// RenderDDL implements driver.DDLRenderer for MySQL and MariaDB. Statements
// come in dependency order: foreign key drops, new tables (referenced tables
// first), table alterations, foreign key adds, table drops, then views,
// routines, triggers and events. Every statement is qualified by the target
// schema except definitions the server renders unqualified (views, routines,
// triggers, events), which the leading USE statement scopes.
func (c *mysqlConn) RenderDDL(diff *schemadiff.Diff, opts schemadiff.RenderOpts) ([]schemadiff.Statement, error) {
	r := &renderer{diff: diff, variant: c.variant, schema: opts.TargetSchema,
		recreate: opts.Mode == schemadiff.StructureRecreate}
	if r.schema == "" {
		r.schema = diff.Target.Schema
	}
	r.emit(schemadiff.Statement{SQL: "USE " + quoteIdent(r.schema), Object: "schema", Name: r.schema})
	if r.recreate {
		r.recreateTables()
	} else {
		r.syncTables()
	}
	r.definitions(schemadiff.ObjectView)
	r.definitions(schemadiff.ObjectRoutine)
	r.definitions(schemadiff.ObjectTrigger)
	r.definitions(schemadiff.ObjectEvent)
	if len(r.out) == 1 {
		return nil, nil // nothing but the USE
	}
	return r.out, nil
}

type renderer struct {
	diff     *schemadiff.Diff
	variant  string
	schema   string
	recreate bool
	out      []schemadiff.Statement
}

func (r *renderer) emit(s schemadiff.Statement) { r.out = append(r.out, s) }

func (r *renderer) q(table string) string { return qualify(r.schema, table) }

// recreateTables drops every selected target table that exists and creates
// it from the source's definition, AUTO_INCREMENT included.
func (r *renderer) recreateTables() {
	src := r.diff.Source
	order := schemadiff.TableOrder(src, src.TableNames())
	for i := len(order) - 1; i >= 0; i-- {
		name := order[i]
		exists := r.diff.Target.Table(name) != nil
		r.emit(schemadiff.Statement{SQL: "DROP TABLE IF EXISTS " + r.q(name), Destructive: exists,
			Object: schemadiff.ObjectTable, Name: name})
	}
	for _, name := range order {
		r.emit(schemadiff.Statement{SQL: r.createTable(src.Table(name), true),
			Object: schemadiff.ObjectTable, Name: name})
	}
	if r.diff.Options.DropExtra {
		r.dropExtraTables()
	}
}

func (r *renderer) syncTables() {
	// 1. Foreign keys that change or go away are dropped first, so column
	// and index changes are free to proceed.
	for _, it := range r.diff.Changes(schemadiff.ObjectForeignKey, schemadiff.Different, schemadiff.OnlyInTarget) {
		r.emit(schemadiff.Statement{
			SQL:         fmt.Sprintf("ALTER TABLE %s DROP FOREIGN KEY %s", r.q(it.Table), quoteIdent(it.Name)),
			Destructive: it.Destructive, Object: schemadiff.ObjectForeignKey, Table: it.Table, Name: it.Name})
	}

	// 2. New tables, referenced tables first.
	var created []string
	for _, it := range r.diff.Changes(schemadiff.ObjectTable, schemadiff.OnlyInSource) {
		created = append(created, it.Name)
	}
	for _, name := range schemadiff.TableOrder(r.diff.Source, created) {
		r.emit(schemadiff.Statement{SQL: r.createTable(r.diff.Source.Table(name), r.diff.Options.CompareAutoIncrement),
			Object: schemadiff.ObjectTable, Name: name})
	}

	// 3. Alterations of tables on both sides.
	for _, name := range r.diff.AlteredTables() {
		r.alterTable(name)
	}

	// 4. Foreign keys that are new or changed on existing tables.
	for _, it := range r.diff.Changes(schemadiff.ObjectForeignKey, schemadiff.OnlyInSource, schemadiff.Different) {
		fk := r.diff.Source.Table(it.Table).ForeignKey(it.Name)
		r.emit(schemadiff.Statement{
			SQL:    fmt.Sprintf("ALTER TABLE %s ADD %s", r.q(it.Table), r.foreignKeyDef(fk)),
			Object: schemadiff.ObjectForeignKey, Table: it.Table, Name: it.Name})
	}

	// 5. Tables only in the target, with --drop-extra.
	r.dropExtraTables()
}

func (r *renderer) dropExtraTables() {
	for _, name := range r.diff.DroppedTables() {
		r.emit(schemadiff.Statement{SQL: "DROP TABLE " + r.q(name), Destructive: true,
			Object: schemadiff.ObjectTable, Name: name})
	}
}

// alterTable renders one table's changes as up to three statements: the
// destructive clauses (column drops, lossy modifications, index drops), the
// rest, and a partitioning change, which MySQL takes on its own.
func (r *renderer) alterTable(name string) {
	st := r.diff.Source.Table(name)
	var destructive, regular []string
	var partition string

	for _, it := range r.diff.TableItems(name) {
		if it.Action == schemadiff.ActionNone || it.Status == schemadiff.Identical {
			continue
		}
		switch it.Object {
		case schemadiff.ObjectTable:
			opts, part := r.tableOptions(st, it)
			regular = append(regular, opts...)
			partition = part
		case schemadiff.ObjectColumn:
			switch it.Action {
			case schemadiff.ActionDrop:
				destructive = append(destructive, "DROP COLUMN "+quoteIdent(it.Name))
			case schemadiff.ActionAdd:
				col := st.Column(it.Name)
				regular = append(regular, "ADD COLUMN "+r.columnDef(col)+r.position(st, col))
			case schemadiff.ActionModify:
				col := st.Column(it.Name)
				clause := "MODIFY COLUMN " + r.columnDef(col)
				if _, moved := it.Changes["after"]; moved {
					clause += r.position(st, col)
				}
				if it.Destructive {
					destructive = append(destructive, clause)
				} else {
					regular = append(regular, clause)
				}
			}
		case schemadiff.ObjectIndex:
			switch it.Action {
			case schemadiff.ActionDrop:
				destructive = append(destructive, r.dropIndex(it.Name))
			case schemadiff.ActionAdd:
				regular = append(regular, "ADD "+r.indexDef(st.Index(it.Name)))
			case schemadiff.ActionRecreate:
				regular = append(regular, r.dropIndex(it.Name), "ADD "+r.indexDef(st.Index(it.Name)))
			}
		}
	}
	if len(destructive) > 0 {
		r.emit(schemadiff.Statement{SQL: "ALTER TABLE " + r.q(name) + "\n  " + strings.Join(destructive, ",\n  "),
			Destructive: true, Object: schemadiff.ObjectTable, Name: name})
	}
	if len(regular) > 0 {
		r.emit(schemadiff.Statement{SQL: "ALTER TABLE " + r.q(name) + "\n  " + strings.Join(regular, ",\n  "),
			Object: schemadiff.ObjectTable, Name: name})
	}
	if partition != "" {
		r.emit(schemadiff.Statement{SQL: "ALTER TABLE " + r.q(name) + " " + partition,
			Object: schemadiff.ObjectTable, Name: name})
	}
}

func (r *renderer) tableOptions(st *schemadiff.Table, it schemadiff.Item) ([]string, string) {
	var out []string
	if _, ok := it.Changes["engine"]; ok {
		out = append(out, "ENGINE="+st.Engine)
	}
	_, cs := it.Changes["charset"]
	_, co := it.Changes["collation"]
	if cs || co {
		out = append(out, fmt.Sprintf("DEFAULT CHARSET=%s COLLATE=%s", st.Charset, st.Collation))
	}
	if _, ok := it.Changes["comment"]; ok {
		out = append(out, "COMMENT="+quoteLiteral(st.Comment))
	}
	if _, ok := it.Changes["auto_increment"]; ok && st.AutoIncrement > 0 {
		out = append(out, fmt.Sprintf("AUTO_INCREMENT=%d", st.AutoIncrement))
	}
	partition := ""
	if _, ok := it.Changes["partitioning"]; ok {
		partition = "REMOVE PARTITIONING"
		if st.Partitioning != "" {
			partition = unwrapVersionComment(st.Partitioning)
		}
	}
	return out, partition
}

var versionComment = regexp.MustCompile(`^/\*!\d+\s*([\s\S]*?)\s*\*/$`)

func unwrapVersionComment(s string) string {
	if m := versionComment.FindStringSubmatch(strings.TrimSpace(s)); m != nil {
		return m[1]
	}
	return s
}

// position renders a column's placement after the source's previous column.
func (r *renderer) position(t *schemadiff.Table, c *schemadiff.Column) string {
	prev := schemadiff.PreviousColumn(t, c)
	if prev == "" {
		return " FIRST"
	}
	return " AFTER " + quoteIdent(prev)
}

func (r *renderer) dropIndex(name string) string {
	if name == "PRIMARY" {
		return "DROP PRIMARY KEY"
	}
	return "DROP INDEX " + quoteIdent(name)
}

var autoIncrementOption = regexp.MustCompile(` AUTO_INCREMENT=\d+`)

// createTable renders the source's CREATE TABLE qualified by the target
// schema, without its AUTO_INCREMENT value unless keepAutoIncrement.
func (r *renderer) createTable(t *schemadiff.Table, keepAutoIncrement bool) string {
	ddl := t.CreateSQL
	if !keepAutoIncrement {
		ddl = autoIncrementOption.ReplaceAllString(ddl, "")
	}
	prefix := "CREATE TABLE " + quoteIdent(t.Name)
	if strings.HasPrefix(ddl, prefix) {
		ddl = "CREATE TABLE " + r.q(t.Name) + ddl[len(prefix):]
	}
	return ddl
}

func (r *renderer) columnDef(c *schemadiff.Column) string {
	var b strings.Builder
	b.WriteString(quoteIdent(c.Name) + " " + c.Type)
	if c.Charset != "" && !strings.EqualFold(c.Charset, "binary") && !strings.Contains(strings.ToLower(c.Type), " character set ") {
		b.WriteString(" CHARACTER SET " + c.Charset)
		if c.Collation != "" {
			b.WriteString(" COLLATE " + c.Collation)
		}
	}
	if c.Generated != "" {
		kind := " VIRTUAL"
		if c.GeneratedStored {
			kind = " STORED"
		}
		b.WriteString(" GENERATED ALWAYS AS (" + c.Generated + ")" + kind)
		if !c.Nullable {
			b.WriteString(" NOT NULL")
		}
	} else {
		if c.Nullable {
			b.WriteString(" NULL")
		} else {
			b.WriteString(" NOT NULL")
		}
		if c.Default != nil {
			b.WriteString(" DEFAULT " + defaultSQL(c))
		}
		if c.AutoIncrement {
			b.WriteString(" AUTO_INCREMENT")
		}
		if c.OnUpdate != "" {
			b.WriteString(" ON UPDATE " + c.OnUpdate)
		}
	}
	if c.Invisible {
		b.WriteString(" INVISIBLE")
	}
	if c.Comment != "" {
		b.WriteString(" COMMENT " + quoteLiteral(c.Comment))
	}
	return b.String()
}

func defaultSQL(c *schemadiff.Column) string {
	v := *c.Default
	if c.DefaultIsExpr {
		if isCurrentTimestamp(v) {
			return v
		}
		return "(" + v + ")"
	}
	if strings.HasPrefix(v, "b'") && strings.HasPrefix(strings.ToLower(c.Type), "bit") {
		return v
	}
	return quoteLiteral(v)
}

func quoteLiteral(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func (r *renderer) indexDef(ix *schemadiff.Index) string {
	parts := make([]string, 0, len(ix.Parts))
	for _, p := range ix.Parts {
		s := quoteIdent(p.Column)
		if p.Expression != "" {
			s = "(" + p.Expression + ")"
		}
		if p.SubPart > 0 {
			s += fmt.Sprintf("(%d)", p.SubPart)
		}
		if p.Desc {
			s += " DESC"
		}
		parts = append(parts, s)
	}
	cols := "(" + strings.Join(parts, ", ") + ")"
	var def string
	switch {
	case ix.Primary:
		def = "PRIMARY KEY " + cols
	case strings.EqualFold(ix.Type, "FULLTEXT"):
		def = "FULLTEXT KEY " + quoteIdent(ix.Name) + " " + cols
	case strings.EqualFold(ix.Type, "SPATIAL"):
		def = "SPATIAL KEY " + quoteIdent(ix.Name) + " " + cols
	case ix.Unique:
		def = "UNIQUE KEY " + quoteIdent(ix.Name) + " " + cols
	default:
		def = "KEY " + quoteIdent(ix.Name) + " " + cols
	}
	if strings.EqualFold(ix.Type, "HASH") {
		def += " USING HASH"
	}
	if ix.Comment != "" {
		def += " COMMENT " + quoteLiteral(ix.Comment)
	}
	if !ix.Visible && !ix.Primary {
		if r.variant == "mariadb" {
			def += " IGNORED"
		} else {
			def += " INVISIBLE"
		}
	}
	return def
}

func (r *renderer) foreignKeyDef(fk *schemadiff.ForeignKey) string {
	quoteAll := func(names []string) string {
		out := make([]string, len(names))
		for i, n := range names {
			out[i] = quoteIdent(n)
		}
		return strings.Join(out, ", ")
	}
	// A same-schema reference follows the copy to the target schema.
	ref := r.q(fk.RefTable)
	if fk.RefSchema != "" && fk.RefSchema != r.diff.Source.Schema {
		ref = qualify(fk.RefSchema, fk.RefTable)
	}
	def := fmt.Sprintf("CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s)",
		quoteIdent(fk.Name), quoteAll(fk.Columns), ref, quoteAll(fk.RefColumns))
	if fk.OnDelete != "" && !strings.EqualFold(fk.OnDelete, "RESTRICT") && !strings.EqualFold(fk.OnDelete, "NO ACTION") {
		def += " ON DELETE " + fk.OnDelete
	}
	if fk.OnUpdate != "" && !strings.EqualFold(fk.OnUpdate, "RESTRICT") && !strings.EqualFold(fk.OnUpdate, "NO ACTION") {
		def += " ON UPDATE " + fk.OnUpdate
	}
	return def
}

// definitions renders views, routines, triggers and events: created from
// the source's definition (DEFINER stripped with --ignore-definer, source
// schema qualifiers pointed at the target), replaced when different, and
// dropped with --drop-extra.
func (r *renderer) definitions(object string) {
	type pending struct {
		item schemadiff.Item
		sql  string
		kind string
	}
	var creates []pending
	if r.recreate && object == schemadiff.ObjectTrigger {
		// Dropping a table drops its triggers: recreate them all.
		for _, x := range r.diff.Source.Triggers {
			creates = append(creates, pending{item: schemadiff.Item{Object: object, Table: x.Table, Name: x.Name, Status: schemadiff.OnlyInSource}, sql: x.CreateSQL, kind: "TRIGGER"})
		}
	}
	for _, it := range r.diff.Items {
		if it.Object != object || it.Action == schemadiff.ActionNone || it.Status == schemadiff.Identical {
			continue
		}
		if r.recreate && object == schemadiff.ObjectTrigger && it.Status != schemadiff.OnlyInTarget {
			continue
		}
		kind, srcSQL := r.definitionSource(object, it)
		if it.Status == schemadiff.OnlyInTarget {
			kind = r.definitionKind(object, it, r.diff.Target)
			r.emit(schemadiff.Statement{SQL: fmt.Sprintf("DROP %s IF EXISTS %s", kind, quoteIdent(it.Name)),
				Destructive: it.Destructive, Object: object, Table: it.Table, Name: it.Name})
			continue
		}
		creates = append(creates, pending{item: it, sql: srcSQL, kind: kind})
	}
	if object == schemadiff.ObjectView {
		// A view that mentions another pending view goes after it.
		order := schemadiff.DependencyOrder(len(creates), func(i, j int) bool {
			return strings.Contains(creates[i].sql, quoteIdent(creates[j].item.Name))
		})
		sorted := make([]pending, len(creates))
		for k, i := range order {
			sorted[k] = creates[i]
		}
		creates = sorted
	}
	for _, p := range creates {
		sql := r.adaptDefinition(p.sql)
		if object == schemadiff.ObjectView {
			sql = strings.Replace(sql, "CREATE ", "CREATE OR REPLACE ", 1)
		} else if p.item.Status == schemadiff.Different {
			r.emit(schemadiff.Statement{SQL: fmt.Sprintf("DROP %s IF EXISTS %s", p.kind, quoteIdent(p.item.Name)),
				Object: object, Table: p.item.Table, Name: p.item.Name})
		}
		r.emit(schemadiff.Statement{SQL: sql, Object: object, Table: p.item.Table, Name: p.item.Name})
	}
}

func (r *renderer) definitionSource(object string, it schemadiff.Item) (string, string) {
	src := r.diff.Source
	switch object {
	case schemadiff.ObjectView:
		for _, v := range src.Views {
			if v.Name == it.Name {
				return "VIEW", v.CreateSQL
			}
		}
	case schemadiff.ObjectRoutine:
		for _, x := range src.Routines {
			if x.Name == it.Name {
				return strings.ToUpper(x.Kind), x.CreateSQL
			}
		}
	case schemadiff.ObjectTrigger:
		for _, x := range src.Triggers {
			if x.Name == it.Name {
				return "TRIGGER", x.CreateSQL
			}
		}
	case schemadiff.ObjectEvent:
		for _, x := range src.Events {
			if x.Name == it.Name {
				return "EVENT", x.CreateSQL
			}
		}
	}
	return strings.ToUpper(object), ""
}

func (r *renderer) definitionKind(object string, it schemadiff.Item, m *schemadiff.Model) string {
	if object == schemadiff.ObjectRoutine {
		for _, x := range m.Routines {
			if x.Name == it.Name {
				return strings.ToUpper(x.Kind)
			}
		}
	}
	return strings.ToUpper(object)
}

func (r *renderer) adaptDefinition(sql string) string {
	if r.diff.Options.IgnoreDefiner {
		sql = schemadiff.StripDefiner(sql)
	}
	// Bodies keep the user's text, so bare `src.tbl` qualifiers are
	// re-pointed too, not just the server-quoted ones.
	return schemadiff.RequalifySchema(sql, r.diff.Source.Schema, r.schema, quoteIdent)
}
