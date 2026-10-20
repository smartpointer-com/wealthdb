package gold

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// yieldTables is the yield config tables as text, one field per table,
// rows in key order.
func yieldTables(t *testing.T, ctx context.Context, db *sql.DB) (curves, withholding, rating, conventions, cycles,
	settings string) {
	t.Helper()
	if err := db.QueryRowContext(ctx, `SELECT
        (SELECT COALESCE(string_agg(currency || ' ' || tenor || ' ' || tenor_months || ' ' || silver_source_id || ':' ||
                series_id, '; ' ORDER BY currency, tenor_months), '') FROM yield_curves),
        (SELECT COALESCE(string_agg(country || ' ' || wrapper || ' ' || rate_pct, '; ' ORDER BY country, wrapper), '')
           FROM yield_withholding),
        (SELECT COALESCE(string_agg(fund_ref || ' ' || rating || ' ' || bucket_high || ' ' || bucket_low, '; '
                ORDER BY fund_ref), '') FROM yield_rating_funds),
        (SELECT COALESCE(string_agg(market || ' ' || frequency || ' ' || day_count || ' ' || origin, '; '
                ORDER BY market), '') FROM yield_conventions),
        (SELECT COALESCE(string_agg(sector || ' ' || cycle || ' ' || origin, '; ' ORDER BY sector), '')
           FROM yield_cycles),
        (SELECT COALESCE(string_agg(CAST(horizon_months AS VARCHAR), ','), '') FROM yield_settings)`).
		Scan(&curves, &withholding, &rating, &conventions, &cycles, &settings); err != nil {
		t.Fatal(err)
	}
	return curves, withholding, rating, conventions, cycles, settings
}

// conventionsText and cyclesText are rows as yieldTables reads them back.
func conventionsText(cs []canonical.YieldConvention) string {
	var out []string
	for _, c := range cs {
		out = append(out, fmt.Sprintf("%s %d %s %s", c.Market, c.Frequency, c.DayCount, c.Origin))
	}
	slices.Sort(out)
	return strings.Join(out, "; ")
}

func cyclesText(cs []canonical.YieldCycle) string {
	var out []string
	for _, c := range cs {
		out = append(out, fmt.Sprintf("%s %s %s", c.Sector, c.Cycle, c.Origin))
	}
	slices.Sort(out)
	return strings.Join(out, "; ")
}

// The yield tables hold exactly the rows of the last sync: an entry
// taken out leaves, and the settings row is always there, once.
func TestSyncYield(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	rows := canonical.YieldRows{
		Curves: []canonical.YieldCurvePoint{
			{Currency: "USD", Tenor: "3M", TenorMonths: 3, SilverSourceID: "rates", SeriesID: "DGS3MO"},
			{Currency: "USD", Tenor: "10Y", TenorMonths: 120, SilverSourceID: "rates", SeriesID: "DGS10"},
		},
		Withholding: []canonical.YieldWithholding{
			{Country: "CH", Wrapper: canonical.WithholdingDefault, RatePct: 35},
			{Country: "US", Wrapper: "taxable_personal", RatePct: 15},
		},
		RatingFunds: []canonical.YieldRatingFund{
			{FundRef: "isin:XX0000000005", Rating: "AAA-A", BucketHigh: canonical.RatingAAA, BucketLow: canonical.RatingA},
		},
		Conventions: append(canonical.BuiltinConventions(),
			canonical.YieldConvention{Market: "JPY", Frequency: 2, DayCount: canonical.DayCountAct365,
				Origin: canonical.OriginConfig}),
		Cycles:   canonical.BuiltinCycles(),
		Settings: canonical.YieldSettings{HorizonMonths: 24},
	}
	rows.Cycles[0] = canonical.YieldCycle{Sector: canonical.SectorEnergy, Cycle: canonical.CycleCyclical,
		Origin: canonical.OriginConfig}
	// Two curve points, two rates, one rating fund, and the config's one
	// convention and one cycle.
	if n, err := SyncYield(ctx, db, rows); err != nil || n != 7 {
		t.Fatalf("sync: %d %v", n, err)
	}
	curves, withholding, rating, conventions, cycles, settings := yieldTables(t, ctx, db)
	if curves != "USD 3M 3 rates:DGS3MO; USD 10Y 120 rates:DGS10" {
		t.Errorf("curves = %q", curves)
	}
	if withholding != "CH default 35.0; US taxable_personal 15.0" {
		t.Errorf("withholding = %q", withholding)
	}
	if rating != "isin:XX0000000005 AAA-A AAA A" {
		t.Errorf("rating funds = %q", rating)
	}
	if want := conventionsText(rows.Conventions); conventions != want || !strings.Contains(conventions, "JPY 2 ACT/365 config") {
		t.Errorf("conventions = %q, want %q", conventions, want)
	}
	if want := cyclesText(rows.Cycles); cycles != want || !strings.Contains(cycles, "energy cyclical config") {
		t.Errorf("cycles = %q, want %q", cycles, want)
	}
	if settings != "24" {
		t.Errorf("settings = %q", settings)
	}

	// The config takes its curve's long end, a rate, its rating fund and
	// its overrides out: they leave, and only the built-in maps remain.
	rows.Curves = rows.Curves[:1]
	rows.Withholding = rows.Withholding[:1]
	rows.RatingFunds = nil
	rows.Conventions = canonical.BuiltinConventions()
	rows.Cycles = canonical.BuiltinCycles()
	rows.Settings.HorizonMonths = canonical.DefaultHorizonMonths
	if n, err := SyncYield(ctx, db, rows); err != nil || n != 2 {
		t.Fatalf("resync: %d %v", n, err)
	}
	curves, withholding, rating, conventions, cycles, settings = yieldTables(t, ctx, db)
	if curves != "USD 3M 3 rates:DGS3MO" || withholding != "CH default 35.0" || rating != "" || settings != "12" {
		t.Errorf("after resync: curves %q withholding %q rating %q settings %q", curves, withholding, rating, settings)
	}
	if want := conventionsText(canonical.BuiltinConventions()); conventions != want || strings.Contains(conventions, "config") {
		t.Errorf("conventions after resync = %q, want %q", conventions, want)
	}
	if want := cyclesText(canonical.BuiltinCycles()); cycles != want || strings.Contains(cycles, "config") {
		t.Errorf("cycles after resync = %q, want %q", cycles, want)
	}

	// The rows of an absent block stamp nothing the count shows.
	if n, err := SyncYield(ctx, db, canonical.YieldRows{Conventions: canonical.BuiltinConventions(),
		Cycles: canonical.BuiltinCycles(), Settings: canonical.YieldSettings{HorizonMonths: 12}}); err != nil || n != 0 {
		t.Errorf("sync of the built-in maps: %d %v", n, err)
	}
}

