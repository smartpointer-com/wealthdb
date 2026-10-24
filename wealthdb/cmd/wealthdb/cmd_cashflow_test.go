package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// setupCashflowGold builds a gold file the cashflow CLI can be driven
// against: a pooled cash account and brokerage, a retirement plan
// beyond the boundary, and one line per section.
//
// Every id, name and figure is invented.
func setupCashflowGold(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	goldPath := filepath.Join(dir, "wealthdb.db")

	db, err := gold.OpenFresh(goldPath)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	ctx := context.Background()
	at := func(m time.Month, d int) int64 {
		return time.Date(2026, m, d, 12, 0, 0, 0, time.UTC).Unix()
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO silver_sources(silver_source_id, silver_kind, silver_path,
			high_watermark, first_loaded_at, last_loaded_at)
		VALUES ('bank', 'chase', '/tmp/bank.db', -1, 0, 0);

		INSERT INTO accounts(silver_source_id, account_external_id, account_kind,
			tax_wrapper, display_name, first_seen_at, last_seen_at)
		VALUES ('bank', 'CASH0005678', 'cash',      'taxable_personal', 'Everyday',  1, 1),
		       ('bank', 'BRK0009999',  'brokerage', 'taxable_personal', 'Brokerage', 1, 1),
		       ('bank', 'IRA0001111',  'brokerage', 'roth_ira',         'Plan',      1, 1);

		INSERT INTO instruments(silver_source_id, instrument_external_id, asset_class,
			symbol, name, first_seen_at, last_seen_at)
		VALUES ('bank', 'INST1', 'public_equity', 'EXDC', 'Example Dividend Corp', 1, 1);

		-- The boundary the enrichment pass stamps.
		INSERT INTO cashflow_wrapper_sides(tax_wrapper, side, class)
		VALUES ('taxable_personal', 'household', NULL),
		       ('roth_ira', 'vehicle', 'retirement');
	`); err != nil {
		t.Fatalf("seed dimensions: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO fx_rates(silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
		VALUES ('bank', ?, 'USD', 'CHF', CAST('1.1' AS DECIMAL(20,10)))`, at(time.January, 1)); err != nil {
		t.Fatalf("seed fx: %v", err)
	}

	rows := []struct {
		id, account, kind, instrument string
		month                         time.Month
		day                           int
		amount                        string
	}{
		{"T-SALARY", "CASH0005678", "deposit", "", time.May, 5, "6000"},
		{"T-DIV", "BRK0009999", "dividend", "INST1", time.May, 8, "400"},
		{"T-SHOP", "CASH0005678", "purchase", "", time.May, 9, "-250"},
		{"T-TAX", "CASH0005678", "tax", "", time.May, 12, "-900"},
		{"T-BUY", "BRK0009999", "buy", "INST1", time.May, 15, "-3000"},
		{"T-MORT", "CASH0005678", "withdrawal", "", time.June, 1, "-1200"},
		{"T-RET", "CASH0005678", "withdrawal", "", time.June, 3, "-500"},
		// Never drawn: a plan's own dividend, and a leg with no
		// canonical sign.
		{"T-PLANDIV", "IRA0001111", "dividend", "INST1", time.June, 5, "120"},
		{"T-FX", "CASH0005678", "fx", "", time.June, 7, "-30"},
	}
	for _, r := range rows {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO transactions(silver_source_id, transaction_external_id, occurred_at,
				account_external_id, instrument_external_id, kind, currency, net_amount)
			VALUES ('bank', ?, ?, ?, ?, ?, 'USD', CAST(? AS DECIMAL(28,4)))`,
			r.id, at(r.month, r.day), r.account, nullIfEmpty(r.instrument), r.kind, r.amount); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO income_txn_enrichment(silver_source_id, transaction_external_id,
			payer_signature, signature_version, income_detailed, provenance, assigned_at)
		VALUES ('bank', 'T-SALARY', 'PAYROLL', 1, 'INCOME_SALARY', 'rule', 1),
		       ('bank', 'T-DIV',    NULL,      1, NULL,            'signature-only', 1),
		       ('bank', 'T-PLANDIV', NULL,     1, NULL,            'signature-only', 1);

		INSERT INTO spend_txn_enrichment(silver_source_id, transaction_external_id,
			merchant_signature, signature_version, spend_detailed, provenance,
			far_silver_source_id, far_account_external_id, far_class, assigned_at)
		VALUES ('bank', 'T-SHOP', 'CORNER MARKET', 1, 'FOOD_AND_DRINK_GROCERIES', 'signature-only', NULL, NULL, NULL, 1),
		       ('bank', 'T-TAX',  NULL,            1, 'GOVERNMENT_AND_NON_PROFIT_TAX_PAYMENT', 'rule', NULL, NULL, NULL, 1),
		       ('bank', 'T-MORT', 'MORTGAGE',      1, 'internal_transfer', 'rule', NULL, NULL, 'mortgage', 1),
		       ('bank', 'T-RET',  'PLAN',          1, 'retirement_transfer', 'rule', NULL, NULL, NULL, 1);

		INSERT INTO spend_merchant_categories(merchant_signature, merchant_name, spend_detailed,
			signature_version, assigned_at, model_name)
		VALUES ('CORNER MARKET', 'Corner Market', 'FOOD_AND_DRINK_GROCERIES', 1, 1, 'test-model');
	`); err != nil {
		t.Fatalf("seed the overlays: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	cfg := filepath.Join(dir, "wealthdb.cfg")
	body := fmt.Sprintf(`{"gold_db": %q, "default_currency": "USD", "silver_sources": []}`, goldPath)
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	return cfg
}

// TestCashflowCLIEndToEnd drives the four aggregating views — summary,
// flows, sankey and transactions — against seeded gold. The fifth,
// coverage, does not aggregate and is pinned at the report layer in
// internal/gold/cashflow_reports_test.go; what the CLI owns of it is
// the -x refusal, below.
func TestCashflowCLIEndToEnd(t *testing.T) {
	t.Parallel()
	cfg := setupCashflowGold(t)
	window := []string{"2026-05-01", "2026-06-30"}
	cf := func(args ...string) (string, string, int) {
		full := append([]string{"-c", cfg, "cashflow"}, args...)
		return run(t, append(full, window...)...)
	}

	t.Run("summary is the statement", func(t *testing.T) {
		so, se, code := cf("summary", "--period", "total")
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
		// 6000 salary + 400 dividend in; 250 + 900 out; 3000 invested;
		// 1200 to the mortgage; 500 to the plan.
		for _, want := range []string{"6400.00", "1150.00", "5250.00",
			"-3000.00", "-1200.00", "-500.00", "550.00"} {
			if !strings.Contains(so, want) {
				t.Errorf("the summary is missing %q:\n%s", want, so)
			}
		}
		// A vehicle's own dividend is never the household's cash flow,
		// and a kind with no canonical sign is in no figure at all.
		for _, unwanted := range []string{"120.00", "30.00"} {
			if strings.Contains(so, unwanted) {
				t.Errorf("the summary reached %q:\n%s", unwanted, so)
			}
		}
		// The memo columns are off by default.
		if strings.Contains(so, "yield") || strings.Contains(so, "savings_rate") {
			t.Errorf("a memo column is on by default:\n%s", so)
		}
	})

	t.Run("summary memo columns behind -C", func(t *testing.T) {
		so, se, code := cf("summary", "--period", "total", "-C", "+yield,+savings_rate,+taxes")
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
		for _, want := range []string{"yield_USD", "savings_rate_pct", "taxes_USD", "400.00", "900.00"} {
			if !strings.Contains(so, want) {
				t.Errorf("the memo is missing %q:\n%s", want, so)
			}
		}
	})

	t.Run("flows nets at the level asked", func(t *testing.T) {
		so, se, code := cf("flows", "--period", "total", "--level", "class")
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
		for _, want := range []string{"Earnings", "Yield", "Consumption", "Taxes",
			"Investments", "Mortgage", "Retirement", "Cash"} {
			if !strings.Contains(so, want) {
				t.Errorf("flows is missing the %q node:\n%s", want, so)
			}
		}
		// The residual is drawn as a use: cash the household kept is
		// cash the pool absorbed.
		if !strings.Contains(so, "-550.00") {
			t.Errorf("the cash row is not the negative of net_cash_flow:\n%s", so)
		}
	})

	t.Run("sankey is an edge list", func(t *testing.T) {
		so, se, code := cf("sankey")
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
		if !strings.Contains(so, "Household") {
			t.Errorf("the diagram has no hub:\n%s", so)
		}
		// The outflow leaf is the spending PRIMARY, so a grocery
		// purchase draws as "Food and drink" rather than as one of
		// close to a hundred detailed values.
		for _, want := range []string{"Salary", "Earnings", "Food and drink", "Consumption"} {
			if !strings.Contains(so, want) {
				t.Errorf("the diagram is missing %q:\n%s", want, so)
			}
		}
		// No node is ever a merchant, a payer, an account or an
		// instrument: the leaves are vocabulary.
		for _, unwanted := range []string{"Corner Market", "Example Dividend Corp", "Everyday", "CASH0005678"} {
			if strings.Contains(so, unwanted) {
				t.Errorf("the diagram names %q:\n%s", unwanted, so)
			}
		}
	})

	t.Run("sankey refuses a period", func(t *testing.T) {
		_, se, code := cf("sankey", "--period", "annual")
		if code != 2 {
			t.Fatalf("exit=%d, want 2; stderr=%s", code, se)
		}
		if !strings.Contains(se, "a diagram is a window, not a series") {
			t.Errorf("the refusal does not say why:\n%s", se)
		}
	})

	t.Run("sankey refuses a section level", func(t *testing.T) {
		_, se, code := cf("sankey", "--level", "section")
		if code != 2 {
			t.Fatalf("exit=%d, want 2; stderr=%s", code, se)
		}
		if !strings.Contains(se, "no inner column") {
			t.Errorf("the refusal does not say why:\n%s", se)
		}
	})

	t.Run("flows takes a period and a section level", func(t *testing.T) {
		if _, se, code := cf("flows", "--period", "annual", "--level", "section"); code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
	})

	t.Run("transactions name the line", func(t *testing.T) {
		so, se, code := cf("transactions")
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
		for _, want := range []string{"operating_in", "Earnings", "Salary",
			"investing", "Public equity", "Trades", "Corner Market",
			"Food and drink"} {
			if !strings.Contains(so, want) {
				t.Errorf("transactions is missing %q:\n%s", want, so)
			}
		}
		if strings.Contains(so, "T-PLANDIV") || strings.Contains(so, "T-FX") {
			t.Errorf("a row the statement never draws is listed:\n%s", so)
		}
	})

	t.Run("privacy redacts amounts and keeps the nodes", func(t *testing.T) {
		so, se, code := cf("transactions", "-p")
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
		if strings.Contains(so, "6000.00") {
			t.Errorf("an amount survived -p:\n%s", so)
		}
		if strings.Contains(so, "Corner Market") {
			t.Errorf("a merchant name survived -p:\n%s", so)
		}
		for _, want := range []string{"operating_in", "Earnings", "Salary"} {
			if !strings.Contains(so, want) {
				t.Errorf("-p redacted the node vocabulary, which is what makes the twin readable:\n%s", so)
			}
		}
	})

	t.Run("the diagram survives the privacy twin whole", func(t *testing.T) {
		plain, _, _ := cf("sankey", "-f", "csv_plain")
		redacted, se, code := cf("sankey", "-p", "-f", "csv_plain")
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
		// Only the value column moves: the nodes are vocabulary and the
		// shares are proportions, which is why the twin is
		// normalisation alone.
		if strings.Count(plain, "\n") != strings.Count(redacted, "\n") {
			t.Errorf("-p changed the edge count:\n%s\n%s", plain, redacted)
		}
		if !strings.Contains(redacted, "Household") {
			t.Errorf("-p redacted the hub:\n%s", redacted)
		}
	})

	t.Run("json carries the node ids", func(t *testing.T) {
		so, se, code := cf("sankey", "-f", "json", "-C", "+source_id,+target_id")
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
		if !strings.Contains(so, "operating_in.earnings.INCOME_SALARY") {
			t.Errorf("the node key is not the whole section.class.group:\n%s", so)
		}
	})

	t.Run("an unknown view and an unknown grain are usage errors", func(t *testing.T) {
		if _, _, code := cf("nodes"); code != 2 {
			t.Errorf("an unknown view exited %d, want 2", code)
		}
		if _, se, code := cf("flows", "--investing", "each"); code != 2 {
			t.Errorf("an unknown --investing exited %d, want 2: %s", code, se)
		}
		if _, se, code := cf("flows", "--level", "leaf"); code != 2 {
			t.Errorf("an unknown --level exited %d, want 2: %s", code, se)
		}
	})

	t.Run("the projection covers the months ahead and refuses what it does not take", func(t *testing.T) {
		plan := func(args ...string) (string, string, int) {
			return run(t, append([]string{"-c", cfg, "cashflow"}, args...)...)
		}
		so, se, code := plan("projection", "-f", "csv", "-C", "+consumption,+taxes,+uncategorized")
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
		header, _, _ := strings.Cut(so, "\n")
		for _, want := range []string{"investment_income_USD", "debt_service_USD", "net_USD", "taxes_USD",
			"uncategorized_USD", "quality"} {
			if !strings.Contains(header, want) {
				t.Errorf("the header %q lacks %q", header, want)
			}
		}
		if !strings.Contains(so, "monthly_average") || !strings.Contains(so, "projected trailing") {
			t.Errorf("the projection lacks its average row or its tags:\n%s", so)
		}
		for _, args := range [][]string{
			{"projection", "2026-05-01", "2026-06-30"},
			{"projection", "--level", "class"},
			{"projection", "--investing", "class"},
			{"projection", "--period", "total"},
			{"summary", "--rates", "forward"},
			{"flows", "--shift", "50"},
			{"projection", "--maturing", "never"},
			{"projection", time.Now().UTC().AddDate(0, 1, 14).Format("2006-01-02")},
		} {
			if _, se, code := plan(args...); code != 2 {
				t.Errorf("`cashflow %s` exited %d, want 2: %s", strings.Join(args, " "), code, se)
			}
		}
		far := strconv.Itoa(time.Now().UTC().Year() + 5)
		if _, se, code := plan("projection", far); code != 2 || !strings.Contains(se, "cashflow: the projection ends on") {
			t.Errorf("`cashflow projection %s` exited %d, want 2 and the projection's end: %s", far, code, se)
		}
	})

	// A refusal that only answers to one spelling of its flag is worse
	// than no refusal: the caller who types the short form gets the
	// plausible answer the refusal exists to withhold.
	t.Run("both spellings of every refused flag are refused", func(t *testing.T) {
		for _, args := range [][]string{
			{"coverage", "-x", "CHF"},
			{"coverage", "--currency", "CHF"},
			{"sankey", "--period", "annual"},
			{"sankey", "--level", "section"},
		} {
			if _, se, code := cf(args...); code != 2 {
				t.Errorf("`cashflow %s` exited %d, want 2: %s",
					strings.Join(args, " "), code, se)
			}
		}
	})
}

// TestCashflowUsageNamesEveryView keeps the help text honest about the
// surface it documents: every view, the flag of its own, every
// refusal, and a column list per view — `coverage` is the one view
// whose columns cannot be guessed from the families' idiom, so it is
// also the one a help text must not omit.
func TestCashflowUsageNamesEveryView(t *testing.T) {
	t.Parallel()
	usage := cashflowUsage()
	for _, want := range []string{
		"summary", "flows", "sankey", "transactions",
		"--investing", "--level", "section is refused",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("the usage text does not mention %q", want)
		}
	}
	for _, view := range cashflowViews {
		if !strings.Contains(usage, view) {
			t.Errorf("the usage text does not name the %q view", view)
		}
	}
	// Naming a view is not documenting it. Every view's default column
	// set has to appear too, or a view can be listed and still have no
	// discoverable columns.
	for view, cols := range map[string][]string{
		"summary":      defaultCashflowSummaryColumns,
		"flows":        defaultCashflowFlowColumns,
		"sankey":       defaultCashflowSankeyColumns,
		"transactions": defaultCashflowTransactionColumns,
		"coverage":     defaultCashflowCoverageColumns,
		"projection":   defaultCashflowPlanColumns,
	} {
		if !strings.Contains(usage, strings.Join(cols, ", ")) {
			t.Errorf("the usage text does not list the %q default columns", view)
		}
	}
}

// TestCashflowHelpEntryNamesEveryView holds the one-line help the
// dispatch listing prints against the views the command actually
// routes. The listing is where a caller learns a view exists at all.
func TestCashflowHelpEntryNamesEveryView(t *testing.T) {
	t.Parallel()
	var entry commandHelp
	for _, c := range commandHelps {
		if c.name == "cashflow" {
			entry = c
		}
	}
	if entry.name == "" {
		t.Fatal("no cashflow entry in commandHelps")
	}
	for _, view := range cashflowViews {
		if !strings.Contains(entry.detail(), view) {
			t.Errorf("`wealthdb help cashflow` does not name the %q view", view)
		}
	}
}

// TestTransactionsCarriesTheCashflowColumns pins the one surface that
// shows a row from every side: the cashflow trio beside the spending
// and income ones.
func TestTransactionsCarriesTheCashflowColumns(t *testing.T) {
	t.Parallel()
	cfg := setupCashflowGold(t)
	so, se, code := run(t, "-c", cfg, "transactions", "2026-05-01", "2026-06-30",
		"-C", "+cashflow_section,+cashflow_class,+cashflow_group,+spend_detailed")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, se)
	}
	for _, want := range []string{"cashflow_section", "cashflow_class", "cashflow_group",
		"operating_in", "earnings", "INCOME_SALARY", "FOOD_AND_DRINK_GROCERIES"} {
		if !strings.Contains(so, want) {
			t.Errorf("`transactions` is missing %q:\n%s", want, so)
		}
	}
}

