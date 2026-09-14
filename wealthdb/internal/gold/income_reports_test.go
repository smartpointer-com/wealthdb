package gold

import (
	"context"
	"database/sql"
	"strconv"
	"testing"
)

// seedIncomeReportFixture adds what the report macros need beyond the
// schema fixture: FX rates, so the conversions have something to
// resolve, and a tax row for the withholding memo.
func seedIncomeReportFixture(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	seedIncomeFixture(t, db, ctx)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate) VALUES
            ('inc-src', 0, 'USD', 'CHF', 0.9),
            ('inc-src', 0, 'USD', 'EUR', 0.8),
            ('inc-src', 0, 'CHF', 'USD', 1.1),
            ('inc-src', 0, 'EUR', 'USD', 1.25);

        -- Tax withheld at source, in the same window on the same
        -- account: the summary's memo, and an OUTFLOW, so it is in no
        -- income population.
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount)
        VALUES ('inc-src', 'T-WHT', 1000, 'BRK1', 'tax', 'USD', -6);

        -- One spending row with a verdict, so the report_transactions
        -- re-issue can be checked from both sides at once.
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount, counterparty)
        VALUES ('inc-src', 'T-BUY', 1000, 'CASH1', 'purchase', 'USD', -25, 'Corner Market');
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, assigned_at)
        VALUES ('inc-src', 'T-BUY', 'CORNER MARKET', 1, 'FOOD_AND_DRINK_GROCERIES', 'rule', 100);
    `); err != nil {
		t.Fatalf("seed the report fixture: %v", err)
	}
}

// TestIncomeReportsBucketAndConvert pins the summary's arithmetic: the
// two columns are positive magnitudes and net_income is the difference,
// mirroring the spending summary exactly.
func TestIncomeReportsBucketAndConvert(t *testing.T) {
	db, ctx := openMigrated(t)
	seedIncomeReportFixture(t, db, ctx)

	rows, err := IncomeSummary(ctx, db, 0, 5000, "USD", "total")
	if err != nil {
		t.Fatalf("IncomeSummary: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("total bucketing returned %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.PeriodStart != nil {
		t.Error("the total bucket must report a nil period start")
	}
	// Every positive line, and every negative one, from the base.
	var income, reversals float64
	if err := db.QueryRowContext(ctx, `
        SELECT COALESCE(SUM(CASE WHEN net_amount > 0 THEN net_amount ELSE 0 END), 0),
               COALESCE(SUM(CASE WHEN net_amount < 0 THEN -net_amount ELSE 0 END), 0)
          FROM income_lines_base(0, 5000)`).Scan(&income, &reversals); err != nil {
		t.Fatalf("sum the base: %v", err)
	}
	if got := decimalOf(t, r.Income); got != income {
		t.Errorf("income = %v, want %v", got, income)
	}
	if got := decimalOf(t, r.Reversals); got != reversals {
		t.Errorf("reversals = %v, want %v", got, reversals)
	}
	if got := decimalOf(t, r.NetIncome); got != income-reversals {
		t.Errorf("net_income = %v, want %v", got, income-reversals)
	}
	if reversals == 0 {
		t.Fatal("the fixture has no reversal; the subtraction proves nothing")
	}
}

// TestIncomeWithheldIsAMemo pins the one column with no spending twin:
// present on the summary, absent from types, and never subtracted.
func TestIncomeWithheldIsAMemo(t *testing.T) {
	db, ctx := openMigrated(t)
	seedIncomeReportFixture(t, db, ctx)

	rows, err := IncomeSummary(ctx, db, 0, 5000, "USD", "total")
	if err != nil {
		t.Fatalf("IncomeSummary: %v", err)
	}
	r := rows[0]
	if got := decimalOf(t, r.Withheld); got != 6 {
		t.Errorf("withheld = %v, want 6 (the tax row's magnitude)", got)
	}
	// The memo does not move the arithmetic.
	if decimalOf(t, r.NetIncome) != decimalOf(t, r.Income)-decimalOf(t, r.Reversals) {
		t.Error("net_income reads the withholding memo; income is gross as booked")
	}

	// A window with income and no tax row keeps the income and reports
	// an empty memo rather than dropping the bucket.
	empty, err := IncomeSummary(ctx, db, 8000, 9500, "USD", "total")
	if err != nil {
		t.Fatalf("IncomeSummary: %v", err)
	}
	if len(empty) != 1 {
		t.Fatalf("a window with income and no withholding returned %d rows, want 1", len(empty))
	}
	if empty[0].Withheld != nil {
		t.Errorf("withheld = %v in a window with no tax row, want nil", *empty[0].Withheld)
	}
	if empty[0].TxnCount == 0 {
		t.Error("the late window has no income; the LEFT JOIN proves nothing")
	}
}

// TestIncomeTypesReconcileWithSummary pins the identity every report
// pair owes its reader: the parts sum to the whole, in the same window
// and the same bucket.
func TestIncomeTypesReconcileWithSummary(t *testing.T) {
	db, ctx := openMigrated(t)
	seedIncomeReportFixture(t, db, ctx)

	summary, err := IncomeSummary(ctx, db, 0, 5000, "USD", "total")
	if err != nil {
		t.Fatalf("IncomeSummary: %v", err)
	}
	types, err := IncomeTypes(ctx, db, 0, 5000, "USD", "total", "detailed")
	if err != nil {
		t.Fatalf("IncomeTypes: %v", err)
	}
	var net, shares float64
	var count int64
	for _, r := range types {
		net += decimalOf(t, r.NetIncome)
		count += r.TxnCount
		if r.Share != nil {
			shares += *r.Share
		}
	}
	if got := decimalOf(t, summary[0].NetIncome); !closeEnough(got, net) {
		t.Errorf("types sum to %v, summary says %v", net, got)
	}
	if count != summary[0].TxnCount {
		t.Errorf("types count %d rows, summary says %d", count, summary[0].TxnCount)
	}
	if !closeEnough(shares, 1) {
		t.Errorf("shares sum to %v, want 1", shares)
	}
}

// TestIncomeWronglyMatchedDepositStaysVisible is the reconciliation
// identity's negative twin, and the reason the identity alone is not
// enough: the two reports are built from one base, so they agree
// whatever that base holds. What has to be checked separately is that
// the base holds the right rows — a deposit the matcher wrongly paired
// leaves it entirely, and no total would notice.
func TestIncomeWronglyMatchedDepositStaysVisible(t *testing.T) {
	db, ctx := openMigrated(t)
	seedIncomeReportFixture(t, db, ctx)

	before, err := IncomeSummary(ctx, db, 0, 5000, "USD", "total")
	if err != nil {
		t.Fatalf("IncomeSummary: %v", err)
	}

	// Mark one ordinary receipt as an own-account move, the way a
	// matcher false positive would.
	if _, err := db.ExecContext(ctx, `
        UPDATE income_txn_enrichment SET income_detailed = 'internal_transfer', provenance = 'matcher'
         WHERE transaction_external_id = 'T-WIRE'`); err != nil {
		t.Fatalf("mark the deposit: %v", err)
	}

	after, err := IncomeSummary(ctx, db, 0, 5000, "USD", "total")
	if err != nil {
		t.Fatalf("IncomeSummary: %v", err)
	}
	if after[0].TxnCount >= before[0].TxnCount {
		t.Fatal("the marked deposit did not leave the base; the fixture proves nothing")
	}
	// ...and the two reports STILL reconcile, which is the point: the
	// identity cannot catch this, and only the population layer can.
	types, err := IncomeTypes(ctx, db, 0, 5000, "USD", "total", "detailed")
	if err != nil {
		t.Fatalf("IncomeTypes: %v", err)
	}
	var net float64
	for _, r := range types {
		net += decimalOf(t, r.NetIncome)
	}
	if !closeEnough(net, decimalOf(t, after[0].NetIncome)) {
		t.Error("the reports disagree after the row left; they are built from one base and cannot")
	}
	// The row is still in the enrichment population, which is what a
	// later pass re-decides it from.
	var inPopulation int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM income_enrichment_population(0, 5000)
         WHERE transaction_external_id = 'T-WIRE'`).Scan(&inPopulation); err != nil {
		t.Fatalf("read the population: %v", err)
	}
	if inPopulation != 1 {
		t.Error("the marked deposit left the enrichment population; the pass could not re-decide it")
	}
}