// A failed sync leaves the tables as the last one wrote them: the
// replacement is one transaction.
func TestSyncYieldIsAtomic(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	rows := canonical.YieldRows{
		Curves: []canonical.YieldCurvePoint{{Currency: "USD", Tenor: "3M", TenorMonths: 3, SilverSourceID: "rates",
			SeriesID: "DGS3MO"}},
		Settings: canonical.YieldSettings{HorizonMonths: 12},
	}
	if _, err := SyncYield(ctx, db, rows); err != nil {
		t.Fatal(err)
	}
	// Two rows of one key break the primary key half-way through.
	bad := rows
	bad.Curves = []canonical.YieldCurvePoint{
		{Currency: "CHF", Tenor: "1Y", TenorMonths: 12, SilverSourceID: "rates", SeriesID: "A"},
		{Currency: "CHF", Tenor: "1Y", TenorMonths: 12, SilverSourceID: "rates", SeriesID: "B"},
	}
	bad.Settings.HorizonMonths = 6
	if _, err := SyncYield(ctx, db, bad); err == nil {
		t.Fatal("a sync breaking the primary key succeeded")
	}
	curves, _, _, _, _, settings := yieldTables(t, ctx, db)
	if curves != "USD 3M 3 rates:DGS3MO" || settings != "12" {
		t.Errorf("after the failed sync: curves %q settings %q", curves, settings)
	}
}

// Migration 0134 replays over a gold that holds its tables filled: the
// fund line keeps its new columns, the reference rows and the config rows
// stay (gold.Migrate's REPLAY note).
func TestMigration0134DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	for _, q := range []string{
		`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, coupon_kind, is_default, duration)
		 VALUES ('ref', 'isin:XX0000000001', 86400, 'holdings', 1, 'Fixed', FALSE, 5.25)`,
		`INSERT INTO fund_risk (silver_source_id, fund_ref, as_of, currency, tenor, dv01)
		 VALUES ('ref', 'isin:XX0000000001', 86400, 'USD', '5Y', 40)`,
		`INSERT INTO rate_observations (silver_source_id, series_id, as_of, value) VALUES ('fx', 'USD-3M', 86400, 4.1)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := SyncYield(ctx, db, canonical.YieldRows{Settings: canonical.YieldSettings{HorizonMonths: 24}}); err != nil {
		t.Fatal(err)
	}

	rerunMigrationDDL(t, db, ctx, "0134_yield_reference.sql")

	var kind string
	var dur float64
	if err := db.QueryRowContext(ctx, `SELECT coupon_kind, duration FROM fund_lines`).Scan(&kind, &dur); err != nil {
		t.Fatal(err)
	}
	if kind != "Fixed" || dur != 5.25 {
		t.Errorf("fund line reads %q, %v after a replay", kind, dur)
	}
	for table, want := range map[string]int{"fund_risk": 1, "rate_observations": 1, "yield_settings": 1,
		"fund_characteristics": 0, "security_period_facts": 0} {
		if got := countRows(t, db, ctx, table); got != want {
			t.Errorf("%s holds %d rows after a replay, want %d", table, got, want)
		}
	}
}
