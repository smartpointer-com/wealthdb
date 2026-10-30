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
// the same legs at a mark of zero. QFW9's composition has no position,
// as a contract that matured has none. Every name and figure is
// invented.
func TestExposureSplitsAForwardHeldDirectlyIntoItsLegs(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	mustExec(t, db, `INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency, first_seen_at, last_seen_at)
	    VALUES ('test-src', 'A1', 'overlay', 'Overlay', 'CHF', 0, 0)`)
	mustExec(t, db, `INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
	    VALUES ('test-src', ?, 'USD', 'CHF', 1.25)`, exDay(1, 1))
	for id, mark := range map[string]int{"QFW1": -20, "QFW0": 0} {
		mustExec(t, db, `INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, name, currency,
		    first_seen_at, last_seen_at) VALUES ('test-src', ?, 'foreign_exchange', 'forward', 'FX forward USD/CHF 2026-12-15',
		    'CHF', 0, 0)`, id)
		mustExec(t, db, `INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, market_value) VALUES ('test-src', ?, 'A1', ?, ?, 'foreign_exchange', 'forward', 'CHF', ?)`,
			exSnap, id, id, mark)
	}
	for id, mark := range map[string]int{"QFW1": -20, "QFW0": 0, "QFW9": 10} {
		key := canonical.ContractKey(id)
		mustExec(t, db, `INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, source_kind, line_count,
		    weight_sum, currency, net_assets, origin) VALUES ('test-src', ?, ?, 'holdings', 'ubs-psn', 'daily_file', 1, 100, 'CHF', ?,
		    'parsed')`, key, exSnap, mark)
		mustExec(t, db, `INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, name, asset_type,
		    weight, market_value, currency, long_currency, long_amount, short_currency, short_amount)
		    VALUES ('test-src', ?, ?, 'holdings', 1, ?, 'FX forward USD/CHF 2026-12-15', 'fx forward', 100, ?, 'CHF',
		    'USD', 1000, 'CHF', -820)`, key, exSnap, canonical.SecurityKey("", "", "", "", "FX forward USD/CHF 2026-12-15"), mark)
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}

	leaves := func(id string) (string, float64) {
		t.Helper()
		var s sql.NullString
		var sum sql.NullFloat64
		if err := db.QueryRowContext(ctx, `SELECT string_agg(depth || '|' || how || '|' || asset_class || '|' || currency || '|'
		    || array_to_string(flags, ',') || '=' || round(value, 4), ' ' ORDER BY value DESC), SUM(value)
		    FROM exposure_leaves(?, 'USD') WHERE top_key = ?`, exAsOf, canonical.ContractKey(id)).Scan(&s, &sum); err != nil {
			t.Fatal(err)
		}
		return s.String, sum.Float64
	}
	// The long leg at notional in its currency, the rest of the mark in
	// the short one: the two sum to the mark.
	if got, sum := leaves("QFW1"); got != "1|looked_through|foreign_exchange|USD|=1000.0 1|looked_through|foreign_exchange|CHF|=-1025.0" ||
		sum < -25.0001 || sum > -24.9999 {
		t.Errorf("QFW1 leaves = %q, sum %v; want the USD leg at 1000 and the CHF leg at -1025, summing to -25", got, sum)
	}
	// A mark of zero leaves the line unsplit and partial.
	if got, _ := leaves("QFW0"); got != "1|looked_through|foreign_exchange|CHF|partial=0.0" {
		t.Errorf("QFW0 leaves = %q, want one partial CHF leaf at 0", got)
	}
	// A composition no position holds has no leaf.
	if got, _ := leaves("QFW9"); got != "" {
		t.Errorf("QFW9 leaves = %q, want none", got)
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
