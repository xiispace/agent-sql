// Package schemadiff is the dialect-neutral core of `schema diff` and `copy`:
// the full schema model drivers introspect into, the comparison that
// classifies objects (only in source, only in target, different, identical),
// destructive-change detection, and dependency ordering. Dialect-specific DDL
// rendering lives in the drivers (driver.DDLRenderer); this package imports
// nothing from the rest of agent-sql so the driver package can depend on it.
package schemadiff

import (
	"sort"
	"strings"
)

// Object kinds, as reported in the "object" field of diff records.
const (
	ObjectTable      = "table"
	ObjectColumn     = "column"
	ObjectIndex      = "index"
	ObjectForeignKey = "foreign_key"
	ObjectView       = "view"
	ObjectRoutine    = "routine"
	ObjectTrigger    = "trigger"
	ObjectEvent      = "event"
	ObjectCheck      = "check"
)

// ObjectSet selects the schema object kinds to introspect and compare. Tables
// are always included: columns, indexes and foreign keys come with them.
type ObjectSet struct {
	Views    bool
	Routines bool
	Triggers bool
	Events   bool
}

// Model is the full model of one schema.
type Model struct {
	Schema string
	// RuleAliases folds foreign key rules the dialect enforces identically
	// before comparison (InnoDB: NO ACTION → RESTRICT). The model keeps the
	// rules as the server reports them.
	RuleAliases map[string]string
	Tables      []*Table
	Views       []*View
	Routines    []*Routine
	Triggers    []*Trigger
	Events      []*Event
}

// Table is one base table.
type Table struct {
	Name      string
	Engine    string
	Charset   string
	Collation string
	Comment   string
	// AutoIncrement is the table's next AUTO_INCREMENT value; 0 when the
	// table has no auto-increment column.
	AutoIncrement uint64
	// Partitioning is the dialect's partition clause, empty when unpartitioned.
	Partitioning string
	Columns      []*Column
	Indexes      []*Index
	ForeignKeys  []*ForeignKey
	// Checks are CHECK (and, on PostgreSQL, exclusion) constraints, compared
	// by definition. Drivers whose CREATE TABLE text already carries them
	// (MySQL's SHOW CREATE TABLE) may leave this empty.
	Checks []*Check
	// CreateSQL is the dialect's CREATE TABLE statement for this table, as
	// the server renders it (SHOW CREATE TABLE on MySQL). Empty when the
	// dialect renders CREATE TABLE from the model (PostgreSQL).
	CreateSQL string
}

// Check is a CHECK or exclusion constraint.
type Check struct {
	Name       string
	Definition string
}

// Column is one table column.
type Column struct {
	Name     string
	Position int
	// Type is the full column type as the server reports it, including
	// length, precision, UNSIGNED and ENUM/SET members ("varchar(64)",
	// "int unsigned", "enum('a','b')").
	Type          string
	Nullable      bool
	Default       *string
	DefaultIsExpr bool
	AutoIncrement bool
	OnUpdate      string
	// Generated is the generation expression for generated columns; empty
	// for ordinary columns. GeneratedStored distinguishes STORED from VIRTUAL.
	Generated       string
	GeneratedStored bool
	Invisible       bool
	// Identity is "ALWAYS" or "BY DEFAULT" for SQL-standard identity
	// columns (PostgreSQL); empty otherwise.
	Identity  string
	Charset   string
	Collation string
	Comment   string
}

// IsGenerated reports whether the column is computed by the server. Generated
// columns are excluded from copied rows.
func (c *Column) IsGenerated() bool { return c.Generated != "" }

// IsBinary reports whether the column holds bytes rather than text, so copied
// values travel as bytes rather than as strings in the column's charset.
func (c *Column) IsBinary() bool {
	base := BaseType(c.Type)
	switch base {
	case "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob", "bit",
		"geometry", "point", "linestring", "polygon", "multipoint", "multilinestring",
		"multipolygon", "geometrycollection", "geomcollection":
		return true
	}
	return strings.EqualFold(c.Charset, "binary")
}

// IndexPart is one key part of an index.
type IndexPart struct {
	Column     string
	SubPart    int
	Desc       bool
	Expression string
}

// Index is one index, including the primary key (Primary).
type Index struct {
	Name    string
	Primary bool
	Unique  bool
	// Type is the index method or kind: BTREE, HASH, FULLTEXT, SPATIAL.
	Type    string
	Parts   []IndexPart
	Visible bool
	Comment string
	// Constraint is set when the index belongs to a PRIMARY KEY or UNIQUE
	// constraint (PostgreSQL), so it is added and dropped as a constraint.
	Constraint bool
	// ConstraintDefinition is the constraint's definition when Constraint
	// is set ("PRIMARY KEY (id)", "UNIQUE (code)").
	ConstraintDefinition string
	// Partial marks an index with a predicate (PostgreSQL): it does not
	// cover every row, so it never serves as a row key.
	Partial bool
	// Definition is the server's full index definition (PostgreSQL's
	// pg_get_indexdef: method, expressions, INCLUDE, partial predicate).
	// When set, it is what the comparison uses.
	Definition string
}

// ForeignKey is one foreign key constraint.
type ForeignKey struct {
	Name       string
	Columns    []string
	RefSchema  string
	RefTable   string
	RefColumns []string
	OnUpdate   string
	OnDelete   string
	// Definition is the server's full constraint definition (PostgreSQL's
	// pg_get_constraintdef: MATCH, DEFERRABLE, SET NULL (cols)), without
	// NOT VALID. When set, it is what the comparison uses.
	Definition string
	// NotValid marks a constraint existing rows were never checked against
	// (PostgreSQL). Validation state is not structure: it is rendered when
	// the constraint is created, but never compared.
	NotValid bool
}

