package canonical

// moneyCurrencies are the ISO 4217 codes the engine reads as a currency
// of money: the US dollar and the currencies the European Central Bank
// publishes a reference rate for, the set the CoinTracking collector's
// rate fetcher converts (collectors/cointracking/frankfurter.py, plus
// USD). Any other code a source books an amount in is a token, a coin
// or a metal. The Python set is not reachable from Go, so the two are
// kept in step by hand.
var moneyCurrencies = map[string]struct{}{
	"USD": {}, "EUR": {}, "CHF": {}, "GBP": {}, "JPY": {},
	"AUD": {}, "CAD": {}, "NZD": {}, "SGD": {}, "HKD": {},
	"SEK": {}, "NOK": {}, "DKK": {}, "PLN": {}, "CZK": {},
	"HUF": {}, "RUB": {}, "INR": {}, "CNY": {}, "KRW": {},
	"TWD": {}, "THB": {}, "BRL": {}, "MXN": {}, "ZAR": {},
	"TRY": {}, "ILS": {},
}

// IsMoneyCurrency says code names a currency of money
// (moneyCurrencies), in upper case as gold stores a currency: what a
// crypto source books as cash and not as a coin, and what a source of
// exchange rates can convert.
func IsMoneyCurrency(code string) bool {
	_, ok := moneyCurrencies[code]
	return ok
}
