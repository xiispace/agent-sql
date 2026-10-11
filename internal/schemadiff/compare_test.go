package schemadiff

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func strp(s string) *string { return &s }

func col(name string, pos int, typ string) *Column {
	return &Column{Name: name, Position: pos, Type: typ, Nullable: true, Charset: "utf8mb4", Collation: "utf8mb4_0900_ai_ci"}
}

// pair returns a source and target model with one identical table "t".
func pair() (*Model, *Model) {
	mk := func(schema string) *Model {
		return &Model{Schema: schema, Tables: []*Table{{
			Name: "t", Engine: "InnoDB", Charset: "utf8mb4", Collation: "utf8mb4_0900_ai_ci",
			Columns: []*Column{
				{Name: "id", Position: 1, Type: "int", AutoIncrement: true},
				col("name", 2, "varchar(64)"),
			},
			Indexes: []*Index{{Name: "PRIMARY", Primary: true, Unique: true, Type: "BTREE", Visible: true,
				Parts: []IndexPart{{Column: "id"}}}},
		}}}
	}
	return mk("src"), mk("dst")
}

func find(d *Diff, object, name string) *Item {
	for i := range d.Items {
		if d.Items[i].Object == object && d.Items[i].Name == name {
			return &d.Items[i]
		}
	}
	return nil
}

func TestCompareIdentical(t *testing.T) {
	s, d := pair()
	diff := Compare(s, d, DefaultOptions())
	if len(diff.Items) != 1 || diff.Items[0].Status != Identical {
		t.Fatalf("items = %+v", diff.Items)
	}
	if diff.HasChanges() {
		t.Error("identical models should have no changes")
	}
	if c := diff.Counts(); c.Identical != 1 || c.Different != 0 {
		t.Errorf("counts = %+v", c)
	}
}

func TestCompareTablesOnlyOnOneSide(t *testing.T) {
	s, d := pair()
	s.Tables = append(s.Tables, &Table{Name: "new_t"})
	d.Tables = append(d.Tables, &Table{Name: "old_t"})

	diff := Compare(s, d, DefaultOptions())
	if it := find(diff, ObjectTable, "new_t"); it == nil || it.Status != OnlyInSource || it.Action != ActionCreate || it.Destructive {
		t.Errorf("new_t = %+v", it)
	}
	it := find(diff, ObjectTable, "old_t")
	if it == nil || it.Status != OnlyInTarget || it.Action != ActionNone || it.Destructive || !strings.Contains(it.Hint, "--drop-extra") {
		t.Errorf("old_t without --drop-extra = %+v", it)
	}

	opts := DefaultOptions()
	opts.DropExtra = true
	diff = Compare(s, d, opts)
	if it := find(diff, ObjectTable, "old_t"); it.Action != ActionDrop || !it.Destructive {
		t.Errorf("old_t with --drop-extra = %+v", it)
	}
}

func TestCompareColumnChanges(t *testing.T) {
	s, d := pair()
	d.Tables[0].Column("name").Type = "varchar(128)" // source narrower
	s.Tables[0].Column("name").Nullable = false      // NULL -> NOT NULL
	diff := Compare(s, d, DefaultOptions())
	it := find(diff, ObjectColumn, "name")
	if it == nil || it.Status != Different || !it.Destructive {
		t.Fatalf("name = %+v", it)
	}
	if got := it.Changes["type"]; got.From != "varchar(128)" || got.To != "varchar(64)" {
		t.Errorf("type change = %+v (from is the target, to the source)", got)
	}
	if _, ok := it.Changes["nullable"]; !ok || !strings.Contains(it.Hint, "NOT NULL") {
		t.Errorf("nullable change missing: %+v", it)
	}
}

func TestCompareWideningIsNotDestructive(t *testing.T) {
	s, d := pair()
	s.Tables[0].Column("name").Type = "varchar(255)"
	it := find(Compare(s, d, DefaultOptions()), ObjectColumn, "name")
	if it == nil || it.Destructive {
		t.Fatalf("widening = %+v", it)
	}
}

