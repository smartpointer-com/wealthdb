package gold

import (
	"cmp"
	"context"
	"database/sql"
	"maps"
	"math"
	"regexp"
	"slices"
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
// every tier of it, the latest snapshot's on or before the day, and a
// ledger rate dated on or before the day over an undated one, which
// leaves the account's stated debit rate; a fund's newest filed rate
// sensitivity with that filing's net assets; the ledger's dividend in
// force; the payments a holding and an account received; the
// daily amounts over the year; reported figures by their periods' ends,
// a restated one as restated. The book is invented.
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
		    ('src', 'QMF', 'cash', 'fund', 'QMF', 'Example Money Fund', 'USD', 0, 0),
		    ('src', 'QOF', 'fixed_income', 'fund', 'QOF', 'Example Old Fund', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value, accrued_interest) VALUES
		    ('src', `+itoa(snap)+`, 'B', 'QB', 'QB', 'fixed_income', 'bond', 'USD', 10000, 10125, 125),
		    ('src', `+itoa(snap)+`, 'B', 'QSTK', 'QSTK', 'public_equity', 'stock', 'USD', 10, 1000, NULL),
		    ('src', `+itoa(snap)+`, 'B', 'QMF', 'QMF', 'cash', 'fund', 'USD', 2000, 2000, NULL),
		    ('src', `+itoa(snap)+`, 'B', 'QOF', 'QOF', 'fixed_income', 'fund', 'USD', 100, 1000, NULL)`,
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
		`INSERT INTO account_rates (silver_source_id, account_external_id, currency, side, tier, rate_pct, origin)
		    VALUES ('src', 'D', 'USD', 'debit', 1, 8.0, 'stated')`,
		// Three snapshots of a tiered rate: a month before the day, the day
		// before, and after it.
		`INSERT INTO account_rates (silver_source_id, account_external_id, currency, side, tier, rate_pct, up_to,
		    origin, observed_at) VALUES
		    ('src', 'U', 'USD', 'credit', 1, 0.3, NULL, 'stated', `+itoa(snap-30*86400)+`),
		    ('src', 'U', 'USD', 'credit', 1, 0.4, 50000, 'stated', `+itoa(snap-86400)+`),
		    ('src', 'U', 'USD', 'credit', 2, 0.2, NULL, 'stated', `+itoa(snap-86400)+`),
		    ('src', 'U', 'USD', 'credit', 1, 9.9, NULL, 'stated', `+itoa(snap+5*86400)+`)`,
		// Three filings of the money fund's rate sensitivity: an older one,
		// the newest before the day with its net assets, filed under a key
		// the security master reads as the fund's, and one after. The old
		// fund's one filing and its one figure lie beyond max_gap_days.
		`INSERT INTO security_aliases (alias, security_key, origin) VALUES ('figi:QQQQQMF00001', 'symbol:QMF', 'lookup')`,
		`INSERT INTO fund_risk (silver_source_id, fund_ref, as_of, currency, tenor, dv01) VALUES
		    ('ref', 'symbol:QMF', `+itoa(snap-60*86400)+`, 'USD', '3M', 99),
		    ('ref', 'figi:QQQQQMF00001', `+itoa(snap-10*86400)+`, 'USD', '3M', 5),
		    ('ref', 'figi:QQQQQMF00001', `+itoa(snap-10*86400)+`, 'USD', '1Y', 2),
		    ('ref', 'symbol:QMF', `+itoa(snap+10*86400)+`, 'USD', '3M', 77),
		    ('ref', 'symbol:QOF', `+itoa(snap-(canonical.DefaultMaxGapDays+1)*86400)+`, 'USD', '5Y', 3)`,
		`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, net_assets, line_count)
		    VALUES ('ref', 'symbol:QMF', `+itoa(snap-60*86400)+`, 'holdings', 'sec', 1, 0),
		    ('ref', 'figi:QQQQQMF00001', `+itoa(snap-10*86400)+`, 'holdings', 'sec', 1000000, 0)`,
		`INSERT INTO fund_characteristics (silver_source_id, fund_ref, as_of, provider, field, label, value) VALUES
		    ('ref', 'figi:QQQQQMF00001', `+itoa(ymd(2026, 6, 20).Unix())+`, 'sec', 'seven_day_yield', '', 4.2),
		    ('ref', 'symbol:QOF', `+itoa(snap-(canonical.DefaultMaxGapDays+1)*86400)+`, 'inbox', 'sec_yield', '', 3)`,
		// Dividends filed under a key the security master reads as the
		// stock's.
		`INSERT INTO security_aliases (alias, security_key, origin) VALUES ('figi:QQQQSTK0001', 'symbol:QSTK', 'lookup')`,
		`INSERT INTO security_period_facts (silver_source_id, security_key, field, as_of, period_start, provider, value,
		    filed) VALUES
		    ('ref', 'figi:QQQQSTK0001', 'dividends_per_share', `+itoa(ymd(2025, 9, 30).Unix())+`, '2025-07-01', 'sec', 0.25, '2025-11-01'),
		    ('ref', 'figi:QQQQSTK0001', 'dividends_per_share', `+itoa(ymd(2025, 12, 31).Unix())+`, '2025-10-01', 'sec', 0.25, '2026-02-01'),
		    ('ref', 'figi:QQQQSTK0001', 'dividends_per_share', `+itoa(ymd(2026, 3, 31).Unix())+`, '2026-01-01', 'sec', 0.25, '2026-05-01'),
		    ('ref', 'figi:QQQQSTK0001', 'dividends_per_share', `+itoa(ymd(2026, 6, 30).Unix())+`, '2026-04-01', 'sec', 0.25, '2026-08-01')`,
		// The ledger's dividends: undated, dated before the day, and after.
		`INSERT INTO yield_declared_dividends (line_no, silver_source_id, instrument_external_id, as_of, amount,
		    frequency, currency) VALUES (1, 'src', 'QSTK', NULL, 0.1, 4, NULL),
		    (2, 'src', 'QSTK', `+itoa(snap-30*86400)+`, 0.2, 2, 'CHF'),
		    (3, 'src', 'QSTK', `+itoa(snap+30*86400)+`, 0.3, 1, NULL)`,
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
	if got := rates("D"); !slices.Equal(got, []float64{2.5, 8}) {
		t.Errorf("declared rates %v", got)
	}
	if got := rates("U"); !slices.Equal(got, []float64{0.4, 0.2}) {
		t.Errorf("snapshot rates %v", got)
	}
	if f := in.Funds["symbol:QMF"]; f == nil || f.Risk["USD"]["3M"] != 5 || f.Risk["USD"]["1Y"] != 2 ||
		f.RiskAsOf == nil || f.RiskAsOf.Unix() != snap-10*86400 || f.NetAssets == nil || *f.NetAssets != 1e6 ||
		f.SevenDayYield == nil || f.SevenDayYield.Value != 4.2 {
		t.Errorf("fund risk and figures %+v", f)
	}
	if f := in.Funds["symbol:QOF"]; f == nil || f.Risk != nil || f.SECYield != nil {
		t.Errorf("a fund's figures beyond max_gap_days %+v", f)
	}
	if d := in.Dividends[yield.InstrumentKey{Source: "src", Instrument: "QSTK"}]; d.Amount != 0.2 ||
		d.Frequency != 2 || d.Currency != "CHF" {
		t.Errorf("dividend %+v", d)
	}
	bk := yield.HoldingKey{Source: "src", Account: "B", PositionKey: "QB"}
	if p := in.Payments[bk]; len(p) != 1 || p[0].Amount != 250 || p[0].Type != yield.TypeCoupons {
		t.Errorf("payments %+v", p)
	}
	if p := in.AccountPayments; len(p) != 2 || p[0].Amount != 12 || p[0].Type != yield.TypeInterest ||
		p[1].Amount != 700 || p[1].Type != yield.TypeRent {
		t.Errorf("account payments %+v", p)
	}
	// Held on the instant's day alone: one of the year's 365 days.
	if d := in.Daily[bk]; d.Days != 365 || math.Abs(d.Mean()-10125.0/365) > 1e-9 {
		t.Errorf("daily %+v", d)
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
	if f := in.Securities["symbol:QSTK"]; f.EPS == nil || *f.EPS != 1+1+1+5 || f.DPS == nil || *f.DPS != 1 {
		t.Errorf("eps and dps %+v", f)
	}
}

// Interest a bond pays is a coupon, but a bill's discount is interest,
// as the projection expects it; so is a bond's whose zero coupon only a
// ledger record states.
func TestABillsDiscountIsInterest(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	execEach(t, db,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		    first_seen_at, last_seen_at) VALUES ('src', 'B', 'brokerage', 'Brokerage', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, name, currency,
		    first_seen_at, last_seen_at) VALUES ('src', 'QB', 'fixed_income', 'bond', 'EXAMPLE NOTE', 'USD', 0, 0),
		    ('src', 'QBILL', 'fixed_income', 'bond', 'EXAMPLE DISCOUNT NOTE', 'USD', 0, 0),
		    ('src', 'QZD', 'fixed_income', 'bond', 'EXAMPLE DEEP NOTE', 'USD', 0, 0)`,
		`INSERT INTO instrument_terms (silver_source_id, instrument_external_id, vehicle, coupon_pct, coupon_kind)
		    VALUES ('src', 'QB', 'bond', 4.5, 'fixed'), ('src', 'QBILL', 'bond', 0, 'zero')`,
		`INSERT INTO yield_declared_terms (line_no, silver_source_id, instrument_external_id, coupon_pct)
		    VALUES (1, 'src', 'QZD', 0)`,
		`INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id, kind,
		    currency, net_amount, instrument_external_id) VALUES
		    ('src', 'T1', `+itoa(ymd(2026, 3, 15).Unix())+`, 'B', 'interest', 'USD', 225, 'QB'),
		    ('src', 'T2', `+itoa(ymd(2026, 4, 30).Unix())+`, 'B', 'interest', 'USD', 120, 'QBILL'),
		    ('src', 'T3', `+itoa(ymd(2026, 5, 15).Unix())+`, 'B', 'interest', 'USD', 300, 'QZD')`,
		`INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id, payer_signature,
		    signature_version, income_detailed, provenance, assigned_at) VALUES
		    ('src', 'T1', 'sig-note', 1, 'INCOME_INTEREST_EARNED', 'rule', 100),
		    ('src', 'T2', 'sig-bill', 1, 'INCOME_INTEREST_EARNED', 'rule', 100),
		    ('src', 'T3', 'sig-deep', 1, 'INCOME_INTEREST_EARNED', 'rule', 100)`,
	)
	got := map[string]yield.IncomeType{}
	if err := incomeLines(ctx, db, ymd(2026, 1, 1).Unix(), ymd(2026, 6, 30).Unix(), "USD", func(l incomeLine) {
		got[l.Instrument] = l.Type
	}); err != nil {
		t.Fatal(err)
	}
	if got["QB"] != yield.TypeCoupons || got["QBILL"] != yield.TypeInterest || got["QZD"] != yield.TypeInterest {
		t.Errorf("types %v", got)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

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
	if _, err := SyncExposure(ctx, db, canonical.ExposureRows{
		Settings: canonical.ExposureSettings{MaxGapDays: canonical.DefaultMaxGapDays, AgedDays: 35, PartialPct: 3},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
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

// Every detailed type incomeTypeSQL reads an income line by is one of
// the income taxonomy's, and the plan's other income leaves out exactly
// those (projectedIncomeDetailed), so it counts none twice and drops
// none.
func TestIncomeTypesReadTheTaxonomy(t *testing.T) {
	t.Parallel()
	read := regexp.MustCompile(`income_detailed = '(\w+)'`).FindAllStringSubmatch(incomeTypeSQL, -1)
	if len(read) == 0 {
		t.Fatal("incomeTypeSQL reads no detailed type")
	}
	var values []string
	for _, m := range read {
		if !canonical.ValidIncomeDetailed(m[1]) {
			t.Errorf("%s is no detailed income type", m[1])
		}
		values = append(values, m[1])
	}
	slices.Sort(values)
	values = slices.Compact(values)
	if want := slices.Sorted(slices.Values(projectedIncomeDetailed)); !slices.Equal(values, want) {
		t.Errorf("incomeTypeSQL reads %v; projectedIncomeDetailed lists %v", values, want)
	}
}

// YieldSecurities lists exposure's securities with the book's figures:
// a stock held only through a fund reads its line's price and its
// reported earnings over it; the reported figures are read for equities
// alone, so a bond line's stated earnings count for nothing. The book is
// invented.
func TestYieldSecurities(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	snap := ymd(2026, 6, 30).Unix()
	q := []string{
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		    first_seen_at, last_seen_at) VALUES ('src', 'B', 'brokerage', 'Brokerage', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, name, currency,
		    first_seen_at, last_seen_at) VALUES ('src', 'QMIX', 'multi_asset', 'etf', 'QMIX', 'Example Mix Fund', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value)
		    VALUES ('src', ` + itoa(snap) + `, 'B', 'QMIX', 'QMIX', 'multi_asset', 'etf', 'USD', 10, 1000)`,
		`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count, currency)
		    VALUES ('ref', 'symbol:QMIX', ` + itoa(snap) + `, 'holdings', 'testprov', 2, 'USD')`,
		`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, name, asset_type, weight,
		    currency, quantity, market_value, maturity, coupon) VALUES
		    ('ref', 'symbol:QMIX', ` + itoa(snap) + `, 'holdings', 1, 'symbol:QSTX', 'Example Stock Co', 'Equity', 60,
		     'USD', 10, 600, NULL, NULL),
		    ('ref', 'symbol:QMIX', ` + itoa(snap) + `, 'holdings', 2, 'cusip:00000000Q', 'EXAMPLE NOTE', 'Fixed Income', 40,
		     'USD', 400, 400, DATE '2030-01-15', 4)`,
	}
	for _, key := range []string{"symbol:QSTX", "cusip:00000000Q"} {
		for i, end := range []time.Time{ymd(2025, 9, 30), ymd(2025, 12, 31), ymd(2026, 3, 31), ymd(2026, 6, 30)} {
			q = append(q, `INSERT INTO security_period_facts (silver_source_id, security_key, field, as_of, period_start,
			    provider, value, unit, filed) VALUES ('ref', '`+key+`', 'eps_diluted', `+itoa(end.Unix())+`, '`+
				end.AddDate(0, -3, 1).Format(time.DateOnly)+`', 'sec', 0.75, 'USD/shares', '2026-08-0`+strconv.Itoa(i+1)+`')`)
		}
	}
	execEach(t, db, q...)
	if _, err := SyncExposure(ctx, db, canonical.ExposureRows{
		Settings: canonical.ExposureSettings{MaxGapDays: canonical.DefaultMaxGapDays, AgedDays: 35, PartialPct: 3},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	in, err := YieldInputs(ctx, db, snap+86399, "USD")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := YieldSecurities(ctx, db, yield.Compute(in))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]yield.SecurityRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	stock, note := byName["Example Stock Co"], byName["EXAMPLE NOTE"]
	if stock.AssetClass != "public_equity" || stock.Value != 600 || stock.Direct != 0 || stock.ViaFunds != 600 ||
		stock.Price == nil || *stock.Price != 60 || stock.EarningsYield == nil || math.Abs(*stock.EarningsYield-5) > 1e-9 {
		t.Errorf("the stock %+v", stock)
	}
	if note.AssetClass != "fixed_income" || note.EarningsYield != nil {
		t.Errorf("the note %+v", note)
	}
}

