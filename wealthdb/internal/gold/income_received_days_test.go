package gold

import (
	"testing"
	"time"
)

// web_income_days holds every day to the yield report's latest day from
// the earliest of the first day of last year, the first day income was
// received and the first day tax was withheld. web_income_withheld holds
// the tax a source withheld on the income's accounts: each negative tax
// row, as a positive amount, in each reporting currency. A refund of
// tax, another kind of row and an account outside the income's scope
// stay out. Replaying migration 0146 changes neither.
func TestIncomeReceivedViews(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	ex := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	day := func(y int, m time.Month, d int) int64 { return time.Date(y, m, d, 12, 0, 0, 0, time.UTC).Unix() }
	ex(`INSERT INTO report_yield_positions (computed_at, as_of, currency, silver_source_id, account_external_id,
	    position_key, kind, value, weighs) VALUES (0, ?, 'USD', 'test-src', 'A', 'cash:USD', 'deposit', 1, TRUE)`,
		day(2026, 10, 21))
	ex(`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
	    first_seen_at, last_seen_at) VALUES ('test-src', 'A', 'brokerage', 'Brokerage', 'USD', 0, 0),
	    ('test-src', 'B', 'brokerage', 'Other', 'USD', 0, 0)`)
	ex(`INSERT INTO income_account_scope (silver_source_id, account_external_id, mode) VALUES ('test-src', 'B', 'exclude')`)
	taxed := func(acct, kind string, at int64, net float64) {
		t.Helper()
		ex(`INSERT INTO serving_transactions BY NAME
		    SELECT 'test-src' AS silver_source_id, ? AS account_external_id, ? AS kind, ? AS occurred_at,
		           ? AS net_amount, ? AS value_usd, ? AS value_chf, ? AS value_eur, ? AS value_gbp`,
			acct, kind, at, net, net, 0.8*net, 0.9*net, 0.75*net)
	}
	taxed("A", "tax", day(2026, 7, 15), -30)
	taxed("A", "tax", day(2026, 8, 15), 5)
	taxed("A", "fee", day(2026, 8, 15), -7)
	taxed("B", "tax", day(2022, 7, 15), -11)
	days := func() (int, string, string) {
		t.Helper()
		var n int
		var first, last string
		if err := db.QueryRowContext(ctx, `SELECT count(*), COALESCE(CAST(min(day) AS VARCHAR), ''),
		    COALESCE(CAST(max(day) AS VARCHAR), '') FROM web_income_days`).Scan(&n, &first, &last); err != nil {
			t.Fatal(err)
		}
		return n, first, last
	}
	withheld := func() string {
		t.Helper()
		var s string
		if err := db.QueryRowContext(ctx, `SELECT string_agg(account_label || ' ' || CAST(day AS VARCHAR) || ' ' ||
		    CAST(value_usd AS VARCHAR) || ' ' || CAST(value_chf AS VARCHAR), '; ' ORDER BY day) FROM web_income_withheld`).
			Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	// The excluded account's earlier tax moves nothing.
	if n, first, last := days(); n != 365+294 || first != "2025-01-01" || last != "2026-10-21" {
		t.Errorf("web_income_days: %d days, %s to %s", n, first, last)
	}
	if got, want := withheld(), "Brokerage (test-src brokerage) 2026-07-15 30.0000 24.0000"; got != want {
		t.Errorf("web_income_withheld: %q, want %q", got, want)
	}
	// Income received before last year starts the days on its day, and
	// tax withheld earlier still on its own.
	ex(`INSERT INTO report_yield_received (computed_at, currency, silver_source_id, account_external_id, position_key,
	    day, type, value) VALUES (0, 'USD', 'test-src', 'A', 'cash:USD', DATE '2024-06-01', 'interest', 10)`)
	if _, first, _ := days(); first != "2024-06-01" {
		t.Errorf("with income received on 2024-06-01 the days start on %s", first)
	}
	taxed("A", "tax", day(2023, 2, 1), -4)
	if _, first, _ := days(); first != "2023-02-01" {
		t.Errorf("with tax withheld on 2023-02-01 the days start on %s", first)
	}
	n, first, last := days()
	w := withheld()
	rerunMigrationDDL(t, db, ctx, "0146_income_received_days.sql")
	if n2, f2, l2 := days(); n2 != n || f2 != first || l2 != last || withheld() != w {
		t.Errorf("the replay changed the views: %d %s %s %q", n2, f2, l2, withheld())
	}
	// With no yield report there is no today, and so no day.
	ex(`DELETE FROM report_yield_positions`)
	if n, _, _ := days(); n != 0 {
		t.Errorf("with no yield report web_income_days holds %d days", n)
	}
}

// The yield tables hold every income line received, however old, so the
// Income dashboard's Time range reaches as far back as the lines do.
func TestMaterializeYieldReceivesTheWholeHistory(t *testing.T) {
	t.Parallel()
	db, ctx, at := seedYieldBookFX(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO transactions (silver_source_id, transaction_external_id,
	    occurred_at, account_external_id, kind, currency, net_amount) VALUES ('test-src', 'OLD1', ?, 'F', 'deposit',
	    'CHF', 1400)`, ymd(2023, 3, 1).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id,
	    payer_signature, signature_version, income_detailed, provenance, assigned_at) VALUES
	    ('test-src', 'OLD1', 'sig-tenant', 1, 'INCOME_RENTAL', 'rule', 100)`); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeYield(ctx, db, at, 42, 12); err != nil {
		t.Fatal(err)
	}
	var first string
	if err := db.QueryRowContext(ctx, `SELECT CAST(min(day) AS VARCHAR) FROM web_yield_received WHERE currency = 'CHF'`).
		Scan(&first); err != nil || first != "2023-03-01" {
		t.Errorf("the income received starts on %s (%v), want the 2023 rent", first, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT CAST(min(day) AS VARCHAR) FROM web_income_days`).Scan(&first); err != nil ||
		first != "2023-03-01" {
		t.Errorf("the days received start on %s (%v)", first, err)
	}
}