func TestCompareCharsetChangeIsDestructive(t *testing.T) {
	s, d := pair()
	d.Tables[0].Column("name").Charset = "latin1"
	d.Tables[0].Column("name").Collation = "latin1_swedish_ci"
	it := find(Compare(s, d, DefaultOptions()), ObjectColumn, "name")
	if it == nil || !it.Destructive {
		t.Fatalf("charset change = %+v", it)
	}
	opts := DefaultOptions()
	opts.IgnoreCharset = true
	if it := find(Compare(s, d, opts), ObjectColumn, "name"); it != nil {
		t.Errorf("--ignore-charset still reports %+v", it)
	}
}

func TestCompareOptions(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(s, d *Model)
		enable  func(o *Options)
		object  string
		item    string
		changes string
	}{
		{"auto_increment", func(s, d *Model) { s.Tables[0].AutoIncrement = 10; d.Tables[0].AutoIncrement = 3 },
			func(o *Options) { o.CompareAutoIncrement = true }, ObjectTable, "t", "auto_increment"},
		{"column position", func(s, d *Model) {
			d.Tables[0].Columns = []*Column{d.Tables[0].Columns[1], d.Tables[0].Columns[0]}
		}, func(o *Options) { o.CompareColumnPosition = true }, ObjectColumn, "name", "after"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, d := pair()
			c.mutate(s, d)
			off := Compare(s, d, DefaultOptions())
			if it := find(off, c.object, c.item); it != nil && it.Status != Identical {
				t.Errorf("default options report %+v", it)
			}
			opts := DefaultOptions()
			c.enable(&opts)
			it := find(Compare(s, d, opts), c.object, c.item)
			if it == nil {
				t.Fatal("enabled option reports nothing")
			}
			if _, ok := it.Changes[c.changes]; !ok {
				t.Errorf("changes = %+v, want %q", it.Changes, c.changes)
			}
		})
	}

	// Options on by default that can be switched off.
	off := []struct {
		name    string
		mutate  func(s, d *Model)
		disable func(o *Options)
		object  string
		item    string
	}{
		{"comments", func(s, d *Model) { s.Tables[0].Column("name").Comment = "x" },
			func(o *Options) { o.IgnoreComments = true }, ObjectColumn, "name"},
		{"engine", func(s, d *Model) { d.Tables[0].Engine = "MyISAM" },
			func(o *Options) { o.CompareEngine = false }, ObjectTable, "t"},
		{"partitions", func(s, d *Model) { s.Tables[0].Partitioning = "PARTITION BY HASH (id) PARTITIONS 4" },
			func(o *Options) { o.ComparePartitions = false }, ObjectTable, "t"},
	}
	for _, c := range off {
		t.Run(c.name, func(t *testing.T) {
			s, d := pair()
			c.mutate(s, d)
			if it := find(Compare(s, d, DefaultOptions()), c.object, c.item); it == nil || it.Status != Different {
				t.Errorf("default options miss the change: %+v", it)
			}
			opts := DefaultOptions()
			c.disable(&opts)
			if it := find(Compare(s, d, opts), c.object, c.item); it != nil && it.Status != Identical {
				t.Errorf("disabled option still reports %+v", it)
			}
		})
	}
}

func TestCompareRenameHint(t *testing.T) {
	s, d := pair()
	s.Tables[0].Columns[1] = col("full_name", 2, "varchar(64)")
	diff := Compare(s, d, DefaultOptions())
	dropped := find(diff, ObjectColumn, "name")
	if dropped == nil || !dropped.Destructive || dropped.Action != ActionDrop {
		t.Fatalf("dropped = %+v", dropped)
	}
	if !strings.Contains(dropped.Hint, "rename of full_name") {
		t.Errorf("hint = %q", dropped.Hint)
	}
	if added := find(diff, ObjectColumn, "full_name"); added == nil || added.Status != OnlyInSource {
		t.Errorf("added = %+v", added)
	}
}