// A fund's debt and equity shares are of the whole fund: a composition
// that lists 60% of it leaves the rest as neither. A line its asset type
// maps nowhere is debt where it states a coupon, not where it only
// matures, as a future does. Debt of under a year is counted apart, and a
// line of type other is a derivative.
func TestALinesShareOfTheWholeFund(t *testing.T) {
	t.Parallel()
	day := ymd(2026, 6, 15)
	bill, note, expiry := ymd(2026, 12, 15), ymd(2031, 6, 15), ymd(2026, 9, 15)
	zero, four := 0.0, 4.0
	lines := []fundLine{{weight: 30, maturity: &bill, coupon: &zero}, {weight: 10, maturity: &note, coupon: &four},
		{weight: 15, class: "public_equity"}, {weight: 5, maturity: &expiry}, {weight: 0, class: "other"}}
	s := summarizeLines(lines, make([]yield.LineInfo, len(lines)), day, false)
	if s.DebtPct != 40 || s.ShortDebtPct != 30 || s.EquityPct != 15 || !s.Derivatives {
		t.Errorf("shares %+v", s)
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
	if _, err := SyncExposure(ctx, db, canonical.ExposureRows{
		Settings: canonical.ExposureSettings{MaxGapDays: canonical.DefaultMaxGapDays, AgedDays: 35, PartialPct: 3},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
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

// A broker books a Treasury's coupon as the account's interest, naming
// no instrument; its description names the bond by coupon and maturity.
// Such a line is no cash's interest. It is the coupon of the one bond the
// account holds that it names. Where the account holds none, two, or one
// whose terms state no maturity, it is the account's coupons, and so is a
// coupon whose line names a bond no longer held. A line that names its
// bond keeps it, and a deposit's interest that states a rate and an end
// is still interest. The book, the descriptions and the terms are
// invented, in a broker's shapes.
func TestACouponBookedAsInterestIsTheBonds(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	start, snap := ymd(2025, 6, 30).Unix(), ymd(2026, 6, 30).Unix()
	var txns, kinds []string
	for i, l := range []struct {
		acct, instrument, desc, amt string
		day                         time.Time
	}{{"B", "", "US TREASU NT 9.875%09/99UST NOTE DUE 09/09/99", "98.75", ymd(2026, 3, 9)},
		{"B", "000000001", "US TREASURY 9.75%08/98UST BOND DUE 08/08/98", "1", ymd(2026, 3, 10)},
		{"B", "", "US TREASURY 9.75%08/98UST BOND DUE 08/08/98", "97.50", ymd(2026, 2, 9)},
		{"B", "", "EXAMPLE BANK CD 9.25% DUE 09/09/99", "4", ymd(2026, 4, 1)},
		{"B", "", "SCHWAB1 INT 01/01-02/01", "30", ymd(2026, 6, 25)},
		{"B", "000000009", "", "11.25", ymd(2026, 5, 1)},
		{"C", "", "US TREASU NT 1.125%07/97UST NOTE DUE 07/07/97", "11.25", ymd(2026, 1, 7)},
		{"D", "", "US TREASU NT 9.875%09/99UST NOTE DUE 09/09/99", "9.87", ymd(2026, 3, 9)},
		{"E", "", "US TREASU NT 9.5%05/98UST NOTE DUE 05/05/98", "9.50", ymd(2026, 5, 5)}} {
		id := "T" + itoa(int64(i))
		instrument := "NULL"
		if l.instrument != "" {
			instrument = "'" + l.instrument + "'"
		}
		txns = append(txns, "('src', '"+id+"', "+itoa(l.day.Unix())+", '"+l.acct+"', 'interest', 'USD', "+l.amt+
			", '"+l.desc+"', "+instrument+")")
		kinds = append(kinds, "('src', '"+id+"', 'sig-"+id+"', 1, 'INCOME_INTEREST_EARNED', 'rule', 100)")
	}
	var positions []string
	for _, at := range []int64{start, snap} {
		for _, p := range [][2]string{{"B", "000000001"}, {"C", "000000003"}, {"C", "000000004"}, {"E", "000000005"}} {
			positions = append(positions, "('src', "+itoa(at)+", '"+p[0]+"', '"+p[1]+"', '"+p[1]+
				"', 'fixed_income', 'bond', 'USD', 1000, 990)")
		}
	}
	execEach(t, db,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		    first_seen_at, last_seen_at) VALUES ('src', 'B', 'brokerage', 'Brokerage', 'USD', 0, 0),
		    ('src', 'C', 'brokerage', 'Brokerage C', 'USD', 0, 0), ('src', 'D', 'brokerage', 'Brokerage D', 'USD', 0, 0),
		    ('src', 'E', 'brokerage', 'Brokerage E', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, name, currency,
		    first_seen_at, last_seen_at) VALUES
		    ('src', '000000001', 'fixed_income', 'bond', 'US TREASURY NOTE 9.875% 09/09/99', 'USD', 0, 0),
		    ('src', '000000003', 'fixed_income', 'bond', 'US TREASURY NOTE 1.125% 07/07/97', 'USD', 0, 0),
		    ('src', '000000004', 'fixed_income', 'bond', 'US TREASURY NOTE 1.125% 07/07/97', 'USD', 0, 0),
		    ('src', '000000005', 'fixed_income', 'bond', 'US TREASURY NOTE 9.5%', 'USD', 0, 0),
		    ('src', '000000009', 'fixed_income', 'bond', 'US TREASURY NOTE 1.375% 03/03/97', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value) VALUES `+strings.Join(positions, ", "),
		`INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount)
		    VALUES ('src', `+itoa(start)+`, 'B', 'USD', 'current', 2000), ('src', `+itoa(snap)+`, 'B', 'USD', 'current', 2000)`,
		`INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id, kind,
		    currency, net_amount, description, instrument_external_id) VALUES `+strings.Join(txns, ", "),
		`INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id, payer_signature,
		    signature_version, income_detailed, provenance, assigned_at) VALUES `+strings.Join(kinds, ", "))
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
	in, err := YieldInputs(ctx, db, snap+86399, "USD")
	if err != nil {
		t.Fatal(err)
	}
	ac := func(acct string) yield.Received {
		return in.AccountIncome[yield.AccountCurrency{AccountKey: yield.AccountKey{Source: "src", Account: acct},
			Currency: "USD"}]
	}
	bond := yield.HoldingKey{Source: "src", Account: "B", PositionKey: "000000001"}
	if in.Income[bond][yield.TypeCoupons] != 99.75 || len(in.Payments[bond]) != 2 ||
		ac("B")[yield.TypeInterest] != 34 || ac("B")[yield.TypeCoupons] != 97.5+11.25 ||
		ac("C")[yield.TypeCoupons] != 11.25 || ac("D")[yield.TypeCoupons] != 9.87 || ac("E")[yield.TypeCoupons] != 9.5 {
		t.Errorf("the bond's %v %v; the accounts' %v %v %v %v", in.Income[bond], in.Payments[bond], ac("B"), ac("C"),
			ac("D"), ac("E"))
	}
	if p := in.AccountPayments; len(p) != 2 || p[0].Type != yield.TypeInterest || p[1].Type != yield.TypeInterest {
		t.Errorf("account payments %+v", p)
	}
	// The cash's realized yield is its own interest's and the deposit's:
	// 34 on 2,000.
	b := yield.Compute(in)
	cash := position(b, yield.HoldingKey{Source: "src", Account: "B", PositionKey: "cash:USD"})
	if cash.Realized == nil || math.Abs(*cash.Realized-1.7) > 1e-9 {
		t.Errorf("the cash's realized yield %v", cash.Realized)
	}
	lines, err := IncomeReceived(ctx, db, in, ymd(2026, 1, 1).Unix(), snap+86399)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, l := range lines {
		keys = append(keys, l.Holding.Account+"/"+l.Holding.PositionKey+"="+string(l.Type))
	}
	if got, want := strings.Join(keys, " "), "C/income:coupons:USD=coupons B/income:coupons:USD=coupons "+
		"B/000000001=coupons D/income:coupons:USD=coupons B/000000001=coupons B/cash:USD=interest "+
		"B/income:coupons:USD=coupons E/income:coupons:USD=coupons B/cash:USD=interest"; got != want {
		t.Errorf("received\n%s\nwant\n%s", got, want)
	}
}

// bookInstrument is an instrument of an invented book: a stock, by the
// ids a source gives it ("" for none).
type bookInstrument struct{ src, iid, isin, cusip, symbol string }

// bookLine is a position of an invented book, keyed by its instrument:
// from each segment's day on, its quantity and value at every month end
// (none at zero), in dollars.
type bookLine struct {
	src, acct, iid string
	segs           []bookSeg
}

type bookSeg struct {
	from       time.Time
	qty, value float64
}

// bookTxn is a transaction of an invented book: a dividend, a buy or a
// sell.
type bookTxn struct {
	src, acct, iid string
	day            time.Time
	kind           string
	amount, qty    float64
}

// dividendBook is an invented book for the dividend tests: its
// instruments, positions and transactions; the portfolio of each account
// that has one, by account; the currency of each instrument quoted in
// another than the dollar, by instrument; and statements run after the
// passes, on what the inputs read as it stands.
type dividendBook struct {
	instruments []bookInstrument
	lines       []bookLine
	txns        []bookTxn
	portfolios  map[string]string
	currencies  map[string]string
	after       []string
}

// inputs writes the book into a migrated gold: each line's positions at
// the month ends from 30 June 2025 to 30 June 2026, at noon; the
// transactions, a dividend's as one; the instruments, listed in their
// currency; and a brokerage account for each account named. Amounts are
// in dollars, or in their instrument's currency. It runs the exposure
// and yield passes, then after, and reads the inputs at the last second
// of 30 June 2026.
func (bk dividendBook) inputs(t *testing.T) (*yield.Inputs, *sql.DB, context.Context) {
	t.Helper()
	db, ctx := openMigrated(t)
	str := func(s string) string {
		if s == "" {
			return "NULL"
		}
		return "'" + s + "'"
	}
	ccy := func(iid string) string { return cmp.Or(bk.currencies[iid], "USD") }
	var insts, positions, rows, kinds []string
	accounts := map[[2]string]bool{}
	for _, i := range bk.instruments {
		insts = append(insts, "('"+i.src+"', '"+i.iid+"', 'public_equity', 'stock', "+str(i.isin)+", "+str(i.cusip)+
			", "+str(i.symbol)+", 'Example "+i.iid+"', '"+ccy(i.iid)+"', 0, 0)")
	}
	for _, l := range bk.lines {
		accounts[[2]string{l.src, l.acct}] = true
		for m := 0; m <= 12; m++ {
			d := ymd(2025, 7+m, 1).AddDate(0, 0, -1)
			var s bookSeg
			for _, x := range l.segs {
				if !x.from.After(d) {
					s = x
				}
			}
			if s.qty != 0 {
				positions = append(positions, "('"+l.src+"', "+itoa(d.Unix()+43200)+", '"+l.acct+"', '"+l.iid+"', '"+
					l.iid+"', 'public_equity', 'stock', '"+ccy(l.iid)+"', "+ftoa(s.qty)+", "+ftoa(s.value)+")")
			}
		}
	}
	for i, x := range bk.txns {
		accounts[[2]string{x.src, x.acct}] = true
		id := "T" + strconv.Itoa(i)
		rows = append(rows, "('"+x.src+"', '"+id+"', "+itoa(x.day.Unix()+36000)+", '"+x.acct+"', '"+x.kind+
			"', '"+ccy(x.iid)+"', "+ftoa(x.amount)+", '"+x.iid+"', "+ftoa(x.qty)+")")
		if x.kind == "dividend" {
			kinds = append(kinds, "('"+x.src+"', '"+id+"', 'sig-"+x.iid+"', 1, 'INCOME_DIVIDENDS', 'rule', 100)")
		}
	}
	var accts []string
	for _, k := range slices.SortedFunc(maps.Keys(accounts), func(a, b [2]string) int {
		return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1]))
	}) {
		accts = append(accts, "('"+k[0]+"', '"+k[1]+"', 'brokerage', '"+k[1]+"', 'USD', 0, 0, "+
			str(bk.portfolios[k[1]])+")")
	}
	execEach(t, db,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		    first_seen_at, last_seen_at, portfolio_external_id) VALUES `+strings.Join(accts, ", "),
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, isin, cusip, symbol,
		    name, currency, first_seen_at, last_seen_at) VALUES `+strings.Join(insts, ", "),
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value) VALUES `+strings.Join(positions, ", "),
		`INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id, kind,
		    currency, net_amount, instrument_external_id, quantity) VALUES `+strings.Join(rows, ", "),
		`INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id, payer_signature,
		    signature_version, income_detailed, provenance, assigned_at) VALUES `+strings.Join(kinds, ", "))
	rebuildForYield(t, db)
	execEach(t, db, bk.after...)
	// The calendar rows an after statement adds teach their gaps, as the
	// yield pass's last step does.
	rebuildGaps(t, db)
	in, err := YieldInputs(ctx, db, ymd(2026, 6, 30).Unix()+86399, "USD")
	if err != nil {
		t.Fatal(err)
	}
	return in, db, ctx
}

