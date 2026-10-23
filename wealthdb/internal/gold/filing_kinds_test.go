package gold

import (
	"context"
	"database/sql"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// filingLine is one invented line of a fund's composition.
type filingLine struct {
	key, isin, cusip, name, assetType string
	weight                            float64
	maturity                          string
	coupon                            any
	kind                              string
	isDefault, inArrears              any
}

// addComposition inserts a fund's composition of source kind kind on
// day, with its lines.
func addComposition(t *testing.T, db *sql.DB, src, ref string, day int64, provider, kind, name string,
	lines ...filingLine) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider,
	    source_kind, fund_name, line_count, currency) VALUES (?, ?, ?, 'holdings', ?, ?, NULLIF(?, ''), ?, 'USD')`,
		src, ref, day, provider, kind, name, len(lines)); err != nil {
		t.Fatal(err)
	}
	for n, l := range lines {
		if _, err := db.ExecContext(ctx, `INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no,
		    security_key, isin, cusip, name, asset_type, weight, currency, maturity, coupon, coupon_kind, is_default,
		    in_arrears) VALUES (?, ?, ?, 'holdings', ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, 'USD',
		    CAST(NULLIF(?, '') AS DATE), ?, NULLIF(?, ''), ?, ?)`, src, ref, day, n+1, l.key, l.isin, l.cusip, l.name,
			l.assetType, l.weight, l.maturity, l.coupon, l.kind, l.isDefault, l.inArrears); err != nil {
			t.Fatal(err)
		}
	}
}

// rebuildForYield stamps the exposure settings and runs the exposure and
// yield passes.
func rebuildForYield(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
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
}

// A line of the composition in use that states no coupon kind, default
// flag or arrears flag takes them from its fund's filing at the instant
// (the latest on or before it), matched by the line's security key read
// through the aliases. A stated value never changes, and a line the
// filing does not hold keeps none. The coupons view and the yield
// report's lines read the same kinds; the report reads the filing's own
// floating share and name too. The fund and its lines are invented.
func TestALineTakesItsFilingsKinds(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	snap := ymd(2026, 6, 30).Unix()
	execEach(t, db,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		    first_seen_at, last_seen_at) VALUES ('test-src', 'A', 'brokerage', 'Brokerage', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, name,
		    currency, first_seen_at, last_seen_at)
		    VALUES ('test-src', 'QFRF', 'fixed_income', 'etf', 'QFRF', 'Example Note Fund', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value)
		    VALUES ('test-src', `+itoa(snap)+`, 'A', 'QFRF', 'QFRF', 'fixed_income', 'etf', 'USD', 100, 5000)`)
	note := func(key, isin, cusip string, kind string, isDefault, inArrears any) filingLine {
		return filingLine{key: key, isin: isin, cusip: cusip, name: "EXAMPLE NOTE 07/31/2027",
			assetType: "Fixed Income", weight: 20, maturity: "2027-07-31", coupon: 4.25, kind: kind,
			isDefault: isDefault, inArrears: inArrears}
	}
	addComposition(t, db, "ref", "symbol:QFRF", snap, "testprov", "daily_file", "",
		note("isin:US00000000F1", "US00000000F1", "", "", nil, nil),
		note("isin:US00000000F2", "US00000000F2", "", "Fixed", nil, nil),
		note("isin:US00000000F3", "US00000000F3", "", "", nil, nil),
		note("cusip:00000000F", "", "00000000F", "", nil, nil),
		note("isin:US00000000F5", "US00000000F5", "", "", false, nil))
	filed := func(key, isin, cusip, kind string, isDefault, inArrears any) filingLine {
		l := note(key, isin, cusip, kind, isDefault, inArrears)
		l.assetType, l.weight = "DBT", 25
		return l
	}
	addComposition(t, db, "ref", "symbol:QFRF", snap-90*86400, "sec_nport", "filing", "Example Floating Rate Note Fund",
		filed("isin:US00000000F1", "US00000000F1", "", "Floating", false, false),
		filed("isin:US00000000F2", "US00000000F2", "", "Floating", false, false),
		filed("isin:US00000000F4", "US00000000F4", "00000000F", "Fixed", false, true),
		filed("isin:US00000000F5", "US00000000F5", "", "Variable", true, false))
	// An older filing, and one after the instant, say otherwise.
	addComposition(t, db, "ref", "symbol:QFRF", snap-180*86400, "sec_nport", "filing", "",
		filed("isin:US00000000F1", "US00000000F1", "", "Fixed", false, false),
		filed("isin:US00000000F3", "US00000000F3", "", "Floating", false, false))
	addComposition(t, db, "ref", "symbol:QFRF", snap+30*86400, "sec_nport", "filing", "",
		filed("isin:US00000000F3", "US00000000F3", "", "Floating", false, false))
	rebuildForYield(t, db)
	at := snap + 86399
	got := scanStrings(t, db, `SELECT key || ' ' || COALESCE(line_coupon_kind, '-') || ' ' || coupon_bucket
	    FROM exposure_yield_leaves(?, 'USD') WHERE depth > 0 AND kind = 'line' ORDER BY key`, at)
	want := "isin:US00000000F1 Floating floating, isin:US00000000F2 Fixed fixed, isin:US00000000F3 - fixed, " +
		"isin:US00000000F4 Fixed fixed, isin:US00000000F5 Variable floating"
	if strings.Join(got, ", ") != want {
		t.Errorf("lines\n%v\nwant\n%v", strings.Join(got, ", "), want)
	}
	in, err := YieldInputs(ctx, db, at, "USD")
	if err != nil {
		t.Fatal(err)
	}
	// Two lines in five float; the one the filing says is in arrears is
	// in default, the one whose own default flag says no stays out of it.
	// Three of the filing's four lines float.
	f := in.Funds["symbol:QFRF"]
	if f == nil || f.Lines == nil || f.Lines.FloatingPct != 40 || f.Lines.DefaultPct != 20 ||
		f.FilingFloatingPct == nil || *f.FilingFloatingPct != 75 || f.FilingName != "Example Floating Rate Note Fund" {
		t.Errorf("fund %+v lines %+v", f, f.Lines)
	}
}

