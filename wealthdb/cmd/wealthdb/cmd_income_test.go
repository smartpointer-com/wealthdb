package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// setupIncomeGold builds a gold file the income CLI can be driven
// against: two accounts, a dividend with an instrument behind it, a
// deposit the payer store named, an unplaced deposit, an own-account
// move, a reversal, and a tax row for the memo.
//
// Every name is invented. The income side is where employers and
// agencies would be, and a fixture is a tracked file.
func setupIncomeGold(t *testing.T) string {
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
			first_seen_at, last_seen_at)
		VALUES ('bank', 'CASH0005678', 'cash', 1, 1),
		       ('bank', 'BRK0009999', 'brokerage', 1, 1);

		INSERT INTO instruments(silver_source_id, instrument_external_id, asset_class,
			symbol, name, first_seen_at, last_seen_at)
		VALUES ('bank', 'INST1', 'equity', 'EXDC', 'Example Dividend Corp', 1, 1);
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
		counterparty                  string
	}{
		{"T-DIV", "BRK0009999", "dividend", "INST1", time.May, 5, "300", ""},
		{"T-DIV-REV", "BRK0009999", "dividend", "INST1", time.May, 20, "-20", ""},
		{"T-SALARY", "CASH0005678", "deposit", "", time.May, 25, "5000", "Blue Harbour Payroll"},
		{"T-UNPLACED", "CASH0005678", "deposit", "", time.June, 3, "90", "Unknown Sender"},
		{"T-OWN", "CASH0005678", "deposit", "", time.June, 10, "700", "Own Transfer"},
		{"T-TAX", "BRK0009999", "tax", "", time.May, 5, "-45", ""},
		{"T-BUY", "CASH0005678", "purchase", "", time.May, 9, "-60", "Corner Market"},
	}
	for _, r := range rows {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO transactions(silver_source_id, transaction_external_id, occurred_at,
				account_external_id, instrument_external_id, kind, currency, net_amount, counterparty)
			VALUES ('bank', ?, ?, ?, ?, ?, 'USD', CAST(? AS DECIMAL(28,4)), ?)`,
			r.id, at(r.month, r.day), r.account, nullIfEmpty(r.instrument), r.kind, r.amount,
			nullIfEmpty(r.counterparty)); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
	// The overlay the pass would have written, plus a payer verdict.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO income_txn_enrichment(silver_source_id, transaction_external_id,
			payer_signature, signature_version, income_detailed, provenance, assigned_at)
		VALUES ('bank', 'T-DIV',      NULL,             1, NULL,                'signature-only', 1),
		       ('bank', 'T-DIV-REV',  NULL,             1, NULL,                'signature-only', 1),
		       ('bank', 'T-SALARY',   'BLUE HARBOUR PAYROLL', 1, NULL,          'signature-only', 1),
		       ('bank', 'T-UNPLACED', 'UNKNOWN SENDER', 1, NULL,                'signature-only', 1),
		       ('bank', 'T-OWN',      'OWN TRANSFER',   1, 'internal_transfer', 'matcher',        1);

		INSERT INTO income_payer_categories(payer_signature, payer_name, income_detailed,
			signature_version, assigned_at, model_name)
		VALUES ('BLUE HARBOUR PAYROLL', 'Blue Harbour Payroll', 'INCOME_SALARY', 1, 1, 'test-model');
	`); err != nil {
		t.Fatalf("seed the overlay: %v", err)
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

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// TestIncomeCLIEndToEnd drives the three views against seeded gold.
func TestIncomeCLIEndToEnd(t *testing.T) {
	t.Parallel()
	cfg := setupIncomeGold(t)
	window := []string{"2026-05-01", "2026-06-30"}
	income := func(args ...string) (string, string, int) {
		full := append([]string{"-c", cfg, "income"}, args...)
		return run(t, append(full, window...)...)
	}

	t.Run("summary buckets and sign split", func(t *testing.T) {
		so, se, code := income("summary", "--period", "total")
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
		// 300 dividend + 5000 salary + 90 unplaced = 5390 received;
		// the 20 clawback is a reversal; the own-account move is neither.
		for _, want := range []string{"5390.00", "20.00", "5370.00"} {
			if !strings.Contains(so, want) {
				t.Errorf("summary missing %q:\n%s", want, so)
			}
		}
		if strings.Contains(so, "700.00") {
			t.Errorf("an own-account move reached the income summary:\n%s", so)
		}
		// The memo is off by default and never in the arithmetic.
		if strings.Contains(so, "45.00") {
			t.Errorf("withheld is on by default:\n%s", so)
		}
	})

	t.Run("withheld is a memo behind -C", func(t *testing.T) {
		so, se, code := income("summary", "--period", "total", "-C", "+withheld")
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
		if !strings.Contains(so, "45.00") {
			t.Errorf("-C +withheld did not show the memo:\n%s", so)
		}
		if !strings.Contains(so, "5370.00") {
			t.Errorf("net_income moved when the memo was shown:\n%s", so)
		}
	})

	t.Run("types reconcile and default to detailed", func(t *testing.T) {
		so, se, code := income("types", "--period", "total")
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
		// --level defaults to detailed, so the vendored values are
		// their own rows rather than folded into INCOME.
		for _, want := range []string{"Dividends", "Salary", "(uncategorized)"} {
			if !strings.Contains(so, want) {
				t.Errorf("types missing %q:\n%s", want, so)
			}
		}
		primary, _, code := income("types", "--period", "total", "--level", "primary")
		if code != 0 {
			t.Fatal("primary level failed")
		}
		if strings.Contains(primary, "Dividends") {
			t.Errorf("--level primary did not fold the vendored types:\n%s", primary)
		}
		if !strings.Contains(primary, "Income") {
			t.Errorf("--level primary lost the primary label:\n%s", primary)
		}
	})

	t.Run("transactions name the payer", func(t *testing.T) {
		so, se, code := income("transactions")
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
		// The instrument on a dividend, the store's name on a deposit,
		// the signature where nothing named it.
		for _, want := range []string{"Example Dividend Corp", "Blue Harbour Payroll", "UNKNOWN SENDER"} {
			if !strings.Contains(so, want) {
				t.Errorf("transactions missing payer %q:\n%s", want, so)
			}
		}
		if strings.Contains(so, "Corner Market") {
			t.Errorf("a purchase reached the income view:\n%s", so)
		}
	})

	t.Run("privacy redacts the payer and the money", func(t *testing.T) {
		so, se, code := income("transactions", "-p")
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, se)
		}
		for _, gone := range []string{"Blue Harbour Payroll", "UNKNOWN SENDER", "CASH0005678", "5000.00"} {
			if strings.Contains(so, gone) {
				t.Errorf("-p leaked %q:\n%s", gone, so)
			}
		}
		// Types and provenance stay legible: that is what makes the
		// redacted report worth reading.
		for _, kept := range []string{"Salary", "dividend", "model"} {
			if !strings.Contains(so, kept) {
				t.Errorf("-p redacted %q, which is taxonomy rather than data:\n%s", kept, so)
			}
		}
	})

	t.Run("a bad view and a bad period are rejected", func(t *testing.T) {
		if _, _, code := run(t, "-c", cfg, "income", "payers"); code != 2 {
			t.Errorf("unknown view exit=%d, want 2", code)
		}
		if _, _, code := income("summary", "--period", "fortnightly"); code != 2 {
			t.Errorf("unknown period exit=%d, want 2", code)
		}
		if _, _, code := income("types", "--level", "coarse"); code != 2 {
			t.Errorf("unknown level exit=%d, want 2", code)
		}
	})
}

