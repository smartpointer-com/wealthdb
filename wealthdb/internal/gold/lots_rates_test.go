package gold

import (
	"io/fs"
	"strings"
	"testing"
)

// The lot engine's cross rates must bridge in the order the reports do,
// or a figure the engine values and one a report converts disagree.
func TestLotRatesBridgeInTheReportsOrder(t *testing.T) {
	b, err := fs.ReadFile(migrationsFS, "migrations/0118_lot_engine.sql")
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	at := strings.Index(body, "CREATE OR REPLACE MACRO fx_rates_to")
	if at < 0 {
		t.Fatal("fx_rates_to not found")
	}
	body = body[at:]
	prev := -1
	for _, ccy := range lotRateBridges {
		i := strings.Index(body, "to_ccy = '"+ccy+"'")
		if i < 0 || i < prev {
			t.Fatalf("fx_rates_to does not bridge through %v in this order", lotRateBridges)
		}
		prev = i
	}

	r := &lotRates{pairs: map[[2]string][]dayRate{
		{"BTC", "CHF"}: {{10, 90}}, {"CHF", "EUR"}: {{10, 1}},
		{"BTC", "USD"}: {{10, 100}}, {"USD", "EUR"}: {{10, 1}},
	}}
	if x, ok := r.rate("BTC", "EUR", 12); !ok || x != 90 {
		t.Errorf("BTC in EUR = %v %v, want the CHF bridge's 90", x, ok)
	}
	if _, ok := r.rate("BTC", "EUR", 10+lotRateMaxAge+1); ok {
		t.Error("a rate older than lotRateMaxAge must price nothing")
	}
}
