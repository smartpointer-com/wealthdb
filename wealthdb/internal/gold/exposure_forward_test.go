package gold

import (
	"database/sql"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// Two FX forwards held directly, as the ubs adapter emits them: each an
// instrument of vehicle forward with no ISIN, and its legs a one-line
// parsed composition on its contract key over its mark. QFW1 is long USD
// 1000 (CHF 800) against CHF 820, marked at CHF -20 (USD -25); QFW0 has
// the same legs at a mark of zero, as a forward no source marks has.
// QFW9's composition has no position, as a contract that matured has
// none. QFND is a fund, held at USD 500 of its USD 1000 net assets, whose one
// line is a forward long EUR 400 (USD 320) against USD. Every name and
// figure is invented.
func TestExposureSplitsAForwardHeldDirectlyIntoItsLegs(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	mustExec(t, db, `INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency, first_seen_at, last_seen_at)
	    VALUES ('test-src', 'A1', 'overlay', 'Overlay', 'CHF', 0, 0)`)
	mustExec(t, db, `INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
	    VALUES ('test-src', ?, 'USD', 'CHF', 1.25), ('test-src', ?, 'EUR', 'USD', 1.25)`, exDay(1, 1), exDay(1, 1))
	for id, mark := range map[string]int{"QFW1": -20, "QFW0": 0} {
		mustExec(t, db, `INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, name, currency,
		    first_seen_at, last_seen_at) VALUES ('test-src', ?, 'foreign_exchange', 'forward', 'FX forward USD/CHF 2026-12-15',
		    'CHF', 0, 0)`, id)
		mustExec(t, db, `INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, market_value) VALUES ('test-src', ?, 'A1', ?, ?, 'foreign_exchange', 'forward', 'CHF', ?)`,
			exSnap, id, id, mark)
	}
	for id, mark := range map[string]int{"QFW1": -20, "QFW0": 0, "QFW9": 10} {
		key := canonical.ContractKey("test-src", id)
		mustExec(t, db, `INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, source_kind, line_count,
		    weight_sum, currency, net_assets, origin) VALUES ('test-src', ?, ?, 'holdings', 'ubs-psn', 'daily_file', 1, 100, 'CHF', ?,
		    'parsed')`, key, exSnap, mark)
		mustExec(t, db, `INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, name, asset_type,
		    weight, market_value, currency, long_currency, long_amount, short_currency, short_amount)
		    VALUES ('test-src', ?, ?, 'holdings', 1, ?, 'FX forward USD/CHF 2026-12-15', 'fx forward', 100, ?, 'CHF',
		    'USD', 1000, 'CHF', -820)`, key, exSnap, canonical.SecurityKey("", "", "", "", "FX forward USD/CHF 2026-12-15"), mark)
	}
	mustExec(t, db, `INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, name, symbol,
	    currency, first_seen_at, last_seen_at) VALUES ('test-src', 'QFND', 'public_equity', 'fund', 'Forward Fund', 'QFND', 'USD', 0, 0)`)
	mustExec(t, db, `INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
	    asset_class, vehicle, currency, market_value) VALUES ('test-src', ?, 'A1', 'QFND', 'QFND', 'public_equity', 'fund', 'USD', 500)`,
		exSnap)
	mustExec(t, db, `INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, source_kind, line_count,
	    weight_sum, currency, net_assets) VALUES ('test-src', 'symbol:QFND', ?, 'holdings', 'test', 'daily_file', 1, 100, 'USD', 1000)`,
		exSnap)
	mustExec(t, db, `INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, name, asset_type,
	    weight, market_value, currency, long_currency, long_amount, short_currency)
	    VALUES ('test-src', 'symbol:QFND', ?, 'holdings', 1, 'symbol:FWD', 'EUR/USD forward', 'fx forward', 100, 1000, 'USD',
	    'EUR', 400, 'USD')`, exSnap)
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}

	leavesIn := func(ccy, key string) (string, float64) {
		t.Helper()
		var s sql.NullString
		var sum sql.NullFloat64
		if err := db.QueryRowContext(ctx, `SELECT string_agg(depth || '|' || how || '|' || asset_class || '|' || currency || '|'
		    || array_to_string(flags, ',') || '=' || round(value, 4), ' ' ORDER BY value DESC), SUM(value)
		    FROM exposure_leaves(?, ?) WHERE top_key = ?`, exAsOf, ccy, key).Scan(&s, &sum); err != nil {
			t.Fatal(err)
		}
		return s.String, sum.Float64
	}
	leavesOf := func(key string) (string, float64) { return leavesIn("USD", key) }
	leaves := func(id string) (string, float64) { return leavesOf(canonical.ContractKey("test-src", id)) }
	// The long leg at notional in its currency, the rest of the mark in
	// the short one: the two sum to the mark.
	if got, sum := leaves("QFW1"); got != "1|looked_through|foreign_exchange|USD|=1000.0 1|looked_through|foreign_exchange|CHF|=-1025.0" ||
		sum < -25.0001 || sum > -24.9999 {
		t.Errorf("QFW1 leaves = %q, sum %v; want the USD leg at 1000 and the CHF leg at -1025, summing to -25", got, sum)
	}
	// A mark of zero splits as well: the long leg at notional, the short
	// leg the same at the day's rate, summing to zero.
	if got, sum := leaves("QFW0"); got != "1|looked_through|foreign_exchange|USD|=1000.0 1|looked_through|foreign_exchange|CHF|=-1000.0" ||
		sum < -0.0001 || sum > 0.0001 {
		t.Errorf("QFW0 leaves = %q, sum %v; want the USD leg at 1000 and the CHF leg at -1000, summing to 0", got, sum)
	}
	// In francs, the long leg is the notional at the day's rate.
	if got, sum := leavesIn("CHF", canonical.ContractKey("test-src", "QFW0")); got != "1|looked_through|foreign_exchange|USD|=800.0 1|looked_through|foreign_exchange|CHF|=-800.0" ||
		sum < -0.0001 || sum > 0.0001 {
		t.Errorf("QFW0 leaves in CHF = %q, sum %v; want the USD leg at 800 and the CHF leg at -800", got, sum)
	}
	// A fund's forward line keeps its share of the fund: half of the EUR
	// 400 (USD 320) notional, the rest of the line in USD.
	if got, sum := leavesOf("symbol:QFND"); got != "1|looked_through|foreign_exchange|USD|=340.0 1|looked_through|foreign_exchange|EUR|=160.0" ||
		sum < 499.9999 || sum > 500.0001 {
		t.Errorf("QFND leaves = %q, sum %v; want the EUR leg at 160 and the USD rest at 340, summing to 500", got, sum)
	}
	// A composition no position holds has no leaf.
	if got, _ := leaves("QFW9"); got != "" {
		t.Errorf("QFW9 leaves = %q, want none", got)
	}

	// The materialized look-through carries the zero-marked forward's
	// legs too, in each reporting currency at the day's rate: in
	// dollars the CHF legs of QFW1 and QFW0 (-1025, -1000) and the USD
	// legs of both and of the fund (1000, 1000, 340); in francs the same
	// at 0.8.
	if _, err := MaterializeExposure(ctx, db, exAsOf, 1); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		out, ccy string
		want     float64
	}{{"USD", "CHF", -2025}, {"USD", "USD", 2340}, {"CHF", "CHF", -1620}, {"CHF", "USD", 1872}} {
		var got sql.NullFloat64
		if err := db.QueryRowContext(ctx, `SELECT SUM(value) FROM report_exposure WHERE currency = ? AND as_of = ?
		    AND asset_class = 'foreign_exchange' AND exposure_currency = ?`, c.out, exAsOf, c.ccy).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got.Float64 < c.want-0.0001 || got.Float64 > c.want+0.0001 {
			t.Errorf("report_exposure in %s, %s legs = %v, want %v", c.out, c.ccy, got.Float64, c.want)
		}
	}

	// The held forwards' legs are held directly, as a forward is no
	// fund; the fund's forward line stays looked through. In dollars the
	// held legs sum to -25 (QFW1's mark, QFW0's zero), the fund's to 500.
	split := func(q string) (direct, via float64) {
		t.Helper()
		if err := db.QueryRowContext(ctx, q, exAsOf).Scan(&direct, &via); err != nil {
			t.Fatal(err)
		}
		return direct, via
	}
	near := func(got, want float64) bool { return got > want-0.0001 && got < want+0.0001 }
	for name, q := range map[string]string{
		"exposure_leaves": `SELECT SUM(value) FILTER (WHERE direct), SUM(value) FILTER (WHERE NOT direct)
		    FROM exposure_leaves(?, 'USD')`,
		"report_exposure": `SELECT SUM(value) FILTER (WHERE NOT via_funds), SUM(value) FILTER (WHERE via_funds)
		    FROM report_exposure WHERE currency = 'USD' AND as_of = ?`,
		"report_exposure_classes": `SELECT value * direct_pct / share_pct, value * via_funds_pct / share_pct
		    FROM report_exposure_classes(?, 'USD') WHERE asset_class = 'foreign_exchange'`,
		"report_exposure_currencies": `SELECT SUM(value * direct_pct / share_pct), SUM(value * via_funds_pct / share_pct)
		    FROM report_exposure_currencies(?, 'USD')`,
		"report_exposure_securities": `SELECT SUM(direct_value), SUM(via_funds_value)
		    FROM report_exposure_securities(?, 'USD')`,
	} {
		if d, v := split(q); !near(d, -25) || !near(v, 500) {
			t.Errorf("%s: direct %v, via funds %v; want -25 and 500", name, d, v)
		}
	}
	// The security slices split the same way.
	if d, v := split(`SELECT SUM(direct_value), SUM(via_funds_value) FROM report_exposure_security_slices
	    WHERE currency = 'USD' AND as_of = ?`); !near(d, -25) || !near(v, 500) {
		t.Errorf("security slices: direct %v, via funds %v; want -25 and 500", d, v)
	}
	var funds0 int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM report_exposure_security_slices
	    WHERE currency = 'USD' AND as_of = ? AND list_contains(funds, ?)`, exAsOf,
		canonical.ContractKey("test-src", "QFW1")).Scan(&funds0); err != nil || funds0 != 0 {
		t.Errorf("%d slices name a held forward among their funds (%v)", funds0, err)
	}
	// The funds list keeps the fund and leaves the held forwards among
	// the other holdings.
	funds := map[string]float64{}
	frows, err := db.QueryContext(ctx, `SELECT fund, value FROM report_exposure_funds(?, 'USD')`, exAsOf)
	if err != nil {
		t.Fatal(err)
	}
	for frows.Next() {
		var f string
		var v float64
		if err := frows.Scan(&f, &v); err != nil {
			t.Fatal(err)
		}
		funds[f] = v
	}
	frows.Close()
	if len(funds) != 2 || !near(funds["symbol:QFND"], 500) || !near(funds["(other holdings)"], -25) {
		t.Errorf("funds = %v, want the fund at 500 and the forwards' marks among the other holdings", funds)
	}

	// Migrations 0173 and 0175 replay: the macros read the same.
	zero, _ := leaves("QFW0")
	for _, m := range []string{"0173_forward_legs_at_notional.sql", "0175_forward_held_directly.sql"} {
		rerunMigrationDDL(t, db, ctx, m)
		if got, _ := leaves("QFW0"); got != zero {
			t.Errorf("QFW0 leaves after replaying %s = %q, want %q", m, got, zero)
		}
	}
	if d, v := split(`SELECT SUM(value) FILTER (WHERE direct), SUM(value) FILTER (WHERE NOT direct)
	    FROM exposure_leaves(?, 'USD')`); !near(d, -25) || !near(v, 500) {
		t.Errorf("after the replays: direct %v, via funds %v; want -25 and 500", d, v)
	}

	// A forward its legs describe is no private holding a declaration
	// should describe.
	rows, err := ExposureCoverage(ctx, db, exAsOf, "USD")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Section == "undeclared" {
			t.Errorf("coverage lists %s %v under undeclared", r.Reason, r.Instrument)
		}
	}
}
