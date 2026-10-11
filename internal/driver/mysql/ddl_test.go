package mysql

import (
	"strings"
	"testing"

	"github.com/shhac/agent-sql/internal/schemadiff"
)

func ddlModels() (*schemadiff.Model, *schemadiff.Model) {
	src := &schemadiff.Model{Schema: "src", Tables: []*schemadiff.Table{
		{Name: "customer", Engine: "InnoDB", Charset: "utf8mb4", Collation: "utf8mb4_0900_ai_ci",
			CreateSQL: "CREATE TABLE `customer` (\n  `id` int NOT NULL AUTO_INCREMENT,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB AUTO_INCREMENT=42 DEFAULT CHARSET=utf8mb4",
			Columns: []*schemadiff.Column{
				{Name: "id", Position: 1, Type: "int", AutoIncrement: true},
				{Name: "name", Position: 2, Type: "varchar(32)", Charset: "utf8mb4", Collation: "utf8mb4_0900_ai_ci"},
				{Name: "status", Position: 3, Type: "varchar(8)", Nullable: true, Default: strp("new"), Charset: "utf8mb4", Collation: "utf8mb4_0900_ai_ci", Comment: "it's"},
			},
			Indexes: []*schemadiff.Index{
				{Name: "PRIMARY", Primary: true, Unique: true, Type: "BTREE", Visible: true, Parts: []schemadiff.IndexPart{{Column: "id"}}},
				{Name: "idx_name", Type: "BTREE", Visible: true, Parts: []schemadiff.IndexPart{{Column: "name", SubPart: 10}}},
			}},
		{Name: "orders", CreateSQL: "CREATE TABLE `orders` (\n  `id` int,\n  `customer_id` int\n) ENGINE=InnoDB",
			Columns:     []*schemadiff.Column{{Name: "id", Position: 1, Type: "int"}, {Name: "customer_id", Position: 2, Type: "int"}},
			ForeignKeys: []*schemadiff.ForeignKey{{Name: "fk_c", Columns: []string{"customer_id"}, RefSchema: "src", RefTable: "customer", RefColumns: []string{"id"}, OnDelete: "CASCADE", OnUpdate: "RESTRICT"}}},
	}}
	dst := &schemadiff.Model{Schema: "dst", Tables: []*schemadiff.Table{
		{Name: "customer", Engine: "InnoDB", Charset: "utf8mb4", Collation: "utf8mb4_0900_ai_ci",
			Columns: []*schemadiff.Column{
				{Name: "id", Position: 1, Type: "int", AutoIncrement: true},
				{Name: "name", Position: 2, Type: "varchar(64)", Charset: "utf8mb4", Collation: "utf8mb4_0900_ai_ci"},
				{Name: "legacy", Position: 3, Type: "int", Nullable: true},
			},
			Indexes: []*schemadiff.Index{
				{Name: "PRIMARY", Primary: true, Unique: true, Type: "BTREE", Visible: true, Parts: []schemadiff.IndexPart{{Column: "id"}}},
			}},
		{Name: "dev_only"},
	}}
	return src, dst
}

func renderSQL(t *testing.T, mode string, opts schemadiff.Options) []schemadiff.Statement {
	t.Helper()
	src, dst := ddlModels()
	c := &mysqlConn{variant: "mysql"}
	stmts, err := c.RenderDDL(schemadiff.Compare(src, dst, opts), schemadiff.RenderOpts{Mode: mode, TargetSchema: "dst"})
	if err != nil {
		t.Fatal(err)
	}
	return stmts
}