// A floating rate fund through the reports (docs/YIELD.md §3, §4;
// docs/INCOME.md §12; docs/EXPOSURE.md §7). Its source names it by its
// ticker and by its CUSIP, an alias pair. Account B held it under the
// CUSIP first and under the ticker at the instant, and its payments name
// the CUSIP throughout. Account A bought it under the ticker a month
// before the instant. The issuer's daily file is the composition in use
// and states no coupon kind; the fund's filing, a quarter older, states
// Floating for its notes. It has paid every month since B bought it, five
// months before the instant.
//
//   - B alone paid and held it before June, under the CUSIP. Its
//     payments meet its holding, and its days count as the fund's, by
//     the id the matching folds the CUSIP into. So five pay months in
//     five months held read monthly; A's one payment alone would read
//     once a year, and the fund would float at its trailing yield, which
//     the fixture sets to the same 4.8%. The positions rows' basis,
//     indicated, tells the two apart.
//   - Its notes read floating in the coupons view, and the fund is
//     floating: one percent higher rates add one percent of its value to
//     its income, and the forward path moves it.
//   - It pays monthly: its next twelve months are its indicated
//     distributions' yield, 4.8%, times its value.
//   - Its positions rows show what it received and is expected to pay,
//     under either id, and sum to the projection per type.
//
// Every holding, figure and identifier is invented.
func TestAFloatingRateFundThroughTheReports(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	start, bought, snap := ymd(2026, 1, 28).Unix(), ymd(2026, 6, 1).Unix(), ymd(2026, 6, 30).Unix()
	at := snap + 86399
	var txns, kinds []string
	paid := func(acct, iid string, day int64, amount string) {
		id := "D" + strconv.Itoa(len(txns))
		txns = append(txns, "('brk', '"+id+"', "+itoa(day)+", '"+acct+"', 'dividend', 'USD', "+amount+", '"+iid+"')")
		kinds = append(kinds, "('brk', '"+id+"', 'sig-exfr', 1, 'INCOME_DIVIDENDS', 'rule', 100)")
	}
	for m := 0; m < 5; m++ {
		paid("B", "000000001", ymd(2026, 2+m, 5).Unix(), "100")
	}
	paid("A", "EXFR", ymd(2026, 6, 5).Unix(), "200")
	execEach(t, db,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		    first_seen_at, last_seen_at) VALUES ('brk', 'A', 'brokerage', 'Brokerage A', 'USD', 0, 0),
		    ('brk', 'B', 'brokerage', 'Brokerage B', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, isin, cusip, symbol,
		    name, currency, first_seen_at, last_seen_at) VALUES
		    ('brk', 'EXFR', 'fixed_income', 'etf', 'US0000000001', '000000001', 'EXFR',
		     'EXAMPLE FLOATING RATE BOND ETF', 'USD', 0, 0),
		    ('brk', '000000001', 'fixed_income', 'etf', 'US0000000001', '000000001', 'EXFR',
		     'EXAMPLE FLOATING RATE BOND ETF', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value) VALUES
		    ('brk', `+itoa(start)+`, 'B', '000000001', '000000001', 'fixed_income', 'etf', 'USD', 500, 25000),
		    ('brk', `+itoa(bought)+`, 'A', 'EXFR', 'EXFR', 'fixed_income', 'etf', 'USD', 1000, 50000),
		    ('brk', `+itoa(snap)+`, 'A', 'EXFR', 'EXFR', 'fixed_income', 'etf', 'USD', 1000, 50000),
		    ('brk', `+itoa(snap)+`, 'B', 'EXFR', 'EXFR', 'fixed_income', 'etf', 'USD', 500, 25000)`,
		`INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id, kind,
		    currency, net_amount, instrument_external_id) VALUES `+strings.Join(txns, ", "),
		`INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id, payer_signature,
		    signature_version, income_detailed, provenance, assigned_at) VALUES `+strings.Join(kinds, ", "),
		`INSERT INTO fund_characteristics (silver_source_id, fund_ref, as_of, provider, field, label, value) VALUES
		    ('ref', 'isin:US0000000001', `+itoa(snap-86400)+`, 'testprov', 'trailing_yield', '', 4.8)`,
		`INSERT INTO rate_observations (silver_source_id, series_id, as_of, value) VALUES
		    ('rates', 'USD-3M', `+itoa(snap)+`, 4.0), ('rates', 'USD-1Y', `+itoa(snap)+`, 4.5),
		    ('rates', 'USD-10Y', `+itoa(snap)+`, 5.0)`)
	var daily, filing []filingLine
	for i := 1; i <= 8; i++ {
		isin := "US00000000F" + strconv.Itoa(i)
		l := filingLine{key: "isin:" + isin, isin: isin, name: "EXAMPLE NOTE " + strconv.Itoa(i), weight: 12,
			maturity: "2027-0" + strconv.Itoa(i) + "-28", coupon: 4.3}
		l.assetType = "Fixed Income"
		daily = append(daily, l)
		l.assetType, l.kind, l.isDefault, l.inArrears = "DBT", "Floating", false, false
		filing = append(filing, l)
	}
	daily = append(daily, filingLine{name: "USD CASH", assetType: "Cash", weight: 2},
		filingLine{key: "symbol:QMMF", name: "EXAMPLE GOVERNMENT MONEY FUND", assetType: "Money Market", weight: 2})
	filing = append(filing, filingLine{name: "REPURCHASE AGREEMENT", assetType: "RA", weight: 2},
		filingLine{name: "USD CASH", assetType: "Cash", weight: 2})
	addComposition(t, db, "ref", "isin:US0000000001", snap-86400, "testprov", "daily_file", "", daily...)
	addComposition(t, db, "ref", "isin:US0000000001", ymd(2026, 3, 31).Unix(), "sec_nport", "filing",
		"Example Floating Rate Bond Fund", filing...)
	rebuildForYield(t, db)
	execEach(t, db, `INSERT INTO yield_curves (currency, tenor, tenor_months, silver_source_id, series_id) VALUES
	    ('USD', '3M', 3, 'rates', 'USD-3M'), ('USD', '1Y', 12, 'rates', 'USD-1Y'), ('USD', '10Y', 120, 'rates', 'USD-10Y')`)

	// The coupons view: the notes float, the cash is no debt.
	coupons, err := ExposureCoupons(ctx, db, at, "USD")
	if err != nil {
		t.Fatal(err)
	}
	var buckets []string
	for _, r := range coupons {
		buckets = append(buckets, r.Bucket+"="+*r.Value)
	}
	if got := strings.Join(buckets, " "); got != "floating=72000 (none)=3000" {
		t.Errorf("coupons %s", got)
	}

	in, err := YieldInputs(ctx, db, at, "USD")
	if err != nil {
		t.Fatal(err)
	}
	// B's position under the CUSIP holds the fund's days under the
	// ticker, the id the matching folds the CUSIP into.
	if ds := in.FormerDaily[yield.InstrumentKey{Source: "brk", Instrument: "EXFR"}]; len(ds) != 1 ||
		len(ds[0].Spans) == 0 {
		t.Errorf("the CUSIP's position holds no days: %+v", ds)
	}
	b := yield.Compute(in)
	a := yield.HoldingKey{Source: "brk", Account: "A", PositionKey: "EXFR"}
	bk := yield.HoldingKey{Source: "brk", Account: "B", PositionKey: "EXFR"}
	for _, p := range b.Positions {
		if !p.Tags[yield.TagFloating] || p.Kind != yield.KindBondFund || p.Income100bp == nil || p.Income12 == nil ||
			math.Abs(*p.Income100bp-p.Value/100) > 1e-6 || math.Abs(*p.Income12-p.Value*4.8/100) > 1e-6 {
			t.Errorf("%s: kind %s tags %v, income %v, 100bp %v", p.HoldingKey.Account, p.Kind, p.Tags,
				ptrVal(p.Income12), ptrVal(p.Income100bp))
		}
	}
	if funds := b.Funds(); len(funds) != 1 || funds[0].FloatingPct == nil || math.Abs(*funds[0].FloatingPct-96) > 1e-9 {
		t.Errorf("funds %+v", funds)
	}
	var constant, forward float64
	for _, x := range b.Amounts(nil, b.NextMonths(12), yield.DefaultAssumptions) {
		constant += x.Value
	}
	for _, x := range b.Amounts(nil, b.NextMonths(12), yield.Assumptions{Path: yield.PathForward,
		Rule: yield.RuleCash}) {
		forward += x.Value
	}
	if math.Abs(constant-75000*0.048) > 1e-6 || forward <= constant+1 {
		t.Errorf("the next twelve months: %v constant, %v forward", constant, forward)
	}

	// income positions 2026 and income projection 2026.
	w := yield.Window{From: ymd(2026, 1, 1), To: ymd(2026, 12, 31).Add(24*time.Hour - time.Second)}
	received, err := IncomeReceived(ctx, db, in, w.From.Unix(), at)
	if err != nil {
		t.Fatal(err)
	}
	amounts := b.Amounts(received, w, yield.DefaultAssumptions)
	proj := b.ProjectionRows(amounts, w, 12)
	var total yield.ProjectionRow
	for _, r := range proj {
		if r.Total {
			total = r
		}
	}
	rows := b.IncomeRows(yield.GrainPositions, amounts, nil, nil)
	var sum [2]float64
	got := map[yield.HoldingKey]yield.IncomeRow{}
	for _, r := range rows {
		got[r.Holding] = r
		sum[0], sum[1] = sum[0]+r.Received, sum[1]+r.Expected
	}
	dist := total.ByType[yield.TypeDistributions]
	if len(rows) != 2 || math.Abs(sum[0]-total.Received) > 1e-6 || math.Abs(sum[1]-total.Expected) > 1e-6 ||
		math.Abs(dist[0]-700) > 1e-6 || math.Abs(dist[1]-1800) > 1e-6 {
		t.Errorf("positions sum to %v, projection %v (distributions %v): %+v", sum, [2]float64{total.Received,
			total.Expected}, dist, rows)
	}
	for k, want := range map[yield.HoldingKey][2]float64{a: {200, 1200}, bk: {500, 600}} {
		r := got[k]
		if math.Abs(r.Received-want[0]) > 1e-6 || math.Abs(r.Expected-want[1]) > 1e-6 || r.Basis != "indicated" {
			t.Errorf("%s: received %v, expected %v, basis %s", k.Account, r.Received, r.Expected, r.Basis)
		}
	}
	months := map[string]bool{}
	for _, c := range b.Calendar(amounts) {
		if c.Holding == a || c.Holding == bk {
			months[c.Month.Format("2006-01")] = true
		}
	}
	if len(months) != 6 {
		t.Errorf("calendar months %v", months)
	}
}

func ptrVal(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// The migration's macros replay over themselves and still read a
// filing.
func TestMigration0141DDLIsRerunnable(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	for range 2 {
		rerunMigrationDDL(t, db, ctx, "0141_filing_line_kinds.sql")
	}
	addComposition(t, db, "ref", "symbol:QFRF", 0, "sec_nport", "filing", "",
		filingLine{key: "isin:US00000000F1", name: "EXAMPLE NOTE", assetType: "DBT", weight: 100, kind: "Floating"})
	if _, err := SyncExposure(ctx, db, canonical.ExposureRows{
		Settings: canonical.ExposureSettings{MaxGapDays: canonical.DefaultMaxGapDays, AgedDays: 35, PartialPct: 3},
	}); err != nil {
		t.Fatal(err)
	}
	got := scanStrings(t, db, `SELECT fund_ref || ' ' || key || ' ' || coupon_kind FROM fund_filing_kinds(86399)`)
	if strings.Join(got, ", ") != "symbol:QFRF isin:US00000000F1 Floating" {
		t.Errorf("filing kinds %v", got)
	}
}
