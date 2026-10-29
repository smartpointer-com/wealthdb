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
// A crossing missing from one base is silent: money moved into a
// deposit reads as spending, its return as income, and no total looks
// wrong.
//
// The lists are literal, so this is what a generator pin can reach: it
// asks the deployed macro whether it names each delta. Migration 0160
// replays to the same bodies.
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
	check := func(when string) {
		t.Helper()
		// spending_lines_base is spending_lines_own and the linked
		// repayments (0160); the exclusions live in the first, and a
		// repayment's category is held to them in spend_reimbursements.
		spending, income := body("spending_lines_own"), body("income_lines_base")
		repaid := body("spend_reimbursements")

		crossings := 0
		for _, c := range canonical.SpendCategories {
			if !strings.HasSuffix(c.Detailed, "_transfer") {
				continue
			}
			crossings++
			if !strings.Contains(spending, "'"+c.Detailed+"'") {
				t.Errorf("%s: spending_lines_own does not exclude the delta %q: "+
					"a movement of the household's own money would be counted as spending",
					when, c.Detailed)
			}
			if !strings.Contains(repaid, "'"+c.Detailed+"'") {
				t.Errorf("%s: spend_reimbursements does not refuse the delta %q: "+
					"a repayment would net against an own-account move", when, c.Detailed)
			}
			if !strings.Contains(income, "'"+c.Detailed+"'") {
				t.Errorf("%s: income_lines_base does not exclude the delta %q: "+
					"a movement of the household's own money would be counted as income",
					when, c.Detailed)
			}
		}
		// And the pin reads something: no crossings would pass every
		// assertion above without checking anything.
		if crossings < 5 {
			t.Fatalf("found %d crossings in the vocabulary; this guard reads too little", crossings)
		}
	}
	check("at head")
	rerunMigrationDDL(t, db, ctx, "0160_reimbursement_netting.sql")
	check("after replaying 0160")
}
