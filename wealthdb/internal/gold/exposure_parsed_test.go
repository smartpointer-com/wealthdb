package gold

import (
	"context"
	"database/sql"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// seedParsedFund adds QPMF, a private fund of 100 held under an ISIN,
// and two compositions a source parsed from its reports: on 1 July,
// bound to 1 October (the next report, which gave none), and on 1
// November, open. Each has two lines, at 75 and 50 of the fund: a line
// below zero the source left out lifts them to 125. The July report's
// split is a parsed record too. Every name and figure is invented.
func seedParsedFund(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	jul, oct, nov := exDay(7, 1), exDay(10, 1), exDay(11, 1)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, isin, name, currency,
		  first_seen_at, last_seen_at) VALUES ('test-src', 'QPMF', 'private_equity', 'fund', 'XX00000000P1',
		  'Example Feeder Fund', 'USD', 0, 0)`, nil},
		{`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		  asset_class, vehicle, currency, quantity, market_value) VALUES ('test-src', ?, 'A1', 'QPMF', 'QPMF',
		  'private_equity', 'fund', 'USD', 1, 100)`, []any{exSnap}},
		{`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count, currency,
		  origin, valid_until) VALUES ('test-src', 'isin:XX00000000P1', ?, 'holdings', 'ubs-report', 2, 'USD',
		  'parsed', ?), ('test-src', 'isin:XX00000000P1', ?, 'holdings', 'ubs-report', 2, 'USD', 'parsed', NULL)`,
			[]any{jul, oct, nov}},
		{`INSERT INTO exposure_reports (silver_source_id, instrument_external_id, as_of, dimensions)
		  VALUES ('test-src', 'QPMF', ?, '{"region":{"europe":100}}')`, []any{jul}},
	} {
		if _, err := db.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	for _, day := range []int64{jul, nov} {
		for i, l := range []struct {
			name, region string
			sector       any
			weight       float64
		}{{"Investment A", "north_america", "technology", 75}, {"Investment B", "europe", nil, 50}} {
			if _, err := db.ExecContext(ctx, `INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no,
			    security_key, name, weight, region, sector) VALUES ('test-src', 'isin:XX00000000P1', ?, 'holdings', ?, ?,
			    ?, ?, ?, ?)`, day, i+1, canonical.SecurityKey("", "", "", "", l.name), l.name, l.weight, l.region,
				l.sector); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// parsedLeaves reads QPMF's leaves on day as depth|how|region|class|
// sector|currency=value, in that order.
func parsedLeaves(t *testing.T, db *sql.DB, ctx context.Context, day int64) string {
	t.Helper()
	var s sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT string_agg(depth || '|' || how || '|' || COALESCE(region, '-') || '|'
	    || asset_class || '|' || COALESCE(sector, '-') || '|' || COALESCE(currency, '-') || '=' || round(value, 6), ' '
	    ORDER BY depth, how, region, value)
	    FROM exposure_leaves(?, 'USD') WHERE iid = 'QPMF'`, day+86399).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s.String
}

