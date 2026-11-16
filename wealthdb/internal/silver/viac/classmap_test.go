package viac

import "testing"

// A report row's printed section reads as the class the live rows carry,
// so a report holding and a live holding of the same fund get one pair.
// A row written before the section was stored as printed holds that
// class already. Names are synthetic placeholders.
func TestTaxonomyForReadsAPrintedSectionAsItsClass(t *testing.T) {
	cases := []struct{ printed, class, name string }{
		{"Liquidity", "money_market", "Synthetic Money Market Fund"},
		{"Equity", "equity", "Synthetic Equity Index Fund"},
		{"Equities", "equity", "Synthetic Equity Index Fund"},
		{"Bonds", "bond", "Synthetic Bond Index Fund"},
		{"Real Estate", "fund", "Synthetic Property Index Fund"},
		{"Commodities", "metal", "Synthetic Metal Index Fund"},
		{"Alternative Investments", "other", "Synthetic Bitcoin Basket"},
		{"Alternative Investments", "other", "Synthetic Mystery Alternatives"},
		{"", "other", "Synthetic Mystery Alternatives"},
		{"Equity", "equity", "Placeholder Listed Private Equity Fund"},
	}
	for _, c := range cases {
		gotClass, gotVehicle := taxonomyFor(c.printed, c.name)
		wantClass, wantVehicle := taxonomyFor(c.class, c.name)
		if gotClass != wantClass || gotVehicle != wantVehicle {
			t.Errorf("taxonomyFor(%q, %q) = (%q, %q), want (%q, %q) as for %q",
				c.printed, c.name, gotClass, gotVehicle, wantClass, wantVehicle, c.class)
		}
	}
}
