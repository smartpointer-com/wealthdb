package gold

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestWebSpendingLabelsUncategorized pins what the view adds over the
// macro it wraps: the epoch is rendered as a TIMESTAMP, and a category
// nothing could resolve is labelled rather than left NULL — a NULL
// would show up in a Metabase breakdown as an unlabelled slice, which
// reads as a rendering fault instead of as the backlog it is.
func TestWebSpendingLabelsUncategorized(t *testing.T) {
	db, ctx := openMigrated(t)
	seedSpendingReportFixture(t, db, ctx)

	rows, err := db.QueryContext(ctx, `
        SELECT CAST(occurred_at AS VARCHAR), silver_source_id, account_external_id,
               display_name, account_kind, merchant_name, spend_primary, spend_detailed,
               CAST(value_usd AS DOUBLE), CAST(value_chf AS DOUBLE), CAST(value_eur AS DOUBLE)
          FROM web_spending`)
	if err != nil {
		t.Fatalf("web_spending: %v", err)
	}
	defer rows.Close()

	type row struct {
		ts, src, acct, kind    string
		display, merchant      sql.NullString
		primary, detailed      string
		valUSD, valCHF, valEUR sql.NullFloat64
	}
	var n int
	var backlog, groceries row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ts, &r.src, &r.acct, &r.display, &r.kind,
			&r.merchant, &r.primary, &r.detailed,
			&r.valUSD, &r.valCHF, &r.valEUR); err != nil {
			t.Fatalf("scan web_spending: %v", err)
		}
		n++
		if r.detailed == "(uncategorized)" {
			backlog = r
		}
		if r.detailed == "FOOD_AND_DRINK_GROCERIES" && r.valUSD.Float64 < 0 {
			groceries = r
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate web_spending: %v", err)
	}
	if n != 7 {
		t.Errorf("web_spending = %d rows, want 7 (the spending population)", n)
	}
	if backlog.primary != "(uncategorized)" {
		t.Errorf("backlog row primary = %q, want (uncategorized) at both levels", backlog.primary)
	}
	if backlog.ts == "" || backlog.acct != "CARD1" || backlog.kind != "card" {
		t.Errorf("backlog row = %+v, want the card account's identity alongside it", backlog)
	}
	if groceries.merchant.String != "Corner Market" || groceries.display.String != "Everyday Card" {
		t.Errorf("groceries row = %+v, want the merchant and the account label", groceries)
	}
	if !nearly(groceries.valUSD.Float64, -100) || !nearly(groceries.valCHF.Float64, -80) {
		t.Errorf("groceries values = (%v USD, %v CHF), want (-100, -80)",
			groceries.valUSD.Float64, groceries.valCHF.Float64)
	}
}

// seedCardBalanceFixture lays down the snapshot pattern a mixed
// deposit + card source produces: the deposit account is re-dumped on
// every run, the card's balance arrives once per statement cycle, and
// the two clocks interleave.
func seedCardBalanceFixture(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	// 1 CHF = 1.25 USD, 1 EUR = 1.10 USD, so the trio resolves for a
	// USD-native card.
	seedFX(t, db, spendAt(2026, time.January, 1), "USD", "CHF", "1.25")
	seedFX(t, db, spendAt(2026, time.January, 1), "USD", "EUR", "1.10")
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, base_currency, first_seen_at, last_seen_at) VALUES
            ('test-src', 'CARD1', 'card', 'Everyday Card', 'USD', 1, 1),
            ('test-src', 'CASH1', 'cash', 'Everyday',      'USD', 1, 1);
    `); err != nil {
		t.Fatalf("seed card balance accounts: %v", err)
	}
	balances := []struct {
		at   int64
		acct string
		kind string
		amt  string
	}{
		{spendAt(2026, time.January, 10), "CARD1", "closing", "-500"}, // statement closing
		{spendAt(2026, time.January, 11), "CASH1", "current", "900"},  // deposit re-dump
		{spendAt(2026, time.January, 12), "CARD1", "closing", "-600"}, // next closing
		{spendAt(2026, time.January, 14), "CARD1", "closing", "0"},    // paid off
	}
	for _, b := range balances {
		if _, err := db.ExecContext(ctx, `
            INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id,
                                       currency, balance_kind, amount)
            VALUES ('test-src', ?, ?, 'USD', ?, CAST(? AS DECIMAL(28,4)))`,
			b.at, b.acct, b.kind, b.amt); err != nil {
			t.Fatalf("seed balance %s@%d: %v", b.acct, b.at, err)
		}
	}
}

// accountsOnDay lists which accounts the source-gated account-history
// macro reports on a given UTC day.
func accountsOnDay(t *testing.T, db *sql.DB, ctx context.Context, day int64) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
        SELECT account_external_id FROM report_accounts_history('USD')
         WHERE as_of_day = ? ORDER BY 1`, day)
	if err != nil {
		t.Fatalf("report_accounts_history: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatalf("scan account: %v", err)
		}
		out = append(out, a)
	}
	return out
}

