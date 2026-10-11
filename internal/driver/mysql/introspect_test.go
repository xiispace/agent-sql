package mysql

import (
	"database/sql"
	"testing"

	"github.com/shhac/agent-sql/internal/schemadiff"
)

func TestNormalizeDefault(t *testing.T) {
	cases := []struct {
		name   string
		raw    sql.NullString
		extra  string
		want   *string
		isExpr bool
	}{
		{"no default", sql.NullString{}, "", nil, false},
		{"mysql literal", sql.NullString{String: "abc", Valid: true}, "", strp("abc"), false},
		{"mysql expression", sql.NullString{String: "CURRENT_TIMESTAMP(3)", Valid: true}, "DEFAULT_GENERATED", strp("CURRENT_TIMESTAMP(3)"), true},
		{"mysql uuid expression", sql.NullString{String: "uuid()", Valid: true}, "DEFAULT_GENERATED", strp("uuid()"), true},
		{"mariadb null", sql.NullString{String: "NULL", Valid: true}, "", nil, false},
		{"mariadb quoted", sql.NullString{String: "'it''s'", Valid: true}, "", strp("it's"), false},
		{"mariadb empty string", sql.NullString{String: "''", Valid: true}, "", strp(""), false},
		{"mariadb number", sql.NullString{String: "0", Valid: true}, "", strp("0"), false},
		{"mariadb expression", sql.NullString{String: "current_timestamp()", Valid: true}, "", strp("current_timestamp()"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, isExpr := normalizeDefault(c.raw, c.extra)
			if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) || isExpr != c.isExpr {
				t.Errorf("normalizeDefault = (%v, %v), want (%v, %v)", deref(got), isExpr, deref(c.want), c.isExpr)
			}
		})
	}
}

func strp(s string) *string { return &s }

func deref(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func TestApplyExtra(t *testing.T) {
	c := &schemadiff.Column{}
	applyExtra(c, "DEFAULT_GENERATED on update CURRENT_TIMESTAMP(3)")
	if c.OnUpdate != "CURRENT_TIMESTAMP(3)" || c.AutoIncrement {
		t.Errorf("on update = %+v", c)
	}
	c = &schemadiff.Column{}
	applyExtra(c, "auto_increment INVISIBLE")
	if !c.AutoIncrement || !c.Invisible {
		t.Errorf("auto_increment invisible = %+v", c)
	}
	c = &schemadiff.Column{}
	applyExtra(c, "STORED GENERATED")
	if !c.GeneratedStored {
		t.Errorf("stored generated = %+v", c)
	}
}

func TestPartitionClause(t *testing.T) {
	ddl := "CREATE TABLE `t` (\n  `id` int\n) ENGINE=InnoDB\n/*!50100 PARTITION BY HASH (`id`)\nPARTITIONS 4 */"
	got := partitionClause(ddl)
	if got != "/*!50100 PARTITION BY HASH (`id`)\nPARTITIONS 4 */" {
		t.Errorf("partitionClause = %q", got)
	}
	if unwrapVersionComment(got) != "PARTITION BY HASH (`id`)\nPARTITIONS 4" {
		t.Errorf("unwrap = %q", unwrapVersionComment(got))
	}
	if partitionClause("CREATE TABLE `t` (`id` int)") != "" {
		t.Error("unpartitioned table has a clause")
	}
}

func TestCharsetOf(t *testing.T) {
	for coll, want := range map[string]string{"utf8mb4_0900_ai_ci": "utf8mb4", "latin1_swedish_ci": "latin1", "binary": "binary"} {
		if got := charsetOf(coll); got != want {
			t.Errorf("charsetOf(%q) = %q", coll, got)
		}
	}
}

func TestInnodbRuleAliases(t *testing.T) {
	fks := func(schema, onDelete string) *schemadiff.Model {
		return &schemadiff.Model{Schema: schema, RuleAliases: innodbRuleAliases, Tables: []*schemadiff.Table{{Name: "t",
			ForeignKeys: []*schemadiff.ForeignKey{{Name: "fk", Columns: []string{"a"}, RefTable: "p", RefColumns: []string{"id"}, OnUpdate: "RESTRICT", OnDelete: onDelete}}}}}
	}
	if d := schemadiff.Compare(fks("src", "RESTRICT"), fks("dst", "NO ACTION"), schemadiff.DefaultOptions()); d.HasChanges() {
		t.Errorf("InnoDB NO ACTION and RESTRICT reported different: %+v", d.Items)
	}
	// The model keeps the server's spelling: a real change reports it.
	d := schemadiff.Compare(fks("src", "CASCADE"), fks("dst", "NO ACTION"), schemadiff.DefaultOptions())
	if len(d.Items) != 1 || d.Items[0].Changes["on_delete"] != (schemadiff.Change{From: "NO ACTION", To: "CASCADE"}) {
		t.Errorf("items = %+v", d.Items)
	}
}
