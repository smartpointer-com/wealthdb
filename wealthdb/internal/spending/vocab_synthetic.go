package spending

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// syntheticCategories and syntheticIncomeCategories translate the synthetic
// kind's vocabulary, which is the taxonomy itself: its provider_category is a
// spend_detailed value on a row the spending tier reads (a refund included,
// though it is an inflow) and an income_detailed value on a row the income
// tier reads, stamped outright. Each map is the identity over its family,
// built from canonical.SpendCategories rather than written out, so the
// vocabulary is exactly the table the migrations seed and cannot drift from
// it.
//
// The vocabulary is NOT categorical, for two reasons. A value here is the
// provider's verdict, not an issuer's coarse bucket: a catch-all it states —
// BANK_FEES_OTHER_BANK_FEES — is the answer, as a booking type's catch-all
// is, and claims the row rather than deferring it to a model tier that could
// know no more (ProviderCategoryClaims). And a value outside the taxonomy is
// not an issuer's vocabulary moving, so it is not drift worth counting; it
// falls through untranslated, as a rail does.
var (
	syntheticCategories       = taxonomyIdentity(canonical.FamilySpending)
	syntheticIncomeCategories = taxonomyIdentity(canonical.FamilyIncome)
)

// taxonomyIdentity maps every detailed value of one family to itself.
func taxonomyIdentity(family canonical.Family) map[string]string {
	out := map[string]string{}
	for _, c := range canonical.SpendCategories {
		if c.Family.InFamily(family) {
			out[c.Detailed] = c.Detailed
		}
	}
	return out
}