// TestIncomeTypesLevelsAndLabels pins the two levels and the columns
// that carry them, including the one default that differs from
// spending's.
func TestIncomeTypesLevelsAndLabels(t *testing.T) {
	db, ctx := openMigrated(t)
	seedIncomeReportFixture(t, db, ctx)

	detailed, err := IncomeTypes(ctx, db, 0, 5000, "USD", "total", "detailed")
	if err != nil {
		t.Fatalf("IncomeTypes: %v", err)
	}
	primary, err := IncomeTypes(ctx, db, 0, 5000, "USD", "total", "primary")
	if err != nil {
		t.Fatalf("IncomeTypes: %v", err)
	}
	if len(primary) >= len(detailed) {
		t.Errorf("primary has %d rows and detailed %d; the primary level must fold",
			len(primary), len(detailed))
	}
	// The fold is the reason `detailed` is the CLI default: one
	// vendored primary swallows every earned and yielded type.
	seen := map[string]string{}
	for _, r := range primary {
		seen[r.Type] = r.TypeLabel
	}
	if seen["INCOME"] != "Income" {
		t.Errorf("the vendored primary reads %q, want Income", seen["INCOME"])
	}
	for _, r := range detailed {
		if r.Type == "" || r.TypeLabel == "" {
			t.Errorf("a type row carries an empty key or label: %+v", r)
		}
		if r.Type == r.TypeLabel && r.Type != "(uncategorized)" {
			t.Errorf("%s: the label is the raw value; the dimension's label did not resolve", r.Type)
		}
	}
	// An unplaced receipt is a row of the report rather than a gap in
	// it, which is what makes the backlog visible to a reader.
	var foundBacklog bool
	for _, r := range detailed {
		if r.Type == "(uncategorized)" {
			foundBacklog = true
		}
	}
	if !foundBacklog {
		t.Error("the fixture's unplaced receipts do not appear as (uncategorized)")
	}
}