// A source may key one stock by its CUSIP and by its ticker
// (instrument_aliases): the lines and the holdings meet under either.
// Each stock pays 0.5 a share every quarter and is priced at 50. The book
// is invented:
//
//   - B1 holds QCSP under its CUSIP id, and is paid under its ticker;
//   - B1 holds QTKR under its ticker, and is paid under its CUSIP id;
//   - H holds QSLD under its ticker, and S held 300 under its CUSIP id
//     until it sold them on 1 April, paid under that id: S's lines are its
//     sold position's, under the ticker, which pools with H's holding.
func TestALineMeetsItsHoldingUnderEitherID(t *testing.T) {
	t.Parallel()
	early := ymd(2025, 1, 1)
	held := []bookSeg{{early, 100, 5000}}
	var txns []bookTxn
	for i, d := range []time.Time{ymd(2025, 9, 10), ymd(2025, 12, 10), ymd(2026, 3, 10), ymd(2026, 6, 10)} {
		txns = append(txns, bookTxn{"src", "B1", "QCSP", d, "dividend", 50, 0},
			bookTxn{"src", "B1", "000000AC2", d, "dividend", 50, 0}, bookTxn{"src", "H", "QSLD", d, "dividend", 50, 0})
		if i < 3 {
			txns = append(txns, bookTxn{"src", "S", "000000AD3", d, "dividend", 150, 0})
		}
	}
	txns = append(txns, bookTxn{"src", "S", "000000AD3", ymd(2026, 4, 1), "sell", 15000, -300})
	in, db, ctx := dividendBook{instruments: []bookInstrument{
		{"src", "000000AB1", "", "000000AB1", "QCSP"}, {"src", "QCSP", "", "", "QCSP"},
		{"src", "QTKR", "", "", "QTKR"}, {"src", "000000AC2", "", "000000AC2", "QTKR"},
		{"src", "QSLD", "", "", "QSLD"}, {"src", "000000AD3", "", "000000AD3", "QSLD"},
	}, lines: []bookLine{
		{"src", "B1", "000000AB1", held}, {"src", "B1", "QTKR", held}, {"src", "H", "QSLD", held},
		{"src", "S", "000000AD3", []bookSeg{{early, 300, 15000}, {ymd(2026, 4, 30), 0, 0}}},
	}, txns: txns}.inputs(t)
	b := yield.Compute(in)
	key := func(acct, id string) yield.HoldingKey {
		return yield.HoldingKey{Source: "src", Account: acct, PositionKey: id}
	}
	for _, k := range []yield.HoldingKey{key("B1", "000000AB1"), key("B1", "QTKR")} {
		ps := in.Payments[k]
		if len(ps) != 4 || ps[3].Quantity == nil || *ps[3].Quantity != 100 || in.Income[k].Total() != 200 {
			t.Errorf("%s: payments %+v, income %v", k.PositionKey, ps, in.Income[k])
		}
		p := position(b, k)
		if p.DividendYield == nil || math.Abs(*p.DividendYield-4) > 1e-9 || p.Realized == nil ||
			math.Abs(*p.Realized-4) > 1e-9 {
			t.Errorf("%s: dividend yield %s, realized %s", k.PositionKey, fmtFloatPtr(p.DividendYield),
				fmtFloatPtr(p.Realized))
		}
	}
	if got := in.Held[yield.InstrumentKey{Source: "src", Instrument: "QSLD"}]; !slices.Equal(got, []string{"H", "S"}) {
		t.Errorf("QSLD held by %v", got)
	}
	sold := key("S", "QSLD")
	if len(in.Payments[sold]) != 3 || in.Income[sold].Total() != 450 {
		t.Errorf("S's sold position: payments %+v, income %v", in.Payments[sold], in.Income[sold])
	}
	// S held 15,000 dollars of it to the snapshot of 30 April, which ends
	// its observation.
	mean := (5000*365 + 15000*303.0) / 365
	p := position(b, key("H", "QSLD"))
	if p.DividendYield == nil || math.Abs(*p.DividendYield-4) > 1e-9 || p.Realized == nil ||
		math.Abs(*p.Realized-650/mean*100) > 1e-9 {
		t.Errorf("QSLD: dividend yield %s, realized %s, want %v", fmtFloatPtr(p.DividendYield),
			fmtFloatPtr(p.Realized), 650/mean*100)
	}
	got, err := IncomeReceived(ctx, db, in, ymd(2025, 7, 1).Unix(), ymd(2026, 6, 30).Unix()+86399)
	if err != nil {
		t.Fatal(err)
	}
	named := map[yield.HoldingKey]int{}
	for _, l := range got {
		named[l.Holding]++
	}
	if named[sold] != 3 || named[key("B1", "000000AB1")] != 4 || named[key("B1", "QTKR")] != 4 {
		t.Errorf("received lines %v", named)
	}
}

