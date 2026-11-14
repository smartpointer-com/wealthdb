package spending

// amexCardCategories translates the American Express card vocabulary —
// the categories Amex files a modern-era card row under when it files one
// at all, which the collector resolves from the code → label map the
// activity payload ships beside the rows, so gold receives the label
// rather than a code. The deep era, read from statement PDFs, carries no
// category and falls to the model tier.
//
// Same rule as chase's table: where the issuer's bucket is coarser
// than the taxonomy's, the translation targets that primary's OTHER_*
// value rather than its most common member, and the model tier can
// refine a merchant later from its name. Amex's buckets are coarse
// almost throughout — nine categories for everything a card can buy —
// so most entries land on an OTHER_*.
//
// "Communications" is the one that reads oddly: Amex files phone,
// internet and cable under it, which the taxonomy splits across
// RENT_AND_UTILITIES. OTHER_UTILITIES is the honest parent of that
// split. TELEPHONE names telephone bills alone, which is only the phone
// part of a bucket that also holds internet and cable.
var amexCardCategories = map[string]string{
	"Business Services":      "GENERAL_SERVICES_OTHER_GENERAL_SERVICES",
	"Communications":         "RENT_AND_UTILITIES_OTHER_UTILITIES",
	"Entertainment":          "ENTERTAINMENT_OTHER_ENTERTAINMENT",
	"Fees & Adjustments":     "BANK_FEES_OTHER_BANK_FEES",
	"Merchandise & Supplies": "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE",
	"Restaurants":            "FOOD_AND_DRINK_RESTAURANT",
	"Transportation":         "TRANSPORTATION_OTHER_TRANSPORTATION",
	"Travel":                 "TRAVEL_OTHER_TRAVEL",
}

// amexUncategorized is Amex's own residual bucket. It is reviewed and
// deliberately left untranslated: it says the ISSUER could not place
// the row, which is the case the model tier exists for.
var amexUncategorized = map[string]bool{"other": true}
