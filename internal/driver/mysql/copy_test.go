package mysql

import (
	"reflect"
	"testing"

	"github.com/shhac/agent-sql/internal/driver"
	"github.com/shhac/agent-sql/internal/schemadiff"
)

func TestWriterSQLMode(t *testing.T) {
	cases := []struct{ target, source, want string }{
		{"ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,NO_BACKSLASH_ESCAPES,ERROR_FOR_DIVISION_BY_ZERO", "",
			"ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,ERROR_FOR_DIVISION_BY_ZERO,NO_AUTO_VALUE_ON_ZERO"},
		{"", "", "NO_AUTO_VALUE_ON_ZERO"},
		// A MariaDB source's NO_AUTO_CREATE_USER never reaches a MySQL 8
		// target; its parse modes do, and the target's own do not.
		{"STRICT_TRANS_TABLES,PIPES_AS_CONCAT", "STRICT_TRANS_TABLES,NO_AUTO_CREATE_USER,ansi_quotes",
			"STRICT_TRANS_TABLES,ANSI_QUOTES,NO_AUTO_VALUE_ON_ZERO"},
	}
	for _, c := range cases {
		if got := writerSQLMode(c.target, c.source); got != c.want {
			t.Errorf("writerSQLMode(%q, %q) = %q, want %q", c.target, c.source, got, c.want)
		}
	}
}

func TestSelectSQL(t *testing.T) {
	got := selectSQL("s", driver.ReadRequest{
		Table: "t", Columns: []string{"id", "na`me"}, Where: "id > 5 OR id < 2",
		OrderBy: []driver.KeyColumn{{Name: "id", Kind: schemadiff.KeyInteger}, {Name: "na`me", Kind: schemadiff.KeyBytes}},
	})
	want := "SELECT `id`, `na``me` FROM `s`.`t` WHERE (id > 5 OR id < 2) ORDER BY `id`, CAST(`na``me` AS BINARY)"
	if got != want {
		t.Errorf("selectSQL =\n %q\nwant\n %q", got, want)
	}
}

func TestRowArgs(t *testing.T) {
	cols := []*schemadiff.Column{{Name: "a", Type: "varchar(3)"}, {Name: "b", Type: "varbinary(3)"}}
	args, values := rowArgs(cols, [][][]byte{{[]byte("x"), []byte{0}}, {nil, []byte{}}})
	if values != "(?, ?), (?, ?)" {
		t.Errorf("values = %q", values)
	}
	want := []any{"x", []byte{0}, nil, []byte{}}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %#v", args)
	}
}
