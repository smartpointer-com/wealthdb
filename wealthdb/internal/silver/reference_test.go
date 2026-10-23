package silver

import (
	"slices"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestEveryReferenceTableHasAKind pins that the change window and the
// stream read each reference table over its kind's span: every table
// names one of the kinds, no other table names one, and every table but
// lines spans the change window by its run (ReferenceRunTables). A file
// older than a table's schema version reads none of its rows.
func TestEveryReferenceTableHasAKind(t *testing.T) {
	kinds := map[string]bool{}
	for _, k := range canonical.RefKinds {
		kinds[k] = true
	}
	tables := slices.Concat(ReferenceTables, ReferenceYieldTables, []string{ReferenceCalendarTable})
	for _, table := range tables {
		if kind := ReferenceTableKinds[table]; !kinds[kind] {
			t.Errorf("%s names kind %q, want one of the reference kinds", table, kind)
		}
	}
	if len(ReferenceTableKinds) != len(tables) {
		t.Errorf("ReferenceTableKinds has %d tables, the reference tables are %d", len(ReferenceTableKinds), len(tables))
	}
	run := ReferenceRunTables(ReferenceSchema{Yield: true, Calendar: true})
	for _, table := range tables {
		if slices.Contains(run, table) == (table == "lines") {
			t.Errorf("ReferenceRunTables at v3 = %v, want every reference table but lines", run)
		}
	}
	if v1 := ReferenceRunTables(ReferenceSchema{}); slices.ContainsFunc(v1, func(t string) bool {
		return slices.Contains(ReferenceYieldTables, t) || t == ReferenceCalendarTable
	}) {
		t.Errorf("ReferenceRunTables at v1 = %v, want no table of schema v2 or v3", v1)
	}
	if v2 := ReferenceRunTables(ReferenceSchema{Yield: true}); slices.Contains(v2, ReferenceCalendarTable) ||
		!slices.Contains(v2, "period_facts") {
		t.Errorf("ReferenceRunTables at v2 = %v, want the tables of schema v2 and none of v3", v2)
	}
}
