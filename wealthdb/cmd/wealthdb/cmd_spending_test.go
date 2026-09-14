package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/output"
)

// ---- window parsing ------------------------------------------------------

// TestParseSpendingWindowDefault pins the trailing-twelve-months
// default: the same day one year back at 00:00 through today at
// 23:59:59, both UTC. This is the one place the spending window
// deliberately differs from the returns window (since-inception).
func TestParseSpendingWindowDefault(t *testing.T) {
	now := time.Date(2026, time.June, 15, 9, 30, 0, 0, time.UTC)
	from, to, err := parseTrailingYearWindow(nil, now)
	if err != nil {
		t.Fatalf("parseTrailingYearWindow: %v", err)
	}
	wantFrom := time.Date(2025, time.June, 15, 0, 0, 0, 0, time.UTC).Unix()
	wantTo := time.Date(2026, time.June, 15, 23, 59, 59, 0, time.UTC).Unix()
	if from != wantFrom {
		t.Errorf("from = %d (%s), want %d", from, formatDate(from), wantFrom)
	}
	if to != wantTo {
		t.Errorf("to = %d (%s), want %d", to, formatDate(to), wantTo)
	}
}

// TestParseSpendingWindowPositional pins that anything positional
// delegates to the shared range parser rather than to a second
// spelling of it.
func TestParseSpendingWindowPositional(t *testing.T) {
	now := time.Date(2026, time.June, 15, 9, 30, 0, 0, time.UTC)
	for _, args := range [][]string{{"2025"}, {"2025-03"}, {"2025-01-01", "2025-06-30"}, {"2025-01-01", "-"}} {
		gotFrom, gotTo, err := parseTrailingYearWindow(args, now)
		if err != nil {
			t.Fatalf("parseTrailingYearWindow(%v): %v", args, err)
		}
		wantFrom, wantTo, err := parseDateRange(args, now)
		if err != nil {
			t.Fatalf("parseDateRange(%v): %v", args, err)
		}
		if gotFrom != wantFrom || gotTo != wantTo {
			t.Errorf("parseTrailingYearWindow(%v) = (%d, %d), want (%d, %d)",
				args, gotFrom, gotTo, wantFrom, wantTo)
		}
	}
}

// TestSpendingFlagReordering pins that every value-consuming spending
// flag is declared, so a flag may follow the positional window without
// its value being read as a date.
func TestSpendingFlagReordering(t *testing.T) {
	args := []string{"2026-05", "--period", "total", "--level", "detailed", "-f", "csv", "-x", "CHF"}
	got := reorderFlagsFirst(args, reportValueFlags)
	want := []string{"--period", "total", "--level", "detailed", "-f", "csv", "-x", "CHF", "2026-05"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("reorderFlagsFirst = %v, want %v", got, want)
	}
}

// TestSpendingPeriodLabels pins the bucket labels against the returns
// family's shapes, plus the NULL bucket `--period total` emits.
func TestSpendingPeriodLabels(t *testing.T) {
	apr := time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC).Unix()
	cases := []struct{ period, want string }{
		{"daily", "2026-04-01"},
		{"weekly", "2026-04-01"},
		{"monthly", "2026-04"},
		{"quarterly", "2026-Q2"},
		{"annual", "2026"},
	}
	for _, c := range cases {
		if got := periodLabel(&apr, c.period); got != c.want {
			t.Errorf("periodLabel(%s) = %q, want %q", c.period, got, c.want)
		}
	}
	if got := periodLabel(nil, "total"); got != "total" {
		t.Errorf("total bucket label = %q, want %q", got, "total")
	}
	if got := periodStart(nil); got != "" {
		t.Errorf("total bucket period_start = %q, want empty", got)
	}
}

// ---- column registries ---------------------------------------------------

