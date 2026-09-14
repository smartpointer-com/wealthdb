package gold

import (
	"database/sql"
	"testing"
	"time"
)

// seededSeconds is every instant seedIncomeReportFixture puts a line
// at. Listed rather than ranged over, so a line seeded at a NEW instant
// is a deliberate edit here and not a silent widening of the check.
var seededSeconds = map[int64]bool{1000: true, 9000: true, 2700000: true, 5300000: true}

// TestWebIncomeViewShape pins what the Income dashboards read: UTC
// epoch-milliseconds, the resolved account label, the uncategorised
// fallback on the type columns — and its deliberate absence on the
// payer, which a delta line has none of.
func TestWebIncomeViewShape(t *testing.T) {
	db, ctx := openMigrated(t)
	seedIncomeReportFixture(t, db, ctx)

	rows, err := db.QueryContext(ctx, `
        SELECT occurred_at, display_name, account_label, income_label,
               income_primary, payer_name
          FROM web_income
         ORDER BY occurred_at`)
	if err != nil {
		t.Fatalf("read web_income: %v", err)
	}
	defer rows.Close()

	var n, uncategorised, blankPayer int
	for rows.Next() {
		var occurredAt time.Time
		var displayName, accountLabel, incomeLabel, incomePrimary string
		var payer sql.NullString
		if err := rows.Scan(&occurredAt, &displayName, &accountLabel,
			&incomeLabel, &incomePrimary, &payer); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
		// epoch_ms over a UTC clock: a view handing back a local-time
		// timestamp would shift every dashboard by the host's offset.
		if !seededSeconds[occurredAt.UTC().Unix()] {
			t.Errorf("occurred_at = %v (unix %d), want one of the seconds the fixture seeded",
				occurredAt, occurredAt.UTC().Unix())
		}
		if displayName == "" || accountLabel == "" {
			t.Errorf("an account resolved to an empty name or label")
		}
		if incomeLabel == "(uncategorized)" {
			uncategorised++
		}
		if !payer.Valid {
			blankPayer++
		}
		if incomeLabel == "" || incomePrimary == "" {
			t.Error("a type column is empty rather than labelled uncategorised")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if n == 0 {
		t.Fatal("web_income is empty; the assertions prove nothing")
	}
	if uncategorised == 0 {
		t.Error("no row labels as (uncategorized); the data-quality canary cannot fire")
	}
	if blankPayer == 0 {
		t.Error("every row has a payer; a delta line must leave the column NULL rather than fill it")
	}
}

// TestMigration0072DDLIsRerunnable holds the serving view to the replay
// bar, and confirms it still answers afterwards.
func TestMigration0072DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedIncomeReportFixture(t, db, ctx)

	rerunMigrationDDL(t, db, ctx, "0072_web_income.sql")

	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM web_income`).Scan(&n); err != nil {
		t.Fatalf("web_income after re-run: %v", err)
	}
	if n == 0 {
		t.Error("web_income is empty after a re-run")
	}
	// The three reporting currencies the picker switches between.
	assertMacroProjects(t, db, ctx, "web_income",
		"occurred_at", "silver_source_id", "account_external_id", "display_name",
		"account_label", "account_kind", "payer_name", "income_primary",
		"income_detailed", "income_primary_label", "income_label",
		"provider_income_label", "value_usd", "value_chf", "value_eur")
}