// TestIncomeTransactionsCarryThePayer pins the transaction grain: the
// payer resolution reaches the report, and a delta line carries none.
func TestIncomeTransactionsCarryThePayer(t *testing.T) {
	db, ctx := openMigrated(t)
	seedIncomeReportFixture(t, db, ctx)

	rows, err := IncomeTransactions(ctx, db, 0, 5000, "USD")
	if err != nil {
		t.Fatalf("IncomeTransactions: %v", err)
	}
	byID := map[string]IncomeTransactionRow{}
	for _, r := range rows {
		byID[r.TransactionExternalID] = r
	}
	if got := byID["T-DIVIDEND"]; got.PayerName == nil || *got.PayerName != "Example Dividend Corp" {
		t.Errorf("T-DIVIDEND payer = %v, want the instrument's name", got.PayerName)
	}
	if got := byID["T-SIGONLY"]; got.PayerName == nil || *got.PayerName != "SAMPLE TAX OFFICE" {
		t.Errorf("T-SIGONLY payer = %v, want the signature", got.PayerName)
	}
	if got, ok := byID["T-GIFT"]; !ok || got.PayerName != nil {
		t.Errorf("T-GIFT payer = %v, want none: a delta line has no payer", got.PayerName)
	}
	// Canonical signs at the transaction grain, unlike the aggregates.
	if got := byID["T-DIV-NEG"]; got.NetAmount == nil || decimalOf(t, got.NetAmount) >= 0 {
		t.Errorf("T-DIV-NEG net_amount = %v, want the reversal's own negative sign", got.NetAmount)
	}
}

