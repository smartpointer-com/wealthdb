package spending

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"

// raiffeisenIncomeCategories is the inflow half of a CATEGORICAL
// vocabulary, so a value outside it counts as drift. The vocabulary's
// own inbound bucket is a catch-all and is translated as one: recorded,
// and left for the tier that can read a payer.
var raiffeisenIncomeCategories = map[string]string{
	"income_other": "INCOME_OTHER",
}

// raiffeisenIncomeUncategorized is the income side's reviewed-and-left
// set. It is `raiffeisenUncategorized` LESS `income_other`, plus every
// value the categorical vocabulary spends on the outflow side.
//
// Two things it has to be, and they pull in opposite directions.
//
// `income_other` is left OUT, which is why this is not simply the other
// map. On the OUTFLOW side that token names a direction rather than a
// spend category and belongs to no spend value at all, so it is skipped
// before the lookup runs; read from the inflow side it is the
// vocabulary's own catch-all, which is a translation this family has a
// word for. It is translated in raiffeisenIncomeCategories above and
// declined by ProviderIncomeCategoryClaims — sharing one map between
// the two sides would make that translation unreachable.
//
// Everything else the vocabulary publishes is IN, and that is the
// second requirement. The vocabulary is categorical, so a value outside
// the reviewed set counts as drift — "the issuer's vocabulary has
// moved", a signal the run report prints for someone to act on. But
// this map translates ONE value by design, and the bank can file an
// admitted inflow under its ordinary spend tokens: a `deposit` filed
// `real_estate_other`, or a refund or a reversal under `supermarket`
// or `bank_fee`. Reviewed on the spending
// side and unlisted here, each would report as drift on every load —
// a permanent false alarm that makes the real signal unreadable.
//
// So the two sets are derived from one another rather than written
// twice: a token added to the spending review is reviewed here too, and
// only a token NEITHER side has seen is drift.
var raiffeisenIncomeUncategorized = raiffeisenIncomeReviewed()

func raiffeisenIncomeReviewed() map[string]bool {
	out := map[string]bool{}
	for k := range raiffeisenUncategorized {
		if k == "income_other" {
			continue // translated on this side; see above
		}
		out[k] = true
	}
	for k := range raiffeisenCategories {
		out[k] = true
	}
	return out
}

// raiffeisenCategories translates the Mein ELBA vocabulary, which the
// bank publishes as lower-case tokens rather than MCC prose.
//
// The bank's "*_other" tokens are not all coarse in the same way, and
// the token's spelling does not decide whether the row is claimed —
// the TRANSLATION does. `electronics_shop_other` and `insurance_other`
// name a real line of business and resolve to one, so they claim;
// `shopping_other` and `utility` resolve to their primary's catch-all
// and so decline, leaving the merchant name to the model. What the
// vocabulary declines outright is in raiffeisenUncategorized below.
var raiffeisenCategories = map[string]string{
	"supermarket":                   "FOOD_AND_DRINK_GROCERIES",
	"tv_phone_internet":             "RENT_AND_UTILITIES_INTERNET_AND_CABLE",
	"utility":                       "RENT_AND_UTILITIES_OTHER_UTILITIES",
	"bank_fee":                      "BANK_FEES_OTHER_BANK_FEES",
	"insurance_other":               "GENERAL_SERVICES_INSURANCE",
	"government_service":            "GOVERNMENT_AND_NON_PROFIT_GOVERNMENT_DEPARTMENTS_AND_AGENCIES",
	"airline":                       "TRAVEL_FLIGHTS",
	"electronics_shop_other":        "GENERAL_MERCHANDISE_ELECTRONICS",
	"tobacco_smoking_related_store": "GENERAL_MERCHANDISE_TOBACCO_AND_VAPE",
	"shopping_other":                "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE",
	// A movement, not a line of business: the bank names the rail.
	"atm_withdrawal": canonical.SpendDetailedCashWithdrawal,
}

// raiffeisenUncategorized are the values Mein ELBA publishes when it
// has placed nothing: its own "not categorised" token, its bucket for
// a payment it could not read, and its income token — which names a
// direction rather than a category and belongs to no spend value at
// all. `real_estate_other` joins them because it can cover both a
// rent payment and a property purchase, and the two are a utility bill
// and a capital movement.
var raiffeisenUncategorized = map[string]bool{
	"not_categorized":   true,
	"payment_other":     true,
	"income_other":      true,
	"real_estate_other": true,
}
