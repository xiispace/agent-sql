package copier

import (
	"testing"

	"github.com/shhac/agent-sql/internal/schemadiff"
)

func TestCompareValue(t *testing.T) {
	cases := []struct {
		kind schemadiff.KeyKind
		a, b string
		want int
	}{
		{schemadiff.KeyInteger, "9", "10", -1},
		{schemadiff.KeyInteger, "-10", "-9", -1},
		{schemadiff.KeyInteger, "-1", "0", -1},
		{schemadiff.KeyInteger, "18446744073709551615", "18446744073709551614", 1},
		{schemadiff.KeyInteger, "42", "42", 0},
		{schemadiff.KeyDecimal, "2.50", "10.00", -1},
		{schemadiff.KeyDecimal, "-0.10", "0.00", -1},
		{schemadiff.KeyFloat, "1e3", "999.5", 1},
		// PostgreSQL sorts NaN after every number, Infinity included.
		{schemadiff.KeyFloat, "NaN", "1", 1},
		{schemadiff.KeyFloat, "Infinity", "NaN", -1},
		{schemadiff.KeyFloat, "NaN", "NaN", 0},
		{schemadiff.KeyFloat, "-Infinity", "-1e308", -1},
		{schemadiff.KeyBytes, "B", "a", -1},
		{schemadiff.KeyBytes, "2026-01-02 00:00:00", "2026-01-10 00:00:00", -1},
	}
	for _, c := range cases {
		if got := compareValue(c.kind, []byte(c.a), []byte(c.b)); got != c.want {
			t.Errorf("compareValue(%v, %q, %q) = %d, want %d", c.kind, c.a, c.b, got, c.want)
		}
	}
}

func TestKeyCmpMultiColumn(t *testing.T) {
	k := keyCmp{idx: []int{1, 0}, kinds: []schemadiff.KeyKind{schemadiff.KeyBytes, schemadiff.KeyInteger}}
	a := [][]byte{[]byte("2"), []byte("x")}
	b := [][]byte{[]byte("10"), []byte("x")}
	if k.compare(a, b) != -1 {
		t.Error("second key column should decide numerically")
	}
}

func TestSameRow(t *testing.T) {
	if sameRow([][]byte{nil}, [][]byte{{}}) {
		t.Error("NULL and empty string must differ")
	}
	if !sameRow([][]byte{[]byte("a"), nil}, [][]byte{[]byte("a"), nil}) {
		t.Error("equal rows reported different")
	}
}

func TestValidateModes(t *testing.T) {
	for _, c := range []struct{ s, d string }{{"", "none"}, {"sync", ""}, {"bogus", "none"}, {"sync", "bogus"}, {"none", "none"}} {
		if ValidateModes(c.s, c.d) == nil {
			t.Errorf("ValidateModes(%q, %q) accepted", c.s, c.d)
		}
	}
	if err := ValidateModes("sync", "upsert"); err != nil {
		t.Errorf("valid modes rejected: %v", err)
	}
}

func TestStructureLabel(t *testing.T) {
	create := schemadiff.Statement{SQL: "CREATE TABLE `d`.`t` (...)", Object: schemadiff.ObjectTable, Name: "t"}
	dropExisting := schemadiff.Statement{SQL: "DROP TABLE IF EXISTS `d`.`t`", Object: schemadiff.ObjectTable, Name: "t", Destructive: true}
	dropAbsent := schemadiff.Statement{SQL: "DROP TABLE IF EXISTS `d`.`t`", Object: schemadiff.ObjectTable, Name: "t"}
	if got := structureLabel("recreate", create, structureLabel("recreate", dropExisting, "unchanged")); got != "recreated" {
		t.Errorf("recreate existing = %q", got)
	}
	if got := structureLabel("recreate", create, structureLabel("recreate", dropAbsent, "unchanged")); got != "created" {
		t.Errorf("recreate absent = %q", got)
	}
	alter := schemadiff.Statement{SQL: "ALTER TABLE `d`.`t` ADD COLUMN x int", Object: schemadiff.ObjectTable, Name: "t"}
	if got := structureLabel("sync", alter, "unchanged"); got != "altered" {
		t.Errorf("alter = %q", got)
	}
}
