package gold

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// Edge cases of the gains macros, each pinned by what docs/GAINS.md
// says. Every id and figure is invented.

func edgeExec(t *testing.T, db *sql.DB, ctx context.Context, q string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, q); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

const edgeBase = `
    INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate) VALUES
        ('test-src', 0, 'CHF', 'USD', 0.5);
    INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name,
                          portfolio_external_id, tax_wrapper, first_seen_at, last_seen_at) VALUES
        ('test-src', 'A1', 'brokerage', 'One', NULL, NULL, 1, 1),
        ('test-src', 'A2', 'brokerage', 'Two', NULL, NULL, 1, 1);
`

func edgePos(snap int64, acct, key, instr, ccy string, qty, mv float64, bv string) string {
	stamp := "'stated','lots','included'"
	if bv == "NULL" {
		stamp = "NULL,NULL,NULL"
	}
	i := "'" + instr + "'"
	if instr == "" {
		i = "NULL"
	}
	return fmt.Sprintf(`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
        instrument_external_id, asset_class, vehicle, currency, quantity, market_value, book_value,
        accrued_interest, basis_origin, basis_method, basis_fees) VALUES
        ('test-src', %d, '%s', '%s', %s, 'public_equity', 'stock', '%s', %v, %v, %s, NULL, %s);`,
		snap, acct, key, i, ccy, qty, mv, bv, stamp)
}

