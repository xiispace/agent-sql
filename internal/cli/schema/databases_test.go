package schema

import (
	"testing"

	"github.com/shhac/agent-sql/internal/driver"
)

func TestFilterDatabases(t *testing.T) {
	all := []driver.DatabaseInfo{{Name: "newrails_risk"}, {Name: "NewRails_Pay"}, {Name: "meowth"}}
	got := filterDatabases(all, "NEWRAILS_*")
	if len(got) != 2 || got[0].Name != "newrails_risk" || got[1].Name != "NewRails_Pay" {
		t.Errorf("filterDatabases = %+v", got)
	}
	if len(filterDatabases(all, "")) != 3 {
		t.Error("empty pattern should keep everything")
	}
}
