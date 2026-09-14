package gold

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"
)

// seedIncomeReportFixture adds what the report macros need beyond the
// schema fixture: FX rates, so the conversions have something to
// resolve, and a tax row for the withholding memo.
func seedIncomeReportFixture(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	seedIncomeFixture(t, db, ctx)
	if _, err := db.ExecContext(ctx, `
        -- fx_norm reads a row as "one QUOTE buys mid_rate BASE", so
        -- (base CHF, quote USD, 0.5) is the USD->CHF direction.
        --
        -- Each pair is quoted BOTH ways and the two are exact inverses.
        -- fx_norm synthesises the reverse of every quote, so a pair
        -- quoted twice inconsistently leaves two candidate rates on the
        -- same day and the same source, and which one fx_daily keeps is
        -- then a tie-break rather than a fact.
        INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate) VALUES
            ('inc-src', 0, 'CHF', 'USD', 0.5),
            ('inc-src', 0, 'USD', 'CHF', 2.0),
            ('inc-src', 0, 'EUR', 'USD', 0.8),
            ('inc-src', 0, 'USD', 'EUR', 1.25);

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

        -- Lines in two LATER months, one of them in a currency that is
        -- not the reporting default. Everything else in this fixture
        -- sits at one instant in USD, which is enough to check the
        -- arithmetic and nothing else: these three are what the period
        -- bucketing and the FX pair are read from. They are outside
        -- every other test's window on purpose.
        --   2_700_000s is 1970-02-01, 5_300_000s is 1970-03-03.
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, instrument_external_id,
                                  kind, currency, net_amount) VALUES
            ('inc-src', 'T-FEB-CHF', 2700000, 'BRK1', 'INST-NAMED', 'dividend', 'CHF', 100),
            ('inc-src', 'T-MAR-USD', 5300000, 'BRK1', 'INST-NAMED', 'dividend', 'USD',  40),
            ('inc-src', 'T-MAR-REV', 5300000, 'BRK1', 'INST-NAMED', 'dividend', 'USD', -10);
    `); err != nil {
		t.Fatalf("seed the report fixture: %v", err)
	}
}

