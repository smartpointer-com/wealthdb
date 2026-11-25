package canonical

import "testing"

// A currency of money is one of the listed ISO 4217 codes, in upper
// case; a token's code, a metal's and "no currency" are none.
func TestIsMoneyCurrency(t *testing.T) {
	for code, want := range map[string]bool{
		"USD": true, "CHF": true, "EUR": true, "GBP": true, "JPY": true, "ILS": true,
		"ETK": false, "DTK": false, "XTK": false, "BTC": false, "ETH": false,
		"XAU": false, NoCurrency: false, "usd": false, "": false,
	} {
		if got := IsMoneyCurrency(code); got != want {
			t.Errorf("IsMoneyCurrency(%q) = %v, want %v", code, got, want)
		}
	}
	// Every reporting currency of the engine is one.
	for _, code := range []string{"USD", "CHF", "EUR", "GBP"} {
		if !IsMoneyCurrency(code) {
			t.Errorf("the reporting currency %s is no currency of money", code)
		}
	}
	if n := len(moneyCurrencies); n != 27 {
		t.Errorf("%d currencies, want the 26 of the rate fetcher and USD", n)
	}
}