// TestSpendingColumnHeaders pins the dynamic money headers on all
// three views: every amount column carries the -x/--currency choice,
// and the share column reads as a percentage.
func TestSpendingColumnHeaders(t *testing.T) {
	summary, err := resolveSpendSummaryColumns("all", "CHF", "monthly")
	if err != nil {
		t.Fatalf("summary columns: %v", err)
	}
	wantSummary := []string{"period", "period_start", "txn_count", "spend_CHF", "refunds_CHF", "net_spend_CHF"}
	if got := headersOf(summary); strings.Join(got, ",") != strings.Join(wantSummary, ",") {
		t.Errorf("summary headers = %v, want %v", got, wantSummary)
	}

	cats, err := resolveSpendCategoryColumns("default", "EUR", "monthly")
	if err != nil {
		t.Fatalf("category columns: %v", err)
	}
	wantCats := []string{"period", "category", "txn_count", "spend_EUR", "refunds_EUR", "net_spend_EUR", "share_%"}
	if got := headersOf(cats); strings.Join(got, ",") != strings.Join(wantCats, ",") {
		t.Errorf("category headers = %v, want %v", got, wantCats)
	}

	txns, err := resolveSpendTransactionColumns("default", "USD")
	if err != nil {
		t.Fatalf("transaction columns: %v", err)
	}
	if got := headersOf(txns); got[len(got)-1] != "value_USD" {
		t.Errorf("transaction value header = %q, want value_USD", got[len(got)-1])
	}
}

func headersOf[T any](cols []columnSpec[T]) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.header()
	}
	return out
}

// TestSpendingTransactionPrivacyClasses pins the per-column privacy
// decisions, which are the substantive part of this view.
//
// The merchant name is the interesting one: it redacts as free text,
// like the narrative it was named from. The fence gates candidacy for
// the merchant STORE, not this column — a wire, an ACH, a P2P
// narrative is refused a verdict, acquires no store name, and falls
// back to the signature folded from that very narrative (migration
// 0054), so the cell prints the payee the fence refused to have named.
// A store name carries the same exposure by a slower route: the store
// is append-only across signature revisions and across widenings of
// the fence itself, and a stored verdict is applied by signature
// forever — so a name bought while the fence was narrower outlives the
// fence that would now refuse it. Only the taxonomy columns and the
// tier that decided are legible under -p.
func TestSpendingTransactionPrivacyClasses(t *testing.T) {
	cols, err := resolveSpendTransactionColumns("all", "USD")
	if err != nil {
		t.Fatalf("resolve columns: %v", err)
	}
	byName := map[string]columnSpec[gold.SpendTransactionRow]{}
	for _, c := range cols {
		byName[c.Name] = c
	}
	want := map[string]PrivacyClass{
		"merchant":           PrivacyFreeText,
		"spend_primary":      PrivacyNone,
		"spend_detailed":     PrivacyNone,
		"provenance":         PrivacyNone,
		"merchant_signature": PrivacyFreeText,
		"counterparty":       PrivacyFreeText,
		"description":        PrivacyFreeText,
		"account":            PrivacyAccountID,
		"account_id":         PrivacyAccountID,
		"tx_id":              PrivacyAccountID,
		"net_amount":         PrivacyMoney,
		"value":              PrivacyMoney,
	}
	for name, class := range want {
		c, ok := byName[name]
		if !ok {
			t.Errorf("column %q is not registered", name)
			continue
		}
		if c.Privacy != class {
			t.Errorf("column %q privacy = %v, want %v", name, c.Privacy, class)
		}
	}
}

// spendTestCounterparty is the synthetic narrative the privacy tests
// key on. Deliberately multi-word: that is the shape a wire / P2P /
// cheque line takes when it carries a person's name, and the shape an
// identifier-based redactor prints in full.
const spendTestCounterparty = "SAMPLE PAYEE ZZ"

