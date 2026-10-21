package gold

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/fixedincome"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// Each curve is read on the basis its series' payloads state: spot rates
// as zero rates, an annual par curve with annual coupons, a curve with no
// basis or the Treasury's as before. The benchmark is in the curve's
// semiannual convention. A curve whose tenors state two bases, a basis no
// curve is built on, or a spot curve NewCurve refuses gives no curve; a
// Treasury curve that does not bootstrap gives its benchmark alone. Each
// refusal logs one line, once. The series and rates are invented.
//
// Not parallel: it swaps the log sink.
func TestReadYieldCurvesBasis(t *testing.T) {
	db, ctx := openMigrated(t)
	var logged []string
	sink := logCurve
	logCurve = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	t.Cleanup(func() { logCurve = sink })
	curveWarned.Lock()
	clear(curveWarned.seen)
	curveWarned.Unlock()

	day := ymd(2026, 6, 30).Unix()
	const (
		spot     = `'{"currency":"EUR","basis":"spot","compounding":"continuous"}'`
		parYear  = `'{"basis":"par","compounding":"annual"}'`
		treasury = `'{"currency":"USD","basis":"constant_maturity"}'`
		forward  = `'{"basis":"forward","compounding":"annual"}'`
	)
	execEach(t, db,
		`INSERT INTO yield_curves (currency, tenor, tenor_months, silver_source_id, series_id) VALUES
		    ('EUR', '3M', 3, 'curves', 'EUR-3M'), ('EUR', '2Y', 24, 'curves', 'EUR-2Y'),
		    ('EUR', '10Y', 120, 'curves', 'EUR-10Y'),
		    ('JPY', '1Y', 12, 'curves', 'JPY-1Y'), ('JPY', '2Y', 24, 'curves', 'JPY-2Y'),
		    ('USD', '3M', 3, 'rates', 'USD-3M'), ('USD', '2Y', 24, 'rates', 'USD-2Y'),
		    ('USD', '10Y', 120, 'rates', 'USD-10Y'),
		    ('CHF', '1Y', 12, 'curves', 'CHF-1Y'), ('CHF', '10Y', 120, 'curves', 'CHF-10Y'),
		    ('GBP', '3M', 3, 'curves', 'GBP-3M'), ('GBP', '5Y', 60, 'curves', 'GBP-5Y'),
		    ('NOK', '3M', 3, 'curves', 'NOK-3M'), ('NOK', '5Y', 60, 'curves', 'NOK-5Y'),
		    ('CAD', '3M', 3, 'rates', 'CAD-3M'), ('CAD', '1Y', 12, 'rates', 'CAD-1Y'),
		    ('CAD', '30Y', 360, 'rates', 'CAD-30Y')`,
		`INSERT INTO rate_observations (silver_source_id, series_id, as_of, value, payload) VALUES
		    ('curves', 'EUR-3M', `+itoa(day)+`, 2.0, `+spot+`),
		    ('curves', 'EUR-2Y', `+itoa(day)+`, 2.5, `+spot+`),
		    ('curves', 'EUR-10Y', `+itoa(day)+`, 3.0, `+spot+`),
		    ('curves', 'JPY-1Y', `+itoa(day)+`, 1.0, `+parYear+`),
		    ('curves', 'JPY-2Y', `+itoa(day)+`, 1.5, `+parYear+`),
		    ('rates', 'USD-3M', `+itoa(day)+`, 4.0, NULL),
		    ('rates', 'USD-2Y', `+itoa(day)+`, 3.5, '{}'),
		    ('rates', 'USD-10Y', `+itoa(day)+`, 4.2, `+treasury+`),
		    ('curves', 'CHF-1Y', `+itoa(day)+`, 0.5, `+spot+`),
		    ('curves', 'CHF-10Y', `+itoa(day)+`, 0.9, `+parYear+`),
		    ('curves', 'GBP-3M', `+itoa(day)+`, 4.0, `+forward+`),
		    ('curves', 'GBP-5Y', `+itoa(day)+`, 4.1, `+forward+`),
		    ('curves', 'NOK-3M', `+itoa(day)+`, 4.0, `+spot+`),
		    ('curves', 'NOK-5Y', `+itoa(day)+`, 140, `+spot+`),
		    ('rates', 'CAD-3M', `+itoa(day)+`, 0, NULL),
		    ('rates', 'CAD-1Y', `+itoa(day)+`, 0, NULL),
		    ('rates', 'CAD-30Y', `+itoa(day)+`, 60, NULL)`,
	)
	in := &yield.Inputs{AsOf: day + 86399, Curves: map[string]*yield.Curve{}}
	if err := readYieldCurves(ctx, db, in); err != nil {
		t.Fatal(err)
	}

	// The euro's spot rates are its zero rates: continuously, 2% for
	// three months discounts by exp(-0.02/4). The benchmark is that 3M
	// rate restated semiannually, so a constant path and the forward
	// path start on the same rate.
	eur := in.Curves["EUR"]
	if eur == nil || eur.Zero == nil {
		t.Fatalf("EUR curve %+v", eur)
	}
	if d := eur.Zero.Discount(3); math.Abs(d-math.Exp(-0.005)) > 1e-12 {
		t.Errorf("EUR discount 3M = %v, want exp(-0.005)", d)
	}
	if d := eur.Zero.Discount(120); math.Abs(d-math.Exp(-0.3)) > 1e-12 {
		t.Errorf("EUR discount 10Y = %v, want exp(-0.3)", d)
	}
	if want := 200 * math.Expm1(0.01); eur.BenchmarkMonths != 3 || math.Abs(eur.Benchmark-want) > 1e-12 {
		t.Errorf("EUR benchmark %v at %d months, want %v at 3", eur.Benchmark, eur.BenchmarkMonths, want)
	}
	if f := eur.Zero.Forward(0, 3); math.Abs(f-eur.Benchmark) > 1e-9 {
		t.Errorf("EUR forward 3M from today = %v, want the benchmark %v", f, eur.Benchmark)
	}

	// Annual par bonds: DF(1) = 1/1.01, and 0.015·DF(1) + 1.015·DF(2) = 1.
	// The benchmark, the shortest tenor, is the 1Y restated semiannually.
	jpy := in.Curves["JPY"]
	if jpy == nil || jpy.Zero == nil {
		t.Fatalf("JPY curve %+v", jpy)
	}
	d1 := 1 / 1.01
	if d := jpy.Zero.Discount(24); math.Abs(d-(1-0.015*d1)/1.015) > 1e-12 {
		t.Errorf("JPY discount 2Y = %v", d)
	}
	if want := 200 * (math.Sqrt(1.01) - 1); jpy.BenchmarkMonths != 12 || math.Abs(jpy.Benchmark-want) > 1e-12 {
		t.Errorf("JPY benchmark %v at %d months, want %v at 12", jpy.Benchmark, jpy.BenchmarkMonths, want)
	}

	// No basis, an empty payload and the Treasury's read on the Treasury
	// basis, the benchmark as published.
	usd := in.Curves["USD"]
	want, err := fixedincome.NewCurve([]fixedincome.Point{{Months: 3, Rate: 4}, {Months: 24, Rate: 3.5},
		{Months: 120, Rate: 4.2}}, fixedincome.Treasury)
	if err != nil {
		t.Fatal(err)
	}
	if usd == nil || usd.Zero == nil || usd.Benchmark != 4 || usd.BenchmarkMonths != 3 {
		t.Fatalf("USD curve %+v", usd)
	}
	for m := 0; m <= 120; m++ { // to the longest tenor
		if usd.Zero.Zero(m) != want.Zero(m) {
			t.Fatalf("USD zero %dM = %v, want %v", m, usd.Zero.Zero(m), want.Zero(m))
		}
	}

	// Mixed bases, a basis no curve is built on and a spot curve
	// NewCurve refuses give no curve; the Treasury curve that does not
	// bootstrap gives its benchmark as published.
	for _, ccy := range []string{"CHF", "GBP", "NOK"} {
		if c := in.Curves[ccy]; c != nil {
			t.Errorf("%s curve %+v, want none", ccy, c)
		}
	}
	if c := in.Curves["CAD"]; c == nil || c.Zero != nil || c.Benchmark != 0 || c.BenchmarkMonths != 3 {
		t.Errorf("CAD curve %+v, want the 3M benchmark alone", c)
	}

	// One line per refusal, naming its currency, however often the curves
	// are read.
	if err := readYieldCurves(ctx, db, &yield.Inputs{AsOf: day + 86399, Curves: map[string]*yield.Curve{}}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(logged)
	wantLines := []string{
		"yield: no CHF curve: its tenors state two bases, spot continuous and par annual",
		`yield: no GBP curve: fixedincome: basis "forward": want spot or par`,
		"yield: no NOK curve: fixedincome: tenor of 60 months: rate 140",
		"yield: the CAD curve does not bootstrap; it gives its benchmark alone: ",
	}
	if len(logged) != len(wantLines) {
		t.Fatalf("logged %q, want %d lines", logged, len(wantLines))
	}
	for i, w := range wantLines {
		if !strings.HasPrefix(logged[i], w) {
			t.Errorf("line %d = %q, want it to start %q", i, logged[i], w)
		}
	}
}
