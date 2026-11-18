package gold

import (
	"math"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// A security's line is the one of the most value held; of equal ones,
// the first fund's by its key, and of one fund's, the first line. A line
// of no weight still names its security. The funds are invented.
func TestTheLineOfASecurity(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	snap := ymd(2026, 6, 30).Unix()
	execEach(t, db,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		    first_seen_at, last_seen_at) VALUES ('src', 'B', 'brokerage', 'Brokerage', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, name, currency,
		    first_seen_at, last_seen_at) VALUES ('src', 'QFB', 'fixed_income', 'etf', 'QFB', 'Example Fund B', 'USD', 0, 0),
		    ('src', 'QFA', 'fixed_income', 'etf', 'QFA', 'Example Fund A', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value) VALUES
		    ('src', `+itoa(snap)+`, 'B', 'QFB', 'QFB', 'fixed_income', 'etf', 'USD', 10, 1000),
		    ('src', `+itoa(snap)+`, 'B', 'QFA', 'QFA', 'fixed_income', 'etf', 'USD', 10, 1000)`,
		`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count, currency) VALUES
		    ('ref', 'symbol:QFB', `+itoa(snap)+`, 'holdings', 'testprov', 2, 'USD'),
		    ('ref', 'symbol:QFA', `+itoa(snap)+`, 'holdings', 'testprov', 3, 'USD')`,
		`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, name, asset_type, weight,
		    currency, maturity, coupon) VALUES
		    ('ref', 'symbol:QFB', `+itoa(snap)+`, 'holdings', 1, 'isin:XX00000000T1', 'EXAMPLE NOTE', 'Fixed Income', 50,
		     'USD', DATE '2030-01-15', 5),
		    ('ref', 'symbol:QFB', `+itoa(snap)+`, 'holdings', 2, 'isin:XX00000000T2', 'EXAMPLE BILL', 'Fixed Income', 0,
		     'USD', DATE '2027-01-15', 6),
		    ('ref', 'symbol:QFA', `+itoa(snap)+`, 'holdings', 1, 'isin:XX00000000T1', 'EXAMPLE NOTE', 'Fixed Income', 50,
		     'USD', DATE '2030-01-15', 3),
		    ('ref', 'symbol:QFA', `+itoa(snap)+`, 'holdings', 2, 'isin:XX00000000T1', 'EXAMPLE NOTE', 'Fixed Income', 50,
		     'USD', DATE '2030-01-15', 4),
		    ('ref', 'symbol:QFA', `+itoa(snap)+`, 'holdings', 3, 'isin:XX00000000T2', 'EXAMPLE BILL', 'Fixed Income', 0,
		     'USD', DATE '2027-01-15', 7)`)
	rebuildExposureWithDefaults(t, db, ctx)
	for range 5 {
		in, err := YieldInputs(ctx, db, snap+86399, "USD")
		if err != nil {
			t.Fatal(err)
		}
		coupon := func(key string) float64 {
			if l, ok := in.Lines[key]; ok && l.Coupon != nil {
				return *l.Coupon
			}
			return 0
		}
		if got := [2]float64{coupon("isin:XX00000000T1"), coupon("isin:XX00000000T2")}; got != [2]float64{3, 7} {
			t.Fatalf("the lines' coupons %v, want QFA's first lines' [3 7]", got)
		}
	}
}

// A fund's filed DV01 reads in the report's sign, positive where the fund
// loses value as rates rise. A filing whose DV01 sums below zero flips
// whole, its rows keeping their signs relative to each other, its DV100
// and credit-spread figures with it; one above zero, and one that sums
// to zero, stay. A filing that states no DV01 flips by its DV100. Two
// sources' filings of one fund and day sign apart. The filings are
// invented.
func TestAFilingsSignIsTheReports(t *testing.T) {
	t.Parallel()
	db, _ := openMigrated(t)
	execEach(t, db, `INSERT INTO fund_risk (silver_source_id, fund_ref, as_of, currency, tenor, dv01, dv100,
	    credit_ig_dv01) VALUES
	    ('ref', 'symbol:QNEG', 1, 'USD', '5Y', -40, -4000, NULL), ('ref', 'symbol:QNEG', 1, 'USD', '10Y', -60, -6000, NULL),
	    ('ref', 'symbol:QNEG', 1, 'XXX', '5Y', NULL, NULL, -7),
	    ('ref', 'symbol:QPOS', 1, 'USD', '5Y', 40, 4000, NULL),
	    ('ref', 'symbol:QMIX', 1, 'USD', '5Y', -50, 3000, NULL), ('ref', 'symbol:QMIX', 1, 'EUR', '5Y', 20, -1000, NULL),
	    ('ref', 'symbol:QMIX', 2, 'USD', '5Y', 50, 4000, NULL),
	    ('ref', 'symbol:QZER', 1, 'USD', '5Y', -10, -1000, NULL), ('ref', 'symbol:QZER', 1, 'EUR', '5Y', 10, 900, NULL),
	    ('ref', 'symbol:Q100', 1, 'USD', '5Y', NULL, -2000, NULL),
	    ('ref', 'symbol:QTWO', 1, 'USD', '5Y', -30, NULL, NULL), ('inbox', 'symbol:QTWO', 1, 'USD', '5Y', 30, NULL, NULL)`)
	got := strings.Join(scanStrings(t, db, `SELECT concat_ws(' ', silver_source_id, fund_ref, as_of, currency, tenor,
	    COALESCE(dv01::DOUBLE::VARCHAR, '-'), COALESCE(dv100::DOUBLE::VARCHAR, '-'),
	    COALESCE(credit_ig_dv01::DOUBLE::VARCHAR, '-')) FROM fund_risk_signed ORDER BY 1`), "\n")
	want := strings.Join([]string{
		"inbox symbol:QTWO 1 USD 5Y 30.0 - -",
		"ref symbol:Q100 1 USD 5Y - 2000.0 -",
		"ref symbol:QMIX 1 EUR 5Y -20.0 1000.0 -",
		"ref symbol:QMIX 1 USD 5Y 50.0 -3000.0 -",
		"ref symbol:QMIX 2 USD 5Y 50.0 4000.0 -",
		"ref symbol:QNEG 1 USD 10Y 60.0 6000.0 -",
		"ref symbol:QNEG 1 USD 5Y 40.0 4000.0 -",
		"ref symbol:QNEG 1 XXX 5Y - - 7.0",
		"ref symbol:QPOS 1 USD 5Y 40.0 4000.0 -",
		"ref symbol:QTWO 1 USD 5Y 30.0 - -",
		"ref symbol:QZER 1 EUR 5Y 10.0 900.0 -",
		"ref symbol:QZER 1 USD 5Y -10.0 -1000.0 -",
	}, "\n")
	if got != want {
		t.Errorf("signed\n%s\nwant\n%s", got, want)
	}
}

// A bond fund that files its DV01 below zero reads positive: its
// position's DV01 and its `yield funds` row, by currency on `yield
// global`'s rows, and per tenor along the curve.
func TestANegativeFilingReadsPositive(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	snap := ymd(2026, 6, 30).Unix()
	execEach(t, db,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		    first_seen_at, last_seen_at) VALUES ('src', 'B', 'brokerage', 'Brokerage', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, name, currency,
		    first_seen_at, last_seen_at) VALUES ('src', 'QBF', 'fixed_income', 'etf', 'QBF', 'Example Bond Fund A', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value) VALUES
		    ('src', `+itoa(snap)+`, 'B', 'QBF', 'QBF', 'fixed_income', 'etf', 'USD', 100, 10000)`,
		`INSERT INTO fund_risk (silver_source_id, fund_ref, as_of, currency, tenor, dv01) VALUES
		    ('ref', 'symbol:QBF', `+itoa(snap-10*86400)+`, 'USD', '5Y', -300),
		    ('ref', 'symbol:QBF', `+itoa(snap-10*86400)+`, 'USD', '10Y', -500),
		    ('ref', 'symbol:QBF', `+itoa(snap-10*86400)+`, 'EUR', '5Y', -200)`,
		`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, net_assets, line_count)
		    VALUES ('ref', 'symbol:QBF', `+itoa(snap-10*86400)+`, 'holdings', 'sec', 1000000, 0)`)
	rebuildExposureWithDefaults(t, db, ctx)
	in, err := YieldInputs(ctx, db, snap+86399, "USD")
	if err != nil {
		t.Fatal(err)
	}
	b := yield.Compute(in)
	p := b.Positions[0]
	if f := b.Funds(); len(f) != 1 || f[0].DV01 == nil || p.DV01 == nil || *f[0].DV01 != *p.DV01 ||
		math.Abs(*p.DV01-10) > 1e-9 {
		t.Errorf("funds %+v, position %v", f, p.DV01)
	}
	rows := map[string]float64{}
	for _, r := range b.Aggregate(yield.GrainAll, false) {
		if r.Total {
			r.Currency = "total"
		}
		if r.DV01 != nil {
			rows[r.Currency] = *r.DV01
		}
	}
	curve := map[string]float64{}
	for _, r := range b.Curve() {
		if r.DV01 != 0 {
			curve[r.Currency+" "+r.Tenor] = r.DV01
		}
	}
	for what, got := range map[string]map[string]float64{"global": rows, "curve": curve} {
		want := map[string]map[string]float64{"global": {"USD": 8, "EUR": 2, "total": 10},
			"curve": {"USD 5Y": 3, "USD 10Y": 5, "EUR 5Y": 2}}[what]
		if len(got) != len(want) {
			t.Errorf("%s %v, want %v", what, got, want)
		}
		for k, v := range want {
			if math.Abs(got[k]-v) > 1e-9 {
				t.Errorf("%s %s %v, want %v", what, k, got[k], v)
			}
		}
	}
}

// A fund's lines say how much of it is debt and how much equity, by the
// class each line's asset type maps to: a fund classed as equity whose
// lines are bonds is a bond fund, and its lines give its duration.
func TestAFundsLinesNameItsDebt(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	snap := ymd(2026, 6, 30).Unix()
	execEach(t, db,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		    first_seen_at, last_seen_at) VALUES ('src', 'B', 'brokerage', 'Brokerage', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, name, currency,
		    first_seen_at, last_seen_at) VALUES
		    ('src', 'QFX', 'public_equity', 'etf', 'QFX', 'Example Bond Fund A', 'USD', 0, 0),
		    ('src', 'QEQ', 'fixed_income', 'etf', 'QEQ', 'Example Income Fund', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value) VALUES
		    ('src', `+itoa(snap)+`, 'B', 'QFX', 'QFX', 'public_equity', 'etf', 'USD', 10, 1000),
		    ('src', `+itoa(snap)+`, 'B', 'QEQ', 'QEQ', 'fixed_income', 'etf', 'USD', 10, 1000)`,
		`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count, currency) VALUES
		    ('ref', 'symbol:QFX', `+itoa(snap)+`, 'holdings', 'testprov', 3, 'USD'),
		    ('ref', 'symbol:QEQ', `+itoa(snap)+`, 'holdings', 'testprov', 2, 'USD')`,
		`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, name, asset_type, weight,
		    currency, maturity, coupon, modified_duration) VALUES
		    ('ref', 'symbol:QFX', `+itoa(snap)+`, 'holdings', 1, 'isin:XX00000000F1', 'EXAMPLE NOTE', 'Fixed Income', 60,
		     'USD', DATE '2031-01-15', 3, 4),
		    ('ref', 'symbol:QFX', `+itoa(snap)+`, 'holdings', 2, 'isin:XX00000000F2', 'EXAMPLE BOND', NULL, 30,
		     'USD', DATE '2036-01-15', 4, 7),
		    ('ref', 'symbol:QFX', `+itoa(snap)+`, 'holdings', 3, NULL, 'DOLLAR CASH', 'Cash', 10, 'USD', NULL, NULL, NULL),
		    ('ref', 'symbol:QEQ', `+itoa(snap)+`, 'holdings', 1, 'isin:XX00000000E1', 'EXAMPLE CO', 'Equity', 80,
		     'USD', NULL, NULL, NULL),
		    ('ref', 'symbol:QEQ', `+itoa(snap)+`, 'holdings', 2, 'isin:XX00000000E2', 'EXAMPLE NOTE', 'Fixed Income', 20,
		     'USD', DATE '2031-01-15', 3, 4)`)
	rebuildExposureWithDefaults(t, db, ctx)
	in, err := YieldInputs(ctx, db, snap+86399, "USD")
	if err != nil {
		t.Fatal(err)
	}
	fx, eq := in.Funds["symbol:QFX"].Lines, in.Funds["symbol:QEQ"].Lines
	if fx == nil || fx.DebtPct != 90 || fx.EquityPct != 0 || fx.DurationPct != 90 || eq == nil || eq.DebtPct != 20 ||
		eq.EquityPct != 80 {
		t.Fatalf("lines %+v %+v", fx, eq)
	}
	kinds := map[string]yield.Kind{}
	for _, p := range yield.Compute(in).Positions {
		kinds[p.Instrument] = p.Kind
		if p.Instrument == "QFX" && (p.Duration == nil || math.Abs(*p.Duration-(60*4+30*7)/90.0) > 1e-9) {
			t.Errorf("duration %v", p.Duration)
		}
	}
	if kinds["QFX"] != yield.KindBondFund || kinds["QEQ"] != yield.KindEquityFund {
		t.Errorf("kinds %v", kinds)
	}
}