// The cash flow projection's window starts next month or later, on a
// month's first day, and ends on a month's last: a window cut at either
// end is refused.
func TestCheckPlanWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	day := func(y int, m time.Month, d int) int64 { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() }
	for _, c := range []struct {
		from, to int64
		want     string
	}{
		{day(2026, 8, 1), day(2026, 9, 1) - 1, ""},
		{day(2026, 8, 1), day(2027, 8, 1) - 1, ""},
		{day(2027, 1, 1), day(2027, 4, 1) - 1, ""},
		{day(2026, 7, 1), day(2026, 9, 1) - 1, "it starts in 2026-08 or later"},
		{day(2026, 6, 1), day(2026, 7, 1) - 1, "it starts in 2026-08 or later"},
		{day(2026, 8, 15), day(2026, 10, 1) - 1, "covers whole months"},
		{day(2026, 8, 1), day(2026, 9, 15) - 1, "covers whole months"},
		{day(2026, 8, 1), day(2026, 9, 1), "covers whole months"},
		{day(2026, 8, 1) + 3600, day(2026, 9, 1) - 1, "covers whole months"},
	} {
		err := checkPlanWindow(c.from, c.to, now)
		if got := fmt.Sprint(err); (c.want == "") != (err == nil) || (c.want != "" && !strings.Contains(got, c.want)) {
			t.Errorf("checkPlanWindow(%s, %s) = %v, want %q", formatDateTime(c.from), formatDateTime(c.to), err,
				c.want)
		}
	}
}

