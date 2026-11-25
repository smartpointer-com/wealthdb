package gold

import (
	"database/sql"
	"slices"
	"testing"
)

// seedHeldCurrencies puts an amount of each kind into gold: a position
// in pos, a cash balance in cash and a booked line in each of txns.
func seedHeldCurrencies(t *testing.T, db *sql.DB, pos, cash string, txns ...string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency, first_seen_at, last_seen_at)
	     VALUES ('test-src', 'ACC1', 'brokerage', 'Brokerage', 'CHF', 0, 0)`)
	if pos != "" {
		mustExec(t, db, `INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, name, currency, first_seen_at, last_seen_at)
		     VALUES ('test-src', 'IDXF', 'public_equity', 'etf', 'IDXF', 'Index Fund', ?, 0, 0)`, pos)
		mustExec(t, db, `INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		     asset_class, vehicle, currency, quantity, market_value)
		     VALUES ('test-src', 1728000000, 'ACC1', 'IDXF', 'IDXF', 'public_equity', 'etf', ?, 10, 1000)`, pos)
	}
	if cash != "" {
		mustExec(t, db, `INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount)
		     VALUES ('test-src', 1728000000, 'ACC1', ?, 'current', 250)`, cash)
	}
	for _, ccy := range txns {
		mustExec(t, db, `INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
		     account_external_id, kind, currency, net_amount, description)
		     VALUES ('test-src', ?, 1728000000, 'ACC1', 'purchase', ?, -40, 'a purchase')`, "T-"+ccy, ccy)
	}
}

// Unconvertible names the currencies of positions, cash and booked
// lines that the reports' conversion finds no rate for, and no other:
// each verdict is the one fxConvert, the report macros' conversion,
// gives for a line on a real day.
func TestUnconvertible(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	held := []string{"EUR", "GBP", "JPY", "USD"}
	// The line's day, long before the day of every rate below: a rate on
	// a later day still converts it (fx_daily's floor).
	const lineDay, rateAt = 20000, 25000 * 86400
	check := func(what, target string, want ...string) {
		t.Helper()
		got, err := Unconvertible(ctx, db, target)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: Unconvertible(%s) = %v, want %v", what, target, got, want)
		}
		for _, c := range held {
			if _, ok := fxConvert(t, db, lineDay, 100, c, target); ok == slices.Contains(got, c) {
				t.Errorf("%s: the reports convert %s to %s is %v, and Unconvertible lists it is %v", what, c, target, ok, !ok)
			}
		}
	}

	// Nothing held: nothing is missing.
	if got, err := Unconvertible(ctx, db, "CHF"); err != nil || len(got) != 0 {
		t.Errorf("an empty gold: Unconvertible = %v, %v, want none", got, err)
	}

	seedHeldCurrencies(t, db, "USD", "EUR", "JPY", "GBP", "USD")
	check("no rate", "CHF", "EUR", "GBP", "JPY", "USD")
	check("no rate, the report in a held currency", "USD", "EUR", "GBP", "JPY")

	// A rate of zero is no rate (fx_norm leaves it out).
	seedFX(t, db, rateAt, "CHF", "USD", "0")
	check("a zero rate", "CHF", "EUR", "GBP", "JPY", "USD")

	// One stored row gives both directions.
	seedFX(t, db, rateAt, "USD", "CHF", "1.25")
	check("USD and CHF", "CHF", "EUR", "GBP", "JPY")
	check("USD and CHF, the report in USD", "USD", "EUR", "GBP", "JPY")

	// EUR reaches CHF through USD, and GBP reaches USD through CHF.
	seedFX(t, db, rateAt, "USD", "EUR", "1.1")
	check("EUR through USD", "CHF", "GBP", "JPY")
	seedFX(t, db, rateAt, "CHF", "GBP", "1.15")
	check("GBP direct", "CHF", "JPY")
	check("GBP through CHF", "USD", "JPY")

	// The conversion bridges once, through CHF or USD. GBP has a rate
	// to CHF alone, and CHF none to EUR, so GBP does not reach EUR. A
	// rate from JPY to EUR alone reaches EUR and nothing else.
	check("GBP to CHF alone, the report in EUR", "EUR", "GBP", "JPY")
	seedFX(t, db, rateAt, "EUR", "JPY", "0.006")
	check("JPY to EUR alone", "CHF", "JPY")
	check("JPY to EUR alone, the report in EUR", "EUR", "GBP")
	seedFX(t, db, rateAt, "USD", "JPY", "0.0065")
	check("every currency", "CHF")
}

// TestUnconvertibleReadsEachTable: a currency that only one of the
// three tables holds is named.
func TestUnconvertibleReadsEachTable(t *testing.T) {
	t.Parallel()
	for name, seed := range map[string][]string{
		"positions": {"SEK", "", ""}, "cash_balances": {"", "SEK", ""}, "transactions": {"", "", "SEK"},
	} {
		db, ctx := openMigrated(t)
		txns := []string{}
		if seed[2] != "" {
			txns = append(txns, seed[2])
		}
		seedHeldCurrencies(t, db, seed[0], seed[1], txns...)
		if got, err := Unconvertible(ctx, db, "CHF"); err != nil || !slices.Equal(got, []string{"SEK"}) {
			t.Errorf("%s alone: Unconvertible = %v, %v, want [SEK]", name, got, err)
		}
	}
}
