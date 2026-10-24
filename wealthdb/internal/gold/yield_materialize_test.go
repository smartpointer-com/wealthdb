package gold

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// partIncomeSQL is a part's income in ccy under the constant path and no
// shift, as yield.Part.Income gives it: each floating rate floored at
// zero, a roll's at its rate over its principal.
func partIncomeSQL(ccy string) string {
	return `(p.fixed + p.principal * GREATEST(0, r.rate_pct + p.spread) / 1200
	        + CASE WHEN p.roll_principal = 0 THEN p.roll_constant
	               ELSE p.roll_principal * GREATEST(0, p.roll_constant * 1200 / p.roll_principal) / 1200 END)
	        * p.fx_` + ccy
}

// The materialized yield tables are the report's: each instant's
// positions sum to the yield report's value, the rate table holds every
// month of both paths, and a rerun replaces the tables.
func TestMaterializeYieldMatchesTheReport(t *testing.T) {
	t.Parallel()
	db, ctx, at := seedYieldExposure(t)
	n, err := MaterializeYield(ctx, db, at, 42, 12)
	if err != nil {
		t.Fatal(err)
	}
	in, err := YieldInputs(ctx, db, at, "USD")
	if err != nil {
		t.Fatal(err)
	}
	b := yield.Compute(in)
	value := 0.0
	for _, p := range b.Positions {
		value += p.Value
	}
	var instants, stamped int
	var got float64
	if err := db.QueryRowContext(ctx, `SELECT count(DISTINCT as_of), count(*) FILTER (WHERE computed_at = 42)
	    FROM report_yield_positions`).Scan(&instants, &stamped); err != nil {
		t.Fatal(err)
	}
	// The book holds nothing before its one snapshot: one instant of the
	// 13 has rows.
	if ats := yieldInstants(at); len(ats) != YieldHistoryMonths+1 || ats[len(ats)-1] != at ||
		ats[len(ats)-2] != 1780271999 || instants != 1 || stamped == 0 || n == 0 {
		t.Errorf("%d instants of %v, %d rows stamped, %d written", instants, ats, stamped, n)
	}
	if err := db.QueryRowContext(ctx, `SELECT SUM(value) FROM report_yield_positions WHERE as_of = ? AND currency = 'USD'`,
		at).Scan(&got); err != nil || math.Abs(got-value) > 1e-6 {
		t.Errorf("positions sum to %v (%v), the report %v", got, err, value)
	}
	var rates, currencies int
	if err := db.QueryRowContext(ctx, `SELECT count(*), count(DISTINCT currency) FROM report_yield_rates`).Scan(&rates,
		&currencies); err != nil || rates != currencies*len(yield.Paths)*(yield.Months+1) {
		t.Errorf("%d rates over %d currencies (%v)", rates, currencies, err)
	}
	// The cash flow projection's look-back parts: every month of the
	// income projection after the current one, in each currency a book
	// was made in, each part once and operating_out once per class.
	var planRows, nMonths, planCurrencies int
	if err := db.QueryRowContext(ctx, `SELECT count(*), count(DISTINCT month), count(DISTINCT currency)
	    FROM web_cashflow_plan`).Scan(&planRows, &nMonths, &planCurrencies); err != nil || nMonths != yield.Months ||
		planCurrencies == 0 || planRows != planCurrencies*yield.Months*(len(PlanParts)-1+len(PlanOutClasses)) {
		t.Errorf("%d plan rows over %d months and %d currencies (%v)", planRows, nMonths, planCurrencies, err)
	}
	parts := scanStrings(t, db, `SELECT DISTINCT part FROM web_cashflow_plan`)
	if !slices.Equal(slices.Sorted(slices.Values(parts)), slices.Sorted(slices.Values(PlanParts))) {
		t.Errorf("plan parts %v, want %v", parts, PlanParts)
	}
	// Their values are the cash flow projection's own, month by month.
	from, to := PlanWindow(time.Unix(at, 0), 12)
	lb, err := CashflowPlanLookback(ctx, db, from, to, "USD")
	if err != nil {
		t.Fatal(err)
	}
	// The book is of the end of June 2026: its plan starts in July.
	months := planMonths(b)
	if !months[0].Start.Equal(ymd(2026, 7, 1)) {
		t.Errorf("the projection's first month starts %s, want 2026-07-01", months[0].Start)
	}
	plan := CashflowPlan(lb, PlanStanding(b), months, 1, false)
	stored, err := db.QueryContext(ctx, `SELECT month, part, SUM(value), any_value(month_start)::VARCHAR
	    FROM web_cashflow_plan WHERE currency = 'USD' GROUP BY ALL`)
	if err != nil {
		t.Fatal(err)
	}
	for stored.Next() {
		var month int
		var part, start string
		var v float64
		if err := stored.Scan(&month, &part, &v, &start); err != nil {
			t.Fatal(err)
		}
		r := plan[month-1]
		want := map[string]float64{"other_income": r.OtherIncome, "operating_out": r.OperatingOut,
			"debt_service": r.DebtService, "vehicles": r.Vehicles}[part]
		if math.Abs(v-want) > 1e-6 || start[:10] != r.Period.Format(time.DateOnly) {
			t.Errorf("month %d %s: %v from %s, want %v from %s", month, part, v, start, want, r.Period)
		}
	}
	stored.Close()
	// A rerun replaces the tables.
	if n2, err := MaterializeYield(ctx, db, at, 43, 12); err != nil || n2 != n {
		t.Errorf("rerun: %d rows (%v), want %d", n2, err, n)
	}
}