// Every tag the cash flow projection's quality column prints reaches the
// CLI usage and the MCP quality help, in the order the projection lists
// them.
func TestPlanTagsReachBothFrontEnds(t *testing.T) {
	t.Parallel()
	maturity := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := gold.CashflowPlan(gold.PlanLookback{Months: 1, Mortgages: []gold.PlanMortgage{{Source: "s", Account: "a",
		Instalment: 1}}}, map[[2]string]*time.Time{{"s", "a"}: &maturity},
		[]gold.PlanMonth{{Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)}}, 1, false)
	var listed []string
	usage := cashflowUsage()
	for _, q := range cashflowPlanTags {
		listed = append(listed, q.tag)
		if !strings.Contains(usage, "\n    "+q.tag+" ") {
			t.Errorf("usage does not list %s", q.tag)
		}
		if !strings.Contains(qualityHelp, "\n- "+q.tag+": "+q.meaning+".") {
			t.Errorf("describe topic=quality does not list %s", q.tag)
		}
	}
	if want := strings.Fields(rows[0].Quality); !slices.Equal(listed, want) {
		t.Errorf("tags %v, want the projection's %v", listed, want)
	}
}

// A household whose vehicles pay: the projection's income on the pool's
// accounts is the plan's investment_income, on a retirement plan's and
// on an account cashflow.accounts excludes its vehicles_income, a
// maturity's principal there too. The three projected parts sum to
// income projection's expected and principal, month by month. net is
// the pool's alone. Every account and figure is invented.
func TestThePlanSplitsTheProjectionAtThePool(t *testing.T) {
	t.Parallel()
	cfg := setupYieldGold(t)
	changeYieldGold(t, cfg,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, tax_wrapper, display_name,
		 base_currency, first_seen_at, last_seen_at) VALUES
		 ('brk', 'IRA4242', 'brokerage', 'roth_ira', 'Plan', 'USD', 0, 0),
		 ('brk', 'ACCT5151', 'brokerage', 'taxable_personal', 'Fenced', 'USD', 0, 0)`,
		`INSERT INTO cashflow_wrapper_sides (tax_wrapper, side, class) VALUES ('roth_ira', 'vehicle', 'retirement')`,
		`INSERT INTO cashflow_account_scope (silver_source_id, account_external_id, mode) VALUES
		 ('brk', 'ACCT5151', 'exclude')`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, cusip, name,
		 currency, first_seen_at, last_seen_at) VALUES
		 ('brk', 'QNOTE', 'fixed_income', 'bond', '000000AC9', 'QORVANT CORP NOTE 3.00000% 03/31/2027', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, quantity, market_value) VALUES
		 ('brk', ?, 'IRA4242', 'QBND', 'QBND', 'fixed_income', 'bond', 'USD', 5000, 5000),
		 ('brk', ?, 'IRA4242', 'QNOTE', 'QNOTE', 'fixed_income', 'bond', 'USD', 2000, 2000),
		 ('brk', ?, 'ACCT5151', 'QBND', 'QBND', 'fixed_income', 'bond', 'USD', 1000, 1000)`)
	out, errb, code := runIncome(t, cfg, "projection", "-f", "csv", "-C", "period,expected,principal")
	if code != 0 {
		t.Fatalf("income projection: exit %d: %s", code, errb)
	}
	income := csvRows(t, out)
	out, errb, code = runToday(t, "-c", cfg, "cashflow", "projection", "-f", "csv")
	if code != 0 {
		t.Fatalf("cashflow projection: exit %d: %s", code, errb)
	}
	plan := csvRows(t, out)
	if len(plan) != len(income) {
		t.Fatalf("%d plan rows, %d projection rows", len(plan), len(income))
	}
	num := func(r map[string]string, col string) float64 {
		v, err := strconv.ParseFloat(r[col+"_USD"], 64)
		if err != nil {
			t.Fatalf("%s: %v in %v", col, err, r)
		}
		return v
	}
	paid := 0.0
	for i, p := range plan {
		in := income[i]
		if p["period"] != in["period"] {
			t.Fatalf("row %d: period %s, projection's %s", i, p["period"], in["period"])
		}
		// The projection's average row states no principal.
		if p["period"] == "monthly_average" {
			continue
		}
		sum := num(p, "investment_income") + num(p, "principal_returned") + num(p, "vehicles_income")
		if math.Abs(sum-num(in, "expected")-num(in, "principal")) > 0.005 {
			t.Errorf("%s: the parts sum to %.2f, the projection's to %.2f", p["period"], sum,
				num(in, "expected")+num(in, "principal"))
		}
		net := num(p, "investment_income") + num(p, "principal_returned") + num(p, "other_income") -
			num(p, "operating_out") - num(p, "debt_service") + num(p, "vehicles")
		if math.Abs(net-num(p, "net")) > 0.015 {
			t.Errorf("%s: net %s, the pool's parts %.2f", p["period"], p["net_USD"], net)
		}
		if p["period"] != "total" {
			paid += num(p, "vehicles_income")
		}
	}
	// Outside the pool, the 4% note pays its coupon twice over the
	// horizon on 6,000, and the 3% note twice on 2,000 before it returns
	// them.
	if paid != 240+60+2000 {
		t.Errorf("the vehicles' income over the horizon %.2f, want 2300:\n%s", paid, out)
	}
}