func edgeTotal(t *testing.T, db *sql.DB, ctx context.Context, ccy string, grain GainsGrain) []GainsBucketRow {
	t.Helper()
	rows, err := GainsBuckets(ctx, db, gainsFrom, gainsTo, ccy, "total", grain, lots.MissingIgnore)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// show prints a decimal field for a failure message.
func show(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// A basis that appears or vanishes between the boundaries leaves the
// change unknown and flags it, rather than booking the whole gain since
// purchase.
func TestGainsBasisChangeIsFlaggedNotBooked(t *testing.T) {
	for name, bv := range map[string][2]string{"appears": {"NULL", "600"}, "vanishes": {"600", "NULL"}} {
		db, ctx := openMigrated(t)
		edgeExec(t, db, ctx, edgeBase+
			edgePos(1000, "A1", "X", "X", "USD", 10, 1000, bv[0])+
			edgePos(200000, "A1", "X", "X", "USD", 10, 1050, bv[1]))
		r := edgeTotal(t, db, ctx, "USD", GainsAll)[0]
		if r.UnrealizedChange != nil || r.Gain != nil || r.Quality != "basis_changed=1" {
			t.Errorf("basis %s: change %s gain %s quality %q", name, show(r.UnrealizedChange), show(r.Gain), r.Quality)
		}
	}
}

// A position whose value cannot be converted leaves the coverage share
// unmeasured, so the verdict cannot read ok.
func TestGainsCoverageNeedsEveryValue(t *testing.T) {
	db, ctx := openMigrated(t)
	edgeExec(t, db, ctx, edgeBase+
		edgePos(1000, "A1", "X", "X", "USD", 1, 100, "80")+
		edgePos(1000, "A1", "J", "J", "JPY", 1, 1000000, "NULL"))
	rows, err := GainsCoverage(ctx, db, gainsFrom, gainsTo, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Verdict != "partial" || rows[0].BasisCoverage != nil {
		t.Errorf("coverage = %+v, want partial with no share", rows)
	}
}

// A lot that names its instrument by hint meets its position, and is
// not unmatched.
func TestGainsHintLotMeetsItsPosition(t *testing.T) {
	db, ctx := openMigrated(t)
	edgeExec(t, db, ctx, edgeBase+
		edgePos(1000, "A1", "X", "X", "USD", 10, 1000, "600")+
		edgePos(200000, "A1", "X", "X", "USD", 5, 600, "300")+`
        INSERT INTO realized_lots (silver_source_id, realized_lot_external_id, account_external_id,
            instrument_external_id, instrument_hint, description, document_kind, tax_year, acquired_various,
            disposal_date, currency, quantity, proceeds, book_value, realized_gain_loss, is_primary) VALUES
            ('test-src', 'R1', 'A1', NULL, 'X', 'EX CORP', 'form_1099b', 1970, FALSE,
             DATE '1970-02-01', 'USD', 5, 550, 300, NULL, TRUE);`)
	rows, err := GainsPositions(ctx, db, gainsFrom, gainsTo, "USD", lots.MissingIgnore)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || show(rows[0].PositionKey) != "X" || !near(num(t, rows[0].Realized), 250) || rows[0].Quality != "" {
		t.Errorf("rows = %+v, want one X row carrying its lot, unflagged", rows)
	}
}

// Every grain carries an account the accounts table does not list.
func TestGainsGrainsCarryAnUnlistedAccount(t *testing.T) {
	db, ctx := openMigrated(t)
	edgeExec(t, db, ctx, edgeBase+
		edgePos(1000, "GHOST", "X", "X", "USD", 10, 1000, "600")+
		edgePos(200000, "GHOST", "X", "X", "USD", 10, 1100, "600"))
	for _, g := range []GainsGrain{GainsAll, GainsSources, GainsPortfolios, GainsAccounts} {
		rows := edgeTotal(t, db, ctx, "USD", g)
		if len(rows) != 1 || !near(num(t, rows[0].Gain), 100) {
			t.Errorf("%s: %d rows, gain %v; want one row with 100", g, len(rows), rows)
		}
	}
}

// An account a later snapshot leaves out is flagged: the snapshot may
// be partial, or the account closed without a closing row.
func TestGainsUnobservedAccountIsFlagged(t *testing.T) {
	db, ctx := openMigrated(t)
	edgeExec(t, db, ctx, edgeBase+
		edgePos(1000, "A1", "X", "X", "USD", 10, 1000, "600")+
		edgePos(1000, "A2", "Y", "Y", "USD", 10, 1000, "900")+
		edgePos(200000, "A2", "Y", "Y", "USD", 10, 1000, "900"))
	r := edgeTotal(t, db, ctx, "USD", GainsAll)[0]
	if r.Quality != "accounts_unobserved=1" {
		t.Errorf("quality = %q, want accounts_unobserved=1", r.Quality)
	}
}

// The positions view and the account grain read the same rows, so they
// agree when one instrument has a line without a basis.
func TestGainsPositionsAgreeWithTheAccount(t *testing.T) {
	db, ctx := openMigrated(t)
	edgeExec(t, db, ctx, edgeBase+
		edgePos(1000, "A1", "X-1", "X", "USD", 10, 1000, "600")+
		edgePos(1000, "A1", "X-2", "X", "USD", 10, 1000, "NULL")+
		edgePos(1000, "A1", "Y", "Y", "USD", 10, 1000, "800")+
		edgePos(200000, "A1", "X-1", "X", "USD", 10, 1100, "600")+
		edgePos(200000, "A1", "X-2", "X", "USD", 10, 1100, "NULL")+
		edgePos(200000, "A1", "Y", "Y", "USD", 10, 1050, "800"))
	acct := edgeTotal(t, db, ctx, "USD", GainsAccounts)[0]
	rows, err := GainsPositions(ctx, db, gainsFrom, gainsTo, "USD", lots.MissingIgnore)
	if err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, r := range rows {
		if r.Gain != nil {
			sum += num(t, r.Gain)
		}
	}
	if !near(sum, num(t, acct.Gain)) || !near(sum, 50) {
		t.Errorf("positions Σ %v, account %s; want both 50 (X has no full basis)", sum, show(acct.Gain))
	}
}

// The change in the output currency includes the exchange-rate change
// on a gain carried through the bucket (docs/GAINS.md §4): end − start.
func TestGainsChangeCarriesTheRateOnTheGain(t *testing.T) {
	db, ctx := openMigrated(t)
	edgeExec(t, db, ctx, edgeBase+`
        INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate) VALUES
            ('test-src', 150000, 'CHF', 'USD', 1.0);`+
		edgePos(1000, "A1", "X", "CHF", "CHF", 10, 200, "100")+
		edgePos(200000, "A1", "X", "CHF", "CHF", 10, 200, "100"))
	r := edgeTotal(t, db, ctx, "USD", GainsAll)[0]
	if d := num(t, r.UnrealizedEnd) - num(t, r.UnrealizedStart); !near(num(t, r.UnrealizedChange), d) || near(d, 0) {
		t.Errorf("start %s end %s change %s", show(r.UnrealizedStart), show(r.UnrealizedEnd), show(r.UnrealizedChange))
	}
}

// A window with realized lots and no positions still opens, and an
// unpriced line counts once.
func TestGainsLotsOnlyAndFxMissingCountOnce(t *testing.T) {
	db, ctx := openMigrated(t)
	edgeExec(t, db, ctx, edgeBase+`
        INSERT INTO realized_lots (silver_source_id, realized_lot_external_id, account_external_id,
            instrument_external_id, document_kind, tax_year, acquired_various,
            disposal_date, currency, quantity, proceeds, book_value, realized_gain_loss, is_primary) VALUES
            ('test-src', 'R1', 'A1', 'X', 'form_1099b', 1970, FALSE, DATE '1970-02-01', 'USD', 5, 550, 300, NULL, TRUE);`+
		edgePos(1000, "A2", "J", "J", "JPY", 1, 1000, "900")+
		edgePos(200000, "A2", "J", "J", "JPY", 1, 1000, "900"))
	rows, err := GainsBuckets(ctx, db, 0, gainsTo, "USD", "total", GainsAll, lots.MissingIgnore)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !near(num(t, rows[0].Realized), 250) || !strings.Contains(rows[0].Quality, "fx_missing=1") {
		t.Errorf("rows = %+v", rows)
	}
}

// report_positions' first 17 columns equal the 0031 body's, with a
// bridged FX path whose legs change on different days.
func TestReportPositionsKeepsItsEarlierColumns(t *testing.T) {
	db, ctx := openMigrated(t)
	edgeExec(t, db, ctx, edgeBase+`
        INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate) VALUES
            ('test-src', 86400*3, 'EUR', 'CHF', 0.95),
            ('test-src', 86400*5, 'CHF', 'USD', 1.13),
            ('test-src', 86400*9, 'EUR', 'USD', 1.07),
            ('test-src', 86400*2, 'GBP', 'USD', 1.3);`+
		edgePos(86400*4+5, "A1", "E", "E", "EUR", 3, 123.4567, "100")+
		edgePos(86400*4+5, "A1", "G", "G", "GBP", 3, 77.7777, "NULL")+
		edgePos(86400*6+5, "A2", "E", "E", "EUR", 3, 333.3333, "100")+
		edgePos(86400*10+5, "A1", "E", "E", "EUR", 7, 999.9999, "100"))
	old := `
    WITH latest AS (
        SELECT silver_source_id, MAX(snapshot_at) AS snap
          FROM positions WHERE snapshot_at <= $1 GROUP BY 1),
    base AS (
        SELECT p.silver_source_id, p.snapshot_at, p.account_external_id,
               a.display_name, a.relationship_id, a.nickname, a.account_category,
               p.position_key, p.instrument_external_id,
               COALESCE(i.symbol, sri.symbol) AS symbol, i.name,
               p.asset_class, p.vehicle, p.currency, p.quantity, p.market_value
          FROM positions p
          JOIN latest l ON p.silver_source_id = l.silver_source_id AND p.snapshot_at = l.snap
          LEFT JOIN accounts a ON p.silver_source_id = a.silver_source_id AND p.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON p.silver_source_id = i.silver_source_id AND p.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = p.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = p.instrument_external_id)
    SELECT b.position_key || '|' || b.account_external_id || '|' || CAST(b.quantity AS VARCHAR) || '|' || CAST(b.market_value AS VARCHAR) || '|' ||
           COALESCE(CAST(COALESCE(CASE WHEN b.currency = $2 THEN b.market_value::DOUBLE END,
               b.market_value::DOUBLE * d.rate, b.market_value::DOUBLE * c1.rate * c2.rate,
               b.market_value::DOUBLE * u1.rate * u2.rate)::DECIMAL(28,4) AS VARCHAR), 'NULL')
      FROM base b
      ASOF LEFT JOIN fx_daily d  ON d.from_ccy = b.currency AND d.to_ccy = $2 AND d.day <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = b.currency AND c1.to_ccy = 'CHF' AND c1.day <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF' AND c2.to_ccy = $2 AND c2.day <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = b.currency AND u1.to_ccy = 'USD' AND u1.day <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD' AND u2.to_ccy = $2 AND u2.day <= (b.snapshot_at // 86400)
     ORDER BY 1`
	nu := `SELECT position_key || '|' || account_external_id || '|' || quantity || '|' || market_value || '|' || COALESCE(value_outccy, 'NULL')
	         FROM report_positions($1, $2) ORDER BY 1`
	collect := func(q string, asof int64, ccy string) []string {
		rs, err := db.QueryContext(ctx, q, asof, ccy)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		var out []string
		for rs.Next() {
			var v string
			if err := rs.Scan(&v); err != nil {
				t.Fatal(err)
			}
			out = append(out, v)
		}
		return out
	}
	for _, asof := range []int64{86400*4 + 10, 86400*6 + 10, 86400*10 + 10} {
		for _, ccy := range []string{"USD", "CHF", "EUR", "GBP"} {
			o, n := collect(old, asof, ccy), collect(nu, asof, ccy)
			if strings.Join(o, ";") != strings.Join(n, ";") {
				t.Errorf("asof %d %s: old %v new %v", asof, ccy, o, n)
			}
		}
	}
}

// Weekly, daily and quarterly buckets add up to the total.
func TestGainsEveryPeriodAddsUpToTheTotal(t *testing.T) {
	db, ctx := openGainsFixture(t)
	total := edgeTotal(t, db, ctx, "USD", GainsAll)
	for _, p := range []string{"week", "day", "quarter"} {
		rows, err := GainsBuckets(ctx, db, gainsFrom, gainsTo, "USD", p, GainsAll, lots.MissingIgnore)
		if err != nil {
			t.Fatal(err)
		}
		var g float64
		for _, r := range rows {
			if r.Gain != nil {
				g += num(t, r.Gain)
			}
		}
		if !near(g, num(t, total[0].Gain)) {
			t.Errorf("%s: Σ %v != total %s", p, g, show(total[0].Gain))
		}
	}
}

// A realized lot that names no instrument, hint or description still
// counts, and is unmatched.
func TestGainsAnonymousLotCounts(t *testing.T) {
	db, ctx := openMigrated(t)
	edgeExec(t, db, ctx, edgeBase+`
        INSERT INTO realized_lots (silver_source_id, realized_lot_external_id, account_external_id,
            document_kind, tax_year, acquired_various, disposal_date, currency, quantity,
            proceeds, book_value, is_primary) VALUES
            ('test-src', 'R1', 'A1', 'statement', 1970, FALSE, DATE '1970-02-01', 'USD', 5, 550, 300, TRUE);`)
	r := edgeTotal(t, db, ctx, "USD", GainsAll)[0]
	if !near(num(t, r.Realized), 250) || r.RealizedLots != 1 {
		t.Errorf("realized %s lots %d, want 250 and 1", show(r.Realized), r.RealizedLots)
	}
	rows, err := GainsPositions(ctx, db, gainsFrom, gainsTo, "USD", lots.MissingIgnore)
	if err != nil || len(rows) != 1 || rows[0].LotKey != nil || rows[0].Quality != "unmatched_lots=1" {
		t.Errorf("positions = %+v err %v", rows, err)
	}
}

// An open-start window opens at the first sell too, so a sell before
// any snapshot or lot is counted.
func TestGainsWindowOpensAtTheFirstSell(t *testing.T) {
	db, ctx := openMigrated(t)
	edgeExec(t, db, ctx, edgeBase+`
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, instrument_external_id, kind, currency, net_amount)
            VALUES ('test-src', 'S1', 90000, 'A1', 'X', 'sell', 'USD', 100);`+
		edgePos(200000, "A1", "Y", "Y", "USD", 1, 100, "80"))
	rows, err := GainsBuckets(ctx, db, 0, gainsTo, "USD", "total", GainsAll, lots.MissingIgnore)
	if err != nil || len(rows) != 1 || rows[0].Sells != 1 || !strings.Contains(rows[0].Quality, "sells_without_documents=1") {
		t.Errorf("rows = %+v err %v", rows, err)
	}
}

// A line with a cost basis but no value leaves its change unknown
// rather than booking the basis as a loss.
func TestGainsLineWithoutValueIsNotALoss(t *testing.T) {
	db, ctx := openMigrated(t)
	edgeExec(t, db, ctx, edgeBase+
		edgePos(1000, "A1", "X", "X", "USD", 10, 1000, "600")+
		strings.Replace(edgePos(200000, "A1", "X", "X", "USD", 10, 0, "600"), ", 0, 600,", ", NULL, 600,", 1))
	r := edgeTotal(t, db, ctx, "USD", GainsAll)[0]
	if r.UnrealizedChange != nil || r.Gain != nil || r.Quality != "basis_changed=1" {
		t.Errorf("change %s gain %s quality %q", show(r.UnrealizedChange), show(r.Gain), r.Quality)
	}
}

// The aggregate share is blank when a holding it should measure cannot
// be converted, as in the coverage view.
func TestGainsBucketCoverageNeedsEveryValue(t *testing.T) {
	db, ctx := openMigrated(t)
	edgeExec(t, db, ctx, edgeBase+
		edgePos(200000, "A1", "X", "X", "USD", 1, 100, "80")+
		edgePos(200000, "A1", "J", "J", "JPY", 1, 1000, "NULL"))
	r := edgeTotal(t, db, ctx, "USD", GainsAll)[0]
	if r.BasisCoverage != nil || !strings.Contains(r.Quality, "fx_missing=1") {
		t.Errorf("coverage %v quality %q, want blank and fx_missing", r.BasisCoverage, r.Quality)
	}
}

// An account that sold out, its sale documented, is not unobserved.
func TestGainsSoldOutAccountIsNotUnobserved(t *testing.T) {
	db, ctx := openMigrated(t)
	edgeExec(t, db, ctx, edgeBase+
		edgePos(1000, "A1", "X", "X", "USD", 10, 1000, "600")+
		edgePos(1000, "A2", "Y", "Y", "USD", 10, 1000, "900")+
		edgePos(200000, "A2", "Y", "Y", "USD", 10, 1000, "900")+`
        INSERT INTO realized_lots (silver_source_id, realized_lot_external_id, account_external_id,
            instrument_external_id, document_kind, tax_year, acquired_various, disposal_date,
            currency, quantity, proceeds, book_value, is_primary) VALUES
            ('test-src', 'R1', 'A1', 'X', 'form_1099b', 1970, FALSE, DATE '1970-02-01', 'USD', 10, 1000, 600, TRUE);`)
	r := edgeTotal(t, db, ctx, "USD", GainsAll)[0]
	if r.Quality != "" || !near(num(t, r.Gain), 0) {
		t.Errorf("gain %s quality %q, want 0 and no flag", show(r.Gain), r.Quality)
	}
}

// Lines of one instrument in two currencies keep their converted sums
// and leave the native ones blank.
func TestGainsMixedCurrenciesLeaveNativeFiguresBlank(t *testing.T) {
	db, ctx := openMigrated(t)
	edgeExec(t, db, ctx, edgeBase+
		edgePos(200000, "A1", "X-USD", "X", "USD", 1, 100, "80")+
		edgePos(200000, "A1", "X-CHF", "X", "CHF", 1, 100, "80"))
	rows, err := GainsPositions(ctx, db, gainsFrom, gainsTo, "USD", lots.MissingIgnore)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v err %v", rows, err)
	}
	r := rows[0]
	if r.MarketValue != nil || r.Currency != nil || r.UnrealizedRatio != nil || !near(num(t, r.ValueOutCcy), 300) {
		t.Errorf("row = %+v", r)
	}
}

// A lot's unrealized gain leaves out its share of the accrued interest,
// stated value or not, so the lots add up to the position.
func TestOpenLotsLeaveOutAccruedInterest(t *testing.T) {
	db, ctx := openMigrated(t)
	edgeExec(t, db, ctx, edgeBase+`
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
            instrument_external_id, asset_class, vehicle, currency, quantity, market_value, book_value,
            accrued_interest, basis_origin, basis_method, basis_fees) VALUES
            ('test-src', 1000, 'A1', 'B', 'B', 'fixed_income', 'bond', 'USD', 2, 1020, 1000, 20, 'stated', 'lots', 'included');
        INSERT INTO position_lots (silver_source_id, snapshot_at, account_external_id, position_key, lot_key,
            currency, quantity, book_value, market_value, basis_origin) VALUES
            ('test-src', 1000, 'A1', 'B', 'L1', 'USD', 1, 500, 510, 'stated'),
            ('test-src', 1000, 'A1', 'B', 'L2', 'USD', 1, 500, NULL, 'stated');`)
	lots, err := OpenLotsAsOf(ctx, db, 2000, "USD", lots.MissingIgnore)
	if err != nil || len(lots) != 2 {
		t.Fatalf("lots = %+v err %v", lots, err)
	}
	if sum := num(t, lots[0].UnrealizedGain) + num(t, lots[1].UnrealizedGain); !near(sum, 0) {
		t.Errorf("Σ lot unrealized = %v, want the position's 0", sum)
	}
}
