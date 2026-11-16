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
// The bases leave the values out through the role predicates
// (detailed_stays_in_spending, detailed_stays_in_income), so this asks the
// deployed predicates about each crossing and the deployed bases whether
// they call them.
func TestEveryDeltaLeavesTheFamilyBases(t *testing.T) {
	t.Parallel()
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
	// spending_lines_base is spending_lines_own and the linked
	// repayments; the exclusions live in the first, and a repayment's
	// category is held to them in spend_reimbursements.
	for macro, predicate := range map[string]string{
		"spending_lines_own":   "detailed_stays_in_spending(",
		"spend_reimbursements": "detailed_stays_in_spending(",
		"income_lines_base":    "detailed_stays_in_income(",
	} {
		if !strings.Contains(body(macro), predicate) {
			t.Errorf("%s does not read %s): the crossings would be counted", macro, predicate)
		}
	}
	crossings := 0
	for _, c := range canonical.SpendCategories {
		if !strings.HasSuffix(c.Detailed, "_transfer") {
			continue
		}
		crossings++
		var spending, income bool
		if err := db.QueryRowContext(ctx, `SELECT NOT detailed_stays_in_spending(?1), NOT detailed_stays_in_income(?1)`,
			c.Detailed).Scan(&spending, &income); err != nil {
			t.Fatal(err)
		}
		if !spending {
			t.Errorf("the spending base keeps the delta %q: a movement of the household's own money "+
				"would be counted as spending, and a repayment would net against it", c.Detailed)
		}
		if !income {
			t.Errorf("the income base keeps the delta %q: a movement of the household's own money "+
				"would be counted as income", c.Detailed)
		}
	}
	// And the pin reads something: no crossings would pass every
	// assertion above without checking anything.
	if crossings < 5 {
		t.Fatalf("found %d crossings in the vocabulary; this guard reads too little", crossings)
	}
}