// A security moved from one source to another pools what the first paid
// and held: its sold positions there, under their own instrument. The
// book is invented: A at source old held 100 of an ISIN until it sold
// them on 20 May, paid 0.5 a share each quarter to 15 May; N at source
// new holds 100 of the same ISIN from 31 May, priced at 50, and has been
// paid nothing yet. N's holding pays 0.5 a share four times a year, and
// its realized yield is what A received over the amounts A and N held:
// some source held it every day.
func TestASecurityMovedBetweenSources(t *testing.T) {
	t.Parallel()
	early := ymd(2025, 1, 1)
	var txns []bookTxn
	for _, d := range []time.Time{ymd(2025, 8, 15), ymd(2025, 11, 15), ymd(2026, 2, 15), ymd(2026, 5, 15)} {
		txns = append(txns, bookTxn{"old", "A", "XMOV", d, "dividend", 50, 0})
	}
	txns = append(txns, bookTxn{"old", "A", "XMOV", ymd(2026, 5, 20), "sell", 5000, -100})
	in, _, _ := dividendBook{instruments: []bookInstrument{
		{"old", "XMOV", "XX0000000001", "", "XMOV"}, {"old", "QPEER", "", "", "QPEER"},
		{"new", "YMOV", "XX0000000001", "", "YMOV"},
	}, lines: []bookLine{
		{"old", "A", "XMOV", []bookSeg{{early, 100, 5000}, {ymd(2026, 5, 31), 0, 0}}},
		{"old", "A2", "QPEER", []bookSeg{{early, 10, 100}}},
		{"new", "N", "YMOV", []bookSeg{{ymd(2026, 5, 31), 100, 5000}}},
	}, txns: txns}.inputs(t)
	moved := yield.InstrumentKey{Source: "old", Instrument: "XMOV"}
	if s := in.HeldSecurity[moved]; s != "isin:XX0000000001" {
		t.Errorf("XMOV's security %q", s)
	}
	if d := in.FormerDaily[moved]; len(d) != 1 || d[0].Currency != "USD" || len(d[0].Spans) != 1 ||
		d[0].Spans[0].From != 0 {
		t.Errorf("XMOV's amounts %+v", d)
	}
	p := position(yield.Compute(in), yield.HoldingKey{Source: "new", Account: "N", PositionKey: "YMOV"})
	if p.DividendYield == nil || math.Abs(*p.DividendYield-0.5*4/50*100) > 1e-9 || !p.Tags[yield.TagIndicated] {
		t.Errorf("dividend yield %s, tags %v", fmtFloatPtr(p.DividendYield), p.Tags)
	}
	if p.Realized == nil || math.Abs(*p.Realized-200.0/5000*100) > 1e-9 || p.Tags[yield.TagPartial] {
		t.Errorf("realized %s, tags %v", fmtFloatPtr(p.Realized), p.Tags)
	}
}

