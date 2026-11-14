package spending

// chaseCardCategories translates the Chase card vocabulary.
//
// Where the provider's bucket is coarser than the taxonomy's, the
// translation targets that primary's OTHER_* value rather than its
// most common member: "Shopping" is not evidence of clothing any more
// than of electronics, and the model tier can refine a merchant later
// from its name. Where the provider's bucket is precise — "Groceries",
// "Education" — the specific value is used.
var chaseCardCategories = map[string]string{
	"Shopping":              "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE",
	"Food & Drink":          "FOOD_AND_DRINK_RESTAURANT",
	"Entertainment":         "ENTERTAINMENT_OTHER_ENTERTAINMENT",
	"Travel":                "TRAVEL_OTHER_TRAVEL",
	"Bills & Utilities":     "RENT_AND_UTILITIES_OTHER_UTILITIES",
	"Groceries":             "FOOD_AND_DRINK_GROCERIES",
	"Education":             "GENERAL_SERVICES_EDUCATION",
	"Professional Services": "GENERAL_SERVICES_OTHER_GENERAL_SERVICES",
	"Personal":              "PERSONAL_CARE_OTHER_PERSONAL_CARE",
	"Health & Wellness":     "MEDICAL_OTHER_MEDICAL",
	"Fees & Adjustments":    "BANK_FEES_OTHER_BANK_FEES",
	"Home":                  "HOME_IMPROVEMENT_OTHER_HOME_IMPROVEMENT",
	"Gifts & Donations":     "GOVERNMENT_AND_NON_PROFIT_DONATIONS",
}