func TestCompareIndexes(t *testing.T) {
	s, d := pair()
	s.Tables[0].Indexes = append(s.Tables[0].Indexes, &Index{Name: "idx_name", Type: "BTREE", Visible: true, Parts: []IndexPart{{Column: "name", SubPart: 10}}})
	d.Tables[0].Indexes = append(d.Tables[0].Indexes,
		&Index{Name: "idx_name", Type: "BTREE", Visible: true, Parts: []IndexPart{{Column: "name"}}},
		&Index{Name: "idx_extra", Type: "BTREE", Visible: true, Parts: []IndexPart{{Column: "name"}}})
	diff := Compare(s, d, DefaultOptions())
	if it := find(diff, ObjectIndex, "idx_name"); it == nil || it.Action != ActionRecreate || it.Destructive {
		t.Errorf("idx_name = %+v", it)
	}
	if it := find(diff, ObjectIndex, "idx_extra"); it == nil || it.Action != ActionNone {
		t.Errorf("idx_extra = %+v", it)
	}
}

func TestCompareCrossVariantNormalization(t *testing.T) {
	s, d := pair()
	d.Tables[0].Column("id").Type = "int(11)"
	s.Tables[0].Columns = append(s.Tables[0].Columns, &Column{Name: "g", Position: 3, Type: "int", Generated: "(`id` * 2)", GeneratedStored: true})
	d.Tables[0].Columns = append(d.Tables[0].Columns, &Column{Name: "g", Position: 3, Type: "int(11)", Generated: "`id` * 2", GeneratedStored: true})
	s.Tables[0].ForeignKeys = []*ForeignKey{{Name: "fk", Columns: []string{"id"}, RefSchema: "src", RefTable: "t", RefColumns: []string{"id"}, OnUpdate: "restrict", OnDelete: "CASCADE"}}
	d.Tables[0].ForeignKeys = []*ForeignKey{{Name: "fk", Columns: []string{"id"}, RefSchema: "dst", RefTable: "t", RefColumns: []string{"id"}, OnUpdate: "RESTRICT", OnDelete: "CASCADE"}}
	z := "0"
	s.Tables[0].Column("name").Default, d.Tables[0].Column("name").Default = &z, strp("0")
	diff := Compare(s, d, DefaultOptions())
	if diff.HasChanges() {
		t.Errorf("cosmetic differences reported: %+v", diff.Items)
	}
}

