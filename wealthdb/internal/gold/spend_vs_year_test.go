package gold

import (
	"context"
	"database/sql"
	"math"
	"strconv"
	"testing"
	"time"
)

// seedSpendVsYearFixture lays down invented spending over three years
// on one card account, every line placed at transaction scope:
//
//	2023-06-15  -10 USD  FOOD_AND_DRINK_GROCERIES  (gold's first line)
//	2024-01-10  -80 USD  FOOD_AND_DRINK_GROCERIES
//	2024-01-20 -100 EUR  TRAVEL_LODGING            (110 USD at 1.10)
//	2024-02-29  -40 USD  FOOD_AND_DRINK_RESTAURANT
//	2025-01-10 -100 USD  FOOD_AND_DRINK_GROCERIES
//	2025-01-12  +20 USD  FOOD_AND_DRINK_GROCERIES  (a refund)
//	2025-01-20 -100 EUR  TRAVEL_LODGING            (120 USD at 1.20)
//	2025-02-28  -50 USD  TRAVEL_FLIGHTS
//
// and extra, each (day, amount) a further grocery line.
func seedSpendVsYearFixture(t *testing.T, db *sql.DB, ctx context.Context, extra ...[2]int64) {
	t.Helper()
	seedFX(t, db, dy(2024, time.January, 1), "USD", "EUR", "1.10")
	seedFX(t, db, dy(2025, time.January, 1), "USD", "EUR", "1.20")
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, base_currency, first_seen_at, last_seen_at) VALUES
            ('test-src', 'CARD1', 'card', 'Everyday Card', 'USD', 1, 1)`); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	type line struct {
		at       int64
		kind     string
		ccy, net string
		cat      string
	}
	lines := []line{
		{dy(2023, time.June, 15), "purchase", "USD", "-10", "FOOD_AND_DRINK_GROCERIES"},
		{dy(2024, time.January, 10), "purchase", "USD", "-80", "FOOD_AND_DRINK_GROCERIES"},
		{dy(2024, time.January, 20), "purchase", "EUR", "-100", "TRAVEL_LODGING"},
		{dy(2024, time.February, 29), "purchase", "USD", "-40", "FOOD_AND_DRINK_RESTAURANT"},
		{dy(2025, time.January, 10), "purchase", "USD", "-100", "FOOD_AND_DRINK_GROCERIES"},
		{dy(2025, time.January, 12), "refund", "USD", "20", "FOOD_AND_DRINK_GROCERIES"},
		{dy(2025, time.January, 20), "purchase", "EUR", "-100", "TRAVEL_LODGING"},
		{dy(2025, time.February, 28), "purchase", "USD", "-50", "TRAVEL_FLIGHTS"},
	}
	for _, e := range extra {
		lines = append(lines, line{e[0], "purchase", "USD", strconv.FormatInt(e[1], 10), "FOOD_AND_DRINK_GROCERIES"})
	}
	for i, l := range lines {
		id := "T-" + strconv.Itoa(i)
		if _, err := db.ExecContext(ctx, `
            INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                      account_external_id, kind, currency, net_amount, description)
            VALUES ('test-src', ?, ?, 'CARD1', ?, ?, CAST(? AS DECIMAL(28,4)), 'SHOP')`,
			id, l.at, l.kind, l.ccy, l.net); err != nil {
			t.Fatalf("seed transaction %s: %v", id, err)
		}
		if _, err := db.ExecContext(ctx, `
            INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                              merchant_signature, signature_version,
                                              spend_detailed, provenance, assigned_at)
            VALUES ('test-src', ?, 'sig-shop', 1, ?, 'provider', 100)`, id, l.cat); err != nil {
			t.Fatalf("seed enrichment %s: %v", id, err)
		}
	}
}

// vsRow is one row of report_spending_categories_vs, keyed for a test.
type vsRow struct {
	txnCount          int64
	net, before       float64
	share, change     sql.NullFloat64
	quality           string
	hasNet, hasBefore bool
}

// vsToday is the today of the reads of the fixture's past windows: the
// last second of a day after every line.
var vsToday = endOfDay(2026, time.July, 15)

// endOfDay is the last second of a UTC day.
func endOfDay(y int, m time.Month, d int) int64 {
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC).Unix() - 1
}

func readVsYear(t *testing.T, db *sql.DB, ctx context.Context, from, to int64, period string) map[[2]string]vsRow {
	t.Helper()
	return readVsYearIn(t, db, ctx, "USD", from, to, period, vsToday)
}

// readVsYearIn is readVsYear in ccy, on today.
func readVsYearIn(t *testing.T, db *sql.DB, ctx context.Context, ccy string, from, to int64, period string, today int64) map[[2]string]vsRow {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
        SELECT COALESCE(strftime(epoch_ms(period_start * 1000), '%Y-%m'), 'total'), category, txn_count,
               CAST(net_spend AS DOUBLE), CAST(year_before AS DOUBLE), share, change_pct, quality
          FROM report_spending_categories_vs(?, ?, ?, ?, 'detailed', ?)`, from, to, ccy, period, today)
	if err != nil {
		t.Fatalf("report_spending_categories_vs: %v", err)
	}
	defer rows.Close()
	out := map[[2]string]vsRow{}
	for rows.Next() {
		var bucket, cat string
		var r vsRow
		var net, before sql.NullFloat64
		if err := rows.Scan(&bucket, &cat, &r.txnCount, &net, &before, &r.share, &r.change, &r.quality); err != nil {
			t.Fatal(err)
		}
		r.net, r.hasNet = net.Float64, net.Valid
		r.before, r.hasBefore = before.Float64, before.Valid
		out[[2]string{bucket, cat}] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSpendingVsYearPairsTheSameMonths: each month's categories beside
// the same month a year earlier, a category one side lacks at zero, and
// the change blank where the year before is zero.
func TestSpendingVsYearPairsTheSameMonths(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedSpendVsYearFixture(t, db, ctx)
	got := readVsYear(t, db, ctx, dy(2025, time.January, 1), dy(2025, time.March, 1)-1, "month")

	groc := got[[2]string{"2025-01", "FOOD_AND_DRINK_GROCERIES"}]
	if groc.net != 80 || groc.before != 80 || !groc.change.Valid || groc.change.Float64 != 0 || groc.quality != "" {
		t.Errorf("January groceries = %+v, want 80 against 80, change 0, no quality", groc)
	}
	// Each side at its own rate: 100 EUR is 120 USD now, 110 a year ago.
	lodge := got[[2]string{"2025-01", "TRAVEL_LODGING"}]
	if lodge.net != 120 || lodge.before != 110 || !near(lodge.change.Float64, 100*10.0/110) {
		t.Errorf("January lodging = %+v, want 120 against 110", lodge)
	}
	// 29 February 2024 is the year before 28 February 2025's month.
	rest := got[[2]string{"2025-02", "FOOD_AND_DRINK_RESTAURANT"}]
	if !rest.hasNet || rest.net != 0 || rest.txnCount != 0 || rest.before != 40 ||
		rest.change.Float64 != -100 || !rest.share.Valid || rest.share.Float64 != 0 {
		t.Errorf("February restaurants (last year only) = %+v, want 0 against 40, change -100, share 0", rest)
	}
	fl := got[[2]string{"2025-02", "TRAVEL_FLIGHTS"}]
	if fl.net != 50 || !fl.hasBefore || fl.before != 0 || fl.change.Valid {
		t.Errorf("February flights (this year only) = %+v, want 50 against 0, change blank", fl)
	}
	if len(got) != 4 {
		t.Errorf("rows = %v, want the four above", got)
	}
}

// TestSpendingVsYearKeepsThePlainRows: with the year before beside it,
// each row the plain report has keeps its figures and its order.
func TestSpendingVsYearKeepsThePlainRows(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedSpendVsYearFixture(t, db, ctx)
	from, to := dy(2025, time.January, 1), dy(2025, time.March, 1)-1
	var differ int
	if err := db.QueryRowContext(ctx, `
        WITH plain AS (SELECT row_number() OVER () AS n, * FROM report_spending_categories(?, ?, 'USD', 'month', 'primary')),
             vs AS (SELECT row_number() OVER () AS n, * FROM report_spending_categories_vs(?, ?, 'USD', 'month', 'primary', ?)
                     WHERE txn_count > 0)
        SELECT count(*) FROM plain p FULL JOIN vs v USING (n)
         WHERE (p.period_start, p.category, p.category_label, p.txn_count, p.spend, p.refunds, p.net_spend, p.share)
               IS DISTINCT FROM (v.period_start, v.category, v.category_label, v.txn_count, v.spend, v.refunds, v.net_spend, v.share)`,
		from, to, from, to, vsToday).Scan(&differ); err != nil {
		t.Fatal(err)
	}
	if differ != 0 {
		t.Errorf("%d rows differ from report_spending_categories", differ)
	}
}

// TestSpendingVsYearTagsAMissingYear: a bucket whose year before starts
// before gold's first line reads no_year_before, the window as one
// bucket too.
func TestSpendingVsYearTagsAMissingYear(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedSpendVsYearFixture(t, db, ctx)
	got := readVsYear(t, db, ctx, dy(2024, time.January, 1), dy(2024, time.December, 31), "month")
	if q := got[[2]string{"2024-01", "FOOD_AND_DRINK_GROCERIES"}].quality; q != "no_year_before" {
		t.Errorf("2024-01 quality = %q, want no_year_before", q)
	}
	total := readVsYear(t, db, ctx, dy(2024, time.January, 1), dy(2025, time.January, 1)-1, "total")
	if r := total[[2]string{"total", "FOOD_AND_DRINK_GROCERIES"}]; r.quality != "no_year_before" || r.before != 10 {
		t.Errorf("2024 total groceries = %+v, want 80 against the 10 of 2023's part, no_year_before", r)
	}
	// From July 2024 the year before lies after the first line.
	got = readVsYear(t, db, ctx, dy(2024, time.July, 1), dy(2025, time.January, 31), "month")
	for k, r := range got {
		if r.quality != "" {
			t.Errorf("%v quality = %q, want none", k, r.quality)
		}
	}
}

// TestSpendingVsYearTagsTheCurrentMonth: the month that has not ended
// reads in_progress, and the serving rows stop at the last whole month,
// in every reporting currency.
func TestSpendingVsYearTagsTheCurrentMonth(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	today := endOfDay(2025, time.March, 15)
	monthStart := time.Date(2025, time.March, 1, 0, 0, 0, 0, time.UTC).Unix()
	lastMonth := time.Date(2025, time.February, 1, 0, 0, 0, 0, time.UTC).Unix()
	seedSpendVsYearFixture(t, db, ctx, [2]int64{monthStart, -5}, [2]int64{monthStart - 86400, -7})
	got := readVsYearIn(t, db, ctx, "USD", monthStart, monthStart+40*86400, "month", today)
	if r := got[[2]string{"2025-03", "FOOD_AND_DRINK_GROCERIES"}]; r.net != 5 || r.quality != "in_progress" {
		t.Errorf("the current month = %+v, want 5, in_progress", r)
	}
	// The same month read after it ended is whole.
	got = readVsYearIn(t, db, ctx, "USD", monthStart, monthStart+40*86400, "month", endOfDay(2025, time.April, 1))
	if r := got[[2]string{"2025-03", "FOOD_AND_DRINK_GROCERIES"}]; r.quality != "" {
		t.Errorf("March read in April = %+v, want no quality", r)
	}
	rows, err := db.QueryContext(ctx, `
        SELECT currency, count(*), max(month), count(*) FILTER (WHERE quality LIKE '%in_progress%')
          FROM report_spending_vs_year_multi(?) GROUP BY 1 ORDER BY 1`, today)
	if err != nil {
		t.Fatalf("report_spending_vs_year_multi: %v", err)
	}
	defer rows.Close()
	var ccys []string
	for rows.Next() {
		var ccy string
		var n, last, open int64
		if err := rows.Scan(&ccy, &n, &last, &open); err != nil {
			t.Fatal(err)
		}
		ccys = append(ccys, ccy)
		if last != lastMonth || open != 0 {
			t.Errorf("%s: last month %d, %d in progress; want the month before this one, none", ccy, last, open)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ccys) != 4 {
		t.Errorf("currencies = %v, want the four reporting ones", ccys)
	}
}

// TestSpendingVsYearBuckets: quarter and year buckets read in_progress
// while today falls in them, a window that cuts a bucket cuts the year
// before alike and reads whole, and a window through 28 February
// reaches the 29th a year earlier.
func TestSpendingVsYearBuckets(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedSpendVsYearFixture(t, db, ctx)
	from, to := dy(2025, time.January, 1)-12*3600, endOfDay(2025, time.December, 31)
	groc := func(got map[[2]string]vsRow, bucket string) vsRow {
		return got[[2]string{bucket, "FOOD_AND_DRINK_GROCERIES"}]
	}
	for _, tc := range []struct {
		period, bucket string
		today          int64
		want           string
	}{
		{"quarter", "2025-01", endOfDay(2025, time.February, 15), "in_progress"},
		{"quarter", "2025-01", endOfDay(2025, time.March, 31), "in_progress"},
		{"quarter", "2025-01", endOfDay(2025, time.April, 1), ""},
		{"year", "2025-01", endOfDay(2025, time.June, 30), "in_progress"},
		{"year", "2025-01", endOfDay(2026, time.January, 1), ""},
	} {
		r := groc(readVsYearIn(t, db, ctx, "USD", from, to, tc.period, tc.today), tc.bucket)
		if r.net != 80 || r.before != 80 || r.quality != tc.want {
			t.Errorf("%s on %s = %+v, want 80 against 80, quality %q",
				tc.period, time.Unix(tc.today, 0).UTC().Format("2006-01-02"), r, tc.want)
		}
	}

	// To 15 January: the lodging lines of the 20th fall outside both
	// sides, and the cut quarter reads whole.
	cut := readVsYear(t, db, ctx, from, endOfDay(2025, time.January, 15), "quarter")
	if r := groc(cut, "2025-01"); r.net != 80 || r.before != 80 || r.quality != "" {
		t.Errorf("the cut quarter's groceries = %+v, want 80 against 80, no quality", r)
	}
	if len(cut) != 1 {
		t.Errorf("the cut quarter = %v, want groceries alone", cut)
	}

	feb := readVsYear(t, db, ctx, dy(2025, time.February, 1)-12*3600, endOfDay(2025, time.February, 28), "month")
	if r := feb[[2]string{"2025-02", "FOOD_AND_DRINK_RESTAURANT"}]; r.before != 40 {
		t.Errorf("February to the 28th: restaurants = %+v, want the 40 of 29 February 2024", r)
	}
}

// TestSpendingVsYearWithoutARate: a side with no rate to the output
// currency reads a blank year_before and change_pct, never a zero.
func TestSpendingVsYearWithoutARate(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedSpendVsYearFixture(t, db, ctx)
	// A flight in a currency gold holds no rate for, a year before one in
	// dollars.
	for i, l := range []struct {
		at       int64
		ccy, net string
	}{{dy(2023, time.June, 20), "JPY", "-3000"}, {dy(2024, time.June, 20), "USD", "-30"}} {
		id := "T-RATE-" + strconv.Itoa(i)
		if _, err := db.ExecContext(ctx, `
            INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                      account_external_id, kind, currency, net_amount, description)
            VALUES ('test-src', ?, ?, 'CARD1', 'purchase', ?, CAST(? AS DECIMAL(28,4)), 'SHOP')`,
			id, l.at, l.ccy, l.net); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
            INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                              merchant_signature, signature_version,
                                              spend_detailed, provenance, assigned_at)
            VALUES ('test-src', ?, 'sig-shop', 1, 'TRAVEL_FLIGHTS', 'provider', 100)`, id); err != nil {
			t.Fatal(err)
		}
	}
	got := readVsYear(t, db, ctx, dy(2024, time.June, 1)-12*3600, endOfDay(2024, time.June, 30), "month")
	r := got[[2]string{"2024-06", "TRAVEL_FLIGHTS"}]
	if r.net != 30 || r.hasBefore || r.change.Valid {
		t.Errorf("June 2024 flights = %+v, want 30, a blank year_before and change_pct", r)
	}
}

// TestSpendYearBefore: a calendar year back, 29 February to the 28th,
// and an open end left alone.
func TestSpendYearBefore(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	for _, tc := range []struct{ at, want int64 }{
		{dy(2025, time.March, 1), dy(2024, time.March, 1)},
		{dy(2024, time.February, 29), dy(2023, time.February, 28)},
		{0, time.Date(1969, time.January, 1, 0, 0, 0, 0, time.UTC).Unix()},
		{math.MaxInt64, math.MaxInt64},
	} {
		var got int64
		if err := db.QueryRowContext(ctx, `SELECT spend_year_before(?)`, tc.at).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("spend_year_before(%d) = %d, want %d", tc.at, got, tc.want)
		}
	}
}

// TestSpendYearBeforeEnd: a window end maps as the exclusive bound after
// it, so 28 February's end reaches through the 29th a year earlier; an
// open end stays itself.
func TestSpendYearBeforeEnd(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	for _, tc := range []struct{ at, want int64 }{
		{endOfDay(2025, time.February, 28), endOfDay(2024, time.February, 29)},
		{endOfDay(2024, time.February, 29), endOfDay(2023, time.February, 28)},
		{dy(2025, time.March, 1), dy(2024, time.March, 1)},
		{253402300799, 253402300799},
		{math.MaxInt64, math.MaxInt64},
	} {
		var got int64
		if err := db.QueryRowContext(ctx, `SELECT spend_year_before_end(?)`, tc.at).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("spend_year_before_end(%d) = %d, want %d", tc.at, got, tc.want)
		}
	}
}
