package copy

import "testing"

func TestParseWhere(t *testing.T) {
	got, err := parseWhere([]string{"orders=created_at >= '2026-01-01' AND a=b", " users = id < 10 "})
	if err != nil {
		t.Fatal(err)
	}
	if got["orders"] != "created_at >= '2026-01-01' AND a=b" || got["users"] != "id < 10" {
		t.Errorf("parseWhere = %#v", got)
	}
	for _, bad := range [][]string{{"no-equals"}, {"=x"}, {"t="}, {"t=a", "t=b"}} {
		if _, err := parseWhere(bad); err == nil {
			t.Errorf("parseWhere(%q) accepted", bad)
		}
	}
}