// TestIncomeFlagReordering pins that a flag may follow the positional
// window on `wealthdb income` without its value being read as a date.
//
// The usage's own worked example is exactly that shape —
// `wealthdb income types 2025 --period annual` — so without the
// reordering the documented invocation fails.
//
// It drives the COMMAND, not the helper. runIncomeView is where the
// reordering is either applied or forgotten; a test of
// reorderFlagsFirst alone passes with the call site deleted, which is
// the defect it was written to catch.
func TestIncomeFlagReordering(t *testing.T) {
	t.Parallel()
	cfg := setupIncomeGold(t)

	// The flag AFTER the window, which is what needs the reordering,
	// and the same invocation with the flag first, which never did.
	after, se, code := run(t, "-c", cfg, "income", "types", "2026-05-01", "2026-06-30",
		"--period", "total", "-f", "csv_plain")
	if code != 0 {
		t.Fatalf("a flag after the window exited %d: %s", code, se)
	}
	before, se, code := run(t, "-c", cfg, "income", "types", "--period", "total",
		"-f", "csv_plain", "2026-05-01", "2026-06-30")
	if code != 0 {
		t.Fatalf("a flag before the window exited %d: %s", code, se)
	}
	if after != before {
		t.Errorf("flag order changed the report:\nafter:\n%s\nbefore:\n%s", after, before)
	}
	// ...and the value really was consumed as a flag value rather than
	// read as a second date: `total` collapses the window to one bucket.
	if n := strings.Count(strings.TrimSpace(after), "\n"); n < 1 {
		t.Fatalf("the report has no rows to judge:\n%s", after)
	}

	// Every flag the income parser declares that takes a value must be
	// in the shared table. One missing from it silently eats the
	// positional, and the table is shared with spending — so a flag
	// added to one command's parser has to be added here or it breaks
	// the other's positional.
	for _, f := range []string{"--period", "--level", "-f", "--format", "-C", "--columns", "-x", "--currency"} {
		if !reportValueFlags[f] {
			t.Errorf("%s consumes a value but is not in reportValueFlags", f)
		}
	}
}