// A parsed composition describes its fund from its day to the next
// report's: the days before it read the declaration, as does a quarter
// whose report gave no composition, and a ledger record of the same or a
// later day wins over it. Its lines carry their region and sector; the
// class, sector and currency the declaration in force states whole fill
// in the rest. The weights above 100 leave a negative remainder that
// reads as cash in the fund's currency.
func TestExposureParsedComposition(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	seedParsedFund(t, db, ctx)
	nov := exDay(11, 1)
	stampDeclarations(t, db, ctx,
		declaration(3, "QPMF", nil, split{"country", []string{"CH"}, []float64{100}},
			split{"currency", []string{"EUR"}, []float64{100}}, split{"sector", []string{"financials"}, []float64{100}},
			split{"asset_class", []string{"private_debt"}, []float64{100}}),
		declaration(4, "QPMF", &nov, split{"country", []string{"FR"}, []float64{100}}))
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	lines := "1|looked_through|europe|private_debt|financials|EUR=50.0 " +
		"1|looked_through|north_america|private_debt|technology|EUR=75.0 " +
		"1|remainder|-|cash|cash|USD=-25.0"
	hand := "0|declared|europe|private_debt|financials|EUR=100.0"
	// Before the first report the undated record holds; in October the
	// July record, as the October report gave no composition; in November
	// the ledger's record of the report's day. Both ledger records state
	// a country in Europe.
	for day, want := range map[int64]string{
		exDay(6, 30):  hand,
		exDay(7, 15):  lines,
		exDay(10, 15): hand,
		exDay(11, 15): "0|declared|europe|private_equity|diversified|USD=100.0",
	} {
		if got := parsedLeaves(t, db, ctx, day); got != want {
			t.Errorf("on %s:\n got %s\nwant %s", time.Unix(day, 0).UTC().Format(time.DateOnly), got, want)
		}
	}
	var countries string
	if err := db.QueryRowContext(ctx, `SELECT string_agg(COALESCE(country, '-'), ',' ORDER BY d)
	    FROM (SELECT ? AS d UNION ALL SELECT ?) days, LATERAL (SELECT country FROM exposure_leaves(days.d, 'USD')
	          WHERE iid = 'QPMF')`, exDay(6, 30)+86399, exDay(11, 15)+86399).Scan(&countries); err != nil {
		t.Fatal(err)
	}
	if countries != "CH,FR" {
		t.Errorf("the ledger records read %q, want CH then FR", countries)
	}

	// The lines sum to 125, beyond the partial bound: the cash leaf is
	// flagged, the lines are not.
	var flags string
	if err := db.QueryRowContext(ctx, `SELECT string_agg(kind || ':' || array_to_string(flags, ','), ' ' ORDER BY kind, value)
	    FROM exposure_leaves(?, 'USD') WHERE iid = 'QPMF'`, exDay(7, 15)+86399).Scan(&flags); err != nil ||
		flags != "line: line: remainder:partial" {
		t.Errorf("July flags = %q (%v)", flags, err)
	}
	// The securities view lists each line by its name.
	secs, err := ExposureSecurities(ctx, db, exDay(7, 15)+86399, "USD", "risk")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, r := range secs {
		if r.Security == canonical.SecurityKey("", "", "", "", "Investment A") {
			found++
			if v := parse(t, r.Value); math.Abs(v-75) > 1e-6 {
				t.Errorf("Investment A = %v, want 75", v)
			}
		}
	}
	if found != 1 {
		t.Errorf("Investment A has %d securities rows, want 1", found)
	}
	// The funds view reads the composition in July, the declaration in
	// November.
	for day, want := range map[int64]string{exDay(7, 15): "direct|125", exDay(11, 15): "declared|-"} {
		funds, err := ExposureFunds(ctx, db, day+86399, "USD")
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		for _, f := range funds {
			if f.Fund == "isin:XX00000000P1" {
				got = fmtStrPtr(f.Via) + "|"
				if f.WeightSum != nil {
					got += strconv.FormatFloat(*f.WeightSum, 'f', -1, 64)
				} else {
					got += "-"
				}
				if f.Via != nil && *f.Via == "direct" && f.Quality != "partial" {
					t.Errorf("July quality = %q, want partial", f.Quality)
				}
			}
		}
		if got != want {
			t.Errorf("funds on %s: %q, want %q", time.Unix(day, 0).UTC().Format(time.DateOnly), got, want)
		}
	}
}

// Without a declaration a parsed composition still reaches no day
// before its own: the fund is a leaf of its own until then.
func TestExposureParsedCompositionNeverReachesBack(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	seedParsedFund(t, db, ctx)
	if _, err := db.ExecContext(ctx, `DELETE FROM exposure_reports`); err != nil {
		t.Fatal(err)
	}
	stampDeclarations(t, db, ctx)
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	if got := parsedLeaves(t, db, ctx, exDay(6, 30)); got != "0|unknown|-|private_equity|diversified|USD=100.0" {
		t.Errorf("before the first report: %s", got)
	}
	if got := parsedLeaves(t, db, ctx, exDay(10, 15)); got != "0|unknown|-|private_equity|diversified|USD=100.0" {
		t.Errorf("in the quarter with no composition: %s", got)
	}
	if got := parsedLeaves(t, db, ctx, exDay(12, 15)); got == "" || got[:2] != "1|" {
		t.Errorf("after the November report: %s, want its lines", got)
	}
}

