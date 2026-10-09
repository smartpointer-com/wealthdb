package silver

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

func TestMarkPrimaryPicksTheBestKindPerAccountYear(t *testing.T) {
	rank := func(k canonical.RealizedDocKind) int {
		switch k {
		case canonical.RealizedForm1099B:
			return 0
		case canonical.RealizedYearEndSummary:
			return 1
		case canonical.RealizedStatement:
			return 2
		}
		return -1
	}
	lot := func(id, acct string, year int, k canonical.RealizedDocKind) canonical.RealizedLotChange {
		return canonical.RealizedLotChange{RealizedLotExternalID: id, AccountExternalID: acct, TaxYear: year, DocumentKind: k}
	}
	lots := []canonical.RealizedLotChange{
		lot("a-1099", "A", 2024, canonical.RealizedForm1099B),
		lot("a-yes", "A", 2024, canonical.RealizedYearEndSummary),
		lot("a-stmt-2025", "A", 2025, canonical.RealizedStatement),
		lot("b-yes", "B", 2024, canonical.RealizedYearEndSummary),
		lot("b-glr", "B", 2024, canonical.RealizedGainLossReport), // rank -1: never
		lot("b-1099-old", "B", 2024, canonical.RealizedForm1099B), // ineligible copy
	}
	MarkPrimary(lots, rank, func(r *canonical.RealizedLotChange) bool {
		return r.RealizedLotExternalID != "b-1099-old"
	})
	want := map[string]bool{
		"a-1099": true, "a-yes": false, "a-stmt-2025": true,
		"b-yes": true, "b-glr": false, "b-1099-old": false,
	}
	for _, r := range lots {
		if r.IsPrimary != want[r.RealizedLotExternalID] {
			t.Errorf("%s primary = %v, want %v", r.RealizedLotExternalID, r.IsPrimary, want[r.RealizedLotExternalID])
		}
	}
}
