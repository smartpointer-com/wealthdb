package gold

import (
	"cmp"
	"context"
	"database/sql"
	"math"
	"slices"
	"strconv"
	"strings"
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
	mustExec(t, db, `INSERT INTO security_facts (silver_source_id, security_key, field, as_of, provider, value)
	    VALUES ('refdata', 'isin:XX0000000001', 'market_cap', 0, 'testfact', '50000000000')`)
	mustExec(t, db, `INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency, tax_wrapper,
	    first_seen_at, last_seen_at) VALUES ('test-src', 'A3', 'brokerage', 'Wrapped', 'USD', 'roth_ira', 0, 0)`)
	mustExec(t, db, `INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
	    asset_class, vehicle, currency, quantity, market_value)
	    VALUES ('test-src', ?, 'A3', 'FUNDX', 'FUNDX', 'public_equity', 'etf', 'USD', 1, 300)`, exSnap)
	mustExec(t, db, `INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount)
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
	// The securities hold the recent instants alone, the last in every
	// currency: summed as the card sums them they are the securities
	// view, and narrowed by source, wrapper or class they are what the
	// other cards read.
	recent := yieldInstants(to)
	var held []int64
	if err := scanAll(ctx, db, `SELECT DISTINCT as_of FROM report_exposure_security_slices ORDER BY 1`,
		func(rows *sql.Rows) error {
			var at int64
			if err := rows.Scan(&at); err != nil {
				return err
			}
			held = append(held, at)
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	stray := slices.DeleteFunc(slices.Clone(held), func(at int64) bool { return slices.Contains(recent, at) })
	if len(held) < 2 || held[len(held)-1] != to || len(stray) > 0 {
		t.Errorf("securities at %v; want several of %v, the last %d", held, recent, to)
	}
	for _, ccy := range MaterializedCurrencies() {
		n := checkMaterializedSecurities(t, ctx, db, to, ccy)
		if n > 0 {
			checkSecuritySlicesNarrow(t, db, to, ccy)
		} else if ccy == "USD" {
			t.Error("no securities in US dollars; the test needs some")
		}
	}
	if checkMaterializedSecurities(t, ctx, db, held[0], "USD") == 0 {
		t.Errorf("no securities at %d", held[0])
	}
	// A security reached through two funds is one row, its funds both.
	if got := scanStrings(t, db, exposureSecuritiesCardSQL, to, "USD"); !slices.ContainsFunc(got, func(r string) bool {
		return strings.HasPrefix(r, "symbol:CNG|") && strings.Contains(r, "|2|")
	}) {
		t.Errorf("no security reached through two funds: %v", got)
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
// wrapper in web_exposure at one instant with its accounts' totals. An
// account that states no wrapper reads taxable_personal, as on the
// holdings dashboards.
func checkMaterializedWrappers(t *testing.T, ctx context.Context, db *sql.DB, at int64) {
	t.Helper()
	accounts, err := AccountsAsOf(ctx, db, at, "USD")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{}
	unwrapped := false
	for _, a := range accounts {
		if a.TotalValueOutCcy == nil {
			continue
		}
		w := "taxable_personal"
		if a.TaxWrapper != nil {
			w = *a.TaxWrapper
		} else {
			unwrapped = true
		}
		want[w] += parse(t, a.TotalValueOutCcy)
	}
	if want["roth_ira"] == 0 || !unwrapped {
		t.Fatalf("at %d: accounts by wrapper %v; the test needs a wrapped and an unwrapped one", at, want)
	}
	got := map[string]float64{}
	if err := scanAll(ctx, db, `SELECT tax_wrapper, SUM(value) FROM web_exposure
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

// 0127, 0130, 0131 and 0133 each replay safely, and forward to the head
// web_exposure.
func TestMigrations0127To0133DDLAreRerunnable(t *testing.T) {
	t.Parallel()
	for _, file := range []string{"0127_exposure_dashboard.sql", "0130_exposure_filters.sql",
		"0131_exposure_unknown.sql", "0133_deterministic_labels.sql"} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			db, ctx := openExposureFixture(t)
			if _, err := MaterializeExposure(ctx, db, exAsOf, 1); err != nil {
				t.Fatal(err)
			}
			rerunMigrationDDL(t, db, ctx, file)
			var n int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM web_exposure WHERE size = 'unknown'`).Scan(&n); err != nil ||
				n == 0 {
				t.Errorf("after replay: %d sized rows, err %v", n, err)
			}
		})
	}
}

// exposureSecuritiesCardSQL sums a security's slices at one instant and
// currency, as the Exposure dashboard's Top securities card sums those
// of its selection with nothing picked: each label of its heaviest
// slice, the greatest name, ticker, market cap and size, the values
// summed, the distinct funds counted, and its share of their total.
const exposureSecuritiesCardSQL = `
WITH g AS (
    SELECT security, MAX(name) AS name, MAX(ticker) AS ticker,
           arg_max(asset_class, (class_weight, asset_class)) AS asset_class,
           arg_max(sector, (sector_weight, sector)) AS sector,
           arg_max(country, (country_weight, country)) AS country,
           MAX(market_cap) AS market_cap, MAX(size) AS size, SUM(value) AS value,
           SUM(direct_value) AS direct_value, SUM(via_funds_value) AS via_funds_value,
           len(list_distinct(flatten(list(funds)))) AS funds
      FROM report_exposure_security_slices WHERE as_of = ? AND currency = ? GROUP BY security)
SELECT concat_ws('|', security, COALESCE(name, '-'), COALESCE(ticker, '-'), COALESCE(asset_class, '-'),
                 COALESCE(sector, '-'), COALESCE(country, '-'), COALESCE(printf('%.0f', market_cap), '-'),
                 COALESCE(size, '-'), printf('%.4f', value), printf('%.4f', direct_value),
                 printf('%.4f', via_funds_value), funds::VARCHAR,
                 COALESCE(printf('%.2f', 100 * value / NULLIF(SUM(value) OVER (), 0)), '-'))
  FROM g
 ORDER BY round(abs(value), 4) DESC, security`

// checkMaterializedSecurities compares one instant and currency of
// report_exposure_security_slices, summed as the card sums them, with
// every row of the securities view, in its order: the largest first,
// two of one value to the cent by their key. It returns the number of
// rows, none in a currency no rate reaches.
func checkMaterializedSecurities(t *testing.T, ctx context.Context, db *sql.DB, at int64, ccy string) int {
	t.Helper()
	rows, err := ExposureSecurities(ctx, db, at, ccy, "risk")
	if err != nil {
		t.Fatal(err)
	}
	slices.SortStableFunc(rows, func(a, b ExposureSecurityRow) int {
		va, vb := math.Round(math.Abs(parse(t, a.Value))*1e4), math.Round(math.Abs(parse(t, b.Value))*1e4)
		if va != vb {
			return cmp.Compare(vb, va)
		}
		return cmp.Compare(a.Security, b.Security)
	})
	or := func(s *string) string {
		if s == nil {
			return "-"
		}
		return *s
	}
	num := func(s *string, prec int) string {
		if s == nil {
			return "-"
		}
		return strconv.FormatFloat(parse(t, s), 'f', prec, 64)
	}
	var want []string
	for _, r := range rows {
		share := "-"
		if r.SharePct != nil {
			share = strconv.FormatFloat(*r.SharePct, 'f', 2, 64)
		}
		want = append(want, strings.Join([]string{r.Security, or(r.Name), or(r.Ticker), or(r.AssetClass), or(r.Sector),
			or(r.Country), num(r.MarketCap, 0), or(r.Size), num(r.Value, 4), num(r.DirectValue, 4),
			num(r.ViaFundsValue, 4), strconv.FormatInt(r.Funds, 10), share}, "|"))
	}
	got := scanStrings(t, db, exposureSecuritiesCardSQL, at, ccy)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s at %d: the slices give\n%s\nthe securities view\n%s", ccy, at,
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	return len(want)
}

// checkSecuritySlicesNarrow compares the value of each source, tax
// wrapper and asset class in report_exposure_security_slices at one
// instant with report_exposure's, so a picker narrows the securities
// card to the selection the other Exposure cards read.
func checkSecuritySlicesNarrow(t *testing.T, db *sql.DB, at int64, ccy string) {
	t.Helper()
	q := `SELECT concat_ws('|', COALESCE(silver_source_id, '-'), COALESCE(tax_wrapper, '-'), COALESCE(asset_class, '-'),
	             printf('%.4f', SUM(value)))
	        FROM %s WHERE as_of = ? AND currency = ? GROUP BY silver_source_id, tax_wrapper, asset_class ORDER BY 1`
	slices := scanStrings(t, db, strings.Replace(q, "%s", "report_exposure_security_slices", 1), at, ccy)
	exposure := scanStrings(t, db, strings.Replace(q, "%s", "report_exposure", 1), at, ccy)
	if len(slices) == 0 || strings.Join(slices, " ") != strings.Join(exposure, " ") {
		t.Errorf("%s at %d: slices by selection %v, report_exposure %v", ccy, at, slices, exposure)
	}
}

// 0165 replays over a materialized gold and keeps its slices.
func TestMigration0165DDLIsRerunnable(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	if _, err := MaterializeExposure(ctx, db, exAsOf, 1); err != nil {
		t.Fatal(err)
	}
	rerunMigrationDDL(t, db, ctx, "0165_exposure_securities_dashboard.sql")
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM web_exposure_securities WHERE tax_wrapper = 'taxable_personal'`).
		Scan(&n); err != nil || n == 0 {
		t.Errorf("after replay: %d slices, err %v", n, err)
	}
}
