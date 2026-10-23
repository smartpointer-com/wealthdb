package silver

import (
	"slices"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestEveryReferenceTableHasAKind pins that the change window and the
// stream read each reference table over its kind's span: every table
// names one of the kinds, no other table names one, and every table but
// lines spans the change window by its run (ReferenceRunTables).
func TestEveryReferenceTableHasAKind(t *testing.T) {
	kinds := map[string]bool{}
	for _, k := range canonical.RefKinds {
		kinds[k] = true
	}
	tables := slices.Concat(ReferenceTables, ReferenceYieldTables)
	for _, table := range tables {
		if kind := ReferenceTableKinds[table]; !kinds[kind] {
			t.Errorf("%s names kind %q, want one of the reference kinds", table, kind)
		}
	}
	if len(ReferenceTableKinds) != len(tables) {
		t.Errorf("ReferenceTableKinds has %d tables, the reference tables are %d", len(ReferenceTableKinds), len(tables))
	}
	run := ReferenceRunTables(true)
	for _, table := range tables {
		if slices.Contains(run, table) == (table == "lines") {
			t.Errorf("ReferenceRunTables(true) = %v, want every reference table but lines", run)
		}
	}
	if v1 := ReferenceRunTables(false); slices.ContainsFunc(v1, func(t string) bool { return slices.Contains(ReferenceYieldTables, t) }) {
		t.Errorf("ReferenceRunTables(false) = %v, want no table of schema v2", v1)
	}
}