func TestCompareForeignKeyRulesAndDefinition(t *testing.T) {
	fk := func(schema, onDelete, def string) []*ForeignKey {
		return []*ForeignKey{{Name: "fk", Columns: []string{"id"}, RefSchema: schema, RefTable: "t", RefColumns: []string{"id"},
			OnUpdate: "NO ACTION", OnDelete: onDelete, Definition: def}}
	}
	cases := []struct {
		name       string
		src, dst   []*ForeignKey
		wantChange string
	}{
		// PostgreSQL: NO ACTION is checked at statement end and can be
		// deferred, RESTRICT cannot; they are not the same rule.
		{"no action vs restrict", fk("src", "RESTRICT", ""), fk("dst", "NO ACTION", ""), "on_delete"},
		{"deferrable", fk("src", "CASCADE", "FOREIGN KEY (id) REFERENCES src.t(id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED"),
			fk("dst", "CASCADE", "FOREIGN KEY (id) REFERENCES dst.t(id) ON DELETE CASCADE"), "definition"},
		{"match full", fk("src", "CASCADE", "FOREIGN KEY (id) REFERENCES src.t(id) MATCH FULL ON DELETE CASCADE"),
			fk("dst", "CASCADE", "FOREIGN KEY (id) REFERENCES dst.t(id) ON DELETE CASCADE"), "definition"},
		// The definition covers the rules: one change, not two.
		{"rule in definition", fk("src", "CASCADE", "FOREIGN KEY (id) REFERENCES src.t(id) ON DELETE CASCADE"),
			fk("dst", "NO ACTION", "FOREIGN KEY (id) REFERENCES dst.t(id)"), "definition"},
		{"one-sided definition", fk("src", "CASCADE", "FOREIGN KEY (id) REFERENCES src.t(id) ON DELETE CASCADE DEFERRABLE"),
			fk("dst", "CASCADE", ""), "definition"},
		{"same modulo schema", fk("src", "CASCADE", "FOREIGN KEY (id) REFERENCES src.t(id) ON DELETE CASCADE DEFERRABLE"),
			fk("dst", "CASCADE", `FOREIGN KEY (id) REFERENCES "dst".t(id) ON DELETE CASCADE DEFERRABLE`), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, d := pair()
			s.Tables[0].ForeignKeys, d.Tables[0].ForeignKeys = c.src, c.dst
			it := find(Compare(s, d, DefaultOptions()), ObjectForeignKey, "fk")
			if c.wantChange == "" {
				if it != nil {
					t.Errorf("identical foreign keys reported: %+v", it)
				}
				return
			}
			if it == nil || it.Action != ActionRecreate {
				t.Fatalf("fk = %+v, want recreate", it)
			}
			if _, ok := it.Changes[c.wantChange]; !ok || len(it.Changes) != 1 {
				t.Errorf("changes = %+v, want only %q", it.Changes, c.wantChange)
			}
		})
	}
}

func TestCompareIgnoresForeignKeyValidation(t *testing.T) {
	s, d := pair()
	def := "FOREIGN KEY (id) REFERENCES %s.t(id) ON DELETE CASCADE"
	s.Tables[0].ForeignKeys = []*ForeignKey{{Name: "fk", Columns: []string{"id"}, RefSchema: "src", RefTable: "t", RefColumns: []string{"id"},
		OnDelete: "CASCADE", Definition: fmt.Sprintf(def, "src"), NotValid: true}}
	d.Tables[0].ForeignKeys = []*ForeignKey{{Name: "fk", Columns: []string{"id"}, RefSchema: "dst", RefTable: "t", RefColumns: []string{"id"},
		OnDelete: "CASCADE", Definition: fmt.Sprintf(def, "dst")}}
	if diff := Compare(s, d, DefaultOptions()); diff.HasChanges() {
		t.Errorf("validation state reported as a schema difference: %+v", diff.Items)
	}
}

func TestCompareDefaults(t *testing.T) {
	s, d := pair()
	s.Tables[0].Column("name").Default = strp("x")
	it := find(Compare(s, d, DefaultOptions()), ObjectColumn, "name")
	if it == nil || !reflect.DeepEqual(it.Changes["default"], Change{nil, "x"}) {
		t.Fatalf("default change = %+v", it)
	}
	ts := strp("CURRENT_TIMESTAMP")
	mts := strp("current_timestamp()")
	s.Tables[0].Column("name").Default, s.Tables[0].Column("name").DefaultIsExpr = ts, true
	d.Tables[0].Column("name").Default, d.Tables[0].Column("name").DefaultIsExpr = mts, true
	if it := find(Compare(s, d, DefaultOptions()), ObjectColumn, "name"); it != nil {
		t.Errorf("MySQL and MariaDB spellings of CURRENT_TIMESTAMP differ: %+v", it)
	}
}

