package wizard

import "strings"

// regionCurrency is the currency of a region, for the common regions.
var regionCurrency = map[string]string{
	"AT": "EUR", "BE": "EUR", "CY": "EUR", "DE": "EUR", "EE": "EUR", "ES": "EUR", "FI": "EUR",
	"FR": "EUR", "GR": "EUR", "HR": "EUR", "IE": "EUR", "IT": "EUR", "LT": "EUR", "LU": "EUR",
	"LV": "EUR", "MT": "EUR", "NL": "EUR", "PT": "EUR", "SI": "EUR", "SK": "EUR",
	"CH": "CHF", "LI": "CHF",
	"US": "USD", "GB": "GBP", "CA": "CAD", "AU": "AUD", "NZ": "NZD", "JP": "JPY",
	"SE": "SEK", "NO": "NOK", "DK": "DKK", "SG": "SGD", "HK": "HKD",
}

// localeRegion is the region a locale names, upper case: "CH" for
// de_CH.UTF-8 or en_CH, the override for en_US@rg=chzzzz; empty when it
// names none.
func localeRegion(locale string) string {
	if _, mods, ok := strings.Cut(locale, "@"); ok {
		for _, m := range strings.Split(mods, ";") {
			if v, ok := strings.CutPrefix(m, "rg="); ok && len(v) >= 2 {
				return strings.ToUpper(v[:2])
			}
		}
	}
	base, _, _ := strings.Cut(locale, "@")
	base, _, _ = strings.Cut(base, ".")
	i := strings.LastIndexAny(base, "_-")
	if i < 0 || len(base)-i-1 != 2 {
		return ""
	}
	return strings.ToUpper(base[i+1:])
}

// guessCurrency is the currency of the locale's region, else USD.
func guessCurrency(locale string) string {
	if c, ok := regionCurrency[localeRegion(locale)]; ok {
		return c
	}
	return "USD"
}