// The config's holding aliases fold too, as the gains reports read them
// (holdingAliasJoins): an account's own alias, else its source's, before
// the instrument aliases. Each stock pays 0.5 a share every quarter and is
// priced at 50, its quantity read under the alias too. The book is
// invented:
//
//   - B1 holds QHAL and is paid under the CUSIP id 000000AE4, which its
//     source also keys by its ticker QSHD: the source's holding alias to
//     QHAL wins over the ticker.
//   - DEPOT keyed its holding by an overview key, XOVR, until January,
//     and by QDEP, a share of the United States, since. Its alias reads
//     XOVR as QDEP for DEPOT alone, over the source's alias of XOVR to
//     QOTH. CASH, beside it in portfolio BANK, is paid under XOVR: the
//     lines are DEPOT's holding's, on what DEPOT held under both keys on
//     their record days, and its realized yield pools the amounts under
//     both.
//   - H's statement valued its holding in francs under ASTM until
//     January; its feed values it in dollars under QFED since. The
//     statement's amounts convert at the franc's rate, not the holding's.
//     M's one position key, QMIX, reads the same way in both currencies.
//     No instrument states a listing currency: each reads the dollars its
//     newest snapshot values it in, though ASTM sorts before QFED.
func TestAHoldingAliasFolds(t *testing.T) {
	t.Parallel()
	early, feb := ymd(2025, 1, 1), ymd(2026, 1, 31)
	var txns []bookTxn
	for _, d := range []time.Time{ymd(2025, 9, 10), ymd(2025, 12, 10), ymd(2026, 3, 10), ymd(2026, 6, 10)} {
		txns = append(txns, bookTxn{"src", "B1", "000000AE4", d, "dividend", 50, 0},
			bookTxn{"src", "CASH", "XOVR", d, "dividend", 50, 0}, bookTxn{"src", "H", "QFED", d, "dividend", 50, 0},
			bookTxn{"src", "M", "QMIX", d, "dividend", 50, 0})
	}
	in, _, _ := dividendBook{instruments: []bookInstrument{
		{"src", "000000AE4", "", "000000AE4", "QSHD"}, {"src", "QSHD", "", "", "QSHD"}, {"src", "QHAL", "", "", "QHAL"},
		{"src", "XOVR", "", "", ""}, {"src", "QDEP", "US0000000008", "", "QDEP"}, {"src", "ASTM", "", "", ""},
		{"src", "QFED", "", "", "QFED"}, {"src", "QMIX", "", "", "QMIX"},
	}, lines: []bookLine{
		{"src", "B1", "QHAL", []bookSeg{{early, 100, 5000}}},
		{"src", "DEPOT", "XOVR", []bookSeg{{early, 100, 5000}, {feb, 0, 0}}},
		{"src", "DEPOT", "QDEP", []bookSeg{{feb, 100, 5000}}},
		{"src", "H", "ASTM", []bookSeg{{early, 100, 4000}, {feb, 0, 0}}},
		{"src", "H", "QFED", []bookSeg{{feb, 100, 5000}}},
		{"src", "M", "QMIX", []bookSeg{{early, 100, 4000}, {feb, 100, 5000}}},
	}, txns: txns, portfolios: map[string]string{"DEPOT": "BANK", "CASH": "BANK"}, after: []string{
		`INSERT INTO holding_aliases (silver_source_id, account_external_id, from_key, to_key) VALUES
		    ('src', '', '000000AE4', 'QHAL'), ('src', 'DEPOT', 'XOVR', 'QDEP'), ('src', '', 'XOVR', 'QOTH'),
		    ('src', '', 'ASTM', 'QFED')`,
		`UPDATE positions SET currency = 'CHF' WHERE position_key = 'ASTM'
		    OR (position_key = 'QMIX' AND snapshot_at < ` + itoa(feb.Unix()) + `)`,
		`UPDATE instruments SET currency = NULL WHERE instrument_external_id IN ('ASTM', 'QFED', 'QMIX')`,
		`INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
		    VALUES ('fx', 0, 'USD', 'CHF', 1.25)`,
	}}.inputs(t)
	for k, want := range map[yield.AliasKey]string{
		{Source: "src", ID: "000000AE4"}: "QHAL", {Source: "src", Account: "DEPOT", ID: "XOVR"}: "QDEP",
		{Source: "src", ID: "XOVR"}: "QOTH", {Source: "src", ID: "ASTM"}: "QFED",
	} {
		if got := in.Aliases[k]; got != want {
			t.Errorf("%v reads as %q, want %q", k, got, want)
		}
	}
	if f := in.FormerDaily[yield.InstrumentKey{Source: "src", Instrument: "QFED"}]; len(f) != 1 ||
		f[0].Currency != "CHF" || in.FXRates["CHF"] != 1.25 {
		t.Errorf("QFED's former amounts %+v, the franc at %v", f, in.FXRates["CHF"])
	}
	b := yield.Compute(in)
	for _, k := range []yield.HoldingKey{{Source: "src", Account: "B1", PositionKey: "QHAL"},
		{Source: "src", Account: "DEPOT", PositionKey: "QDEP"}, {Source: "src", Account: "H", PositionKey: "QFED"},
		{Source: "src", Account: "M", PositionKey: "QMIX"}} {
		ps := in.Payments[k]
		if len(ps) != 4 || in.Income[k].Total() != 200 {
			t.Errorf("%s: payments %+v, income %v", k.PositionKey, ps, in.Income[k])
			continue
		}
		// Each is a share quoted in dollars: its record days read what its
		// account held under every key, QDEP's what DEPOT held.
		for _, x := range ps {
			if x.Quantity == nil || *x.Quantity != 100 || x.LagDays != yield.RecordLagUSShare.Days || x.Record == nil ||
				*x.Record != 100 || x.Before != nil {
				t.Errorf("%s on %s: quantity %s, record %s, before %s, lag %d", k.PositionKey,
					x.Day.Format(time.DateOnly), fmtFloatPtr(x.Quantity), fmtFloatPtr(x.Record), fmtFloatPtr(x.Before),
					x.LagDays)
			}
		}
		p := position(b, k)
		if p.DividendYield == nil || math.Abs(*p.DividendYield-4) > 1e-9 || p.Realized == nil ||
			math.Abs(*p.Realized-4) > 1e-9 || p.Tags[yield.TagPartial] {
			t.Errorf("%s: dividend yield %s, realized %s, tags %v", k.PositionKey, fmtFloatPtr(p.DividendYield),
				fmtFloatPtr(p.Realized), p.Tags)
		}
	}
}

