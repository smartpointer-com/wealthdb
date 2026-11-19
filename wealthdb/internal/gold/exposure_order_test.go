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
	classes, err := ExposureClasses(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	checkRowOrder(t, "classes", len(classes), func(i, j int) int {
		a, b := classes[i], classes[j]
		return cmp.Or(desc(val(a.Value), val(b.Value)), cmp.Compare(a.AssetClass, b.AssetClass))
	})

	countries, err := ExposureCountries(ctx, db, exAsOf, "USD", "risk")
	if err != nil {
		t.Fatal(err)
	}
	checkRowOrder(t, "countries", len(countries), func(i, j int) int {
		a, b := countries[i], countries[j]
		return cmp.Or(desc(val(a.Value), val(b.Value)), cmp.Compare(a.Country, b.Country),
			cmp.Compare(str(a.Region), str(b.Region)))
	})

	currencies, err := ExposureCurrencies(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	checkRowOrder(t, "currencies", len(currencies), func(i, j int) int {
		a, b := currencies[i], currencies[j]
		return cmp.Or(desc(val(a.Value), val(b.Value)), cmp.Compare(a.Currency, b.Currency))
	})

	situs, err := ExposureSitus(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	checkRowOrder(t, "situs", len(situs), func(i, j int) int {
		a, b := situs[i], situs[j]
		return cmp.Or(desc(val(a.Value), val(b.Value)), cmp.Compare(a.Situs, b.Situs))
	})

	securities, err := ExposureSecurities(ctx, db, exAsOf, "USD", "risk")
	if err != nil {
		t.Fatal(err)
	}
	checkRowOrder(t, "securities", len(securities), func(i, j int) int {
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
	checkRowOrder(t, "funds", len(funds), func(i, j int) int {
		a, b := funds[i], funds[j]
		return cmp.Or(cmp.Compare(other(a.Fund), other(b.Fund)),
			desc(math.Abs(val(a.Value)), math.Abs(val(b.Value))), cmp.Compare(a.Fund, b.Fund))
	})
}

// checkRowOrder fails unless each of the n rows sorts before the next
// by order, so no two rows tie.
func checkRowOrder(t *testing.T, name string, n int, order func(i, j int) int) {
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

// The bucket views list their buckets in their dimension's order
// (exposure_bucket_order), and the issuers rank by value, a (none) row
// last.
func TestExposureBucketAndIssuerReadersKeepTheMacroOrder(t *testing.T) {
	t.Parallel()
	db, ctx, at := seedYieldExposure(t)
	for view, dim := range map[string]string{"credit": "credit", "maturities": "maturity", "coupons": "coupon",
		"cycles": "cycle"} {
		rows, err := exposureBuckets(ctx, db, view, at, "USD")
		if err != nil {
			t.Fatal(err)
		}
		pos := make([]int, len(rows))
		for i, r := range rows {
			if err := db.QueryRow(`SELECT exposure_bucket_order(?, ?)`, dim, r.Bucket).Scan(&pos[i]); err != nil {
				t.Fatalf("%s %s: %v", view, r.Bucket, err)
			}
		}
		checkRowOrder(t, view, len(rows), func(i, j int) int { return cmp.Compare(pos[i], pos[j]) })
	}
	iss, err := ExposureIssuers(ctx, db, at, "USD")
	if err != nil {
		t.Fatal(err)
	}
	none := func(r ExposureIssuerRow) int {
		if r.Issuer == "(none)" {
			return 1
		}
		return 0
	}
	for i := 1; i < len(iss); i++ {
		a, b := iss[i-1], iss[i]
		va, _ := strconv.ParseFloat(*a.Value, 64)
		vb, _ := strconv.ParseFloat(*b.Value, 64)
		if cmp.Or(cmp.Compare(none(a), none(b)), cmp.Compare(vb, va)) > 0 {
			t.Errorf("issuer %q sorts before %q", a.Issuer, b.Issuer)
		}
	}
}

// A quality share on a rounding boundary prints the same in every row
// order. The proxy leaves sum to 37.5% of the group's gross value, so a
// float sum would print 37 or 38 by the order it adds them in; the test
// first shows that it does, so the values stay on the boundary.
func TestExposureQualityShareIgnoresRowOrder(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	leaves := [][2]string{{"0.1", "proxy"}, {"0.2", "proxy"}, {"-0.3", "proxy"}, {"0.4", ""}, {"0.6", ""}}
	floats, texts := map[int]bool{}, map[string]bool{}
	for _, order := range [][]int{{0, 1, 2, 3, 4}, {1, 3, 4, 2, 0}, {2, 1, 0, 4, 3}, {4, 1, 3, 2, 0}} {
		values, args := "", []any{}
		for i, k := range order {
			if i > 0 {
				values += ", "
			}
			values += "(?::DOUBLE, CASE WHEN ? = '' THEN [] ELSE [?] END, ?)"
			args = append(args, leaves[k][0], leaves[k][1], leaves[k][1], i)
		}
		// Five rows sum on one thread, in the subquery's order, so the float
		// control adds them in the order given.
		var float int
		var text string
		if err := db.QueryRowContext(ctx, `SELECT CAST(round(100 * SUM(abs(v)) FILTER (WHERE flags <> []) /
		       SUM(abs(v))) AS INTEGER), exposure_quality(flags, v)
		  FROM (SELECT v, flags FROM (VALUES `+values+`) t(v, flags, n) ORDER BY n)`, args...).
			Scan(&float, &text); err != nil {
			t.Fatal(err)
		}
		floats[float], texts[text] = true, true
	}
	if len(floats) < 2 {
		t.Fatalf("the float sums agree (%v): the fixture is no boundary", floats)
	}
	if len(texts) != 1 || !texts["proxy=38%"] {
		t.Errorf("quality reads %v, want proxy=38%% in every order", texts)
	}
}
