package gold

import (
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// A CROSSING — any delta spelled `*_transfer` — is the household's own
// money arriving somewhere else: between two of its accounts, or into a
// pool earmarked for a stage of life. It is in the taxonomy so a tier
// can PLACE it, and out of BOTH family bases so no report COUNTS it.
// The two go together.
//
// Not every delta leaves: `card_spend` and `cash_withdrawal` are
// spending, `inheritance` and `cash_deposit` are income. The crossings
// are the set that must leave, and the set this pins.
//
// They came apart once and nothing noticed. `deposit_transfer` was
// added to the taxonomy and to neither base, and the docs were written
// as though it had been added to both — so money moved into a bank
// deposit read as spending, the principal coming back read as income,
// and the only thing that would have caught it was a reader who knew
// the year's figures by heart.
//
// The lists are literal, so this is what a generator pin can reach: it
// asks the deployed macro whether it names each delta, which is the
// question the omission answered wrongly.
func TestEveryDeltaLeavesTheFamilyBases(t *testing.T) {
	db, ctx := openMigrated(t)
	body := func(macro string) string {
		var def string
		if err := db.QueryRowContext(ctx,
			`SELECT macro_definition FROM duckdb_functions()
              WHERE function_name = ? LIMIT 1`, macro).Scan(&def); err != nil {
			t.Fatalf("read %s: %v", macro, err)
		}
		return def
	}
	spending, income := body("spending_lines_base"), body("income_lines_base")

	crossings := 0
	for _, c := range canonical.SpendCategories {
		if !strings.HasSuffix(c.Detailed, "_transfer") {
			continue
		}
		crossings++
		if !strings.Contains(spending, "'"+c.Detailed+"'") {
			t.Errorf("spending_lines_base does not exclude the delta %q: "+
				"a movement of the household's own money would be counted as spending",
				c.Detailed)
		}
		if !strings.Contains(income, "'"+c.Detailed+"'") {
			t.Errorf("income_lines_base does not exclude the delta %q: "+
				"a movement of the household's own money would be counted as income",
				c.Detailed)
		}
	}
	// And the pin reads something: no crossings would pass every
	// assertion above without checking anything.
	if crossings < 5 {
		t.Fatalf("found %d crossings in the vocabulary; this guard reads too little", crossings)
	}
}