// TestIncomeReportColumnsSurviveAReissue is the positional-scan guard:
// each reader scans `SELECT *`, so a macro re-issue that adds, removes
// or reorders a column breaks the scan rather than the query.
func TestIncomeReportColumnsSurviveAReissue(t *testing.T) {
	db, ctx := openMigrated(t)
	assertMacroProjects(t, db, ctx, "report_income_summary(0, 5000, 'USD', 'total')",
		"period_start", "txn_count", "income", "reversals", "net_income", "withheld")
	assertMacroProjects(t, db, ctx, "report_income_types(0, 5000, 'USD', 'total', 'detailed')",
		"period_start", "type", "type_label", "txn_count", "income", "reversals",
		"net_income", "share")
	assertMacroProjects(t, db, ctx, "report_income_transactions(0, 5000, 'USD')",
		"silver_source_id", "transaction_external_id", "occurred_at",
		"account_external_id", "account_kind", "display_name", "nickname",
		"account_category", "kind", "payer_signature", "payer_name",
		"income_primary", "income_detailed", "income_label", "income_primary_label",
		"provenance", "provider_income_detailed", "provider_income_label",
		"currency", "net_amount", "description", "counterparty", "value_outccy")
}

// TestMigration0071DDLIsRerunnable holds the report migration to the
// replay bar, and checks the one thing in it that touches an existing
// surface: report_transactions carries the spending trio it already
// had, plus the income one.
func TestMigration0071DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedIncomeReportFixture(t, db, ctx)

	rerunMigrationDDL(t, db, ctx, "0071_report_income.sql")

	for _, macro := range []string{
		"report_income_summary(0, 5000, 'USD', 'total')",
		"report_income_types(0, 5000, 'USD', 'total', 'detailed')",
		"report_income_types_multi(0, 5000, 'total', 'detailed')",
		"report_income_transactions(0, 5000, 'USD')",
		"report_income_transactions_multi(0, 5000)",
		"income_lines_outccy(0, 5000, 'USD')",
		"income_lines_multi(0, 5000)",
		"report_transactions(0, 5000, 'USD')",
		"report_spending_transactions(0, 5000, 'USD')",
	} {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+macro).Scan(&n); err != nil {
			t.Errorf("%s after re-run: %v", macro, err)
		}
	}
	// The re-issue carried 0042's whole body forward: the spending
	// trio is still there, and the income trio is beside it.
	rows, err := TransactionsBetween(ctx, db, 0, 5000, "USD", SortAscending)
	if err != nil {
		t.Fatalf("TransactionsBetween: %v", err)
	}
	var sawIncome, sawSpend bool
	for _, r := range rows {
		if r.IncomeDetailed != nil || r.PayerName != nil {
			sawIncome = true
		}
		if r.SpendDetailed != nil || r.MerchantName != nil {
			sawSpend = true
		}
	}
	if !sawIncome {
		t.Error("no row carries an income verdict; the new join is not wired")
	}
	if !sawSpend {
		t.Error("no row carries a spending verdict; the re-issue dropped 0042's join")
	}
}

// decimalOf parses a canonical decimal string the readers hand back.
func decimalOf(t *testing.T, s *string) float64 {
	t.Helper()
	if s == nil {
		return 0
	}
	v, err := strconv.ParseFloat(*s, 64)
	if err != nil {
		t.Fatalf("parse %q: %v", *s, err)
	}
	return v
}

// closeEnough compares two sums of DECIMAL(28,4) values.
func closeEnough(a, b float64) bool {
	d := a - b
	return d < 0.0001 && d > -0.0001
}