// TestIncomeColumnHeaders pins the default column sets and the money
// headers' currency suffix.
func TestIncomeColumnHeaders(t *testing.T) {
	t.Parallel()
	summary, err := resolveColumns("default", defaultIncomeSummaryColumns, buildIncomeSummaryColumnRegistry("CHF", "monthly"))
	if err != nil {
		t.Fatalf("summary columns: %v", err)
	}
	var got []string
	for _, c := range summary {
		got = append(got, c.header())
	}
	want := []string{"period", "txn_count", "income_CHF", "reversals_CHF", "net_income_CHF"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("summary headers = %v, want %v", got, want)
	}

	types, err := resolveColumns("default", defaultIncomeTypeColumns, buildIncomeTypeColumnRegistry("USD", "monthly"))
	if err != nil {
		t.Fatalf("type columns: %v", err)
	}
	got = nil
	for _, c := range types {
		got = append(got, c.header())
	}
	// share_%, spelled exactly as the spending view spells it. The two
	// commands share their column machinery, so a header that differed
	// would give one `-f csv` key on one and another on the other for
	// the same quantity.
	want = []string{"period", "type", "txn_count", "income_USD", "reversals_USD", "net_income_USD", "share_%"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("type headers = %v, want %v", got, want)
	}

	txns, err := resolveColumns("default", defaultIncomeTransactionColumns, buildIncomeTransactionColumnRegistry("USD"))
	if err != nil {
		t.Fatalf("transaction columns: %v", err)
	}
	got = nil
	for _, c := range txns {
		got = append(got, c.header())
	}
	want = []string{"silver_source", "date", "account", "kind", "payer", "income_type",
		"provenance", "currency", "net_amount", "value_USD"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("transaction headers = %v, want %v", got, want)
	}
}

// TestIncomeTransactionPrivacyClasses pins what -p covers, by column.
// A column that can hold a person's name must not be legible; taxonomy
// must be.
func TestIncomeTransactionPrivacyClasses(t *testing.T) {
	t.Parallel()
	all, err := resolveColumns("all", defaultIncomeTransactionColumns, buildIncomeTransactionColumnRegistry("USD"))
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	want := map[string]PrivacyClass{
		"payer":                PrivacyFreeText,
		"payer_signature":      PrivacyFreeText,
		"counterparty":         PrivacyFreeText,
		"description":          PrivacyFreeText,
		"account":              PrivacyAccountID,
		"account_id":           PrivacyAccountID,
		"net_amount":           PrivacyMoney,
		"value":                PrivacyMoney,
		"income_type":          PrivacyNone,
		"income_type_id":       PrivacyNone,
		"income_primary":       PrivacyNone,
		"provenance":           PrivacyNone,
		"kind":                 PrivacyNone,
		"provider_income_type": PrivacyNone,
		// The source's own id for the line: an identifier, so it takes
		// the identifier class even though it names nobody.
		"tx_id": PrivacyAccountID,
		// The rest, stated rather than skipped, so that a column added
		// to the registry has to be classified here before the suite
		// goes green.
		//
		// `account_nickname` is the holder's own label for an account,
		// from `account_overrides`, and legible under -p on all three
		// transaction surfaces. Stated rather than assumed: if it is
		// ever reclassified it must be reclassified on spending and
		// `wealthdb transactions` in the same change.
		"account_nickname":        PrivacyNone,
		"silver_source":           PrivacyNone,
		"date":                    PrivacyNone,
		"datetime":                PrivacyNone,
		"account_kind":            PrivacyNone,
		"account_category":        PrivacyNone,
		"income_primary_id":       PrivacyNone,
		"provider_income_type_id": PrivacyNone,
		"currency":                PrivacyNone,
	}
	seen := map[string]bool{}
	for _, c := range all {
		if wantClass, ok := want[c.Name]; ok {
			seen[c.Name] = true
			if c.Privacy != wantClass {
				t.Errorf("%s: privacy = %v, want %v", c.Name, c.Privacy, wantClass)
			}
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("column %q is not in the registry", name)
		}
	}
	// ...and EVERY column of the registry is classified here. The map
	// above only checks the columns it names, so a column added to the
	// registry and forgotten here would carry whatever class its author
	// happened to give it, unread — which is how `tx_id` arrived
	// unclassified.
	for _, c := range all {
		if _, ok := want[c.Name]; !ok {
			t.Errorf("column %q is in the registry but has no expected privacy class here", c.Name)
		}
	}
}