// TestSpendingPrivacyRedacts proves the classes actually redact, on
// values chosen so a wrong class would show:
//
//   - the merchant name and the signature are both single
//     alphanumeric tokens carrying no digit, so an identifier-shaped
//     rule would print both in full; the free-text class is the only
//     thing that masks them.
//   - the counterparty and the description are multi-word, the shape
//     a transfer narrative takes when it carries a person's name.
//     They must mask too — an identifier-shaped rule would print
//     them in full, which is precisely what the free-text class
//     exists to stop.
//   - the category, the provenance and the account nickname are what
//     stays legible, so a redacted listing still reads.
func TestSpendingPrivacyRedacts(t *testing.T) {
	cols, err := resolveSpendTransactionColumns(
		"account_id,merchant,merchant_signature,counterparty,description,value", "USD")
	if err != nil {
		t.Fatalf("resolve columns: %v", err)
	}
	merchant, signature := "CornerMart", "CORNERMARTGMBH"
	counterparty := spendTestCounterparty
	description := "ZELLE PAYMENT TO " + spendTestCounterparty
	value := "42.5"
	row := gold.SpendTransactionRow{
		AccountExternalID: "CARD0001234",
		MerchantName:      &merchant,
		MerchantSignature: &signature,
		Counterparty:      &counterparty,
		Description:       &description,
		ValueOutCcy:       &value,
	}

	off := rowsToTable([]gold.SpendTransactionRow{row}, cols, false, output.FormatTable)
	for i, want := range []string{"CARD0001234", merchant, signature, counterparty, description, "42.50"} {
		if off.Rows[0][i] != want {
			t.Errorf("privacy off, cell %d = %q, want %q", i, off.Rows[0][i], want)
		}
	}

	on := rowsToTable([]gold.SpendTransactionRow{row}, cols, true, output.FormatTable)
	if on.Rows[0][0] == "CARD0001234" {
		t.Errorf("account_id survived -p: %q", on.Rows[0][0])
	}
	for i, name := range map[int]string{1: "merchant", 2: "merchant_signature", 3: "counterparty", 4: "description"} {
		if on.Rows[0][i] != "***" {
			t.Errorf("%s under -p = %q, want the free-text placeholder", name, on.Rows[0][i])
		}
	}
	if on.Rows[0][5] != "*****.**" {
		t.Errorf("value under -p = %q, want the money placeholder", on.Rows[0][5])
	}
}

// ---- end to end ----------------------------------------------------------

