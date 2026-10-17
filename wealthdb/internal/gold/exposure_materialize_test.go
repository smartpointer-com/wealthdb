package gold

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"
)

// The materialized look-through at every instant is the live one: its
// rows sum to holdings global and split by class and size exactly as the
// sizes view does, in US dollars at every instant and in every
// reporting currency at the last. The run reaches past the fixture's
// month end, so it writes several instants.
func TestMaterializeExposureMatchesTheLiveViews(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	// A stock with a market cap, so the sizes have a size to carry, a
	// holding of each situs basis, and an account in a tax wrapper beside
	// the fixture's, which state none.
	seedSitus(t, db, ctx)
	ex := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	ex(`INSERT INTO security_facts (silver_source_id, security_key, field, as_of, provider, value)
	    VALUES ('refdata', 'isin:XX0000000001', 'market_cap', 0, 'testfact', '50000000000')`)
	ex(`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency, tax_wrapper,
	    first_seen_at, last_seen_at) VALUES ('test-src', 'A3', 'brokerage', 'Wrapped', 'USD', 'roth_ira', 0, 0)`)
	ex(`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
	    asset_class, vehicle, currency, quantity, market_value)
	    VALUES ('test-src', ?, 'A3', 'FUNDX', 'FUNDX', 'public_equity', 'etf', 'USD', 1, 300)`, exSnap)
	ex(`INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount)
	    VALUES ('test-src', ?, 'A3', 'USD', 'current', 40)`, exSnap)
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	to := exAsOf + 45*86400
	n, err := MaterializeExposure(ctx, db, to, 42)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no rows materialized")
	}
	first, _, err := FirstHoldingsAt(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	instants := PeriodEnds(first, to, 1)
	if len(instants) < 3 {
		t.Fatalf("instants %v: the test needs several", instants)
	}
	var stored int
	if err := db.QueryRowContext(ctx, `SELECT count(DISTINCT as_of) FROM report_exposure`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != len(instants) {
		t.Errorf("%d instants stored, want %d", stored, len(instants))
	}
	for i, at := range instants {
		ccys := []string{"USD"}
		if i == len(instants)-1 {
			ccys = MaterializedCurrencies()
		}
		for _, ccy := range ccys {
			checkMaterializedInstant(t, ctx, db, at, ccy)
		}
		// Every row of an instant names its holding's source, and the
		// sources' values add up to the whole: the dashboard's pickers
		// narrow by them.
		var unsourced int
		var whole, bySource float64
		if err := db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE silver_source_id IS NULL), COALESCE(SUM(value), 0),
		    COALESCE((SELECT SUM(s) FROM (SELECT SUM(value) AS s FROM web_exposure WHERE currency = 'USD'
		                                   AND as_of_day = CAST(epoch_ms(? * 1000) AS DATE) GROUP BY silver_source_id)), 0)
		    FROM web_exposure WHERE currency = 'USD' AND as_of_day = CAST(epoch_ms(? * 1000) AS DATE)`,
			at, at).Scan(&unsourced, &whole, &bySource); err != nil {
			t.Fatal(err)
		}
		if unsourced > 0 || math.Abs(whole-bySource) > 1e-6 {
			t.Errorf("at %d: %d rows without a source; by source %.6f, whole %.6f", at, unsourced, bySource, whole)
		}
		checkMaterializedWrappers(t, ctx, db, at)
	}
	var sized int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM web_exposure WHERE size = 'large'`).Scan(&sized); err != nil ||
		sized == 0 {
		t.Errorf("no large row materialized (%v)", err)
	}
	// A second run rewrites the table, it does not add to it.
	m, err := MaterializeExposure(ctx, db, to, 43)
	if err != nil || m != n {
		t.Errorf("second run: %d rows, %v; want %d", m, err, n)
	}
}

