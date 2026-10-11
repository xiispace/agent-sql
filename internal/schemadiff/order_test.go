package schemadiff

import (
	"reflect"
	"strings"
	"testing"
)

func fkTo(ref string) *ForeignKey { return &ForeignKey{Name: "fk_" + ref, RefTable: ref} }

func TestTableOrder(t *testing.T) {
	m := &Model{Schema: "s", Tables: []*Table{
		{Name: "orders", ForeignKeys: []*ForeignKey{fkTo("customer"), fkTo("product")}},
		{Name: "customer"},
		{Name: "product", ForeignKeys: []*ForeignKey{fkTo("vendor")}},
		{Name: "vendor"},
		{Name: "elsewhere", ForeignKeys: []*ForeignKey{{RefSchema: "other", RefTable: "vendor"}}},
	}}
	got := TableOrder(m, []string{"orders", "customer", "product", "vendor", "elsewhere"})
	want := []string{"customer", "elsewhere", "vendor", "product", "orders"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("TableOrder = %v, want %v", got, want)
	}
}

func TestTableOrderCycleAndSubset(t *testing.T) {
	m := &Model{Tables: []*Table{
		{Name: "a", ForeignKeys: []*ForeignKey{fkTo("b")}},
		{Name: "b", ForeignKeys: []*ForeignKey{fkTo("a")}},
		{Name: "c", ForeignKeys: []*ForeignKey{fkTo("a"), fkTo("c")}},
	}}
	got := TableOrder(m, []string{"c", "b", "a"})
	if len(got) != 3 {
		t.Fatalf("TableOrder = %v", got)
	}
	// Only the requested subset is ordered; references outside it are ignored.
	if got := TableOrder(m, []string{"c"}); !reflect.DeepEqual(got, []string{"c"}) {
		t.Errorf("subset = %v", got)
	}
}

func TestDependencyOrderChain(t *testing.T) {
	// A reads B, B reads C: pairwise sorting left [A C B] unchanged.
	names := []string{"A", "C", "B"}
	deps := map[string]string{"A": "B", "B": "C"}
	order := DependencyOrder(len(names), func(i, j int) bool { return deps[names[i]] == names[j] })
	var got []string
	for _, i := range order {
		got = append(got, names[i])
	}
	if strings.Join(got, ",") != "C,B,A" {
		t.Errorf("order = %v, want C,B,A", got)
	}
	// A cycle still emits everything.
	cyc := DependencyOrder(2, func(i, j int) bool { return true })
	if len(cyc) != 2 {
		t.Errorf("cycle order = %v", cyc)
	}
}