// A holding's daily amounts in another currency than its own convert at
// the instant's rates, its own as they are; where no rate converts them
// they count nothing. The holding and its amounts are invented: valued in
// francs for its first hundred days, in kronor, which no rate converts,
// for the next hundred, and in dollars since.
func TestADailyAmountNoRateConverts(t *testing.T) {
	t.Parallel()
	k := yield.HoldingKey{Source: "src", Account: "A", PositionKey: "QSTK"}
	in := &yield.Inputs{Currency: "USD", FXRates: map[string]float64{"CHF": 1.25},
		Holdings: []yield.Holding{{HoldingKey: k, Currency: "USD"}}, Daily: map[yield.HoldingKey]yield.Daily{},
		FormerDaily: map[yield.InstrumentKey][]yield.Daily{}}
	b := &yieldBase{amounts: map[amountKey]*yield.Daily{
		{k, "CHF"}: {Days: 365, Currency: "CHF", Spans: []yield.Span{{From: 0, To: 100, Amount: 4000}}},
		{k, "SEK"}: {Days: 365, Currency: "SEK", Spans: []yield.Span{{From: 100, To: 200, Amount: 50000}}},
		{k, "USD"}: {Days: 365, Currency: "USD", Spans: []yield.Span{{From: 200, To: 365, Amount: 5000}}},
	}}
	b.daily(in)
	want := []yield.Span{{From: 0, To: 100, Amount: 5000}, {From: 200, To: 365, Amount: 5000}}
	if d := in.Daily[k]; d.Days != 365 || d.Currency != "USD" || !slices.Equal(d.Spans, want) {
		t.Errorf("daily %+v, want spans %+v", d, want)
	}
}

