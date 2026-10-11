package pg

import (
	"strings"
	"testing"

	"github.com/shhac/agent-sql/internal/driver"
	"github.com/shhac/agent-sql/internal/schemadiff"
)

func strp(s string) *string { return &s }

func pgModels() (*schemadiff.Model, *schemadiff.Model) {
	src := &schemadiff.Model{Schema: "src", Tables: []*schemadiff.Table{
		{Name: "customer", Comment: "customers",
			Columns: []*schemadiff.Column{
				{Name: "id", Position: 1, Type: "integer", Default: strp("nextval('src.customer_id_seq'::regclass)"), DefaultIsExpr: true, AutoIncrement: true},
				{Name: "name", Position: 2, Type: "character varying(32)"},
				{Name: "feeling", Position: 3, Type: "src.mood", Nullable: true},
			},
			Indexes: []*schemadiff.Index{
				{Name: "customer_pkey", Primary: true, Unique: true, Constraint: true, ConstraintDefinition: "PRIMARY KEY (id)", Parts: []schemadiff.IndexPart{{Column: "id"}}},
				{Name: "idx_name", Definition: "CREATE INDEX idx_name ON src.customer USING btree (lower((name)::text))", Parts: []schemadiff.IndexPart{{Expression: "lower((name)::text)"}}},
			},
			Checks: []*schemadiff.Check{{Name: "ck_name", Definition: "CHECK ((length((name)::text) > 0))"}}},
		{Name: "orders",
			Columns: []*schemadiff.Column{
				{Name: "id", Position: 1, Type: "bigint", Identity: "ALWAYS", AutoIncrement: true},
				{Name: "customer_id", Position: 2, Type: "integer"},
				{Name: "x2", Position: 3, Type: "numeric", Nullable: true, Generated: "(id * 2)", GeneratedStored: true},
			},
			ForeignKeys: []*schemadiff.ForeignKey{{Name: "fk_c", Columns: []string{"customer_id"}, RefSchema: "src", RefTable: "customer", RefColumns: []string{"id"}, OnDelete: "CASCADE", OnUpdate: "NO ACTION"}}},
	}}
	dst := &schemadiff.Model{Schema: "dst", Tables: []*schemadiff.Table{
		{Name: "customer", Comment: "customers",
			Columns: []*schemadiff.Column{
				{Name: "id", Position: 1, Type: "integer", Default: strp("nextval('dst.customer_id_seq'::regclass)"), DefaultIsExpr: true, AutoIncrement: true},
				{Name: "name", Position: 2, Type: "character varying(64)"},
				{Name: "feeling", Position: 3, Type: "dst.mood", Nullable: true},
				{Name: "legacy", Position: 4, Type: "integer", Nullable: true},
			},
			Indexes: []*schemadiff.Index{
				{Name: "customer_pkey", Primary: true, Unique: true, Constraint: true, ConstraintDefinition: "PRIMARY KEY (id)", Parts: []schemadiff.IndexPart{{Column: "id"}}},
			},
			Checks: []*schemadiff.Check{{Name: "ck_name", Definition: "CHECK ((length((name)::text) > 0))"}}},
	}}
	return src, dst
}

func render(t *testing.T, mode string) []schemadiff.Statement {
	t.Helper()
	src, dst := pgModels()
	stmts, err := (&pgConn{}).RenderDDL(schemadiff.Compare(src, dst, schemadiff.DefaultOptions()),
		schemadiff.RenderOpts{Mode: mode, TargetSchema: "dst"})
	if err != nil {
		t.Fatal(err)
	}
	return stmts
}

func joined(stmts []schemadiff.Statement) string {
	out := make([]string, len(stmts))
	for i, s := range stmts {
		out[i] = s.SQL
	}
	return strings.Join(out, ";\n")
}

func TestPgDiffNormalizesSchemaQualifiers(t *testing.T) {
	src, dst := pgModels()
	d := schemadiff.Compare(src, dst, schemadiff.DefaultOptions())
	for _, it := range d.Items {
		if it.Object == schemadiff.ObjectColumn && (it.Name == "id" || it.Name == "feeling") {
			t.Errorf("schema-qualified default/type reported as a difference: %+v", it)
		}
	}
}