// TestAccountHistoryDropsCardStatementDays is the reason
// web_card_balances_history exists, pinned rather than asserted in a
// comment. The account-history macros pick ONE active snapshot per
// SOURCE per day (`hist_active_cash`) and join it by equality, which is
// right for a source that dumps every account together. A card's
// balances arrive on the statement clock while a source's deposit
// accounts are re-dumped constantly, so each day reports whichever
// account happened to snapshot last — and the other simply disappears
// from the series.
func TestAccountHistoryDropsCardStatementDays(t *testing.T) {
	db, ctx := openMigrated(t)
	seedCardBalanceFixture(t, db, ctx)

	day := func(d int) int64 { return bucketAt(2026, time.January, d) }

	// Day 11's newest snapshot is the deposit re-dump, so the card's
	// day-10 closing is not "active" and the card is gone.
	if got := accountsOnDay(t, db, ctx, day(11)); len(got) != 1 || got[0] != "CASH1" {
		t.Errorf("accounts on the deposit-move day = %v, want only CASH1 "+
			"(if this now includes CARD1, the history macros learned per-account "+
			"carry-forward and web_card_balances_history can be reconsidered)", got)
	}
	// Day 12's newest snapshot is the card closing, so the deposit
	// balance disappears instead — the same flaw, mirrored.
	if got := accountsOnDay(t, db, ctx, day(12)); len(got) != 1 || got[0] != "CARD1" {
		t.Errorf("accounts on the statement-closing day = %v, want only CARD1", got)
	}
}

