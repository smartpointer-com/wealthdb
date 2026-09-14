package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
)

// TestIncomeGauntletUsesTheIncomeVocabulary pins what a model may and
// may not say on the income side. The loop is the spending one; only
// the predicates change, so this is what proves they were changed.
func TestIncomeGauntletUsesTheIncomeVocabulary(t *testing.T) {
	cands := map[string]bool{"SIG": true}
	accept := func(value string) bool {
		valid, _ := parseAndValidateCategorizations(incomeCategorizeFamily,
			"SIG,Example Payer,"+value+"\n", cands)
		return len(valid) == 1
	}
	for _, good := range []string{"INCOME_WAGES", "INCOME_RENT", "INCOME_STAKING", "INCOME_OTHER_INCOME"} {
		if !accept(good) {
			t.Errorf("the income gauntlet refused %q, which is an income type a model may emit", good)
		}
	}
	// A delta is refused on principle, and the model is told why.
	for _, delta := range []string{"capital_return", "internal_transfer", "gift", "cash_deposit"} {
		_, invalid := parseAndValidateCategorizations(incomeCategorizeFamily,
			"SIG,Example Payer,"+delta+"\n", cands)
		if len(invalid) != 1 {
			t.Fatalf("the income gauntlet accepted the delta %q", delta)
		}
		if !strings.Contains(invalid[0].Reason, "never by a model") {
			t.Errorf("%s: reason = %q, want it to say a delta is assigned elsewhere", delta, invalid[0].Reason)
		}
	}
	// A SPENDING value is not this family's, and is refused as
	// nonsense rather than as a delta.
	for _, wrong := range []string{"FOOD_AND_DRINK_GROCERIES", "BANK_FEES_ATM_FEES"} {
		if accept(wrong) {
			t.Errorf("the income gauntlet accepted %q, which is a spending category", wrong)
		}
	}
	// ...and the spending gauntlet refuses income values, for the same
	// reason in the other direction.
	valid, _ := parseAndValidateCategorizations(spendingCategorizeFamily,
		"SIG,Example Merchant,INCOME_WAGES\n", cands)
	if len(valid) != 0 {
		t.Error("the spending gauntlet accepted an income type")
	}
}

// TestIncomePromptSpeaksOfPayers pins the conversation's nouns and its
// forbidden list.
func TestIncomePromptSpeaksOfPayers(t *testing.T) {
	sys := buildCategorizeSystemPrompt(incomeCategorizeFamily)
	for _, want := range []string{"payer signatures", "RECEIVED", "income"} {
		if !strings.Contains(sys, want) {
			t.Errorf("the income system prompt is missing %q:\n%s", want, sys)
		}
	}
	if strings.Contains(sys, "spending category") {
		t.Errorf("the income system prompt asks for a spending category:\n%s", sys)
	}

	p := buildCategorizeUserPrompt(incomeCategorizeFamily, twoCandidates(), nil,
		config.SpendContextMerchant, nil)
	// The vocabulary offered is income's, and every income delta is
	// named as forbidden.
	if !strings.Contains(p, "INCOME_WAGES") {
		t.Errorf("the income prompt does not carry the income vocabulary:\n%s", p)
	}
	if strings.Contains(p, "FOOD_AND_DRINK_COFFEE") {
		t.Errorf("the income prompt carries the spending vocabulary:\n%s", p)
	}
	for _, d := range canonical.DeltaIncomeCategories() {
		if !strings.Contains(p, d.Detailed) {
			t.Errorf("the income prompt must name %q as forbidden", d.Detailed)
		}
	}
	// A delta may appear only in the prohibition sentence, never as a
	// choosable row.
	if strings.Contains(p, "\n  "+canonical.IncomeDetailedCapitalReturn+"\t") {
		t.Error("a delta appears as a choosable taxonomy row in the income prompt")
	}
}

// TestCollectPayerCandidatesBacklogOnly is the difference between a few
// hundred questions and several thousand pointless ones: the backlog is
// the RESOLVED value being NULL, so a row the kind floor placed is
// never a candidate.
func TestCollectPayerCandidatesBacklogOnly(t *testing.T) {
	db, ctx := openIncomeCategorizeGold(t)
	cands, skipped, err := collectMerchantCandidates(ctx, db, incomeCategorizeFamily,
		config.SpendContextMerchant, 3, backlogUnplaced)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := map[string]bool{}
	for _, c := range cands {
		got[c.Signature] = true
	}
	if !got["UNKNOWN SENDER"] {
		t.Error("an unplaced deposit is not a candidate; that is the whole backlog")
	}
	// The floor placed this one at query time. It has a signature and
	// no stored verdict, and asking about it would be work with a
	// known answer.
	if got["EXAMPLE DEPOT"] {
		t.Error("a row the kind floor placed became a candidate")
	}
	// A person-shaped narrative never leaves the machine at any level.
	if got["JOHN EXAMPLE IBAN CH00"] {
		t.Error("a fenced, person-shaped deposit became a candidate")
	}
	if skipped.Fenced == 0 {
		t.Error("nothing was fenced; the fence is not running on the income side")
	}
}

