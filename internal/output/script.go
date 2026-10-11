package output

import (
	"fmt"
	"os"
	"strings"

	"github.com/shhac/agent-sql/internal/schemadiff"
)

// PrintListWithSummary writes records then a summary: NDJSON records
// followed by one summary line, or a {"data": [...], "summary": {...}}
// envelope. Records are not nil-pruned: a change "from": null (no default,
// say) is meaningful. Callers that stream their NDJSON records themselves
// pass nil items.
func PrintListWithSummary(flagFormat string, items []any, summary any) {
	if ResolveFormat(flagFormat) == FormatNDJSON {
		PrintList(flagFormat, items, nil, false)
		PrintResult(flagFormat, summary, false)
		return
	}
	PrintList(flagFormat, items, map[string]any{"summary": summary}, false)
}

// WriteSQLScript prints statements as a script (`--format sql`).
// Destructive statements are commented out unless allowDestructive, so the
// script is safe to read and to copy.
func WriteSQLScript(stmts []schemadiff.Statement, allowDestructive bool) {
	w := os.Stdout
	if len(stmts) == 0 {
		_, _ = fmt.Fprintln(w, "-- no differences")
		return
	}
	for _, s := range stmts {
		if s.Destructive && !allowDestructive {
			_, _ = fmt.Fprintln(w, "-- destructive: pass --allow-destructive to include")
			for _, line := range strings.Split(s.SQL+";", "\n") {
				_, _ = fmt.Fprintln(w, "-- "+line)
			}
			continue
		}
		_, _ = fmt.Fprintln(w, s.SQL+";")
	}
}
