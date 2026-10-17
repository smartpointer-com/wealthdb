package gold

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The table holds exactly the config's aliases after each sync.
func TestSyncHoldingAliases(t *testing.T) {
	db, ctx := openMigrated(t)
	rows := []canonical.HoldingAlias{
		{SilverSourceID: "test-src", AccountExternalID: "A", FromKey: "OLD", ToKey: "NEW"},
		{SilverSourceID: "test-src", AccountExternalID: "", FromKey: "X", ToKey: "Y"},
	}
	if n, err := SyncHoldingAliases(ctx, db, rows); err != nil || n != 2 {
		t.Fatalf("sync: %d %v", n, err)
	}
	if n, err := SyncHoldingAliases(ctx, db, rows[:1]); err != nil || n != 1 {
		t.Fatalf("resync: %d %v", n, err)
	}
	var got string
	if err := db.QueryRowContext(ctx, `SELECT string_agg(account_external_id || ':' || from_key || '>' || to_key, ',') FROM holding_aliases()`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "A:OLD>NEW" {
		t.Errorf("aliases = %q", got)
	}
}

// A holding the source keys two ways reads under one key: the overview
// line OLD (no basis) at the bucket's start and the ISIN NEW (a stated
// basis) at its end pair up as one row whose change is a change of
// basis, under both readings; without the alias the zero reading books
// the basis appearing as a loss, and the ignore reading NEW's whole
// gain as the bucket's.
func TestAHoldingAliasPairsAKeyAcrossFeeds(t *testing.T) {
	db, ctx := openGainsFixture(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, first_seen_at, last_seen_at)
            VALUES ('test-src', 'BRK4', 'brokerage', 'Fourth', 1, 1);
        INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, symbol, name, currency, first_seen_at, last_seen_at)
            VALUES ('test-src', 'NEW', 'metal', 'NEW', 'Bars', 'USD', 1, 1);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
                               asset_class, vehicle, currency, quantity, market_value, book_value, basis_origin, basis_method, basis_fees) VALUES
            ('test-src', 200000,  'BRK4', 'OLD', NULL,  'metal', 'physical', 'USD', NULL, 100, NULL, NULL, NULL, NULL),
            ('test-src', 6000000, 'BRK4', 'NEW', 'NEW', 'metal', 'physical', 'USD', 2,    120, 60,   'stated', 'lots', 'included')`); err != nil {
		t.Fatal(err)
	}
	march := `SELECT k, basis_changed, unrealized_change_x FROM gains_windows_all(0, 31535999, 'month', ['USD'], [?])
	           WHERE account_external_id = 'BRK4' AND b_from = 5097600`
	type row struct {
		k       string
		changed bool
		change  *float64
	}
	read := func(reading string) []row {
		rows, err := db.QueryContext(ctx, march, reading)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.k, &r.changed, &r.change); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}
	if got := read("zero"); len(got) != 2 || got[0].change == nil || got[1].change == nil {
		t.Fatalf("without the alias, two rows with a change each: %+v", got)
	}
	if _, err := SyncHoldingAliases(ctx, db, []canonical.HoldingAlias{
		{SilverSourceID: "test-src", AccountExternalID: "BRK4", FromKey: "OLD", ToKey: "NEW"}}); err != nil {
		t.Fatal(err)
	}
	for _, reading := range []string{"ignore", "zero"} {
		got := read(reading)
		if len(got) != 1 || got[0].k != "NEW" || !got[0].changed || got[0].change != nil {
			t.Errorf("%s: %+v, want one NEW row with the basis changed and no change", reading, got)
		}
	}
}

// Two instruments an alias folds into one key, held at one instant
// under different names: the holding reads under the smaller name on
// every read, not whichever line a scan meets first.
func TestAnAliasedHoldingReadsOneName(t *testing.T) {
	db, ctx := openGainsFixture(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, first_seen_at, last_seen_at)
            VALUES ('test-src', 'BRK5', 'brokerage', 'Fifth', 1, 1);
        INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, symbol, name, currency, first_seen_at, last_seen_at)
            VALUES ('test-src', 'ZZ', 'public_equity', 'ZZ', 'Zeta Holdings', 'USD', 1, 1),
                   ('test-src', 'AA', 'public_equity', 'AA', 'Alpha Holdings', 'USD', 1, 1);
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
                               asset_class, vehicle, currency, quantity, market_value, book_value, basis_origin, basis_method, basis_fees) VALUES
            ('test-src', 6000000, 'BRK5', 'ZZ', 'ZZ', 'public_equity', 'stock', 'USD', 1, 10, 5, 'stated', 'lots', 'included'),
            ('test-src', 6000000, 'BRK5', 'AA', 'AA', 'public_equity', 'stock', 'USD', 1, 20, 5, 'stated', 'lots', 'included')`); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncHoldingAliases(ctx, db, []canonical.HoldingAlias{
		{SilverSourceID: "test-src", AccountExternalID: "BRK5", FromKey: "ZZ", ToKey: "AA"}}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		var symbol, name string
		var n int
		if err := db.QueryRowContext(ctx, `SELECT min(symbol), min(name), count(DISTINCT name)
		      FROM gains_windows_all(0, 31535999, 'month', ['USD'], ['ignore'])
		     WHERE account_external_id = 'BRK5' AND at_end`).Scan(&symbol, &name, &n); err != nil {
			t.Fatal(err)
		}
		if symbol != "AA" || name != "Alpha Holdings" || n != 1 {
			t.Fatalf("aliased holding reads %q %q (%d names), want AA Alpha Holdings", symbol, name, n)
		}
	}
}