// View is one view. CreateSQL is the server's CREATE VIEW statement.
type View struct {
	Name      string
	Definer   string
	CreateSQL string
}

// Routine is one stored procedure or function.
type Routine struct {
	Name      string
	Kind      string // PROCEDURE or FUNCTION
	Definer   string
	CreateSQL string
}

// Trigger is one trigger on a table.
type Trigger struct {
	Name      string
	Table     string
	Definer   string
	CreateSQL string
}

// Event is one scheduled event.
type Event struct {
	Name      string
	Definer   string
	CreateSQL string
}

// Table returns the named table, or nil.
func (m *Model) Table(name string) *Table {
	for _, t := range m.Tables {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// TableNames returns the names of m's tables in model order.
func (m *Model) TableNames() []string {
	out := make([]string, len(m.Tables))
	for i, t := range m.Tables {
		out[i] = t.Name
	}
	return out
}

// SortedTables returns m's tables in name order.
func (m *Model) SortedTables() []*Table {
	return sortedByName(m.Tables, func(t *Table) string { return t.Name })
}

// SortedIndexes returns t's indexes in name order.
func (t *Table) SortedIndexes() []*Index {
	return sortedByName(t.Indexes, func(ix *Index) string { return ix.Name })
}

func sortedByName[T any](in []T, name func(T) string) []T {
	out := append([]T(nil), in...)
	sort.Slice(out, func(i, j int) bool { return name(out[i]) < name(out[j]) })
	return out
}

// View returns the named view, or nil.
func (m *Model) View(name string) *View {
	for _, v := range m.Views {
		if v.Name == name {
			return v
		}
	}
	return nil
}

// Column returns the named column, or nil.
func (t *Table) Column(name string) *Column {
	for _, c := range t.Columns {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// Index returns the named index, or nil.
func (t *Table) Index(name string) *Index {
	for _, ix := range t.Indexes {
		if ix.Name == name {
			return ix
		}
	}
	return nil
}

// Check returns the named check constraint, or nil.
func (t *Table) Check(name string) *Check {
	for _, c := range t.Checks {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// ForeignKey returns the named foreign key, or nil.
func (t *Table) ForeignKey(name string) *ForeignKey {
	for _, fk := range t.ForeignKeys {
		if fk.Name == name {
			return fk
		}
	}
	return nil
}

// PrimaryKey returns the primary key index, or nil.
func (t *Table) PrimaryKey() *Index {
	for _, ix := range t.Indexes {
		if ix.Primary {
			return ix
		}
	}
	return nil
}

// RowKey returns the columns that identify a row: the primary key, or failing
// that the first unique index (by name) whose key parts are all plain NOT NULL
// columns. Generated columns never key a row: copied rows do not carry them
// (see CopyColumns). Nil means the table has no usable key; agent-sql never
// guesses one.
func (t *Table) RowKey() []*Column {
	if pk := t.PrimaryKey(); pk != nil {
		if cols := t.plainColumns(pk, false); cols != nil {
			return cols
		}
	}
	uniques := make([]*Index, 0)
	for _, ix := range t.Indexes {
		if ix.Unique && !ix.Primary {
			uniques = append(uniques, ix)
		}
	}
	sort.Slice(uniques, func(i, j int) bool { return uniques[i].Name < uniques[j].Name })
	for _, ix := range uniques {
		if cols := t.plainColumns(ix, true); cols != nil {
			return cols
		}
	}
	return nil
}

// plainColumns resolves an index to whole, non-expression, non-generated
// columns; nil when any part is a prefix, an expression, a generated column
// or (with requireNotNull) nullable.
func (t *Table) plainColumns(ix *Index, requireNotNull bool) []*Column {
	if len(ix.Parts) == 0 || ix.Partial {
		return nil
	}
	out := make([]*Column, 0, len(ix.Parts))
	for _, p := range ix.Parts {
		if p.Expression != "" || p.SubPart > 0 {
			return nil
		}
		c := t.Column(p.Column)
		if c == nil || c.IsGenerated() || (requireNotNull && c.Nullable) {
			return nil
		}
		out = append(out, c)
	}
	return out
}

// CopyColumns returns the columns copied rows carry: every non-generated
// column, in ordinal order.
func (t *Table) CopyColumns() []*Column {
	out := make([]*Column, 0, len(t.Columns))
	for _, c := range t.Columns {
		if !c.IsGenerated() {
			out = append(out, c)
		}
	}
	return out
}

// Filter returns a copy of m restricted to the named tables (when include is
// non-empty) minus the excluded ones. Views are selected by name, triggers
// follow their table, and routines and events, which belong to no table, are
// kept only when no include list narrows the model to named tables.
func (m *Model) Filter(include, exclude []string) *Model {
	if len(include) == 0 && len(exclude) == 0 {
		return m
	}
	inc := toSet(include)
	exc := toSet(exclude)
	keep := func(name string) bool {
		if exc[name] {
			return false
		}
		return len(inc) == 0 || inc[name]
	}
	out := &Model{Schema: m.Schema, RuleAliases: m.RuleAliases}
	if len(inc) == 0 {
		out.Routines, out.Events = m.Routines, m.Events
	}
	for _, t := range m.Tables {
		if keep(t.Name) {
			out.Tables = append(out.Tables, t)
		}
	}
	for _, v := range m.Views {
		if keep(v.Name) {
			out.Views = append(out.Views, v)
		}
	}
	for _, tr := range m.Triggers {
		if keep(tr.Table) {
			out.Triggers = append(out.Triggers, tr)
		}
	}
	return out
}

func toSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			set[n] = true
		}
	}
	return set
}
