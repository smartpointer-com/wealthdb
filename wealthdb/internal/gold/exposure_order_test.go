package gold

import (
	"cmp"
	"math"
	"strconv"
	"testing"
)

// The readers of the macros that order by their own columns return
// their rows in that order, the tie-break included: the reading query
// carries the ORDER BY, so tied rows print in one order.
func TestExposureReadersKeepTheMacroOrder(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	val := func(s *string) float64 {
		if s == nil {
			return math.Inf(-1)
		}
		v, err := strconv.ParseFloat(*s, 64)
		if err != nil {
			t.Fatalf("value %q: %v", *s, err)
		}
		return v
	}
	desc := func(a, b float64) int { return cmp.Compare(b, a) }
	str := func(s *string) string {
		if s == nil {
			return "\xff"
		}
		return *s
	}
	check := func(name string, n int, order func(i, j int) int) {
		t.Helper()
		if n < 2 {
			t.Fatalf("%s: %d row(s), want a fixture with several", name, n)
		}
		for i := 1; i < n; i++ {
			if order(i-1, i) >= 0 {
				t.Errorf("%s: row %d sorts at or after row %d", name, i-1, i)
			}
		}
	}

	classes, err := ExposureClasses(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	check("classes", len(classes), func(i, j int) int {
		a, b := classes[i], classes[j]
		return cmp.Or(desc(val(a.Value), val(b.Value)), cmp.Compare(a.AssetClass, b.AssetClass))
	})

	countries, err := ExposureCountries(ctx, db, exAsOf, "USD", "risk")
	if err != nil {
		t.Fatal(err)
	}
	check("countries", len(countries), func(i, j int) int {
		a, b := countries[i], countries[j]
		return cmp.Or(desc(val(a.Value), val(b.Value)), cmp.Compare(a.Country, b.Country),
			cmp.Compare(str(a.Region), str(b.Region)))
	})

	currencies, err := ExposureCurrencies(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	check("currencies", len(currencies), func(i, j int) int {
		a, b := currencies[i], currencies[j]
		return cmp.Or(desc(val(a.Value), val(b.Value)), cmp.Compare(a.Currency, b.Currency))
	})

	situs, err := ExposureSitus(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	check("situs", len(situs), func(i, j int) int {
		a, b := situs[i], situs[j]
		return cmp.Or(desc(val(a.Value), val(b.Value)), cmp.Compare(a.Situs, b.Situs))
	})

	securities, err := ExposureSecurities(ctx, db, exAsOf, "USD", "risk")
	if err != nil {
		t.Fatal(err)
	}
	check("securities", len(securities), func(i, j int) int {
		a, b := securities[i], securities[j]
		return cmp.Or(desc(math.Abs(val(a.Value)), math.Abs(val(b.Value))), cmp.Compare(a.Security, b.Security))
	})

	funds, err := ExposureFunds(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	other := func(f string) int {
		if f == "(other holdings)" {
			return 1
		}
		return 0
	}
	check("funds", len(funds), func(i, j int) int {
		a, b := funds[i], funds[j]
		return cmp.Or(cmp.Compare(other(a.Fund), other(b.Fund)),
			desc(math.Abs(val(a.Value)), math.Abs(val(b.Value))), cmp.Compare(a.Fund, b.Fund))
	})
}
