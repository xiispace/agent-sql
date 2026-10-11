package schemadiff

import "testing"

func TestTypeChangeLoss(t *testing.T) {
	cases := []struct {
		from, to string
		lossy    bool
	}{
		{"varchar(64)", "varchar(32)", true},
		{"varchar(32)", "varchar(64)", false},
		{"char(10)", "char(4)", true},
		{"varbinary(16)", "varbinary(8)", true},
		{"int", "bigint", false},
		{"bigint", "int", true},
		{"int(11)", "int", false},
		{"tinyint(1)", "tinyint(4)", false},
		{"decimal(12,2)", "decimal(14,2)", false},
		{"decimal(12,2)", "decimal(12,1)", true},
		{"decimal(12,2)", "decimal(11,2)", true},
		{"text", "varchar(255)", true},
		{"varchar(255)", "text", false},
		{"text", "longtext", false},
		{"longtext", "text", true},
		{"blob", "tinyblob", true},
		{"enum('a','b','c')", "enum('a','b')", true},
		{"enum('a','b')", "enum('a','b','c')", false},
		{"set('x','y')", "set('x')", true},
		{"int unsigned", "int", true},
		{"int", "int unsigned", true},
		{"float", "double", false},
		{"double", "float", true},
		{"datetime(3)", "datetime", true},
		{"datetime", "datetime(3)", false},
		{"char(4)", "varchar(8)", false},
		{"varchar(10)", "int", true},
		{"VARCHAR(10)", "varchar(10)", false},
	}
	for _, c := range cases {
		lossy, why := typeChangeLoss(c.from, c.to)
		if lossy != c.lossy {
			t.Errorf("typeChangeLoss(%q, %q) = %v (%s), want %v", c.from, c.to, lossy, why, c.lossy)
		}
		if lossy && why == "" {
			t.Errorf("typeChangeLoss(%q, %q): lossy without a reason", c.from, c.to)
		}
	}
}

func TestParseTypeEnumWithParens(t *testing.T) {
	p := parseType("enum('a(1)','it''s')")
	if p.base != "enum" || len(p.members) != 2 || p.members[0] != "a(1)" || p.members[1] != "it's" {
		t.Fatalf("parseType = %+v", p)
	}
}

func TestKeyKindOf(t *testing.T) {
	cases := map[string]KeyKind{
		"int unsigned": KeyInteger, "bigint": KeyInteger, "year": KeyInteger,
		"decimal(10,2)": KeyDecimal, "double": KeyFloat,
		"varchar(10)": KeyBytes, "datetime(3)": KeyBytes, "binary(16)": KeyBytes,
	}
	for typ, want := range cases {
		if got := KeyKindOf(&Column{Type: typ}); got != want {
			t.Errorf("KeyKindOf(%q) = %v, want %v", typ, got, want)
		}
	}
}

func TestColumnIsBinary(t *testing.T) {
	if !(&Column{Type: "varbinary(16)"}).IsBinary() || !(&Column{Type: "longblob"}).IsBinary() {
		t.Error("binary types should be binary")
	}
	if !(&Column{Type: "char(3)", Charset: "binary"}).IsBinary() {
		t.Error("binary charset should be binary")
	}
	if (&Column{Type: "varchar(3)", Charset: "utf8mb4"}).IsBinary() || (&Column{Type: "json"}).IsBinary() {
		t.Error("text types should not be binary")
	}
}

func TestTypeChangeLossPostgres(t *testing.T) {
	cases := []struct {
		from, to string
		lossy    bool
	}{
		{"character varying(64)", "character varying(32)", true},
		{"character varying(32)", "text", false},
		{"text", "character varying(32)", true},
		{"integer", "bigint", false},
		{"bigint", "integer", true},
		{"numeric(12,2)", "numeric(14,2)", false},
		{"double precision", "real", true},
		{"timestamp without time zone", "timestamp(3) without time zone", true},
		{"timestamp(3) without time zone", "timestamp without time zone", false},
		{"timestamp with time zone", "timestamp(6) with time zone", false},
	}
	for _, c := range cases {
		if lossy, why := typeChangeLoss(c.from, c.to); lossy != c.lossy {
			t.Errorf("typeChangeLoss(%q, %q) = %v (%s), want %v", c.from, c.to, lossy, why, c.lossy)
		}
	}
}

func TestRequalifySchema(t *testing.T) {
	q := func(s string) string { return `"` + s + `"` }
	in := `SELECT x FROM src.t JOIN "src".u ON nextval('src.seq'::regclass) = 1 WHERE mysrc.v = 1`
	want := `SELECT x FROM "dst".t JOIN "dst".u ON nextval('"dst".seq'::regclass) = 1 WHERE mysrc.v = 1`
	if got := RequalifySchema(in, "src", "dst", q); got != want {
		t.Errorf("RequalifySchema =\n %s\nwant\n %s", got, want)
	}
}
