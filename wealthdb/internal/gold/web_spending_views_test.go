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
	// 1 CHF = 1.25 USD, 1 EUR = 1.10 USD, so USD, CHF and EUR resolve for a
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

// TestAccountHistoryKeepsBothClocks pins the account-history macros
// against the fixture that used to defeat them (gold migration 0051).
// A card's balances arrive on the statement clock while the same
// source's deposit accounts are re-dumped constantly. Resolving the
// active snapshot per (source, ACCOUNT, day) reports both accounts on
// every day; one active snapshot per source reported only whichever of
// them happened to snapshot last.
func TestAccountHistoryKeepsBothClocks(t *testing.T) {
	db, ctx := openMigrated(t)
	seedCardBalanceFixture(t, db, ctx)

	day := func(d int) int64 { return bucketAt(2026, time.January, d) }

	// Day 11's newest snapshot is the deposit re-dump; the card's day-10
	// closing is still its own latest, so it is carried alongside.
	if got := accountsOnDay(t, db, ctx, day(11)); len(got) != 2 ||
		got[0] != "CARD1" || got[1] != "CASH1" {
		t.Errorf("accounts on the deposit-move day = %v, want both CARD1 and CASH1", got)
	}
	// Day 12's newest snapshot is the card closing, and the deposit
	// balance is carried across it — the mirror of the same rule.
	if got := accountsOnDay(t, db, ctx, day(12)); len(got) != 2 ||
		got[0] != "CARD1" || got[1] != "CASH1" {
		t.Errorf("accounts on the statement-closing day = %v, want both CARD1 and CASH1", got)
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
		{11, -500}, // carried across the deposit re-dump, no closing of its own
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
	{"web_sources_latest", "snapshot_at",
		"SELECT snapshot_at FROM report_sources_multi(9223372036854775807)"},
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

// TestWebSpendingLabelsAnUnnamedAccount pins the fallback. Several
// adapters leave display_name NULL deliberately — UBS cash accounts are
// the clearest — and a view that groups by the label must not collapse
// every one of them into a single "null" that leads the chart.
func TestWebSpendingLabelsAnUnnamedAccount(t *testing.T) {
	db, ctx := openMigrated(t)
	seedSpendingFixture(t, db, ctx)
	// Two accounts with no name of their own, and one with a name.
	if _, err := db.ExecContext(ctx, `
        UPDATE accounts SET display_name = NULL WHERE account_external_id = 'CASH1';
    `); err != nil {
		t.Fatalf("unname CASH1: %v", err)
	}
	rows, err := db.QueryContext(ctx, `
        SELECT DISTINCT account_external_id, display_name FROM web_spending
         ORDER BY 1`)
	if err != nil {
		t.Fatalf("web_spending: %v", err)
	}
	defer rows.Close()
	labels := map[string]string{}
	for rows.Next() {
		var id string
		var label sql.NullString
		if err := rows.Scan(&id, &label); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !label.Valid || label.String == "" {
			t.Errorf("account %s has no label; the view must fall back to the id", id)
		}
		labels[id] = label.String
	}
	if got, ok := labels["CASH1"]; ok && got != "CASH1" {
		t.Errorf("unnamed account labelled %q, want its own id", got)
	}
	if len(labels) < 2 {
		t.Fatalf("fixture produced %d account(s); the collapse this guards against needs at least 2", len(labels))
	}
}

// TestWebSpendingLabelsAnAccountWithSourceAndKind pins what migration
// 0063 adds. An account's own name is what its institution calls it and
// no more: a bar reading `Checking` says neither which bank it belongs
// to nor whether the money left a deposit account or a card, and the
// names collide across sources. The label carries all three, name
// first — a row chart truncates from the end, so what a narrow tile
// clips is the annotation rather than the thing being labelled — and
// the bare name and the id stay projected beside it.
func TestWebSpendingLabelsAnAccountWithSourceAndKind(t *testing.T) {
	db, ctx := openMigrated(t)
	seedSpendingFixture(t, db, ctx)
	// An account with no name of its own is labelled by its id: 0061's
	// rule, now shared by both views through account_display_name.
	if _, err := db.ExecContext(ctx, `
        UPDATE accounts SET display_name = NULL WHERE account_external_id = 'CASH1';
    `); err != nil {
		t.Fatalf("unname CASH1: %v", err)
	}
	rows, err := db.QueryContext(ctx, `
        SELECT DISTINCT account_external_id, silver_source_id, account_kind,
               display_name, account_label
          FROM web_spending ORDER BY 1`)
	if err != nil {
		t.Fatalf("web_spending: %v", err)
	}
	defer rows.Close()
	labels := map[string]string{}
	for rows.Next() {
		var id, src, kind, name, label string
		if err := rows.Scan(&id, &src, &kind, &name, &label); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if want := name + " (" + src + " " + kind + ")"; label != want {
			t.Errorf("account %s labelled %q, want %q", id, label, want)
		}
		labels[id] = label
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate web_spending: %v", err)
	}
	if got := labels["CASH1"]; got != "CASH1 (test-src cash)" {
		t.Errorf("unnamed account labelled %q, want its id annotated", got)
	}
	if got := labels["CARD1"]; got != "Card (test-src card)" {
		t.Errorf("named account labelled %q, want its name annotated", got)
	}
	// An account whose kind gold never learned still says so, rather
	// than trailing an empty bracket or collapsing the whole label to
	// NULL — a NULL label is the "null" bar migration 0061 removed.
	var unknown string
	if err := db.QueryRowContext(ctx,
		`SELECT account_label(NULL, 'ID1', 'test-src', NULL)`).Scan(&unknown); err != nil {
		t.Fatalf("account_label with no kind: %v", err)
	}
	if want := "ID1 (test-src unknown)"; unknown != want {
		t.Errorf("kindless account labelled %q, want %q", unknown, want)
	}
}

// TestCardBalancesLabelsAnAccountWithSourceAndKind is the same for the
// balances view, which reaches its label the same way. Its kind is a
// literal there: report_card_balances_history_multi keeps only
// account_kind = 'card', so every row is one by construction.
func TestCardBalancesLabelsAnAccountWithSourceAndKind(t *testing.T) {
	db, ctx := openMigrated(t)
	seedCardBalanceFixture(t, db, ctx)
	rows, err := db.QueryContext(ctx, `
        SELECT DISTINCT display_name, account_label FROM web_card_balances_history`)
	if err != nil {
		t.Fatalf("web_card_balances_history: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var name, label string
		if err := rows.Scan(&name, &label); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
		if name != "Everyday Card" {
			t.Errorf("display_name = %q, want the account's own name", name)
		}
		if want := "Everyday Card (test-src card)"; label != want {
			t.Errorf("account_label = %q, want %q", label, want)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate card balances: %v", err)
	}
	if n != 1 {
		t.Errorf("%d labelled card(s), want 1", n)
	}
}