func TestCompareDefinitions(t *testing.T) {
	s, d := pair()
	opts := DefaultOptions()
	opts.Objects = ObjectSet{Views: true, Routines: true}
	s.Views = []*View{{Name: "v", Definer: "root@%", CreateSQL: "CREATE DEFINER=`root`@`%` VIEW `v` AS select `src`.`t`.`id` AS `id` from `src`.`t`"}}
	d.Views = []*View{{Name: "v", Definer: "dev@%", CreateSQL: "CREATE DEFINER=`dev`@`%` VIEW `v` AS select `dst`.`t`.`id` AS `id` from `dst`.`t`"}}
	d.Routines = []*Routine{{Name: "p", Kind: "PROCEDURE", CreateSQL: "CREATE PROCEDURE `p`() SELECT 1"}}

	diff := Compare(s, d, opts)
	if it := find(diff, ObjectView, "v"); it == nil || it.Status != Different {
		t.Fatalf("view with another definer = %+v", it)
	} else if _, ok := it.Changes["definition"]; ok {
		t.Errorf("schema qualifiers should normalize away: %+v", it.Changes)
	}
	opts.IgnoreDefiner = true
	diff = Compare(s, d, opts)
	if it := find(diff, ObjectView, "v"); it == nil || it.Status != Identical {
		t.Errorf("--ignore-definer view = %+v", it)
	}
	if it := find(diff, ObjectRoutine, "p"); it == nil || it.Status != OnlyInTarget || it.Action != ActionNone {
		t.Errorf("target-only routine = %+v", it)
	}
}

func TestStripDefiner(t *testing.T) {
	got := StripDefiner("CREATE DEFINER=`root`@`%` PROCEDURE `p`() SELECT 1")
	if got != "CREATE PROCEDURE `p`() SELECT 1" {
		t.Errorf("StripDefiner = %q", got)
	}
}

func TestRowKey(t *testing.T) {
	s, _ := pair()
	tbl := s.Tables[0]
	if k := tbl.RowKey(); len(k) != 1 || k[0].Name != "id" {
		t.Fatalf("pk key = %v", k)
	}
	tbl.Indexes = []*Index{
		{Name: "uq_nullable", Unique: true, Parts: []IndexPart{{Column: "name"}}},
	}
	if k := tbl.RowKey(); k != nil {
		t.Errorf("unique over a nullable column is no key: %v", k)
	}
	tbl.Column("name").Nullable = false
	if k := tbl.RowKey(); len(k) != 1 || k[0].Name != "name" {
		t.Errorf("unique NOT NULL key = %v", k)
	}
	tbl.Indexes[0].Parts[0].SubPart = 5
	if k := tbl.RowKey(); k != nil {
		t.Errorf("prefix index is no key: %v", k)
	}
	tbl.Indexes[0].Parts[0].SubPart = 0
	tbl.Column("name").Generated = "upper(id)"
	if k := tbl.RowKey(); k != nil {
		t.Errorf("generated column is no key: %v", k)
	}
}

func TestFilter(t *testing.T) {
	m := &Model{Tables: []*Table{{Name: "a"}, {Name: "b"}, {Name: "c"}},
		Triggers: []*Trigger{{Name: "tr", Table: "b"}}}
	got := m.Filter([]string{"a", "b"}, []string{"a"})
	if len(got.Tables) != 1 || got.Tables[0].Name != "b" || len(got.Triggers) != 1 {
		t.Errorf("Filter = %+v", got)
	}
}

func TestFilterSchemaWideObjects(t *testing.T) {
	aliases := map[string]string{"NO ACTION": "RESTRICT"}
	m := &Model{RuleAliases: aliases, Tables: []*Table{{Name: "a"}, {Name: "b"}},
		Routines: []*Routine{{Name: "p"}}, Events: []*Event{{Name: "e"}}}
	got := m.Filter([]string{"a"}, nil)
	if len(got.Routines) != 0 || len(got.Events) != 0 {
		t.Errorf("--tables kept routines/events: %+v %+v", got.Routines, got.Events)
	}
	if got.RuleAliases["NO ACTION"] != "RESTRICT" {
		t.Errorf("Filter dropped RuleAliases")
	}
	got = m.Filter(nil, []string{"a"})
	if len(got.Routines) != 1 || len(got.Events) != 1 || got.RuleAliases == nil {
		t.Errorf("--exclude-tables alone must keep routines, events and aliases: %+v", got)
	}
}