func TestPgRenderSync(t *testing.T) {
	all := joined(render(t, schemadiff.StructureSync))
	for _, want := range []string{
		`CREATE TABLE "dst"."orders" (` + "\n" + `  "id" bigint GENERATED ALWAYS AS IDENTITY NOT NULL,`,
		`"x2" numeric GENERATED ALWAYS AS ((id * 2)) STORED`,
		`ALTER COLUMN "name" TYPE character varying(32) USING "name"::character varying(32)`,
		`DROP COLUMN "legacy"`,
		`CREATE INDEX idx_name ON "dst".customer USING btree (lower((name)::text))`,
		`ALTER TABLE "dst"."orders" ADD CONSTRAINT "fk_c" FOREIGN KEY ("customer_id") REFERENCES "dst"."customer" ("id") ON DELETE CASCADE`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	// Foreign keys come after every table exists.
	if strings.Index(all, "ADD CONSTRAINT \"fk_c\"") < strings.Index(all, `CREATE TABLE "dst"."orders"`) {
		t.Error("foreign key before its table")
	}
}

func TestPgRenderCreateSerialTable(t *testing.T) {
	src, _ := pgModels()
	stmts, _ := (&pgConn{}).RenderDDL(schemadiff.Compare(src, &schemadiff.Model{Schema: "dst"}, schemadiff.DefaultOptions()),
		schemadiff.RenderOpts{Mode: schemadiff.StructureSync, TargetSchema: "dst"})
	all := joined(stmts)
	for _, want := range []string{
		`CREATE SEQUENCE IF NOT EXISTS "dst".customer_id_seq`,
		`"id" integer DEFAULT nextval('"dst".customer_id_seq'::regclass) NOT NULL`,
		`"feeling" "dst".mood`,
		`CONSTRAINT "customer_pkey" PRIMARY KEY (id)`,
		`CONSTRAINT "ck_name" CHECK ((length((name)::text) > 0))`,
		`ALTER SEQUENCE "dst".customer_id_seq OWNED BY "dst"."customer"."id"`,
		`COMMENT ON TABLE "dst"."customer" IS 'customers'`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	if strings.Index(all, "CREATE SEQUENCE") > strings.Index(all, `CREATE TABLE "dst"."customer"`) {
		t.Error("sequence created after the table that uses it")
	}
}

func TestPgRenderRecreate(t *testing.T) {
	stmts := render(t, schemadiff.StructureRecreate)
	if stmts[0].SQL != `DROP TABLE IF EXISTS "dst"."orders"` || stmts[0].Destructive {
		t.Errorf("first = %+v (orders does not exist on the target)", stmts[0])
	}
	if stmts[1].SQL != `DROP TABLE IF EXISTS "dst"."customer"` || !stmts[1].Destructive {
		t.Errorf("second = %+v", stmts[1])
	}
}

func TestPgSelectSQL(t *testing.T) {
	got := selectSQL("s", driver.ReadRequest{Table: "t", Columns: []string{"id", "k"}, Where: "id > 1",
		OrderBy: []driver.KeyColumn{{Name: "id", Kind: schemadiff.KeyInteger}, {Name: "k", Kind: schemadiff.KeyBytes}}})
	want := `SELECT "id", "k" FROM "s"."t" WHERE (id > 1) ORDER BY "id", ("k")::text COLLATE "C"`
	if got != want {
		t.Errorf("selectSQL = %s", got)
	}
}

func TestPgTuples(t *testing.T) {
	values, params := tuples(2, [][][]byte{{[]byte("1"), nil}, {[]byte("2"), []byte("")}})
	if values != "($1, $2), ($3, $4)" || len(params) != 4 || params[1] != nil || params[3] == nil {
		t.Errorf("tuples = %q %#v", values, params)
	}
}

func TestPgKeyKinds(t *testing.T) {
	for typ, want := range map[string]schemadiff.KeyKind{
		"integer": schemadiff.KeyInteger, "bigint": schemadiff.KeyInteger, "smallint": schemadiff.KeyInteger,
		"numeric(10,2)": schemadiff.KeyDecimal, "double precision": schemadiff.KeyFloat,
		"text": schemadiff.KeyBytes, "uuid": schemadiff.KeyBytes, "character varying(10)": schemadiff.KeyBytes,
	} {
		if got := schemadiff.KeyKindOf(&schemadiff.Column{Type: typ}); got != want {
			t.Errorf("KeyKindOf(%q) = %v, want %v", typ, got, want)
		}
	}
}

func TestPgRenderRecreateKeepsTargetOnlyViews(t *testing.T) {
	for _, dropExtra := range []bool{false, true} {
		src, dst := pgModels()
		src.Views = []*schemadiff.View{{Name: "v_src", CreateSQL: "CREATE VIEW src.v_src AS SELECT id FROM src.customer"}}
		dst.Views = []*schemadiff.View{{Name: "reporting_v", CreateSQL: "CREATE VIEW dst.reporting_v AS SELECT id FROM dst.customer"}}
		opts := schemadiff.DefaultOptions()
		opts.Objects.Views = true
		opts.DropExtra = dropExtra
		stmts, err := (&pgConn{}).RenderDDL(schemadiff.Compare(src, dst, opts),
			schemadiff.RenderOpts{Mode: schemadiff.StructureRecreate, TargetSchema: "dst"})
		if err != nil {
			t.Fatal(err)
		}
		all := joined(stmts)
		recreated := strings.Contains(all, "CREATE VIEW dst.reporting_v AS SELECT id FROM dst.customer")
		if recreated == dropExtra {
			t.Errorf("drop-extra=%v: target-only view recreated=%v in\n%s", dropExtra, recreated, all)
		}
		if !strings.Contains(all, `CREATE VIEW "dst".v_src AS SELECT id FROM "dst".customer`) {
			t.Errorf("source view not recreated in\n%s", all)
		}
		for _, s := range stmts {
			if s.SQL == `DROP VIEW IF EXISTS "dst"."reporting_v" CASCADE` && s.Destructive != dropExtra {
				t.Errorf("drop-extra=%v: dropping reporting_v destructive=%v", dropExtra, s.Destructive)
			}
		}
	}
}

func TestPgRenderForeignKeyDefinition(t *testing.T) {
	src, dst := pgModels()
	fk := src.Table("orders").ForeignKeys[0]
	fk.Definition = "FOREIGN KEY (customer_id) REFERENCES src.customer(id) MATCH FULL ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED"
	dst.Tables = append(dst.Tables, &schemadiff.Table{Name: "orders", Columns: src.Table("orders").Columns,
		ForeignKeys: []*schemadiff.ForeignKey{{Name: "fk_c", Columns: []string{"customer_id"}, RefSchema: "dst", RefTable: "customer",
			RefColumns: []string{"id"}, OnDelete: "CASCADE", OnUpdate: "NO ACTION",
			Definition: "FOREIGN KEY (customer_id) REFERENCES dst.customer(id) ON DELETE CASCADE"}}})
	stmts, err := (&pgConn{}).RenderDDL(schemadiff.Compare(src, dst, schemadiff.DefaultOptions()),
		schemadiff.RenderOpts{Mode: schemadiff.StructureSync, TargetSchema: "dst"})
	if err != nil {
		t.Fatal(err)
	}
	all := joined(stmts)
	drop := strings.Index(all, `ALTER TABLE "dst"."orders" DROP CONSTRAINT "fk_c"`)
	add := strings.Index(all, `ALTER TABLE "dst"."orders" ADD CONSTRAINT "fk_c" FOREIGN KEY (customer_id) REFERENCES "dst".customer(id) MATCH FULL ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED`)
	if drop < 0 || add < 0 || drop > add {
		t.Errorf("foreign key not recreated from its definition:\n%s", all)
	}
}

func TestPgRenderForeignKeyNotValid(t *testing.T) {
	src, _ := pgModels()
	fk := src.Table("orders").ForeignKeys[0]
	fk.NotValid = true
	all := joined(func() []schemadiff.Statement {
		stmts, _ := (&pgConn{}).RenderDDL(schemadiff.Compare(src, &schemadiff.Model{Schema: "dst"}, schemadiff.DefaultOptions()),
			schemadiff.RenderOpts{Mode: schemadiff.StructureSync, TargetSchema: "dst"})
		return stmts
	}())
	if !strings.Contains(all, `REFERENCES "dst"."customer" ("id") ON DELETE CASCADE NOT VALID`) {
		t.Errorf("fallback rendering lost NOT VALID:\n%s", all)
	}
	fk.Definition = "FOREIGN KEY (customer_id) REFERENCES src.customer(id) ON DELETE CASCADE"
	stmts, _ := (&pgConn{}).RenderDDL(schemadiff.Compare(src, &schemadiff.Model{Schema: "dst"}, schemadiff.DefaultOptions()),
		schemadiff.RenderOpts{Mode: schemadiff.StructureSync, TargetSchema: "dst"})
	if all := joined(stmts); !strings.Contains(all, `REFERENCES "dst".customer(id) ON DELETE CASCADE NOT VALID`) {
		t.Errorf("definition rendering lost NOT VALID:\n%s", all)
	}
}

func TestStripDatabaseForeignKeys(t *testing.T) {
	// CockroachDB renders three-part names in constraint definitions.
	m := &schemadiff.Model{Schema: "src", Tables: []*schemadiff.Table{{Name: "orders",
		ForeignKeys: []*schemadiff.ForeignKey{{Name: "fk", Definition: `FOREIGN KEY (customer_id) REFERENCES srcdb.src.customer(id) ON DELETE CASCADE`}}}}}
	stripDatabase(m, "srcdb")
	if got, want := m.Tables[0].ForeignKeys[0].Definition, "FOREIGN KEY (customer_id) REFERENCES src.customer(id) ON DELETE CASCADE"; got != want {
		t.Errorf("Definition = %q, want %q", got, want)
	}
}
