package schemadiff

import "sort"

// TableOrder returns the named tables of m ordered so that every table comes
// after the tables its foreign keys reference (within the set). Cycles are
// broken by name order; with foreign key checks off the server accepts them.
func TableOrder(m *Model, names []string) []string {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)

	visited := map[string]int{} // 0 unvisited, 1 in progress, 2 done
	var out []string
	var visit func(n string)
	visit = func(n string) {
		if visited[n] != 0 {
			return
		}
		visited[n] = 1
		if t := m.Table(n); t != nil {
			refs := make([]string, 0, len(t.ForeignKeys))
			for _, fk := range t.ForeignKeys {
				if (fk.RefSchema == "" || fk.RefSchema == m.Schema) && want[fk.RefTable] && fk.RefTable != n {
					refs = append(refs, fk.RefTable)
				}
			}
			sort.Strings(refs)
			for _, r := range refs {
				visit(r)
			}
		}
		visited[n] = 2
		out = append(out, n)
	}
	for _, n := range sorted {
		visit(n)
	}
	return out
}

// TablesInOrder returns every table of m in TableOrder.
func (m *Model) TablesInOrder() []*Table {
	order := TableOrder(m, m.TableNames())
	out := make([]*Table, len(order))
	for i, n := range order {
		out[i] = m.Table(n)
	}
	return out
}

// DependencyOrder returns the indexes 0..n-1 ordered so that each comes
// after every other index it depends on, keeping input order otherwise.
// Members of a cycle follow in input order once nothing else is ready.
func DependencyOrder(n int, dependsOn func(i, j int) bool) []int {
	out := make([]int, 0, n)
	done := make([]bool, n)
	for len(out) < n {
		progressed := false
		for i := 0; i < n; i++ {
			if done[i] {
				continue
			}
			ready := true
			for j := 0; j < n && ready; j++ {
				if j != i && !done[j] && dependsOn(i, j) {
					ready = false
				}
			}
			if ready {
				out, done[i], progressed = append(out, i), true, true
			}
		}
		if !progressed { // cycle: emit the rest as they are
			for i := 0; i < n; i++ {
				if !done[i] {
					out, done[i] = append(out, i), true
				}
			}
		}
	}
	return out
}

// Statement is one rendered DDL statement.
type Statement struct {
	SQL         string `json:"sql"`
	Destructive bool   `json:"destructive"`
	Object      string `json:"object"`
	Table       string `json:"table,omitempty"`
	Name        string `json:"name"`
}

// Structure modes of `copy --structure`.
const (
	StructureSync     = "sync"
	StructureRecreate = "recreate"
	StructureNone     = "none"
)

// RenderOpts controls DDL rendering.
type RenderOpts struct {
	// Mode is StructureSync (apply the diff) or StructureRecreate (drop and
	// create every source table).
	Mode string
	// TargetSchema qualifies every emitted statement.
	TargetSchema string
}