// TestMigration0167DDLIsRerunnable holds 0167 to the replay bar: a
// replay keeps each composition's origin and bound.
func TestMigration0167DDLIsRerunnable(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	seedParsedFund(t, db, ctx)
	stampDeclarations(t, db, ctx)
	rerunMigrationDDL(t, db, ctx, "0167_parsed_compositions.sql")
	var got string
	if err := db.QueryRowContext(ctx, `SELECT string_agg(origin || ':' || COALESCE(CAST(valid_until AS VARCHAR), '-'), ','
	    ORDER BY fund_ref, as_of) FROM fund_compositions WHERE fund_ref = 'isin:XX00000000P1'`).Scan(&got); err != nil ||
		got != "parsed:"+strconv.FormatInt(exDay(10, 1), 10)+",parsed:-" {
		t.Errorf("after replay: %q (%v)", got, err)
	}
	var providers int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM fund_compositions WHERE origin = 'provider'`).
		Scan(&providers); err != nil || providers == 0 {
		t.Errorf("after replay: %d provider compositions (%v), want the fixture's", providers, err)
	}
}

// A ledger record dated between a parsed composition's day and its
// bound wins from its day on; one dated before the composition yields
// to it. Without a bound the composition holds up to max_gap_days.
func TestExposureParsedCompositionAndDatedLedgerRecords(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	seedParsedFund(t, db, ctx)
	jun, aug := exDay(6, 30), exDay(8, 1)
	rows := exposureFixtureRows()
	rows.Declarations = append(rows.Declarations,
		declaration(3, "QPMF", &jun, split{"country", []string{"CH"}, []float64{100}}),
		declaration(4, "QPMF", &aug, split{"country", []string{"FR"}, []float64{100}}))
	rows.Settings.MaxGapDays = 30
	if _, err := SyncExposure(ctx, db, rows); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	read := func(day int64) string {
		t.Helper()
		var s sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT string_agg(DISTINCT depth || '|' || COALESCE(country, '-'), ',')
		    FROM exposure_leaves(?, 'USD') WHERE iid = 'QPMF'`, day+86399).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s.String
	}
	for day, want := range map[int64]string{
		exDay(7, 15):  "1|-",  // the composition over the June record
		exDay(8, 15):  "0|FR", // the August record over the July composition
		exDay(11, 15): "1|-",  // the November composition, newer than both
		exDay(12, 15): "0|FR", // 44 days on, beyond the bound of 30
	} {
		if got := read(day); got != want {
			t.Errorf("on %s: %q, want %q", time.Unix(day, 0).UTC().Format(time.DateOnly), got, want)
		}
	}
}

// A provider's composition whose weights sum above 100 leaves a
// negative remainder that reads as cash in the fund's currency; beyond
// the partial bound it is flagged. A line's country wins over the region
// it states.
func TestExposureProviderWeightsAboveHundred(t *testing.T) {
	t.Parallel()
	db, ctx := openExposureFixture(t)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, name, currency,
		  first_seen_at, last_seen_at) VALUES ('test-src', 'QOVR', 'public_equity', 'etf', 'QOVR', 'Overweight Fund',
		  'USD', 0, 0)`, nil},
		{`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		  asset_class, vehicle, currency, quantity, market_value) VALUES ('test-src', ?, 'A1', 'QOVR', 'QOVR',
		  'public_equity', 'etf', 'USD', 1, 100)`, []any{exSnap}},
		{`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count, currency)
		  VALUES ('test-src', 'symbol:QOVR', ?, 'holdings', 'testprov', 1, 'EUR')`, []any{exDay(5, 31)}},
		{`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, name, weight, country,
		  region) VALUES ('test-src', 'symbol:QOVR', ?, 'holdings', 1, 'symbol:QOVA', 'Overweight A', 104, 'Germany',
		  'north_america')`, []any{exDay(5, 31)}},
	} {
		if _, err := db.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	got := leafText(t, db, ctx, `string_agg(kind || '|' || asset_class || '|' || COALESCE(sector, '-') || '|' ||
	    COALESCE(region, '-') || '|' || currency || '|' || array_to_string(flags, ',') || '=' || round(value, 6), ' '
	    ORDER BY kind)`, `top_key = 'symbol:QOVR'`)
	if want := "line|public_equity|-|europe|EUR|=104.0 remainder|cash|cash|-|EUR|partial=-4.0"; got != want {
		t.Errorf("QOVR:\n got %s\nwant %s", got, want)
	}
}