func TestRenderDDLSync(t *testing.T) {
	stmts := renderSQL(t, schemadiff.StructureSync, schemadiff.DefaultOptions())
	var got []string
	for _, s := range stmts {
		got = append(got, s.SQL)
	}
	all := strings.Join(got, ";\n")
	if stmts[0].SQL != "USE `dst`" {
		t.Errorf("first statement = %q", stmts[0].SQL)
	}
	for _, want := range []string{
		"CREATE TABLE `dst`.`orders` (",
		"ALTER TABLE `dst`.`customer`\n  MODIFY COLUMN `name` varchar(32) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci NOT NULL,\n  DROP COLUMN `legacy`",
		"ADD COLUMN `status` varchar(8) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci NULL DEFAULT 'new' COMMENT 'it''s' AFTER `name`",
		"ADD KEY `idx_name` (`name`(10))",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	if strings.Contains(all, "dev_only") {
		t.Errorf("target-only table touched without --drop-extra:\n%s", all)
	}
	for _, s := range stmts {
		if strings.Contains(s.SQL, "DROP COLUMN") && !s.Destructive {
			t.Errorf("drop column not destructive: %q", s.SQL)
		}
		if strings.Contains(s.SQL, "ADD COLUMN") && s.Destructive {
			t.Errorf("additive alter marked destructive: %q", s.SQL)
		}
	}
}

func TestRenderDDLDropExtraAndForeignKeys(t *testing.T) {
	opts := schemadiff.DefaultOptions()
	opts.DropExtra = true
	stmts := renderSQL(t, schemadiff.StructureSync, opts)
	last := stmts[len(stmts)-1]
	if last.SQL != "DROP TABLE `dst`.`dev_only`" || !last.Destructive {
		t.Errorf("last = %+v", last)
	}
}

func TestRenderDDLRecreate(t *testing.T) {
	stmts := renderSQL(t, schemadiff.StructureRecreate, schemadiff.DefaultOptions())
	var sqls []string
	for _, s := range stmts[1:] {
		sqls = append(sqls, s.SQL)
	}
	// Drops in reverse dependency order, creates referenced tables first.
	want := []string{
		"DROP TABLE IF EXISTS `dst`.`orders`",
		"DROP TABLE IF EXISTS `dst`.`customer`",
	}
	for i, w := range want {
		if sqls[i] != w {
			t.Errorf("stmt %d = %q, want %q", i, sqls[i], w)
		}
	}
	if !strings.HasPrefix(sqls[2], "CREATE TABLE `dst`.`customer`") || !strings.Contains(sqls[2], "AUTO_INCREMENT=42") {
		t.Errorf("recreate keeps AUTO_INCREMENT: %q", sqls[2])
	}
	if !stmts[2].Destructive || stmts[1].Destructive {
		t.Errorf("only dropping an existing table is destructive: %+v %+v", stmts[1], stmts[2])
	}
}

func TestRenderDDLStripsAutoIncrementOnSyncCreate(t *testing.T) {
	src, _ := ddlModels()
	c := &mysqlConn{variant: "mysql"}
	stmts, _ := c.RenderDDL(schemadiff.Compare(src, &schemadiff.Model{Schema: "dst"}, schemadiff.DefaultOptions()),
		schemadiff.RenderOpts{Mode: schemadiff.StructureSync, TargetSchema: "dst"})
	for _, s := range stmts {
		if strings.Contains(s.SQL, "AUTO_INCREMENT=") {
			t.Errorf("sync create keeps AUTO_INCREMENT value: %q", s.SQL)
		}
	}
	// Referenced table first.
	if !strings.Contains(stmts[1].SQL, "`customer`") || !strings.Contains(stmts[2].SQL, "`orders`") {
		t.Errorf("order: %q, %q", stmts[1].SQL, stmts[2].SQL)
	}
}

func TestForeignKeyDef(t *testing.T) {
	src, dst := ddlModels()
	r := &renderer{diff: schemadiff.Compare(src, dst, schemadiff.DefaultOptions()), schema: "dst"}
	got := r.foreignKeyDef(src.Tables[1].ForeignKeys[0])
	want := "CONSTRAINT `fk_c` FOREIGN KEY (`customer_id`) REFERENCES `dst`.`customer` (`id`) ON DELETE CASCADE"
	if got != want {
		t.Errorf("foreignKeyDef = %q", got)
	}
}

func TestColumnDefGeneratedAndExpressionDefault(t *testing.T) {
	r := &renderer{variant: "mysql"}
	gen := &schemadiff.Column{Name: "g", Type: "int", Nullable: true, Generated: "`a` * 2", GeneratedStored: true}
	if got := r.columnDef(gen); got != "`g` int GENERATED ALWAYS AS (`a` * 2) STORED" {
		t.Errorf("generated = %q", got)
	}
	ts := &schemadiff.Column{Name: "ts", Type: "datetime(3)", Nullable: true, Default: strp("CURRENT_TIMESTAMP(3)"), DefaultIsExpr: true, OnUpdate: "CURRENT_TIMESTAMP(3)"}
	if got := r.columnDef(ts); got != "`ts` datetime(3) NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3)" {
		t.Errorf("timestamp = %q", got)
	}
	u := &schemadiff.Column{Name: "u", Type: "char(36)", Default: strp("uuid()"), DefaultIsExpr: true}
	if got := r.columnDef(u); got != "`u` char(36) NOT NULL DEFAULT (uuid())" {
		t.Errorf("expression default = %q", got)
	}
}

func TestRenderDDLRequalifiesBareSchemaInBodies(t *testing.T) {
	src, dst := ddlModels()
	src.Routines = []*schemadiff.Routine{{Name: "bump", Kind: "PROCEDURE",
		CreateSQL: "CREATE PROCEDURE `bump`() UPDATE src.orders SET id = id + 1; SELECT * FROM `src`.customer, mysrc.customer"}}
	opts := schemadiff.DefaultOptions()
	opts.Objects.Routines = true
	stmts, err := (&mysqlConn{variant: "mysql"}).RenderDDL(schemadiff.Compare(src, dst, opts),
		schemadiff.RenderOpts{Mode: schemadiff.StructureSync, TargetSchema: "dst"})
	if err != nil {
		t.Fatal(err)
	}
	var body string
	for _, s := range stmts {
		if strings.Contains(s.SQL, "PROCEDURE `bump`") && strings.HasPrefix(s.SQL, "CREATE") {
			body = s.SQL
		}
	}
	want := "UPDATE `dst`.orders SET id = id + 1; SELECT * FROM `dst`.customer, mysrc.customer"
	if !strings.Contains(body, want) {
		t.Errorf("routine body = %q, want it to contain %q", body, want)
	}
}

func TestRenderDDLOrdersViewChains(t *testing.T) {
	src, dst := ddlModels()
	src.Views = []*schemadiff.View{
		{Name: "a", CreateSQL: "CREATE VIEW `a` AS select * from `src`.`b`"},
		{Name: "c", CreateSQL: "CREATE VIEW `c` AS select * from `src`.`customer`"},
		{Name: "b", CreateSQL: "CREATE VIEW `b` AS select * from `src`.`c`"},
	}
	opts := schemadiff.DefaultOptions()
	opts.Objects.Views = true
	stmts, err := (&mysqlConn{variant: "mysql"}).RenderDDL(schemadiff.Compare(src, dst, opts),
		schemadiff.RenderOpts{Mode: schemadiff.StructureSync, TargetSchema: "dst"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range stmts {
		if s.Object == schemadiff.ObjectView {
			got = append(got, s.Name)
		}
	}
	if strings.Join(got, ",") != "c,b,a" {
		t.Errorf("view order = %v, want c,b,a", got)
	}
}
