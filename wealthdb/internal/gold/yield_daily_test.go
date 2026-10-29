package gold

import (
	"database/sql"
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// A holding's daily amounts are what positions_at and cash_chosen read on
// each day of the year, at the day's last second and on the instant's day
// at the instant: its account's active snapshot at it, a day not held as
// zero. The book is invented: snapshots at random instants over two
// years, two on one day, one at a day's first second and one at its
// last, one after the instant; positions in and out of them, one of no
// value; cash in two balance kinds and in zero; a stale source of one
// old snapshot; and a balance held from the 5th to the 25th of every
// month, which every month end reads as zero.
func TestDailyAmountsAreEachDaysHoldings(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	asOf := ymd(2026, 6, 30).Unix() + 86399
	first := ymd(2025, 7, 1).Unix() + 86399
	rng := rand.New(rand.NewSource(19))
	snaps := []int64{first, first + 1, ymd(2025, 7, 1).Unix(), ymd(2025, 11, 3).Unix() + 36000,
		ymd(2025, 11, 3).Unix() + 54000, asOf, asOf + 1}
	for range 150 {
		snaps = append(snaps, ymd(2024, 6, 1).Unix()+rng.Int63n(ymd(2026, 7, 15).Unix()-ymd(2024, 6, 1).Unix()))
	}
	var positions, balances []string
	for _, at := range snaps {
		for _, k := range []string{"A1/QP1", "A1/QP2", "A2/QP3", "A2/QP4"} {
			if rng.Intn(10) < 7 {
				acct, iid, _ := strings.Cut(k, "/")
				value := itoa(100 + rng.Int63n(10000))
				if iid == "QP4" && rng.Intn(3) == 0 {
					value = "NULL"
				}
				positions = append(positions, "('a', "+itoa(at)+", '"+acct+"', '"+iid+"', '"+iid+
					"', 'public_equity', 'stock', 'USD', 1, "+value+")")
			}
		}
	}
	for range 120 {
		at := ymd(2024, 6, 1).Unix() + rng.Int63n(ymd(2026, 7, 15).Unix()-ymd(2024, 6, 1).Unix())
		for _, b := range []struct{ acct, ccy, kind string }{{"C1", "USD", "current"}, {"C1", "USD", "available"},
			{"C1", "EUR", "current"}, {"C2", "USD", "available"}} {
			if rng.Intn(10) < 7 {
				amount := itoa(rng.Int63n(5000))
				if rng.Intn(5) == 0 {
					amount = "0"
				}
				balances = append(balances, "('a', "+itoa(at)+", '"+b.acct+"', '"+b.ccy+"', '"+b.kind+"', "+amount+")")
			}
		}
	}
	for m := 0; m < 19; m++ {
		balances = append(balances,
			"('m', "+itoa(ymd(2025, 1+m, 5).Unix()+43200)+", 'M', 'USD', 'current', 5000)",
			"('m', "+itoa(ymd(2025, 1+m, 25).Unix()+43200)+", 'M', 'USD', 'current', 0)")
	}
	positions = append(positions, "('b', "+itoa(ymd(2024, 1, 1).Unix())+
		", 'B1', 'QOLD', 'QOLD', 'public_equity', 'stock', 'USD', 1, 777)")
	execEach(t, db,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value) VALUES `+strings.Join(positions, ", "),
		`INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount)
		    VALUES `+strings.Join(balances, ", "))

	b := &yieldBase{in: &yield.Inputs{AsOf: asOf, Day: ymd(2026, 6, 30)}}
	if err := b.readAmounts(ctx, db); err != nil {
		t.Fatal(err)
	}
	const days = 365
	want := map[yield.HoldingKey][]float64{}
	add := func(k yield.HoldingKey, day int, v float64) {
		if want[k] == nil {
			want[k] = make([]float64, days)
		}
		want[k][day] += v
	}
	for day := range days {
		at := first + int64(day)*86400
		if day == days-1 {
			at = asOf
		}
		if err := scanAll(ctx, db, `
SELECT silver_source_id, account_external_id, position_key, SUM(market_value)::DOUBLE
  FROM positions_at([?::BIGINT]) GROUP BY ALL
 UNION ALL
SELECT silver_source_id, account_external_id, 'cash:' || currency, amount::DOUBLE FROM cash_chosen(?)`,
			func(rows *sql.Rows) error {
				var k yield.HoldingKey
				var v sql.NullFloat64
				if err := rows.Scan(&k.Source, &k.Account, &k.PositionKey, &v); err != nil {
					return err
				}
				add(k, day, v.Float64)
				return nil
			}, at, at); err != nil {
			t.Fatal(err)
		}
	}
	// Each holding of this book is valued in one currency.
	amounts := map[yield.HoldingKey]*yield.Daily{}
	for k, d := range b.amounts {
		amounts[k.HoldingKey] = d
	}
	for k, d := range amounts {
		if want[k] == nil {
			want[k] = make([]float64, days)
		}
		if d.Days != days {
			t.Errorf("%v: %d days", k, d.Days)
		}
	}
	if len(want) != 9 {
		t.Errorf("%d holdings: %v", len(want), want)
	}
	for k, w := range want {
		got := make([]float64, days)
		d := amounts[k]
		if d != nil {
			for _, s := range d.Spans {
				for i := s.From; i < s.To; i++ {
					got[i] += s.Amount
				}
			}
		}
		sum := 0.0
		for i := range days {
			sum += w[i]
			if got[i] != w[i] {
				t.Errorf("%v on day %d: %v, positions_at %v", k, i, got[i], w[i])
				break
			}
		}
		if d != nil && math.Abs(d.Mean()-sum/days) > 1e-9 {
			t.Errorf("%v: mean %v, day by day %v", k, d.Mean(), sum/days)
		}
	}
	// Zero at every month end, held two thirds of the year.
	m := amounts[yield.HoldingKey{Source: "m", Account: "M", PositionKey: "cash:USD"}]
	if m == nil || m.Mean() < 5000*0.6 || m.Mean() > 5000*0.7 {
		t.Errorf("between the month ends: %+v", m)
	}
	for _, at := range monthEndsTo(asOf, 12) {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cash_chosen(?) WHERE silver_source_id = 'm'`, at).
			Scan(&n); err != nil || n != 0 {
			t.Errorf("held at the month end %d: %d %v", at, n, err)
		}
	}
	if old := amounts[yield.HoldingKey{Source: "b", Account: "B1", PositionKey: "QOLD"}]; old == nil ||
		old.Mean() != 777 || len(old.Spans) != 1 {
		t.Errorf("a stale source %+v", old)
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