// A bank may book a dividend in another portfolio than the custody
// account's. A line of an account whose portfolio never held the
// instrument, where one account of its source alone did, is that
// account's sold position, and pools with the security's holdings. The
// book is invented: CUST, in portfolio P1 at the bank, held 100 of an
// ISIN until it sold them on 15 March; its dividends went to CSH, in P2.
// BRK at the broker holds 100 of the same ISIN, priced at 50. Each pays
// 0.5 a share a quarter: BRK's realized yield is what both received over
// the amounts both held.
func TestALineBookedInAnotherPortfolio(t *testing.T) {
	t.Parallel()
	early := ymd(2025, 1, 1)
	var txns []bookTxn
	for i, d := range []time.Time{ymd(2025, 9, 10), ymd(2025, 12, 10), ymd(2026, 3, 10), ymd(2026, 6, 10)} {
		txns = append(txns, bookTxn{"broker", "BRK", "YBRK", d, "dividend", 50, 0})
		if i < 3 {
			txns = append(txns, bookTxn{"bank", "CSH", "XBNK", d, "dividend", 50, 0})
		}
	}
	txns = append(txns, bookTxn{"bank", "CUST", "XBNK", ymd(2026, 3, 15), "sell", 5000, -100})
	in, db, ctx := dividendBook{instruments: []bookInstrument{
		{"bank", "XBNK", "XX0000000002", "", "XBNK"}, {"bank", "QPEER", "", "", "QPEER"},
		{"broker", "YBRK", "XX0000000002", "", "YBRK"},
	}, lines: []bookLine{
		{"bank", "CUST", "XBNK", []bookSeg{{early, 100, 5000}, {ymd(2026, 3, 31), 0, 0}}},
		{"bank", "PEER", "QPEER", []bookSeg{{early, 10, 100}}},
		{"broker", "BRK", "YBRK", []bookSeg{{early, 100, 5000}}},
	}, txns: txns, portfolios: map[string]string{"CUST": "P1", "PEER": "P1", "CSH": "P2"}}.inputs(t)
	sold := yield.HoldingKey{Source: "bank", Account: "CUST", PositionKey: "XBNK"}
	if k, ok := in.Sold("bank", "CSH", "XBNK"); !ok || k != sold {
		t.Errorf("CSH's line: %v %v", k, ok)
	}
	if ps := in.Payments[sold]; len(ps) != 3 || ps[0].Quantity == nil || *ps[0].Quantity != 100 ||
		in.Income[sold].Total() != 150 {
		t.Errorf("CUST's sold position: payments %+v, income %v", ps, in.Income[sold])
	}
	custody := ymd(2026, 3, 31).Sub(ymd(2025, 7, 1)).Hours() / 24
	mean := (5000*365 + 5000*custody) / 365
	p := position(yield.Compute(in), yield.HoldingKey{Source: "broker", Account: "BRK", PositionKey: "YBRK"})
	if p.Realized == nil || math.Abs(*p.Realized-350/mean*100) > 1e-9 {
		t.Errorf("realized %s, want %v", fmtFloatPtr(p.Realized), 350/mean*100)
	}
	got, err := IncomeReceived(ctx, db, in, ymd(2025, 7, 1).Unix(), ymd(2026, 6, 30).Unix()+86399)
	if err != nil {
		t.Fatal(err)
	}
	named := 0
	for _, l := range got {
		if l.Holding == sold {
			named++
		}
	}
	if named != 3 {
		t.Errorf("%d received lines name CUST's sold position: %+v", named, got)
	}
}
