package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
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
		{"T-WAGES", "CASH0005678", "deposit", "", time.May, 5, "6000"},
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
		VALUES ('bank', 'T-WAGES', 'PAYROLL', 1, 'INCOME_WAGES', 'rule', 1),
		       ('bank', 'T-DIV',   NULL,      1, NULL,           'signature-only', 1),
		       ('bank', 'T-PLANDIV', NULL,    1, NULL,           'signature-only', 1);

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
		// 6000 wages + 400 dividend in; 250 + 900 out; 3000 invested;
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
		for _, want := range []string{"yield_USD", "savings_rate_%", "taxes_USD", "400.00", "900.00"} {
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
		// ninety detailed values.
		for _, want := range []string{"Wages", "Earnings", "Food and drink", "Consumption"} {
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
		for _, want := range []string{"operating_in", "Earnings", "Wages",
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
		for _, want := range []string{"operating_in", "Earnings", "Wages"} {
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
		if !strings.Contains(so, "operating_in.earnings.INCOME_WAGES") {
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
	for view := range cashflowViews {
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
	for view := range cashflowViews {
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
		"operating_in", "earnings", "INCOME_WAGES", "FOOD_AND_DRINK_GROCERIES"} {
		if !strings.Contains(so, want) {
			t.Errorf("`transactions` is missing %q:\n%s", want, so)
		}
	}
}