// seedYieldBookFX is seedYieldExposure's book with what the yield tables
// convert and float: rates to and from the dollar, a dollar curve, a
// stated rate on the dollar cash, and a franc account that holds cash and
// receives rent. Every figure is invented.
func seedYieldBookFX(t *testing.T) (*sql.DB, context.Context, int64) {
	t.Helper()
	db, ctx, at := seedYieldExposure(t)
	snap := at - 86399
	q := []string{
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		    first_seen_at, last_seen_at) VALUES ('test-src', 'F', 'cash', 'Franc account', 'CHF', 0, 0)`,
		`INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount)
		    VALUES ('test-src', ` + itoa(snap) + `, 'F', 'CHF', 'current', 2000)`,
		`INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id, kind,
		    currency, net_amount) VALUES ('test-src', 'R1', ` + itoa(ymd(2026, 3, 1).Unix()) + `, 'F', 'deposit', 'CHF', 1500)`,
		`INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id, payer_signature,
		    signature_version, income_detailed, provenance, assigned_at) VALUES
		    ('test-src', 'R1', 'sig-tenant', 1, 'INCOME_RENTAL', 'rule', 100)`,
		`INSERT INTO rate_observations (silver_source_id, series_id, as_of, value) VALUES
		    ('rates', 'USD-3M', ` + itoa(snap) + `, 4.0), ('rates', 'USD-1Y', ` + itoa(snap) + `, 4.2),
		    ('rates', 'USD-10Y', ` + itoa(snap) + `, 4.6)`,
		`INSERT INTO yield_curves (currency, tenor, tenor_months, silver_source_id, series_id) VALUES
		    ('USD', '3M', 3, 'rates', 'USD-3M'), ('USD', '1Y', 12, 'rates', 'USD-1Y'), ('USD', '10Y', 120, 'rates', 'USD-10Y')`,
		`INSERT INTO account_rates (silver_source_id, account_external_id, currency, side, tier, rate_pct, origin)
		    VALUES ('test-src', 'A', 'USD', 'credit', 1, 2.5, 'stated'), ('test-src', 'F', 'CHF', 'credit', 1, 0.5, 'stated')`,
	}
	for ccy, usd := range map[string]float64{"CHF": 1.25, "EUR": 1.1, "GBP": 1.3} {
		q = append(q, `INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
		    VALUES ('fx', `+itoa(snap)+`, '`+ccy+`', 'USD', `+strconv.FormatFloat(usd, 'f', -1, 64)+`),
		           ('fx', `+itoa(snap)+`, 'USD', '`+ccy+`', `+strconv.FormatFloat(1/usd, 'f', -1, 64)+`)`)
	}
	execEach(t, db, q...)
	return db, ctx, at
}

// In every reporting currency, each holding's parts times its rate to
// that currency, summed under constant rates and the cash rule over the
// twelve months after this one, give its income_12m in that currency's
// book; the rent of the franc account is a part of its own; the rate
// table holds every currency the parts are in.
func TestMaterializeYieldMatchesEachBook(t *testing.T) {
	t.Parallel()
	db, ctx, at := seedYieldBookFX(t)
	if _, err := MaterializeYield(ctx, db, at, 42, 12); err != nil {
		t.Fatal(err)
	}
	for _, ccy := range materializeCurrencies {
		in, err := YieldInputs(ctx, db, at, ccy)
		if err != nil {
			t.Fatal(err)
		}
		b := yield.Compute(in)
		for _, p := range b.Positions {
			if p.Income12 == nil {
				continue
			}
			var got float64
			if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(`+partIncomeSQL(ccy)+`), 0)
			    FROM report_yield_parts p
			    JOIN report_yield_rates r ON r.currency = p.holding_currency AND r.month = p.month AND r.path = 'constant'
			   WHERE p.rule = 'cash' AND p.month BETWEEN 1 AND 12 AND p.silver_source_id = ?
			     AND p.account_external_id = ? AND p.position_key = ?`, p.Source, p.HoldingKey.Account,
				p.PositionKey).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if math.Abs(got-*p.Income12) > 1e-6 {
				t.Errorf("%s %v: parts give %v, income_12m %v", ccy, p.HoldingKey, got, *p.Income12)
			}
		}
	}
	if got := scanStrings(t, db, `SELECT DISTINCT holding_currency FROM report_yield_parts ORDER BY 1`); strings.Join(got,
		" ") != strings.Join(scanStrings(t, db, `SELECT DISTINCT currency FROM report_yield_rates ORDER BY 1`), " ") {
		t.Errorf("parts in %v", got)
	}
	if got := scanStrings(t, db, `SELECT DISTINCT position_key FROM report_yield_parts WHERE account_external_id = 'F'
	    ORDER BY 1`); strings.Join(got, " ") != "cash:CHF income:rent:CHF" {
		t.Errorf("the franc account's parts %v", got)
	}
	// The rent the account received names no holding: the dashboard reads
	// its asset class as income.
	rent := yield.AccountIncomeKey(yield.AccountCurrency{AccountKey: yield.AccountKey{Source: "test-src", Account: "F"},
		Currency: "CHF"}, yield.TypeRent)
	if got := scanStrings(t, db, `SELECT DISTINCT asset_class FROM web_yield_received WHERE position_key = ?`,
		rent.PositionKey); strings.Join(got, " ") != "income" {
		t.Errorf("the rent received reads asset class %v", got)
	}
}