// TestIncomeSummaryArithmetic pins the summary's arithmetic: the two
// columns are positive magnitudes and net_income is the difference,
// mirroring the spending summary exactly.
func TestIncomeSummaryArithmetic(t *testing.T) {
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

// TestIncomeReportsBucketAndConvert pins the two things the summary
// does to a line beyond adding it up: it puts the line in a PERIOD
// bucket, and it CONVERTS the amount into the currency asked for.
//
// Both are easy to get silently right on a one-instant, one-currency
// fixture — a summary that ignored --period and one that ignored -x
// would both pass such a check — so this reads the three lines seeded
// in later months, one of them in CHF, and asks for CHF monthly.
func TestIncomeReportsBucketAndConvert(t *testing.T) {
	db, ctx := openMigrated(t)
	seedIncomeReportFixture(t, db, ctx)

	const window = 6_000_000
	rows, err := IncomeSummary(ctx, db, 0, window, "CHF", "month")
	if err != nil {
		t.Fatalf("IncomeSummary: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("monthly bucketing returned %d buckets, want 3 (Jan, Feb, Mar)", len(rows))
	}
	for i, r := range rows {
		if r.PeriodStart == nil {
			t.Fatalf("bucket %d has no period start; only `total` may", i)
		}
		if i > 0 && *rows[i-1].PeriodStart >= *r.PeriodStart {
			t.Errorf("buckets are not ascending: %d then %d",
				*rows[i-1].PeriodStart, *r.PeriodStart)
		}
	}

	// February holds the one CHF line. Asked for in CHF it must pass
	// through UNCONVERTED — a rate applied to a line already in the
	// reporting currency is the classic double conversion.
	feb := rows[1]
	if got := decimalOf(t, feb.Income); got != 100 {
		t.Errorf("February income = %v CHF, want 100 (the CHF line, unconverted)", got)
	}
	if feb.TxnCount != 1 {
		t.Errorf("February txn_count = %d, want 1", feb.TxnCount)
	}

	// March holds two USD lines, and USD->CHF is 0.5 in the fixture.
	// Both columns convert, so the difference is a CHF difference.
	mar := rows[2]
	if got := decimalOf(t, mar.Income); got != 20 {
		t.Errorf("March income = %v CHF, want 20 (USD 40 at 0.5)", got)
	}
	if got := decimalOf(t, mar.Reversals); got != 5 {
		t.Errorf("March reversals = %v CHF, want 5 (USD 10 at 0.5)", got)
	}
	if got := decimalOf(t, mar.NetIncome); got != 15 {
		t.Errorf("March net_income = %v CHF, want 15", got)
	}

	// `total` over the same window is the same money in one bucket, so
	// the buckets partition the window rather than sampling it.
	totals, err := IncomeSummary(ctx, db, 0, window, "CHF", "total")
	if err != nil {
		t.Fatalf("IncomeSummary total: %v", err)
	}
	if len(totals) != 1 {
		t.Fatalf("total bucketing returned %d rows, want 1", len(totals))
	}
	var sumIncome, sumReversals float64
	var sumCount int64
	for _, r := range rows {
		sumIncome += decimalOf(t, r.Income)
		sumReversals += decimalOf(t, r.Reversals)
		sumCount += r.TxnCount
	}
	if got := decimalOf(t, totals[0].Income); !closeEnough(got, sumIncome) {
		t.Errorf("total income = %v, the monthly buckets sum to %v", got, sumIncome)
	}
	if got := decimalOf(t, totals[0].Reversals); !closeEnough(got, sumReversals) {
		t.Errorf("total reversals = %v, the monthly buckets sum to %v", got, sumReversals)
	}
	if totals[0].TxnCount != sumCount {
		t.Errorf("total txn_count = %d, the monthly buckets sum to %d", totals[0].TxnCount, sumCount)
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
		t.Error("the late window has no income; the join proves nothing")
	}

	// ...and the mirror case, which a LEFT join would have dropped: a
	// bucket holding withholding and NO income at all. The memo is the
	// one row a reader went looking for, and it must not vanish with
	// the income that was not there.
	if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount)
        VALUES ('inc-src', 'T-WHT-ONLY', 6000, 'BRK1', 'tax', 'USD', -9)`); err != nil {
		t.Fatalf("seed a withholding-only window: %v", err)
	}
	only, err := IncomeSummary(ctx, db, 5500, 6500, "USD", "total")
	if err != nil {
		t.Fatalf("IncomeSummary: %v", err)
	}
	if len(only) != 1 {
		t.Fatalf("a window with withholding and no income returned %d rows, want 1", len(only))
	}
	if got := decimalOf(t, only[0].Withheld); got != 9 {
		t.Errorf("withheld = %v in a window with no income, want 9", got)
	}
	if only[0].TxnCount != 0 {
		t.Errorf("txn_count = %d, want 0: there were no income lines", only[0].TxnCount)
	}
	if only[0].NetIncome != nil {
		t.Errorf("net_income = %v, want nil: no income converted", *only[0].NetIncome)
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

// TestIncomeTypesMultiMirrorsTheSpendingPair pins what migration 0074
// made true: the multi-currency form of the types report is a faithful
// WIDENING of its single-currency twin, not a lossy summary of it.
//
// Nothing consumes it yet, which is exactly why it is worth pinning
// now — a shape nobody reads is a shape nobody notices drifting, and
// the first Metabase card built on it would inherit whatever it had.
func TestIncomeTypesMultiMirrorsTheSpendingPair(t *testing.T) {
	db, ctx := openMigrated(t)
	seedIncomeReportFixture(t, db, ctx)

	// The same 16 columns the spending pair publishes, in the same
	// order, with income's nouns.
	assertMacroProjects(t, db, ctx, "report_income_types_multi(0, 5000, 'total', 'detailed')",
		"period_start", "type", "type_label", "txn_count",
		"income_usd", "income_chf", "income_eur",
		"reversals_usd", "reversals_chf", "reversals_eur",
		"net_income_usd", "net_income_chf", "net_income_eur",
		"share_usd", "share_chf", "share_eur")

	// Money is DECIMAL, not VARCHAR: a string cannot be summed, ordered
	// numerically or divided, which is why the shares were impossible
	// to publish before.
	rows, err := db.QueryContext(ctx, `
        SELECT column_name, column_type
          FROM (DESCRIBE SELECT * FROM report_income_types_multi(0, 5000, 'total', 'detailed'))`)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	defer rows.Close()
	types := map[string]string{}
	for rows.Next() {
		var col, typ string
		if err := rows.Scan(&col, &typ); err != nil {
			t.Fatalf("scan describe: %v", err)
		}
		types[col] = typ
	}
	for _, col := range []string{
		"income_usd", "income_chf", "income_eur",
		"reversals_usd", "reversals_chf", "reversals_eur",
		"net_income_usd", "net_income_chf", "net_income_eur",
	} {
		if !strings.HasPrefix(types[col], "DECIMAL") {
			t.Errorf("%s is %s, want DECIMAL", col, types[col])
		}
	}

	// The USD column of the multi agrees, row for row, with the
	// single-currency report asked for in USD. That is what "mirror"
	// has to mean; a projection that merely looked right could still be
	// summing the wrong side of the split.
	single := map[string][3]string{}
	srows, err := db.QueryContext(ctx, `
        SELECT type, income, reversals, net_income
          FROM report_income_types(0, 5000, 'USD', 'total', 'detailed')`)
	if err != nil {
		t.Fatalf("single: %v", err)
	}
	for srows.Next() {
		var typ string
		var inc, rev, net sql.NullString
		if err := srows.Scan(&typ, &inc, &rev, &net); err != nil {
			srows.Close()
			t.Fatalf("scan single: %v", err)
		}
		single[typ] = [3]string{inc.String, rev.String, net.String}
	}
	srows.Close()
	if len(single) == 0 {
		t.Fatal("the fixture produced no type rows; the comparison would be vacuous")
	}

	mrows, err := db.QueryContext(ctx, `
        SELECT type, CAST(income_usd AS VARCHAR), CAST(reversals_usd AS VARCHAR),
               CAST(net_income_usd AS VARCHAR), share_usd
          FROM report_income_types_multi(0, 5000, 'total', 'detailed')`)
	if err != nil {
		t.Fatalf("multi: %v", err)
	}
	defer mrows.Close()
	seen, shareTotal := 0, 0.0
	for mrows.Next() {
		var typ string
		var inc, rev, net sql.NullString
		var share sql.NullFloat64
		if err := mrows.Scan(&typ, &inc, &rev, &net, &share); err != nil {
			t.Fatalf("scan multi: %v", err)
		}
		seen++
		want, ok := single[typ]
		if !ok {
			t.Errorf("the multi reports type %q the single-currency report does not", typ)
			continue
		}
		if got := [3]string{inc.String, rev.String, net.String}; got != want {
			t.Errorf("%s: multi USD = %v, single USD = %v", typ, got, want)
		}
		if share.Valid {
			shareTotal += share.Float64
		}
	}
	if seen != len(single) {
		t.Errorf("the multi reports %d type(s), the single-currency report %d", seen, len(single))
	}
	// One bucket, so the shares over its magnitudes sum to 1.
	if !closeEnough(shareTotal, 1) {
		t.Errorf("share_usd over one bucket sums to %v, want 1", shareTotal)
	}
}

// TestIncomeTypesMultiGuardsAndPartitions pins the three things the
// mirror check above cannot reach, because it reads one bucket in one
// currency.
//
// Each was a defect in the shape 0071 published, so each is worth a
// test that fails if the repair is undone:
//
//   - the PER-CURRENCY n_converted guard. A bucket where nothing
//     converted must read NULL, not 0, and the guard is counted per
//     currency — so a type whose lines convert to USD but not to EUR
//     reports a figure in one column and a blank in the other.
//   - the CHF and EUR columns at all. The body is a hand-written
//     three-fold repetition, so a slot filled from the wrong currency
//     is invisible to a USD-only assertion.
//   - the PARTITION BY on the shares. Over `total` there is exactly one
//     bucket, so a partition and no partition are indistinguishable;
//     only a multi-bucket window tells them apart.
func TestIncomeTypesMultiGuardsAndPartitions(t *testing.T) {
	db, ctx := openMigrated(t)
	seedIncomeReportFixture(t, db, ctx)

	// A line in a currency the fixture quotes NOTHING for, in its own
	// month: every other column converts, this one cannot.
	if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, instrument_external_id,
                                  kind, currency, net_amount)
        VALUES ('inc-src', 'T-APR-GBP', 8000000, 'BRK1', 'INST-NAMED', 'coupon', 'GBP', 60)`); err != nil {
		t.Fatalf("seed an unconvertible line: %v", err)
	}

	const window = 9_000_000
	rows, err := db.QueryContext(ctx, `
        SELECT period_start, type,
               CAST(income_usd AS VARCHAR), CAST(income_chf AS VARCHAR),
               CAST(income_eur AS VARCHAR), share_usd
          FROM report_income_types_multi(0, ?, 'month', 'detailed')
         ORDER BY period_start, type`, window)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	type row struct {
		usd, chf, eur sql.NullString
		share         sql.NullFloat64
	}
	byBucket := map[int64][]row{}
	unconvertible := 0
	for rows.Next() {
		var period sql.NullInt64
		var typ string
		var r row
		if err := rows.Scan(&period, &typ, &r.usd, &r.chf, &r.eur, &r.share); err != nil {
			t.Fatalf("scan: %v", err)
		}
		byBucket[period.Int64] = append(byBucket[period.Int64], r)
		// The GBP line: nothing converts it, so every money column of
		// its row is NULL rather than 0.
		if !r.usd.Valid {
			unconvertible++
			if r.chf.Valid || r.eur.Valid {
				t.Errorf("a bucket that did not convert to USD reported CHF=%v EUR=%v",
					r.chf, r.eur)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if unconvertible != 1 {
		t.Errorf("unconvertible rows = %d, want 1: the n_converted guard reports NULL, never 0",
			unconvertible)
	}

	// More than one bucket, or the partition below is untested.
	if len(byBucket) < 2 {
		t.Fatalf("monthly bucketing produced %d bucket(s); the partition is untested", len(byBucket))
	}
	// Each bucket's shares sum to 1 ON ITS OWN. Without the PARTITION BY
	// they would sum to 1 across the whole window instead, so every
	// bucket but one would come up short.
	for period, rs := range byBucket {
		total := 0.0
		any := false
		for _, r := range rs {
			if r.share.Valid {
				total += r.share.Float64
				any = true
			}
		}
		if !any {
			continue // the unconvertible bucket has no share to sum
		}
		if !closeEnough(total, 1) {
			t.Errorf("bucket %d: share_usd sums to %v, want 1 — the shares are not partitioned by bucket",
				period, total)
		}
	}

	// The CHF and EUR columns carry their own currency's arithmetic,
	// not a copy of USD's. The fixture's USD->CHF is 0.5 and USD->EUR
	// 1.25, so a February CHF line and a March USD line differ by
	// construction.
	var chf, eur, usd sql.NullString
	if err := db.QueryRowContext(ctx, `
        SELECT CAST(income_usd AS VARCHAR), CAST(income_chf AS VARCHAR),
               CAST(income_eur AS VARCHAR)
          FROM report_income_types_multi(0, ?, 'total', 'detailed')
         WHERE type = 'INCOME_DIVIDENDS'`, window).Scan(&usd, &chf, &eur); err != nil {
		t.Fatalf("read the dividend row: %v", err)
	}
	if usd.String == chf.String || usd.String == eur.String || chf.String == eur.String {
		t.Errorf("two currency columns are equal (usd=%q chf=%q eur=%q); "+
			"a hand-written three-fold body can fill a slot from the wrong one",
			usd.String, chf.String, eur.String)
	}
}

// TestMigration0074DDLIsRerunnable holds the re-issue to the replay bar.
func TestMigration0074DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedIncomeReportFixture(t, db, ctx)

	rerunMigrationDDL(t, db, ctx, "0074_report_income_types_multi.sql")

	var n int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM report_income_types_multi(0, 5000, 'total', 'detailed')").Scan(&n); err != nil {
		t.Errorf("report_income_types_multi after the re-run: %v", err)
	}
	if n == 0 {
		t.Error("the re-run left the macro answering nothing")
	}
}

// TestTransactionsCarriesBothFamiliesInOrder is the positional-scan
// guard for report_transactions, which 0071 re-issued with three new
// columns. The row struct, the scan list and the macro must move
// together, and a permutation of two same-typed columns is a SILENT
// shift rather than an error — so the values are read back and matched
// to the rows that should carry them.
func TestTransactionsCarriesBothFamiliesInOrder(t *testing.T) {
	db, ctx := openMigrated(t)
	seedIncomeReportFixture(t, db, ctx)

	rows, err := TransactionsBetween(ctx, db, 0, 5000, "USD", SortAscending)
	if err != nil {
		t.Fatalf("TransactionsBetween: %v", err)
	}
	byID := map[string]TransactionRow{}
	for _, r := range rows {
		byID[r.TransactionExternalID] = r
	}

	// A row with an income verdict and no spending one: the trios must
	// not be crossed.
	div, ok := byID["T-DIVIDEND"]
	if !ok {
		t.Fatal("the dividend is missing from report_transactions")
	}
	if div.IncomeDetailed == nil || *div.IncomeDetailed != "INCOME_DIVIDENDS" {
		t.Errorf("T-DIVIDEND income_detailed = %v, want INCOME_DIVIDENDS", div.IncomeDetailed)
	}
	if div.PayerName == nil || *div.PayerName != "Example Dividend Corp" {
		t.Errorf("T-DIVIDEND payer = %v, want the instrument's name", div.PayerName)
	}
	if div.IncomePrimary == nil || *div.IncomePrimary != "INCOME" {
		t.Errorf("T-DIVIDEND income_primary = %v, want INCOME", div.IncomePrimary)
	}
	if div.SpendDetailed != nil || div.MerchantName != nil {
		t.Errorf("T-DIVIDEND carries a spending verdict (%v, %v); the two trios are crossed",
			div.SpendDetailed, div.MerchantName)
	}

	// ...and the mirror: a row with a spending verdict and no income
	// one. Between them these pin that the six columns land in their
	// own fields rather than one shifted set.
	buy, ok := byID["T-BUY"]
	if !ok {
		t.Fatal("the purchase is missing from report_transactions")
	}
	if buy.SpendDetailed == nil || *buy.SpendDetailed != "FOOD_AND_DRINK_GROCERIES" {
		t.Errorf("T-BUY spend_detailed = %v", buy.SpendDetailed)
	}
	if buy.MerchantName == nil || *buy.MerchantName != "CORNER MARKET" {
		t.Errorf("T-BUY merchant = %v, want the signature fallback", buy.MerchantName)
	}
	if buy.IncomeDetailed != nil || buy.PayerName != nil {
		t.Errorf("T-BUY carries an income verdict (%v, %v)", buy.IncomeDetailed, buy.PayerName)
	}

	// The columns either side of the new trio still land where they
	// were: an inserted column shifts everything after it, and these
	// are what would catch that.
	if buy.Description == nil && buy.Kind == "" {
		t.Error("the columns before the new trio no longer resolve")
	}
	if div.ValueOutCcy == nil {
		t.Error("value_outccy, which follows the new trio, no longer resolves")
	}
}