// TestCardBalancesHistoryCarriesEachAccountIndependently pins the
// dedicated view: every card day from its first balance onward, carried
// forward per account rather than per source, valued in all three
// reporting currencies, and keeping a zero balance (a paid-off card is
// at zero — dropping the row the way cash_chosen does would leave the
// series owing money forever).
func TestCardBalancesHistoryCarriesEachAccountIndependently(t *testing.T) {
	db, ctx := openMigrated(t)
	seedCardBalanceFixture(t, db, ctx)

	rows, err := db.QueryContext(ctx, `
        SELECT as_of_day, account_external_id, display_name, currency,
               CAST(balance AS DOUBLE), CAST(balance_usd AS DOUBLE), CAST(balance_chf AS DOUBLE)
          FROM report_card_balances_history_multi()
         WHERE as_of_day BETWEEN ? AND ?
         ORDER BY as_of_day, account_external_id`,
		bucketAt(2026, time.January, 10), bucketAt(2026, time.January, 15))
	if err != nil {
		t.Fatalf("report_card_balances_history_multi: %v", err)
	}
	defer rows.Close()

	type balRow struct {
		acct, display, ccy string
		bal, usd, chf      sql.NullFloat64
	}
	got := map[int64]balRow{}
	for rows.Next() {
		var day int64
		var r balRow
		if err := rows.Scan(&day, &r.acct, &r.display, &r.ccy, &r.bal, &r.usd, &r.chf); err != nil {
			t.Fatalf("scan card balance: %v", err)
		}
		if r.acct != "CARD1" {
			t.Errorf("day %d: account %q in a card-only series", day, r.acct)
		}
		got[day] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate card balances: %v", err)
	}

	for _, w := range []struct {
		d    int
		want float64
	}{
		{10, -500}, // the closing itself
		{11, -500}, // carried across the deposit re-dump the history macros lose it behind
		{12, -600},
		{13, -600}, // carried, no snapshot of its own
		{14, 0},    // paid off, and the zero survives
		{15, 0},
	} {
		day := bucketAt(2026, time.January, w.d)
		r, ok := got[day]
		if !ok {
			t.Errorf("no card row on Jan %d", w.d)
			continue
		}
		if !nearly(r.bal.Float64, w.want) {
			t.Errorf("Jan %d balance = %v, want %v", w.d, r.bal.Float64, w.want)
		}
		if !nearly(r.usd.Float64, w.want) {
			t.Errorf("Jan %d balance_usd = %v, want %v (native USD)", w.d, r.usd.Float64, w.want)
		}
		if r.display != "Everyday Card" || r.ccy != "USD" {
			t.Errorf("Jan %d identity = (%q, %q), want (Everyday Card, USD)", w.d, r.display, r.ccy)
		}
	}
	// Days before the card's first balance have nothing to carry.
	if _, ok := got[bucketAt(2026, time.January, 9)]; ok {
		t.Error("a card row exists before the card's first balance")
	}

	// The view is the macro with the epoch rendered as a TIMESTAMP, and
	// the rendering is UTC whatever zone the session is set to. The
	// query runs on a connection pinned to a non-UTC zone (a pooled
	// *sql.DB would not carry the per-connection SET to the query), so
	// a zone-dependent rendering shifts the day off midnight and the
	// equality below misses.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SET TimeZone = 'America/New_York'`); err != nil {
		t.Fatalf("SET TimeZone: %v", err)
	}
	var viewDay string
	var viewBal sql.NullFloat64
	if err := conn.QueryRowContext(ctx, `
        SELECT CAST(as_of_day AS VARCHAR), CAST(balance_chf AS DOUBLE)
          FROM web_card_balances_history
         WHERE account_external_id = 'CARD1' AND as_of_day = ?`,
		time.Date(2026, time.January, 12, 0, 0, 0, 0, time.UTC)).Scan(&viewDay, &viewBal); err != nil {
		t.Fatalf("web_card_balances_history: %v", err)
	}
	if viewDay != "2026-01-12 00:00:00" {
		t.Errorf("web_card_balances_history as_of_day = %q, want a UTC-midnight TIMESTAMP", viewDay)
	}
	if !nearly(viewBal.Float64, -480) { // -600 USD at 1 CHF = 1.25 USD
		t.Errorf("balance_chf = %v, want -480", viewBal.Float64)
	}
}

// TestMigration0043DDLIsRerunnable holds the web views to the replay
// bar and confirms both still answer afterwards.
func TestMigration0043DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedSpendingReportFixture(t, db, ctx)

	rerunMigrationDDL(t, db, ctx, "0043_web_spending_views.sql")

	for _, v := range []string{"web_spending", "web_card_balances_history"} {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+v).Scan(&n); err != nil {
			t.Errorf("%s after re-run: %v", v, err)
		}
	}
}

