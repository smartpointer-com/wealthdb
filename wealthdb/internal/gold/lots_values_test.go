package gold

import (
	"strings"
	"testing"
)

// The lot engine's cross rates must bridge in the order the reports do,
// or a figure the engine values and one a report converts disagree.
func TestLotRatesBridgeInTheReportsOrder(t *testing.T) {
	t.Parallel()
	var body string
	for _, s := range mustDerived(t).stmts {
		if s.name == "fx_rates_into" {
			body = stripComments(s.sql)
		}
	}
	if body == "" {
		t.Fatal("fx_rates_into not found")
	}
	prev := -1
	for _, ccy := range lotRateBridges {
		i := strings.Index(body, "to_ccy = '"+ccy+"'")
		if i < 0 || i < prev {
			t.Fatalf("fx_rates_into does not bridge through %v in this order", lotRateBridges)
		}
		prev = i
	}

	r := &lotRates{daily: &DailyRates{pairs: map[[2]string][]dayRate{
		{"BTC", "CHF"}: {{10, 90}}, {"CHF", "EUR"}: {{10, 1}},
		{"BTC", "USD"}: {{10, 100}}, {"USD", "EUR"}: {{10, 1}},
	}}}
	if x, ok := r.rate("BTC", "EUR", 12); !ok || x != 90 {
		t.Errorf("BTC in EUR = %v %v, want the CHF bridge's 90", x, ok)
	}
	if _, ok := r.rate("BTC", "EUR", 10+marketRateMaxAge+1); ok {
		t.Error("a rate older than marketRateMaxAge must price nothing")
	}
}
