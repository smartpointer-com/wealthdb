package gold

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

func ymd(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }

func period(start, end time.Time, v float64) periodFact {
	return periodFact{start: start, end: end, value: v}
}

// The trailing twelve months of a reported figure: the four newest
// quarters; a quarter a year-to-date figure implies, the difference from
// the one before it (six months less the first quarter, nine less six, a
// year less nine); a fiscal year where the quarters are stale; a year
// earlier for growth. The figures are invented.
func TestTrailingTwelveMonths(t *testing.T) {
	t.Parallel()
	day := ymd(2026, 6, 15)
	var quarters []periodFact
	for i := 0; i < 8; i++ {
		start := ymd(2024, 4, 1).AddDate(0, 3*i, 0)
		quarters = append(quarters, period(start, start.AddDate(0, 3, -1), float64(i+1)))
	}
	check := func(what string, ps []periodFact, years int, want float64, ok bool) {
		t.Helper()
		got, gotOK := trailing(ps, day, years)
		if gotOK != ok || math.Abs(got-want) > 1e-9 {
			t.Errorf("%s: %v %v, want %v %v", what, got, gotOK, want, ok)
		}
	}
	check("quarters", quarters, 0, 5+6+7+8, true)
	check("a year earlier", quarters, 1, 1+2+3+4, true)
	ytd := []periodFact{
		period(ymd(2024, 7, 1), ymd(2025, 3, 31), 5), // nine months
		period(ymd(2024, 7, 1), ymd(2025, 6, 30), 8), // the fiscal year
		period(ymd(2025, 7, 1), ymd(2025, 9, 30), 1), // the first quarter
		period(ymd(2025, 7, 1), ymd(2025, 12, 31), 3),
		period(ymd(2025, 7, 1), ymd(2026, 3, 31), 6),
	}
	check("year to date", ytd, 0, 3+2+1+3, true)
	stale := []periodFact{period(ymd(2025, 1, 1), ymd(2025, 12, 31), 7), period(ymd(2025, 10, 1), ymd(2025, 12, 31), 2)}
	check("a fiscal year", stale, 0, 7, true)
	check("nothing", nil, 0, 0, false)
}

// A fund's lines: the yield and duration of the lines that imply them,
// weighted; the shares of floating, defaulted and paid-in-kind lines. A
// line's price is its value in its own currency per 100 of face, and its
// yield to worst wins over its yield to maturity.
func TestFundLines(t *testing.T) {
	t.Parallel()
	day := ymd(2026, 6, 15)
	m1, m2 := ymd(2031, 6, 15), ymd(2028, 6, 15)
	lines := []fundLine{{weight: 40, maturity: &m1}, {weight: 20, kind: "Floating"}, {weight: 10, defaulted: true},
		{weight: 10, inKind: true}, {weight: 20, maturity: &m2}}
	five, four, three, two := 5.0, 4.0, 3.0, 2.0
	infos := []yield.LineInfo{{Yield: &five, Duration: &four}, {}, {}, {}, {Yield: &three, Duration: &two}}
	s := summarizeLines(lines, infos, day, false)
	if s.CoveredPct != 60 || s.FloatingPct != 20 || s.DefaultPct != 10 || s.PaidInKindPct != 10 {
		t.Errorf("shares %+v", s)
	}
	if math.Abs(*s.Yield-(40*5+20*3)/60.0) > 1e-12 || math.Abs(*s.Duration-(40*4+20*2)/60.0) > 1e-12 {
		t.Errorf("yield %v duration %v", *s.Yield, *s.Duration)
	}
	in := &yield.Inputs{Conventions: map[string]canonical.YieldConvention{}}
	coupon, rate := 4.0, 0.9 // euros per dollar
	l := fundLine{value: 1000, quantity: 1000, ccy: "EUR", maturity: &m1, coupon: &coupon, rate: &rate}
	info := readLine(in, l, day)
	if math.Abs(*info.Price-90) > 1e-9 || *info.Yield <= 4 {
		t.Errorf("price %v yield %v", *info.Price, *info.Yield)
	}
	ytm, ytw := 5.0, 4.5
	l.ytm, l.ytw = &ytm, &ytw
	if info := readLine(in, l, day); *info.Yield != 4.5 {
		t.Errorf("to worst %v", *info.Yield)
	}
	// A line in default, or paid in kind, implies nothing.
	l.ytm, l.ytw, l.defaulted = nil, nil, true
	if info := readLine(in, l, day); info.Yield != nil || !info.Default {
		t.Errorf("in default %+v", info)
	}
	l.defaulted, l.inKind = false, true
	if info := readLine(in, l, day); info.Yield != nil {
		t.Errorf("in kind %+v", info)
	}
}