// checkMaterializedInstant compares one instant and currency of
// report_exposure with holdings global, the sizes view and the situs
// view.
func checkMaterializedInstant(t *testing.T, ctx context.Context, db *sql.DB, at int64, ccy string) {
	t.Helper()
	g, err := GlobalAsOf(ctx, db, at, ccy)
	if err != nil {
		t.Fatal(err)
	}
	var total sql.NullFloat64
	if err := db.QueryRowContext(ctx, `SELECT SUM(value) FROM report_exposure WHERE currency = ? AND as_of = ?`,
		ccy, at).Scan(&total); err != nil {
		t.Fatal(err)
	}
	// A currency no rate reaches has no net worth and no rows.
	if g.TotalValueOutCcy == nil {
		if total.Valid {
			t.Errorf("%s at %d: materialized %.4f with no holdings global", ccy, at, total.Float64)
		}
		return
	}
	if want := parse(t, g.TotalValueOutCcy); math.Abs(total.Float64-want) > 0.01 {
		t.Errorf("%s at %d: materialized %.4f, holdings global %.4f", ccy, at, total.Float64, want)
	}
	sizes, err := ExposureSizes(ctx, db, at, ccy)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range sizes {
		var v float64
		if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(value), 0) FROM web_exposure
		    WHERE currency = ? AND as_of_day = CAST(epoch_ms(? * 1000) AS DATE) AND asset_class = ? AND size = ?`,
			ccy, at, r.AssetClass, r.Size).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if want := parse(t, r.Value); math.Abs(v-want) > 0.01 {
			t.Errorf("%s %s %s at %d: materialized %.4f, sizes view %.4f", ccy, r.AssetClass, r.Size, at, v, want)
		}
	}
	situs, err := ExposureSitus(ctx, db, at, ccy)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range situs {
		var v float64
		if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(value), 0) FROM web_exposure
		    WHERE currency = ? AND as_of_day = CAST(epoch_ms(? * 1000) AS DATE) AND situs = ?`,
			ccy, at, r.Situs).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if want := parse(t, r.Value); math.Abs(v-want) > 0.01 {
			t.Errorf("%s situs %s at %d: materialized %.4f, situs view %.4f", ccy, r.Situs, at, v, want)
		}
	}
}

// checkMaterializedWrappers compares the US-dollar value of each tax
// wrapper in web_exposure at one instant with its accounts' totals, an
// account that states no wrapper as "-".
func checkMaterializedWrappers(t *testing.T, ctx context.Context, db *sql.DB, at int64) {
	t.Helper()
	accounts, err := AccountsAsOf(ctx, db, at, "USD")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{}
	for _, a := range accounts {
		if a.TotalValueOutCcy != nil {
			want[fmtStrPtr(a.TaxWrapper)] += parse(t, a.TotalValueOutCcy)
		}
	}
	if want["roth_ira"] == 0 || want["-"] == 0 {
		t.Fatalf("at %d: accounts by wrapper %v; the test needs a wrapped and an unwrapped one", at, want)
	}
	got := map[string]float64{}
	if err := scanAll(ctx, db, `SELECT COALESCE(tax_wrapper, '-'), SUM(value) FROM web_exposure
	    WHERE currency = 'USD' AND as_of_day = CAST(epoch_ms(? * 1000) AS DATE) GROUP BY 1`, func(rows *sql.Rows) error {
		var w string
		var v float64
		if err := rows.Scan(&w, &v); err != nil {
			return err
		}
		got[w] = v
		return nil
	}, at); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Errorf("at %d: wrappers %v, accounts give %v", at, got, want)
	}
	for w, v := range want {
		if math.Abs(got[w]-v) > 0.01 {
			t.Errorf("at %d: wrapper %s materialized %.4f, its accounts %.4f", at, w, got[w], v)
		}
	}
}

// An empty gold materializes nothing.
func TestMaterializeExposureOnAnEmptyGold(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	n, err := MaterializeExposure(ctx, db, exAsOf, 1)
	if err != nil || n != 0 {
		t.Errorf("empty gold: %d rows, %v", n, err)
	}
}

// Cash counts as a holding: a gold that holds cash alone starts its
// history at the cash's first snapshot.
func TestFirstHoldingsAtCountsCash(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	if _, ok, err := FirstHoldingsAt(ctx, db); err != nil || ok {
		t.Fatalf("empty gold: ok %v, %v", ok, err)
	}
	cashAt := time.Date(2025, 3, 31, 0, 0, 0, 0, time.UTC).Unix()
	if _, err := db.ExecContext(ctx, `INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id,
	    currency, balance_kind, amount) VALUES ('s', ?, 'a', 'USD', 'ledger', 10)`, cashAt); err != nil {
		t.Fatal(err)
	}
	if at, ok, err := FirstHoldingsAt(ctx, db); err != nil || !ok || at != cashAt {
		t.Errorf("cash only: %d, ok %v, %v; want %d", at, ok, err, cashAt)
	}
}

// 0127, 0130 and 0131 each replay safely. 0131 re-creates web_exposure
// last, so it is replayed after an earlier one, as Migrate would.
func TestMigrations0127To0131DDLAreRerunnable(t *testing.T) {
	t.Parallel()
	for _, replay := range [][]string{
		{"0127_exposure_dashboard.sql", "0131_exposure_unknown.sql"},
		{"0130_exposure_filters.sql", "0131_exposure_unknown.sql"},
		{"0131_exposure_unknown.sql"},
	} {
		t.Run(replay[0], func(t *testing.T) {
			t.Parallel()
			db, ctx := openExposureFixture(t)
			if _, err := MaterializeExposure(ctx, db, exAsOf, 1); err != nil {
				t.Fatal(err)
			}
			for _, file := range replay {
				rerunMigrationDDL(t, db, ctx, file)
			}
			var n int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM web_exposure WHERE size = 'unknown'`).Scan(&n); err != nil ||
				n == 0 {
				t.Errorf("after replay: %d sized rows, err %v", n, err)
			}
		})
	}
}