// setupSpendingGold seeds a synthetic gold DB with two months of
// spending across a card and a cash account: a merchant-store verdict
// and its refund, an ATM withdrawal placed by the rule tier, a
// provider-placed restaurant row, one row nothing could place, and an
// own-account move that must reach no view. Amounts are round so the
// arithmetic is readable in an assertion.
//
//	May  2026: spend 300, refunds 20, net 280, 3 lines
//	June 2026: spend  90, refunds  0, net  90, 2 lines
func setupSpendingGold(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	goldPath := filepath.Join(dir, "wealthdb.db")

	db, err := gold.Open(goldPath, gold.ModeReadWrite)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	ctx := context.Background()
	if err := gold.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO silver_sources(silver_source_id, silver_kind, silver_path,
			high_watermark, first_loaded_at, last_loaded_at)
		VALUES ('cards', 'chase', '/tmp/cards.db', -1, 0, 0)`); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	at := func(m time.Month, d int) int64 {
		return time.Date(2026, m, d, 12, 0, 0, 0, time.UTC).Unix()
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO fx_rates(silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
		VALUES ('cards', ?, 'USD', 'CHF', CAST('1.1' AS DECIMAL(20,10)))`, at(time.January, 1)); err != nil {
		t.Fatalf("seed fx: %v", err)
	}

	usd := "USD"
	dec := func(s string) *canonical.Decimal {
		v, err := canonical.NewDecimalFromString(s)
		if err != nil {
			t.Fatalf("decimal %q: %v", s, err)
		}
		return &v
	}
	seed := func(fn func(*gold.Writer) error) {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := fn(gold.NewWriter(tx)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("seed: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	// No display names: the account column falls back to the external
	// id, which is the identifier-shaped value the account-id privacy
	// class is there to mask.
	seed(func(w *gold.Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{
			{SilverSourceID: "cards", AccountExternalID: "CARD0001234", AccountKind: canonical.AccountKindCard,
				BaseCurrency: &usd, FirstSeenAt: at(time.May, 1), LastSeenAt: at(time.June, 30)},
			{SilverSourceID: "cards", AccountExternalID: "CASH0005678", AccountKind: canonical.AccountKindCash,
				BaseCurrency: &usd, FirstSeenAt: at(time.May, 1), LastSeenAt: at(time.June, 30)},
		})
	})

	counterparty := spendTestCounterparty
	lines := []struct {
		id       string
		at       int64
		acct     string
		kind     canonical.TxKind
		net      string
		sig      string
		detailed string // spend_txn_enrichment.spend_detailed; "" = NULL
		prov     string
		descr    string
		party    *string
	}{
		{"T-GROC", at(time.May, 5), "CARD0001234", canonical.TxKindPurchase, "-100", "sig-market", "", "signature-only", "CORNERMART STORE 12", &counterparty},
		{"T-GROC-REFUND", at(time.May, 7), "CARD0001234", canonical.TxKindRefund, "20", "sig-market", "", "signature-only", "CORNERMART STORE 12", &counterparty},
		{"T-ATM", at(time.May, 20), "CASH0005678", canonical.TxKindWithdrawal, "-200", "sig-atm", "cash_withdrawal", "rule", "ATM WITHDRAWAL", nil},
		{"T-DINER", at(time.June, 3), "CARD0001234", canonical.TxKindPurchase, "-60", "sig-diner", "FOOD_AND_DRINK_RESTAURANT", "provider", "THE DINER", nil},
		{"T-BACKLOG", at(time.June, 4), "CARD0001234", canonical.TxKindPurchase, "-30", "sig-unknown", "", "signature-only", "UNKNOWN SHOP", nil},
		{"T-CARDPAY", at(time.June, 10), "CASH0005678", canonical.TxKindWithdrawal, "-500", "sig-bank", "internal_transfer", "matcher", "PAYMENT TO CARD", nil},
	}
	for _, l := range lines {
		descr := l.descr
		seed(func(w *gold.Writer) error {
			return w.InsertTransactions(ctx, []canonical.TransactionChange{{
				SilverSourceID: "cards", TransactionExternalID: l.id, OccurredAt: l.at,
				AccountExternalID: l.acct, Kind: l.kind, Currency: "USD",
				NetAmount: dec(l.net), Description: &descr, Counterparty: l.party,
			}})
		})
		var detailed any
		if l.detailed != "" {
			detailed = l.detailed
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
				merchant_signature, signature_version, spend_detailed, provenance, assigned_at)
			VALUES ('cards', ?, ?, 1, ?, ?, 100)`, l.id, l.sig, detailed, l.prov); err != nil {
			t.Fatalf("seed enrichment %s: %v", l.id, err)
		}
	}

	// The merchant store places the grocery signature. The CLI privacy
	// case asserts this name survives -p while the raw narrative beside
	// it does not, so the two must not share a spelling.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
			signature_version, assigned_at, model_name)
		VALUES ('sig-market', 'CornerMart', 'FOOD_AND_DRINK_GROCERIES', 1, 100, 'test-model')`); err != nil {
		t.Fatalf("seed merchant store: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close gold: %v", err)
	}

	cfg := filepath.Join(dir, "wealthdb.cfg")
	body := fmt.Sprintf(`{"gold_db": %q, "default_currency": "USD", "silver_sources": []}`, goldPath)
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	return cfg
}

// nameSpendingSignature adds a store verdict for a signature
// setupSpendingGold leaves unnamed — the name a model would have given
// a narrative the fence let through — so a test can assert where that
// name does and does not surface. Kept out of the fixture itself: the
// categorize plan tests count the fixture's store rows as anchors. The
// fixture's gold file sits beside its cfg.
func nameSpendingSignature(t *testing.T, cfg, signature, name, detailed string) {
	t.Helper()
	db, err := gold.Open(filepath.Join(filepath.Dir(cfg), "wealthdb.db"), gold.ModeReadWrite)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
			signature_version, assigned_at, model_name)
		VALUES (?, ?, ?, 1, 100, 'test-model')`, signature, name, detailed); err != nil {
		t.Fatalf("name %s: %v", signature, err)
	}
}

func TestSpendingCLIEndToEnd(t *testing.T) {
	cfg := setupSpendingGold(t)
	// The store names the ATM signature, whose line the rule tier
	// resolves to cash_withdrawal: the transactions case asserts that
	// name stays in the store and off the delta line.
	nameSpendingSignature(t, cfg, "sig-atm", "EXAMPLE BANK ATM", "BANK_FEES_ATM_FEES")
	window := []string{"2026-05-01", "2026-06-30"}
	spending := func(args ...string) (string, string, int) {
		full := append([]string{"-c", cfg, "spending"}, args...)
		return run(t, append(full, window...)...)
	}

	t.Run("summary buckets and sign split", func(t *testing.T) {
		so, se, code := spending("summary")
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
		for _, want := range []string{"2026-05", "2026-06", "300.00", "20.00", "280.00", "90.00"} {
			if !strings.Contains(so, want) {
				t.Errorf("summary missing %q:\n%s", want, so)
			}
		}
		// The own-account move is not spending at any grain.
		if strings.Contains(so, "500.00") || strings.Contains(so, "590.00") {
			t.Errorf("an internal transfer reached the summary:\n%s", so)
		}
	})

	t.Run("period reaches the macro", func(t *testing.T) {
		so, se, code := spending("summary", "--period", "total")
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
		if !strings.Contains(so, "total") || strings.Contains(so, "2026-05") {
			t.Errorf("--period total did not collapse the window:\n%s", so)
		}
		if !strings.Contains(so, "390.00") || !strings.Contains(so, "370.00") {
			t.Errorf("total bucket sums wrong:\n%s", so)
		}
		for _, p := range reportPeriodNames {
			if _, se, code := spending("summary", "--period", p); code != 0 {
				t.Errorf("--period %s exit=%d stderr=%s", p, code, se)
			}
		}
	})

	t.Run("level reaches the macro", func(t *testing.T) {
		primary, se, code := spending("categories", "--period", "total", "--level", "primary")
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
		// The column renders the display label (migration 0058); the
		// vendored value is `category_id`, which is not selected here.
		if !strings.Contains(primary, "Food and drink") {
			t.Errorf("--level primary missing the primary bucket:\n%s", primary)
		}
		if strings.Contains(primary, "Groceries") {
			t.Errorf("--level primary emitted a detailed value:\n%s", primary)
		}

		detailed, _, code := spending("categories", "--period", "total", "--level", "detailed")
		if code != 0 {
			t.Fatalf("detailed exit=%d", code)
		}
		for _, want := range []string{"Groceries", "Restaurant", "Cash withdrawal"} {
			if !strings.Contains(detailed, want) {
				t.Errorf("--level detailed missing %q:\n%s", want, detailed)
			}
		}
		// The value is still reachable, under its own column.
		byID, _, code := spending("categories", "--period", "total", "--level", "detailed", "-C", "+category_id")
		if code != 0 {
			t.Fatalf("category_id exit=%d", code)
		}
		if !strings.Contains(byID, "FOOD_AND_DRINK_GROCERIES") {
			t.Errorf("category_id lost the vendored value:\n%s", byID)
		}
		// The backlog is a labelled bucket, never a blank row.
		if !strings.Contains(detailed, "(uncategorized)") {
			t.Errorf("backlog is not labelled:\n%s", detailed)
		}
	})

	t.Run("categories reconcile with the summary", func(t *testing.T) {
		so, _, code := spending("categories", "--period", "total", "-f", "csv")
		if code != 0 {
			t.Fatalf("exit=%d", code)
		}
		// share is a share of the bucket: the rows sum to 100%.
		var total float64
		for _, line := range strings.Split(strings.TrimSpace(so), "\n")[1:] {
			fields := strings.Split(line, ",")
			var share float64
			if _, err := fmt.Sscanf(fields[len(fields)-1], "%f", &share); err != nil {
				t.Fatalf("parse share from %q: %v", line, err)
			}
			total += share
		}
		if total < 99.99 || total > 100.01 {
			t.Errorf("shares sum to %v%%, want 100%%:\n%s", total, so)
		}
	})

	t.Run("transactions carry merchant, category and provenance", func(t *testing.T) {
		so, se, code := spending("transactions", "-C", "+provenance,counterparty")
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
		// `rule` is a tier that stamps the overlay row; `model` is the
		// one that does not — a line the merchant store placed reads it
		// only because the two scopes resolve in the macro.
		for _, want := range []string{"CornerMart", "Groceries", "Cash withdrawal", "rule", "model", spendTestCounterparty} {
			if !strings.Contains(so, want) {
				t.Errorf("transactions missing %q:\n%s", want, so)
			}
		}
		if strings.Contains(so, "T-CARDPAY") || strings.Contains(so, "Internal transfer") {
			t.Errorf("an own-account move reached the transactions view:\n%s", so)
		}
		// The store names the ATM signature and the line is a delta:
		// the name stays in the store and off the line.
		if strings.Contains(so, "EXAMPLE BANK ATM") {
			t.Errorf("a store name reached the merchant column of a cash_withdrawal line:\n%s", so)
		}
		// The provider placed the restaurant line, so the model tier
		// never sees it and the store never names it: the merchant
		// column falls back to the line's own signature (migration
		// 0054). The backlog line has no verdict at all and falls back
		// the same way. merchant_signature is not among the columns
		// selected here, so either value can only be the merchant
		// column's — which is what makes the -p subtest's matching
		// assertion non-vacuous.
		for _, fold := range []string{"sig-diner", "sig-unknown"} {
			if !strings.Contains(so, fold) {
				t.Errorf("a line the store never named shows no merchant, want %q:\n%s", fold, so)
			}
		}
	})

	t.Run("output currency reaches the headers and the values", func(t *testing.T) {
		so, se, code := spending("summary", "-x", "chf")
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
		if !strings.Contains(so, "spend_CHF") || !strings.Contains(so, "net_spend_CHF") {
			t.Errorf("CHF headers missing:\n%s", so)
		}
		// The fixture rate is 1 CHF = 1.1 USD (fx_rates' base/quote
		// convention), so May's 300 USD reads as 272.73 CHF.
		if !strings.Contains(so, "272.73") {
			t.Errorf("CHF conversion missing (300 USD / 1.1):\n%s", so)
		}
	})

	t.Run("privacy redacts amounts and names, not the taxonomy", func(t *testing.T) {
		so, _, code := spending("transactions", "-p", "-C", "+counterparty,account_id")
		if code != 0 {
			t.Fatalf("exit=%d", code)
		}
		if strings.Contains(so, "100.00") {
			t.Errorf("privacy leaked an amount:\n%s", so)
		}
		if strings.Contains(so, "SAMPLE PAYEE") {
			t.Errorf("privacy leaked a counterparty:\n%s", so)
		}
		if strings.Contains(so, "CARD0001234") {
			t.Errorf("privacy leaked an account id:\n%s", so)
		}
		// The merchant name is a name taken off a statement narrative,
		// and the store keeps one for as long as it is there — longer
		// than the fence that admitted it. It masks with the narrative
		// columns; the taxonomy is what stays readable.
		if strings.Contains(so, "CornerMart") {
			t.Errorf("privacy leaked the merchant name:\n%s", so)
		}
		// The column's other content masks with it: a line the store
		// never named prints its own signature there (migration 0054),
		// and merchant_signature is not among the columns selected
		// here, so either token can only have come through the
		// merchant column.
		for _, fold := range []string{"sig-diner", "sig-unknown"} {
			if strings.Contains(so, fold) {
				t.Errorf("privacy leaked the merchant fold %q:\n%s", fold, so)
			}
		}
		// The taxonomy stays readable, in the spelling the column
		// renders: the default `category` column is the display label
		// (migration 0058), not the vendored value.
		if !strings.Contains(so, "Groceries") {
			t.Errorf("privacy redacted a category:\n%s", so)
		}
	})

	t.Run("flags may follow the window", func(t *testing.T) {
		so, se, code := run(t, "-c", cfg, "spending", "summary", "2026-05", "-f", "csv", "--period", "monthly")
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
		if !strings.Contains(so, "period,txn_count,spend_USD") {
			t.Errorf("csv header missing:\n%s", so)
		}
		if !strings.Contains(so, "2026-05") || strings.Contains(so, "2026-06") {
			t.Errorf("window after flags did not apply:\n%s", so)
		}
	})

	t.Run("json and csv", func(t *testing.T) {
		so, _, code := spending("categories", "-f", "json")
		if code != 0 {
			t.Fatalf("json exit=%d", code)
		}
		if !strings.Contains(so, "\"category\"") {
			t.Errorf("json missing the category field:\n%s", so)
		}
		if _, _, code := spending("transactions", "-f", "csv_plain"); code != 0 {
			t.Errorf("csv_plain exit=%d", code)
		}
	})

	t.Run("usage errors", func(t *testing.T) {
		if _, se, code := run(t, "-c", cfg, "spending"); code != 2 {
			t.Errorf("bare spending exit=%d, want 2 (stderr=%s)", code, se)
		}
		_, se, code := run(t, "-c", cfg, "spending", "nope")
		if code != 2 {
			t.Errorf("unknown view exit=%d, want 2", code)
		}
		if !strings.Contains(se, "unknown view") {
			t.Errorf("stderr missing 'unknown view': %s", se)
		}
		for _, bad := range [][]string{
			{"summary", "--period", "fortnightly"},
			{"categories", "--level", "granular"},
			{"summary", "-C", "nope"},
			{"summary", "-x", "DOLLARS"},
		} {
			if _, _, code := run(t, append([]string{"-c", cfg, "spending"}, bad...)...); code != 2 {
				t.Errorf("%v exit=%d, want 2", bad, code)
			}
		}
	})

	t.Run("help is discoverable", func(t *testing.T) {
		_, se, code := run(t, "-c", cfg, "spending", "-h")
		if code != 0 {
			t.Errorf("spending -h exit=%d, want 0", code)
		}
		for _, want := range []string{"summary", "categories", "transactions", "--period", "--level"} {
			if !strings.Contains(se, want) {
				t.Errorf("usage missing %q:\n%s", want, se)
			}
		}
	})
}

// TestSpendingTransactionsCarriesBothClassifications pins the columns the
// issuer view reaches the CLI through — and that they are two vocabularies
// side by side, never one silently standing in for the other.
func TestSpendingTransactionsCarriesBothClassifications(t *testing.T) {
	cfg := setupSpendingGold(t)
	// Stamp an issuer view that DISAGREES with ours, which is the case a
	// column quietly rendering the wrong one would hide.
	db, err := sql.Open("duckdb", goldPathFromCfg(cfg))
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	// On every row: the enrichment table's own spend_detailed is NULL
	// wherever the merchant store answered, so keying the stamp on it
	// would silently match nothing.
	res, err := db.Exec(`
        UPDATE spend_txn_enrichment
           SET provider_spend_detailed = 'GENERAL_MERCHANDISE_SUPERSTORES'`)
	if err != nil {
		t.Fatalf("stamp issuer view: %v", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		t.Fatal("stamped no rows; the fixture has no enrichment to carry an issuer view")
	}
	db.Close()

	so, se, code := run(t, "-c", cfg, "spending", "transactions",
		"2026-05-01", "2026-06-30",
		"-C", "+category_primary,issuer_category,issuer_category_id,spend_detailed")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, se)
	}
	for _, want := range []string{
		"Groceries",                       // ours, as a label
		"FOOD_AND_DRINK_GROCERIES",        // ours, as the value
		"Food and drink",                  // our primary, as a label
		"Superstores",                     // the issuer's, as a label
		"GENERAL_MERCHANDISE_SUPERSTORES", // the issuer's, as the value
	} {
		if !strings.Contains(so, want) {
			t.Errorf("spending transactions is missing %q:\n%s", want, so)
		}
	}
}