// TestCategorizeRunsBothFamiliesWhenNoneIsNamed pins the positional
// selector on both commands.
func TestCategorizeRunsBothFamiliesWhenNoneIsNamed(t *testing.T) {
	for _, tc := range []struct {
		arg   string
		want  []string
		valid bool
	}{
		{"", []string{"spending", "income"}, true},
		{"spending", []string{"spending"}, true},
		{"income", []string{"income"}, true},
		{"payers", nil, false},
		{"both", nil, false},
	} {
		got, ok := resolveCategorizeFamilies(tc.arg)
		if ok != tc.valid {
			t.Errorf("%q: ok = %v, want %v", tc.arg, ok, tc.valid)
			continue
		}
		if !ok {
			continue
		}
		var names []string
		for _, f := range got {
			names = append(names, f.name)
		}
		if strings.Join(names, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%q: families = %v, want %v", tc.arg, names, tc.want)
		}
	}
}

// TestIncomeVerdictsPersistToThePayerStore pins the upsert target and
// that the two stores stay separate.
func TestIncomeVerdictsPersistToThePayerStore(t *testing.T) {
	db, ctx := openIncomeCategorizeGold(t)
	rows := []categorization{
		{Signature: "UNKNOWN SENDER", MerchantName: "Example Letting Agent", Detailed: "INCOME_RENT"},
	}
	total, err := persistCategorizations(ctx, db, incomeCategorizeFamily, rows, 100, "test-model")
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if total != 1 {
		t.Errorf("payer store total = %d, want 1", total)
	}
	var name, detailed string
	if err := db.QueryRowContext(ctx, `
        SELECT payer_name, income_detailed FROM income_payer_categories
         WHERE payer_signature = 'UNKNOWN SENDER'`).Scan(&name, &detailed); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if name != "Example Letting Agent" || detailed != "INCOME_RENT" {
		t.Errorf("stored (%q, %q)", name, detailed)
	}
	// The merchant store is untouched.
	var merchants int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spend_merchant_categories`).Scan(&merchants); err != nil {
		t.Fatalf("count: %v", err)
	}
	if merchants != 0 {
		t.Errorf("the income upsert wrote %d merchant verdict(s)", merchants)
	}

	// An upsert replaces rather than duplicating.
	rows[0].Detailed = "INCOME_WAGES"
	if total, err = persistCategorizations(ctx, db, incomeCategorizeFamily, rows, 200, "test-model"); err != nil {
		t.Fatalf("re-persist: %v", err)
	}
	if total != 1 {
		t.Errorf("payer store total = %d after an upsert, want 1", total)
	}
}

// openIncomeCategorizeGold builds gold holding one floored dividend,
// one unplaced deposit, and one person-shaped deposit the fence keeps
// from the model. Every name is invented.
func openIncomeCategorizeGold(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	db, err := gold.Open(":memory:", gold.ModeReadWrite)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if err := gold.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources(silver_source_id, silver_kind, silver_path,
            high_watermark, first_loaded_at, last_loaded_at)
        VALUES ('bank', 'chase', '/tmp/b.db', -1, 0, 0);

        INSERT INTO accounts(silver_source_id, account_external_id, account_kind,
            first_seen_at, last_seen_at)
        VALUES ('bank', 'CASH1', 'cash', 1, 1), ('bank', 'BRK1', 'brokerage', 1, 1);

        INSERT INTO transactions(silver_source_id, transaction_external_id, occurred_at,
            account_external_id, kind, currency, net_amount, counterparty) VALUES
            ('bank', 'T-DIV',    1000, 'BRK1',  'dividend', 'USD', 40,  'EXAMPLE DEPOT'),
            ('bank', 'T-WIRE',   1000, 'CASH1', 'deposit',  'USD', 900, 'Unknown Sender'),
            ('bank', 'T-PERSON', 1000, 'CASH1', 'deposit',  'USD', 250, 'John Example IBAN CH00');

        INSERT INTO income_txn_enrichment(silver_source_id, transaction_external_id,
            payer_signature, signature_version, income_detailed, provenance, assigned_at) VALUES
            ('bank', 'T-DIV',    'EXAMPLE DEPOT',          1, NULL, 'signature-only', 1),
            ('bank', 'T-WIRE',   'UNKNOWN SENDER',         1, NULL, 'signature-only', 1),
            ('bank', 'T-PERSON', 'JOHN EXAMPLE IBAN CH00', 1, NULL, 'signature-only', 1);
    `); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db, ctx
}