// The curve view's DV01 is the book's: in every reporting currency, its
// sum by curve currency, tenor and what holds it is the curve view's
// (yield.Book.Curve), each tenor in the order of the risk tenors. The
// book adds a priced bond, a fund's stated duration and a mortgage.
func TestMaterializeYieldCurveIsTheBooks(t *testing.T) {
	t.Parallel()
	db, ctx, at := seedYieldBookFX(t)
	snap := at - 86399
	execEach(t, db,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, name, currency,
		    first_seen_at, last_seen_at) VALUES
		    ('test-src', 'QB2', 'fixed_income', 'bond', 'QORVANT CORP 5% 06/15/2031', 'USD', 0, 0),
		    ('test-src', 'QMORT', 'real_estate', 'mortgage', 'Example home loan', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value) VALUES
		    ('test-src', `+itoa(snap)+`, 'A', 'QB2', 'QB2', 'fixed_income', 'bond', 'USD', 10000, 10100),
		    ('test-src', `+itoa(snap)+`, 'A', 'QMORT', 'QMORT', 'real_estate', 'mortgage', 'USD', 1, -50000)`,
		`INSERT INTO yield_declared_terms (line_no, silver_source_id, instrument_external_id, coupon_pct, maturity)
		    VALUES (1, 'test-src', 'QMORT', 2, DATE '2036-06-15')`,
		`INSERT INTO fund_characteristics (silver_source_id, fund_ref, as_of, provider, field, label, value)
		    VALUES ('ref', 'symbol:BNDF', `+itoa(snap)+`, 'ishares', 'effective_duration', '', 6)`)
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildYield(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeYield(ctx, db, at, 42, 12); err != nil {
		t.Fatal(err)
	}
	for _, ccy := range materializeCurrencies {
		in, err := YieldInputs(ctx, db, at, ccy)
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, r := range yield.Compute(in).Curve() {
			order := slices.Index(canonical.RiskTenors, r.Tenor) + 1
			for held, v := range map[string]float64{yield.HeldInFunds: r.Funds, yield.HeldInBonds: r.Bonds,
				yield.HeldInCash: r.Cash, yield.HeldInLiabilities: r.Liabilities} {
				if v != 0 {
					want = append(want, fmt.Sprintf("%s %d %s %s %.6f", r.Currency, order, r.Tenor, held, v))
				}
			}
		}
		slices.Sort(want)
		got := scanStrings(t, db, `SELECT concat_ws(' ', curve_currency, tenor_order, tenor, held_in,
		    printf('%.6f', SUM(dv01))) FROM web_yield_curve
		  WHERE currency = ? AND as_of_day = (SELECT max(as_of_day) FROM web_yield_curve)
		  GROUP BY curve_currency, tenor_order, tenor, held_in ORDER BY 1`, ccy)
		held := strings.Join(want, "\n")
		if !strings.Contains(held, " bonds ") || !strings.Contains(held, " funds ") ||
			!strings.Contains(held, " liabilities ") || strings.Join(got, "\n") != held {
			t.Errorf("%s curve:\n%s\nwant\n%s", ccy, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
}

// Migration 0138 replays over a materialized gold: its tables keep their
// rows and its views their shape.
func TestMigration0138DDLIsRerunnable(t *testing.T) {
	t.Parallel()
	db, ctx, at := seedYieldBookFX(t)
	if _, err := MaterializeYield(ctx, db, at, 42, 12); err != nil {
		t.Fatal(err)
	}
	count := func() string {
		return strings.Join(scanStrings(t, db, `SELECT (SELECT count(*) FROM report_yield_positions)::VARCHAR || ' ' ||
		    (SELECT count(*) FROM report_yield_parts) || ' ' || (SELECT count(*) FROM web_yield_parts) || ' ' ||
		    (SELECT count(*) FROM web_yield_holdings)`), "")
	}
	before := count()
	for range 2 {
		rerunMigrationDDL(t, db, ctx, "0138_yield_dashboard.sql")
	}
	if after := count(); after != before {
		t.Errorf("rows %s, before %s", after, before)
	}
}

// Migration 0149 replays over a materialized gold: its table keeps its
// rows and its view its shape.
func TestMigration0149DDLIsRerunnable(t *testing.T) {
	t.Parallel()
	db, ctx, at := seedYieldBookFX(t)
	if _, err := MaterializeYield(ctx, db, at, 42, 12); err != nil {
		t.Fatal(err)
	}
	count := func() string {
		return strings.Join(scanStrings(t, db, `SELECT (SELECT count(*) FROM report_cashflow_plan)::VARCHAR || ' ' ||
		    (SELECT count(*) FROM web_cashflow_plan)`), "")
	}
	before := count()
	if before == "0 0" {
		t.Fatal("no plan rows materialized")
	}
	for range 2 {
		rerunMigrationDDL(t, db, ctx, "0149_cashflow_plan.sql")
	}
	if after := count(); after != before {
		t.Errorf("rows %s, before %s", after, before)
	}
}

// seedYieldYear is a book of a year of month-end snapshots, invented:
// a broker's portfolio of a dollar account, with a stock held all year
// and one bought a month ago, and a franc account, with a fund and cash;
// a bank's savings account, near zero until a deposit twenty days ago;
// its account of a fund bought a hundred days ago beside cash held all
// year; and its mortgage, alone in its account. The stocks, the funds,
// the franc cash and the savings pay, and the franc moves against the
// dollar during the year.
func seedYieldYear(t *testing.T) (*sql.DB, context.Context, int64) {
	t.Helper()
	db, ctx := openMigrated(t)
	at := ymd(2026, 6, 30).Unix() + 86399
	q := []string{
		`INSERT INTO portfolios (silver_source_id, portfolio_external_id, display_name, first_seen_at, last_seen_at)
		    VALUES ('broker', 'P1', 'Family', 0, 0)`,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		    portfolio_external_id, first_seen_at, last_seen_at) VALUES
		    ('broker', 'B1', 'brokerage', 'Dollar account', 'USD', 'P1', 0, 0),
		    ('broker', 'B2', 'brokerage', 'Franc account', 'CHF', 'P1', 0, 0),
		    ('bank', 'S', 'cash', 'Savings', 'USD', NULL, 0, 0),
		    ('bank', 'K', 'brokerage', 'Bank brokerage', 'USD', NULL, 0, 0),
		    ('bank', 'M', 'mortgage', 'Home loan', 'USD', NULL, 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, name, currency,
		    first_seen_at, last_seen_at) VALUES
		    ('broker', 'QA', 'public_equity', 'stock', 'QA', 'Example Steady Co', 'USD', 0, 0),
		    ('broker', 'QN', 'public_equity', 'stock', 'QN', 'Example New Co', 'USD', 0, 0),
		    ('broker', 'QF', 'public_equity', 'fund', 'QF', 'Example Franc Fund', 'CHF', 0, 0),
		    ('bank', 'QK', 'public_equity', 'fund', 'QK', 'Example Late Fund', 'USD', 0, 0),
		    ('bank', 'QMORT', 'real_estate', 'mortgage', NULL, 'Example home loan', 'USD', 0, 0)`,
	}
	var positions, balances []string
	snap := func(at int64, qn, qk bool, savings float64) {
		positions = append(positions,
			"('broker', "+itoa(at)+", 'B1', 'QA', 'QA', 'public_equity', 'stock', 'USD', 100, "+
				itoa(10000+(at/86400)%7*100)+")",
			"('broker', "+itoa(at)+", 'B2', 'QF', 'QF', 'public_equity', 'fund', 'CHF', 800, 8000)",
			"('bank', "+itoa(at)+", 'M', 'QMORT', 'QMORT', 'real_estate', 'mortgage', 'USD', 1, -100000)")
		if qn {
			positions = append(positions, "('broker', "+itoa(at)+", 'B1', 'QN', 'QN', 'public_equity', 'stock', 'USD', 50, 5000)")
		}
		if qk {
			positions = append(positions, "('bank', "+itoa(at)+", 'K', 'QK', 'QK', 'public_equity', 'fund', 'USD', 1000, 100000)")
		}
		balances = append(balances, "('broker', "+itoa(at)+", 'B2', 'CHF', 'current', 2000)",
			"('bank', "+itoa(at)+", 'S', 'USD', 'current', "+strconv.FormatFloat(savings, 'f', -1, 64)+")",
			"('bank', "+itoa(at)+", 'K', 'USD', 'current', 5000)")
	}
	for m := 0; m < 13; m++ {
		snap(ymd(2025, 6+m, 1).AddDate(0, 0, -1).Unix()+43200, false, m > 9, 100)
	}
	snap(ymd(2026, 3, 22).Unix()+43200, false, true, 100)
	snap(ymd(2026, 6, 1).Unix()+43200, true, true, 100)
	snap(ymd(2026, 6, 10).Unix()+43200, true, true, 50000)
	snap(ymd(2026, 6, 30).Unix()+43200, true, true, 50000)
	q = append(q,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value) VALUES `+strings.Join(positions, ", "),
		`INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount)
		    VALUES `+strings.Join(balances, ", "))
	var txns, kinds []string
	income := func(id, src, acct, kind, ccy, iid, detailed string, day time.Time, amount int64) {
		instrument := "NULL"
		if iid != "" {
			instrument = "'" + iid + "'"
		}
		txns = append(txns, "('"+src+"', '"+id+"', "+itoa(day.Unix()+36000)+", '"+acct+"', '"+kind+"', '"+ccy+"', "+
			itoa(amount)+", "+instrument+")")
		kinds = append(kinds, "('"+src+"', '"+id+"', 'sig-"+id+"', 1, '"+detailed+"', 'rule', 100)")
	}
	for i, d := range []time.Time{ymd(2025, 9, 15), ymd(2025, 12, 15), ymd(2026, 3, 15), ymd(2026, 6, 15)} {
		income("D"+itoa(int64(i)), "broker", "B1", "dividend", "USD", "QA", "INCOME_DIVIDENDS", d, 50)
	}
	income("F1", "broker", "B2", "dividend", "CHF", "QF", "INCOME_DIVIDENDS", ymd(2025, 12, 20), 80)
	income("F2", "broker", "B2", "dividend", "CHF", "QF", "INCOME_DIVIDENDS", ymd(2026, 6, 20), 80)
	income("I1", "broker", "B2", "interest", "CHF", "", "INCOME_INTEREST_EARNED", ymd(2026, 3, 31), 10)
	income("I2", "bank", "S", "interest", "USD", "", "INCOME_INTEREST_EARNED", ymd(2026, 6, 29), 5)
	income("K1", "bank", "K", "dividend", "USD", "QK", "INCOME_DIVIDENDS", ymd(2026, 5, 15), 1000)
	income("K2", "bank", "K", "interest", "USD", "", "INCOME_INTEREST_EARNED", ymd(2026, 6, 29), 20)
	q = append(q,
		`INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id, kind,
		    currency, net_amount, instrument_external_id) VALUES `+strings.Join(txns, ", "),
		`INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id, payer_signature,
		    signature_version, income_detailed, provenance, assigned_at) VALUES `+strings.Join(kinds, ", "))
	for _, r := range []struct {
		day  time.Time
		rate float64
	}{{ymd(2025, 1, 1), 1.2}, {ymd(2026, 3, 1), 1.25}} {
		q = append(q, `INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
		    VALUES ('fx', `+itoa(r.day.Unix())+`, 'CHF', 'USD', `+strconv.FormatFloat(r.rate, 'f', -1, 64)+`),
		           ('fx', `+itoa(r.day.Unix())+`, 'USD', 'CHF', `+strconv.FormatFloat(1/r.rate, 'f', -1, 64)+`)`)
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
	if _, err := RebuildYield(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db, ctx, at
}

// The dashboard's realized yield is the report's: at every instant and in
// every reporting currency, yield_realized_pct over a row's holdings in
// report_yield_positions gives the realized yield of yield.Book's row,
// for every grain, by currency and converted, a total too. The table
// holds no days, so a row the report reads as thin, the savings account,
// shows the pooled figure: the row's income over its summed mean daily
// values. The bank's brokerage account is not thin, though its fund,
// thin, holds most of its mean value. The dollar account's yield pools
// its new stock, so it is not its steady stock's. The mortgage's account,
// a debt that received nothing, reads zero, not a negative zero.
func TestMaterializedRealizedYieldIsTheBooks(t *testing.T) {
	t.Parallel()
	db, ctx, at := seedYieldYear(t)
	if _, err := MaterializeYield(ctx, db, at, 42, 12); err != nil {
		t.Fatal(err)
	}
	figures, thin := 0, 0
	for _, instant := range yieldInstants(at) {
		for _, ccy := range materializeCurrencies {
			in, err := YieldInputs(ctx, db, instant, ccy)
			if err != nil {
				t.Fatal(err)
			}
			b := yield.Compute(in)
			for _, g := range []yield.Grain{yield.GrainAll, yield.GrainSources, yield.GrainPortfolios,
				yield.GrainAccounts} {
				for _, converted := range []bool{false, true} {
					for _, r := range b.Aggregate(g, converted) {
						in := func(p yield.Position) bool {
							switch {
							case r.Total:
								return true
							case !converted && p.Currency != r.Currency:
								return false
							case g == yield.GrainSources:
								return p.Source == r.Source
							case g == yield.GrainPortfolios:
								return p.Source == r.Source && p.Account.Portfolio == r.Portfolio
							case g == yield.GrainAccounts:
								return p.Source == r.Source && p.HoldingKey.Account == r.AccountID
							}
							return true
						}
						where, args := " WHERE p.as_of = ? AND p.currency = ?", []any{instant, ccy}
						if !r.Total {
							switch g {
							case yield.GrainSources:
								where, args = where+" AND p.silver_source_id = ?", append(args, r.Source)
							case yield.GrainPortfolios:
								where = where + " AND p.silver_source_id = ? AND COALESCE(a.portfolio_external_id, '') = ?"
								args = append(args, r.Source, r.Portfolio)
							case yield.GrainAccounts:
								where = where + " AND p.silver_source_id = ? AND p.account_external_id = ?"
								args = append(args, r.Source, r.AccountID)
							}
							if !converted {
								where, args = where+" AND p.holding_currency = ?", append(args, r.Currency)
							}
						}
						var got sql.NullFloat64
						if err := db.QueryRowContext(ctx, `SELECT yield_realized_pct(p.received_12m, p.mean_value, p.weighs)
						    FROM report_yield_positions p
						    LEFT JOIN accounts a ON a.silver_source_id = p.silver_source_id
						         AND a.account_external_id = p.account_external_id`+where, args...).Scan(&got); err != nil {
							t.Fatal(err)
						}
						want := r.Realized
						if want == nil {
							want = pooledRealized(b.Positions, in)
							if want != nil {
								thin++
							}
						}
						what := fmt.Sprintf("%d %s %s converted=%v %s/%s/%s %s", instant, ccy, g, converted, r.Source,
							r.Portfolio, r.AccountID, r.Currency)
						switch {
						case want == nil && !got.Valid:
						case want == nil || !got.Valid || math.Abs(*want-got.Float64) > 1e-9:
							t.Errorf("%s: table %v, book %v", what, got, want)
						default:
							figures++
						}
						if instant == at && ccy == "USD" && g == yield.GrainAccounts && !converted {
							switch r.AccountID {
							case "S":
								if r.Realized != nil || !got.Valid {
									t.Errorf("the savings account: book %v, table %v", r.Realized, got)
								}
							case "K":
								if r.Realized == nil || !strings.Contains(r.Quality, "thin=") {
									t.Errorf("the bank's brokerage account %v, %q", r.Realized, r.Quality)
								}
							case "M":
								if r.Realized == nil || *r.Realized != 0 || math.Signbit(*r.Realized) ||
									math.Signbit(got.Float64) {
									t.Errorf("the mortgage's account: book %v, table %v", r.Realized, got)
								}
							case "B1":
								steady := position(b, yield.HoldingKey{Source: "broker", Account: "B1", PositionKey: "QA"})
								if r.Realized == nil || steady.Realized == nil || *r.Realized >= *steady.Realized {
									t.Errorf("the dollar account %v, its steady stock %v", r.Realized, steady.Realized)
								}
							}
						}
					}
				}
			}
		}
	}
	if figures < 50 || thin == 0 {
		t.Errorf("%d figures, %d thin rows", figures, thin)
	}
}

// pooledRealized is the realized yield of the positions in keeps, pooled
// as a row pools them but with no thin rule: their income over their
// summed mean daily values, over the assets, else over every position;
// nil where that sum is zero.
func pooledRealized(ps []yield.Position, keeps func(yield.Position) bool) *float64 {
	var kept, assets []yield.Position
	for _, p := range ps {
		if keeps(p) {
			kept = append(kept, p)
			if p.Weighs() {
				assets = append(assets, p)
			}
		}
	}
	if len(assets) > 0 {
		kept = assets
	}
	received, mean := 0.0, 0.0
	for _, p := range kept {
		r, m := p.RealizedParts()
		received, mean = received+r, mean+m
	}
	if mean == 0 {
		return nil
	}
	v := received / mean * 100
	return &v
}

// position is the book's position of key k.
func position(b *yield.Book, k yield.HoldingKey) yield.Position {
	for _, p := range b.Positions {
		if p.HoldingKey == k {
			return p
		}
	}
	return yield.Position{}
}