// seedWebViewEpochFixture lays down at least one row for every serving
// view: the spending fixture's accounts and transactions, a position
// snapshot on a brokerage account (the positions, asset-class and
// vehicle histories) and deposit and card balances (the source,
// account and card-balance histories). It extends the spending
// fixture rather than composing it with seedCardBalanceFixture, whose
// accounts and FX rows carry the same keys.
func seedWebViewEpochFixture(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	seedSpendingReportFixture(t, db, ctx)
	snap := spendAt(2026, time.January, 10)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, base_currency, tax_wrapper, management_style,
                              first_seen_at, last_seen_at)
        VALUES ('test-src', 'BRK1', 'brokerage', 'Brokerage', 'USD',
                'taxable_personal', 'self_directed', 1, 1)`); err != nil {
		t.Fatalf("seed brokerage account: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id,
                               position_key, asset_class, vehicle, currency, market_value)
        VALUES ('test-src', ?, 'BRK1', 'P1', 'public_equity', 'stock', 'USD',
                CAST('1000' AS DECIMAL(28,4)))`, snap); err != nil {
		t.Fatalf("seed position: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
        INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id,
                                   currency, balance_kind, amount) VALUES
            ('test-src', ?, 'CASH1', 'USD', 'current', CAST('900'  AS DECIMAL(28,4))),
            ('test-src', ?, 'CARD1', 'USD', 'closing', CAST('-500' AS DECIMAL(28,4)))`,
		snap, snap); err != nil {
		t.Fatalf("seed balances: %v", err)
	}
}

// webViews pairs each serving view with the epoch column it renders and
// the macro rows that column comes from. A src that reads from more
// than one macro is parenthesised: set operators associate left at
// equal precedence, so an unbracketed `<view> EXCEPT <a> UNION <b>`
// would parse as `(<view> EXCEPT <a>) UNION <b>` and count a union
// where the assertion below reads a difference.
var webViews = []struct{ view, col, src string }{
	{"web_sources_history", "as_of_day",
		"SELECT as_of_day FROM report_sources_history_multi()"},
	{"web_transactions", "occurred_at",
		"SELECT occurred_at FROM report_transactions_multi(0, 9223372036854775807)"},
	{"web_asset_classes_history", "as_of_day",
		"SELECT as_of_day FROM (SELECT as_of_day FROM report_positions_history_multi() " +
			"UNION SELECT as_of_day FROM report_sources_history_multi())"},
	{"web_vehicles_history", "as_of_day",
		"SELECT as_of_day FROM (SELECT as_of_day FROM report_positions_history_multi() " +
			"UNION SELECT as_of_day FROM report_sources_history_multi())"},
	{"web_accounts_history", "as_of_day",
		"SELECT as_of_day FROM report_accounts_history_multi()"},
	{"web_positions_history", "as_of_day",
		"SELECT as_of_day FROM report_positions_history_multi()"},
	{"web_spending", "occurred_at",
		"SELECT occurred_at FROM report_spending_transactions_multi(0, 9223372036854775807)"},
	{"web_card_balances_history", "as_of_day",
		"SELECT as_of_day FROM report_card_balances_history_multi()"},
}

// TestWebViewsRenderEpochsInUTC pins what every serving view promises a
// reader: its TIMESTAMP column is the macro's epoch rendered in UTC, so
// two sessions in different zones read the same day boundaries. The
// query runs on a connection pinned to a non-UTC zone — a pooled
// *sql.DB would not carry a per-connection SET to it — and a rendering
// that goes through TIMESTAMP WITH TIME ZONE shifts every value by that
// zone's offset, so no rendered timestamp maps back to an epoch the
// macro carries.
func TestWebViewsRenderEpochsInUTC(t *testing.T) {
	db, ctx := openMigrated(t)
	seedWebViewEpochFixture(t, db, ctx)

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SET TimeZone = 'Asia/Tokyo'`); err != nil {
		t.Fatalf("SET TimeZone: %v", err)
	}

	for _, v := range webViews {
		var rendered, shifted int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+v.view).Scan(&rendered); err != nil {
			t.Errorf("%s: %v", v.view, err)
			continue
		}
		if err := conn.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM (SELECT DISTINCT CAST(epoch("+v.col+") AS BIGINT) FROM "+
				v.view+" EXCEPT "+v.src+")").Scan(&shifted); err != nil {
			t.Errorf("%s: %v", v.view, err)
			continue
		}
		if shifted != 0 {
			t.Errorf("%s.%s: %d rendered value(s) match no epoch the macro carries",
				v.view, v.col, shifted)
		}
		if rendered == 0 {
			t.Errorf("%s is empty — an EXCEPT over no rows asserts nothing", v.view)
		}
	}
}

// TestMigration0049DDLIsRerunnable holds the re-issued serving views to
// the replay bar and confirms each still answers.
func TestMigration0049DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedWebViewEpochFixture(t, db, ctx)

	rerunMigrationDDL(t, db, ctx, "0049_web_view_utc_timestamps.sql")

	for _, v := range webViews {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+v.view).Scan(&n); err != nil {
			t.Errorf("%s after re-run: %v", v.view, err)
		}
	}
}