// YieldInputs reads from gold: a holding's amount with its accrual and
// the key exposure names it by; the newest rate schedule that holds,
// every tier of it, and a ledger rate dated on or before the day over an
// undated one; the payments a holding and an account received; the
// average amount over the month ends; reported figures by their periods'
// ends, a restated one as restated. The book is invented.
func TestYieldInputsReadsGold(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	snap := ymd(2026, 6, 30).Unix()
	execEach(t, db,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		    first_seen_at, last_seen_at) VALUES ('src', 'B', 'brokerage', 'Brokerage', 'USD', 0, 0),
		    ('src', 'S', 'cash', 'Savings', 'USD', 0, 0), ('src', 'D', 'cash', 'Declared', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, name, currency,
		    first_seen_at, last_seen_at) VALUES ('src', 'QB', 'fixed_income', 'bond', NULL, 'EXAMPLE NOTE', 'USD', 0, 0),
		    ('src', 'QSTK', 'public_equity', 'stock', 'QSTK', 'Example Co', 'USD', 0, 0),
		    ('src', 'QMF', 'cash', 'fund', 'QMF', 'Example Money Fund', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value, accrued_interest) VALUES
		    ('src', `+itoa(snap)+`, 'B', 'QB', 'QB', 'fixed_income', 'bond', 'USD', 10000, 10125, 125),
		    ('src', `+itoa(snap)+`, 'B', 'QSTK', 'QSTK', 'public_equity', 'stock', 'USD', 10, 1000, NULL),
		    ('src', `+itoa(snap)+`, 'B', 'QMF', 'QMF', 'cash', 'fund', 'USD', 2000, 2000, NULL)`,
		`INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount)
		    VALUES ('src', `+itoa(snap)+`, 'S', 'USD', 'current', 5000), ('src', `+itoa(snap)+`, 'D', 'USD', 'current', 100)`,
		`INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id, kind,
		    currency, net_amount, instrument_external_id) VALUES
		    ('src', 'T1', `+itoa(ymd(2026, 3, 15).Unix())+`, 'B', 'interest', 'USD', 250, 'QB'),
		    ('src', 'T2', `+itoa(ymd(2026, 4, 30).Unix())+`, 'S', 'interest', 'USD', 12, NULL),
		    ('src', 'T3', `+itoa(ymd(2026, 5, 1).Unix())+`, 'S', 'deposit', 'USD', 700, NULL),
		    ('src', 'T4', `+itoa(ymd(2026, 2, 1).Unix())+`, 'B', 'interest', 'USD', 80, 'QOLD')`,
		`INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id, payer_signature,
		    signature_version, income_detailed, provenance, assigned_at) VALUES
		    ('src', 'T1', 'sig-note', 1, 'INCOME_INTEREST_EARNED', 'rule', 100),
		    ('src', 'T2', 'sig-bank', 1, 'INCOME_INTEREST_EARNED', 'rule', 100),
		    ('src', 'T3', 'sig-tenant', 1, 'INCOME_RENTAL', 'rule', 100),
		    ('src', 'T4', 'sig-old', 1, 'INCOME_INTEREST_EARNED', 'rule', 100)`,
		`INSERT INTO fund_characteristics (silver_source_id, fund_ref, as_of, provider, field, label, value) VALUES
		    ('ref', 'symbol:QMF', `+itoa(ymd(2026, 6, 10).Unix())+`, 'sec', 'weighted_average_maturity', '', 36.525),
		    ('ref', 'symbol:QMF', `+itoa(ymd(2026, 6, 25).Unix())+`, 'inbox', 'weighted_average_maturity_years', '', 0.2)`,
		`INSERT INTO rate_observations (silver_source_id, series_id, as_of, value) VALUES
		    ('rates', 'USD-3M', `+itoa(ymd(2026, 6, 28).Unix())+`, 4.0),
		    ('rates', 'USD-3M', `+itoa(ymd(2026, 5, 20).Unix())+`, 4.5),
		    ('rates', 'USD-1Y', `+itoa(ymd(2026, 5, 1).Unix())+`, 3.0)`,
		`INSERT INTO security_period_facts (silver_source_id, security_key, field, as_of, period_start, provider, value,
		    filed) VALUES
		    ('ref', 'symbol:QSTK', 'eps_diluted', `+itoa(ymd(2025, 6, 30).Unix())+`, '2025-04-01', 'sec', 1, '2026-07-15'),
		    ('ref', 'symbol:QSTK', 'eps_diluted', `+itoa(ymd(2025, 9, 30).Unix())+`, '2025-07-01', 'sec', 1, '2025-11-01'),
		    ('ref', 'symbol:QSTK', 'eps_diluted', `+itoa(ymd(2025, 12, 31).Unix())+`, '2025-10-01', 'sec', 1, '2026-02-01'),
		    ('ref', 'symbol:QSTK', 'eps_diluted', `+itoa(ymd(2026, 3, 31).Unix())+`, '2026-01-01', 'sec', 1, '2026-05-01'),
		    ('ref', 'symbol:QSTK', 'eps_diluted', `+itoa(ymd(2026, 6, 30).Unix())+`, '2026-04-01', 'sec', 5, '2026-08-01')`,
	)
	if _, err := SyncExposure(ctx, db, canonical.ExposureRows{
		Settings: canonical.ExposureSettings{MaxGapDays: canonical.DefaultMaxGapDays, AgedDays: 35, PartialPct: 3},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildYield(ctx, db); err != nil {
		t.Fatal(err)
	}
	// The rates as the yield pass and the ledger leave them, and the
	// curve the config names.
	execEach(t, db,
		`INSERT INTO yield_curves (currency, tenor, tenor_months, silver_source_id, series_id) VALUES
		    ('USD', '3M', 3, 'rates', 'USD-3M'), ('USD', '1Y', 12, 'rates', 'USD-1Y')`,
		`INSERT INTO account_rates (silver_source_id, account_external_id, currency, side, tier, rate_pct, valid_from,
		    origin) VALUES ('src', 'S', 'USD', 'credit', 1, 1.0, '2026-01-01', 'stated'),
		    ('src', 'S', 'USD', 'credit', 1, 1.5, '2026-05-01', 'stated'),
		    ('src', 'S', 'USD', 'credit', 2, 0.2, '2026-01-01', 'stated'),
		    ('src', 'S', 'USD', 'credit', 1, 9.0, '2026-09-01', 'stated'),
		    ('src', 'D', 'USD', 'credit', 1, 0.5, '2026-01-01', 'stated')`,
		`INSERT INTO account_rates (silver_source_id, account_external_id, currency, side, tier, rate_pct, valid_from,
		    origin, line_no) VALUES ('src', 'D', NULL, 'credit', 1, 2.0, NULL, 'declared', 1),
		    ('src', 'D', NULL, 'credit', 1, 2.5, '2026-03-01', 'declared', 2),
		    ('src', 'D', NULL, 'credit', 1, 3.0, '2026-08-01', 'declared', 3)`,
	)
	in, err := YieldInputs(ctx, db, snap+86399, "USD")
	if err != nil {
		t.Fatal(err)
	}
	var bond, stock yield.Holding
	for _, h := range in.Holdings {
		switch h.Instrument {
		case "QB":
			bond = h
		case "QSTK":
			stock = h
		}
	}
	if bond.Amount != 10125 || bond.Accrued == nil || *bond.Accrued != 125 || bond.Key != HoldingKeyPrefix+"src:QB" {
		t.Errorf("bond %+v", bond)
	}
	if stock.Key != "symbol:QSTK" || stock.SecurityID != "symbol:QSTK" {
		t.Errorf("stock keys %q %q", stock.Key, stock.SecurityID)
	}
	rates := func(acct string) []float64 {
		var out []float64
		for _, r := range in.Rates[yield.AccountKey{Source: "src", Account: acct}] {
			out = append(out, r.Pct)
		}
		return out
	}
	if got := rates("S"); len(got) != 1 || got[0] != 1.5 {
		t.Errorf("stated rates %v", got)
	}
	if got := rates("D"); len(got) != 1 || got[0] != 2.5 {
		t.Errorf("declared rates %v", got)
	}
	bk := yield.HoldingKey{Source: "src", Account: "B", PositionKey: "QB"}
	if p := in.Payments[bk]; len(p) != 1 || p[0].Amount != 250 || p[0].Type != yield.TypeCoupons {
		t.Errorf("payments %+v", p)
	}
	if p := in.AccountPayments; len(p) != 2 || p[0].Amount != 12 || p[0].Type != yield.TypeInterest ||
		p[1].Amount != 700 || p[1].Type != yield.TypeRent {
		t.Errorf("account payments %+v", p)
	}
	// Held at the instant alone: one of thirteen instants.
	if a := in.AverageAmount[bk]; math.Abs(a-10125.0/13) > 1e-9 {
		t.Errorf("average %v", a)
	}
	// The newer of two fields that fill one figure; the curve's newest
	// point of each tenor within 31 days, its benchmark the 3M.
	if f := in.Funds["symbol:QMF"]; f == nil || f.MaturityYears == nil || f.MaturityYears.Value != 0.2 {
		t.Errorf("fund %+v", f)
	}
	if c := in.Curves["USD"]; c == nil || c.Benchmark != 4 || c.BenchmarkMonths != 3 {
		t.Errorf("curve %+v", c)
	}
	// Each received line is named by its holding: a held one's, the
	// account's cash for its interest, `income:<type>:<CCY>` for its rent,
	// its own instrument for one sold.
	lines, err := IncomeReceived(ctx, db, in, ymd(2026, 1, 1).Unix(), snap+86399)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, l := range lines {
		keys = append(keys, l.Holding.Account+"/"+l.Holding.PositionKey+"="+string(l.Type))
	}
	if got := strings.Join(keys, " "); got != "B/QOLD=interest B/QB=coupons S/cash:USD=interest S/income:rent:USD=rent" {
		t.Errorf("received %s", got)
	}
	// The four quarters to the day's: one restated in a later filing, one
	// filed after the day.
	if f := in.Securities["symbol:QSTK"]; f.EPS == nil || *f.EPS != 1+1+1+5 {
		t.Errorf("eps %+v", f)
	}
}

// Interest a bond pays is a coupon, but a bill's discount is interest,
// as the projection expects it.
func TestABillsDiscountIsInterest(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	execEach(t, db,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		    first_seen_at, last_seen_at) VALUES ('src', 'B', 'brokerage', 'Brokerage', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, name, currency,
		    first_seen_at, last_seen_at) VALUES ('src', 'QB', 'fixed_income', 'bond', 'EXAMPLE NOTE', 'USD', 0, 0),
		    ('src', 'QBILL', 'fixed_income', 'bond', 'EXAMPLE DISCOUNT NOTE', 'USD', 0, 0)`,
		`INSERT INTO instrument_terms (silver_source_id, instrument_external_id, vehicle, coupon_pct, coupon_kind)
		    VALUES ('src', 'QB', 'bond', 4.5, 'fixed'), ('src', 'QBILL', 'bond', 0, 'zero')`,
		`INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id, kind,
		    currency, net_amount, instrument_external_id) VALUES
		    ('src', 'T1', `+itoa(ymd(2026, 3, 15).Unix())+`, 'B', 'interest', 'USD', 225, 'QB'),
		    ('src', 'T2', `+itoa(ymd(2026, 4, 30).Unix())+`, 'B', 'interest', 'USD', 120, 'QBILL')`,
		`INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id, payer_signature,
		    signature_version, income_detailed, provenance, assigned_at) VALUES
		    ('src', 'T1', 'sig-note', 1, 'INCOME_INTEREST_EARNED', 'rule', 100),
		    ('src', 'T2', 'sig-bill', 1, 'INCOME_INTEREST_EARNED', 'rule', 100)`,
	)
	got := map[string]yield.IncomeType{}
	if err := incomeLines(ctx, db, ymd(2026, 1, 1).Unix(), ymd(2026, 6, 30).Unix(), "USD", func(l IncomeLine) {
		got[l.Instrument] = l.Type
	}); err != nil {
		t.Fatal(err)
	}
	if got["QB"] != yield.TypeCoupons || got["QBILL"] != yield.TypeInterest {
		t.Errorf("types %v", got)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
