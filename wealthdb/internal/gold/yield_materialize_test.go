package gold

import (
	"context"
	"database/sql"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// partIncomeSQL is a part's income in USD under the constant path and no
// shift, as yield.Part.Income gives it.
const partIncomeSQL = `(p.fixed + p.roll_constant + p.principal * (r.rate_pct + p.spread) / 1200) * p.fx_USD`

// The materialized yield tables are the report's: each instant's
// positions sum to the yield report's value, and today's parts, summed
// by the projection's formula over the twelve months after this one,
// give the report's income_12m, in every reporting currency's book.
func TestMaterializeYieldMatchesTheReport(t *testing.T) {
	t.Parallel()
	db, ctx, at := seedYieldExposure(t)
	n, err := MaterializeYield(ctx, db, at, 42)
	if err != nil {
		t.Fatal(err)
	}
	in, err := YieldInputs(ctx, db, at, "USD")
	if err != nil {
		t.Fatal(err)
	}
	b := yield.Compute(in)
	value, income := 0.0, 0.0
	for _, p := range b.Positions {
		value += p.Value
		if p.Income12 != nil {
			income += *p.Income12
		}
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
	if err := db.QueryRowContext(ctx, `SELECT SUM(`+partIncomeSQL+`)
	    FROM report_yield_parts p
	    JOIN report_yield_rates r ON r.currency = p.holding_currency AND r.month = p.month AND r.path = 'constant'
	   WHERE p.rule = 'cash' AND p.month BETWEEN 1 AND 12`).Scan(&got); err != nil || math.Abs(got-income) > 1e-6 {
		t.Errorf("parts give %v (%v), the report's income_12m %v", got, err, income)
	}
	var rates, currencies int
	if err := db.QueryRowContext(ctx, `SELECT count(*), count(DISTINCT currency) FROM report_yield_rates`).Scan(&rates,
		&currencies); err != nil || rates != currencies*len(yield.Paths)*(yield.Months+1) {
		t.Errorf("%d rates over %d currencies (%v)", rates, currencies, err)
	}
	// A rerun replaces the tables.
	if n2, err := MaterializeYield(ctx, db, at, 43); err != nil || n2 != n {
		t.Errorf("rerun: %d rows (%v), want %d", n2, err, n)
	}
}

// seedYieldBookFX is the B4 book with what the yield tables convert and
// float: rates to and from the dollar, a dollar curve, a stated rate on
// the dollar cash, and a franc account that holds cash and receives
// rent. Every figure is invented.
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
	if _, err := MaterializeYield(ctx, db, at, 42); err != nil {
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
			if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM((p.fixed + p.roll_constant
			        + p.principal * (r.rate_pct + p.spread) / 1200) * p.fx_`+ccy+`), 0)
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
}

// Migration 0138 replays over a materialized gold: its tables keep their
// rows and its views their shape.
func TestMigration0138DDLIsRerunnable(t *testing.T) {
	t.Parallel()
	db, ctx, at := seedYieldBookFX(t)
	if _, err := MaterializeYield(ctx, db, at, 42); err != nil {
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
