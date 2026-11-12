package gold

import (
	"slices"
	"testing"
)

// TestSourceTablesNameEachTableOnce: a table listed twice is deleted
// twice by a reset and twice by a windowed delete.
func TestSourceTablesNameEachTableOnce(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, st := range SourceTables {
		if seen[st.Name] {
			t.Errorf("%s is listed twice", st.Name)
		}
		seen[st.Name] = true
		if st.RefKind != "" && st.TimeCol == "" {
			t.Errorf("%s has a record kind and no time column to window it by", st.Name)
		}
	}
}

// TestLotLedgerTablesAreTheRegistrysLedger: the lot pass clears
// LotLedgerTables and a reset clears the registry's ledger class, so
// the two lists are one.
func TestLotLedgerTablesAreTheRegistrysLedger(t *testing.T) {
	t.Parallel()
	if got := sourceTableNames(ClassLotLedger); !slices.Equal(got, LotLedgerTables) {
		t.Errorf("ledger class = %v, LotLedgerTables = %v", got, LotLedgerTables)
	}
}

// TestLoadKeyStampsWhatNoWatermarkCovers: a source's facts, references
// and dimensions move with its watermark in silver_sources. A model's
// store, an overlay a pass rewrites, and the lot ledger, whose run log
// is its stamp, do not, so the load key stamps each of them.
func TestLoadKeyStampsWhatNoWatermarkCovers(t *testing.T) {
	t.Parallel()
	want := []string{"silver_sources", "lot_runs"}
	want = append(want, sourceTableNames(ClassStore)...)
	want = append(want, sourceTableNames(ClassOverlay)...)
	for _, table := range want {
		if !slices.Contains(loadKeyTables, table) {
			t.Errorf("%s is not in loadKeyTables", table)
		}
	}
}
