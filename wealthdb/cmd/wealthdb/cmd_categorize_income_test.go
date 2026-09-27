package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/spending"
)

// TestIncomeGauntletUsesTheIncomeVocabulary pins what a model may and
// may not say on the income side. The loop is the spending one; only
// the predicates change, so this is what proves they were changed.
func TestIncomeGauntletUsesTheIncomeVocabulary(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	sys := incomeCategorizeFamily.systemPrompt()
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

	// The word "merchant" appears NOWHERE. The prompt used to announce
	// the spending family's output contract on an income run — three
	// columns named merchant_signature, merchant_name, spend_detailed —
	// three lines after asking for payers, so one batch told the model
	// three different things about the columns it had to emit. A
	// substring test is the right shape here precisely because the
	// failure was a stray literal.
	if strings.Contains(strings.ToLower(p), "merchant") {
		t.Errorf("the income prompt says \"merchant\":\n%s", p)
	}
	if strings.Contains(p, "spend_detailed") {
		t.Errorf("the income prompt names the spending value column:\n%s", p)
	}
	// ...and it names its own, in the same words the gauntlet uses to
	// reject a row, so the instruction and the complaint agree.
	for _, want := range []string{
		incomeCategorizeFamily.outputContract(),
		"payer_signature must appear verbatim",
		"income_detailed must be one of",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the income prompt is missing %q:\n%s", want, p)
		}
	}

	// The spending prompt is unchanged by the parameterisation, which
	// is what makes this a rename of a literal rather than a change of
	// behaviour on the side that already worked.
	sp := buildCategorizeUserPrompt(spendingCategorizeFamily, twoCandidates(), nil,
		config.SpendContextMerchant, nil)
	for _, want := range []string{
		"Merchants to categorise — merchant_signature,transaction_count:",
		"CSV with columns: merchant_signature,merchant_name,spend_detailed",
		"merchant_name is the merchant's real-world name",
		"spend_detailed must be one of",
	} {
		if !strings.Contains(sp, want) {
			t.Errorf("the spending prompt lost %q:\n%s", want, sp)
		}
	}
}

// TestCollectPayerCandidatesBacklogOnly is the difference between a few
// hundred questions and several thousand pointless ones: the backlog is
// the RESOLVED value being NULL, so a row the kind floor placed is
// never a candidate.
func TestCollectPayerCandidatesBacklogOnly(t *testing.T) {
	t.Parallel()
	db, ctx := openIncomeCategorizeGold(t)
	cands, skipped, err := collectMerchantCandidates(ctx, db, incomeCategorizeFamily,
		config.SpendContextMerchant, 3, backlogUnplaced, true, "spending.categorization")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	got := map[string]bool{}
	for _, c := range cands {
		got[c.Signature] = true
	}
	if !got["BLUE HARBOUR PAYROLL"] {
		t.Error("an unplaced deposit is not a candidate; that is the whole backlog")
	}
	// The floor placed this one at query time. It has a signature and
	// no stored verdict, and asking about it would be work with a
	// known answer.
	if got["EXAMPLE DEPOT AG"] {
		t.Error("a row the kind floor placed became a candidate")
	}
	// A narrative with an IBAN in it never leaves the machine.
	if got["JOHN EXAMPLE IBAN CH00"] {
		t.Error("a deposit carrying an IBAN became a candidate")
	}
	if skipped.Fenced == 0 {
		t.Error("nothing was fenced; the fence is not running on the income side")
	}
	// ...and neither does a bare name with nothing beside it, which no
	// rail token, IBAN or masked number would have caught.
	if got["JANE EXAMPLE"] {
		t.Error("a bare person's name became a candidate")
	}
	if skipped.PersonShaped != 1 {
		t.Errorf("person-shaped count = %d, want 1", skipped.PersonShaped)
	}
}

// TestCollectPayerCandidatesAllStaysWithinTheFloorlessKind pins the
// second of the two guards on the model's reach into the income
// taxonomy.
//
// `--all` lifts the BACKLOG filter — that is what re-asks a
// counterparty after a taxonomy revision. On the income side it must
// not also lift the KIND gate: a dividend the floor placed carries a
// signature, so an unrestricted --all would send the holdings list to
// a model and buy a verdict for every instrument in it. Migration 0073
// makes such a verdict harmless (the floor outranks it); this makes it
// unbought.
//
// The two halves are asserted together on purpose. A gate that
// excluded everything would pass the first assertion alone.
func TestCollectPayerCandidatesAllStaysWithinTheFloorlessKind(t *testing.T) {
	t.Parallel()
	db, ctx := openIncomeCategorizeGold(t)
	// A verdict already bought for the deposit, so the default backlog
	// excludes it and only --all can reach it.
	if _, err := db.ExecContext(ctx, `
        INSERT INTO income_payer_categories(payer_signature, payer_name,
            income_detailed, signature_version, assigned_at, model_name)
        VALUES ('BLUE HARBOUR PAYROLL', 'Blue Harbour Payroll', 'INCOME_WAGES', 1, 1, 'test-model')`); err != nil {
		t.Fatalf("seed a verdict: %v", err)
	}

	sigs := func(which backlog) map[string]bool {
		t.Helper()
		cands, _, err := collectMerchantCandidates(ctx, db, incomeCategorizeFamily,
			config.SpendContextMerchant, 3, which, true, "spending.categorization")
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		got := map[string]bool{}
		for _, c := range cands {
			got[c.Signature] = true
		}
		return got
	}

	// The signature must survive every gate but the kind one, or the
	// assertion below is about some other refusal. This guard is not
	// decoration: the person-shape arm landed after this test did, and
	// a two-word signature on a brokerage account would have shadowed
	// the kind gate completely.
	const floored = "EXAMPLE DEPOT AG"
	if spending.TransferShaped(floored) || spending.Uninformative(floored) ||
		spending.PersonShaped(floored) {
		t.Fatal("fixture is wrong: another gate already refuses the floor-placed signature, " +
			"so the kind gate would be untested")
	}

	unplaced := sigs(backlogUnplaced)
	if unplaced["BLUE HARBOUR PAYROLL"] {
		t.Error("an answered deposit is in the default backlog; the store should cover it")
	}

	all := sigs(backlogAll)
	if !all["BLUE HARBOUR PAYROLL"] {
		t.Error("--all did not re-ask an answered deposit, which is what --all is for")
	}
	if all["EXAMPLE DEPOT AG"] {
		t.Error("--all reached a dividend: the kind gate must hold whatever the backlog filter says")
	}

	// And the spending family keeps the unrestricted semantic, so the
	// gate is a family's property rather than a new rule for both.
	if len(spendingCategorizeFamily.candidateKinds) != 0 {
		t.Error("spending grew a kind gate; a merchant is a merchant on every outflow kind")
	}
}

// TestIncomeKindGateHoldsAtEveryPlaceASignatureLeaves pins the kind
// gate at the other site a signature leaves the machine.
//
// A signature reaches a prompt from three places — the candidate list,
// the neighbour lists the `transaction` context level attaches, and the
// anchor block — and the fence has always been read over all three for
// exactly that reason. The kind gate went in at the candidate list
// first, which left the neighbour list free to carry the signature of
// every dividend booked within a day of a deposit: the holdings list,
// arriving as context for the one row the model is allowed to place.
func TestIncomeKindGateHoldsAtEveryPlaceASignatureLeaves(t *testing.T) {
	t.Parallel()
	db, ctx := openIncomeCategorizeGold(t)
	// A dividend on the same day as the deposits, so it is a genuine
	// neighbour candidate and the assertion is not vacuous.
	var sameDay int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM transactions
         WHERE kind = 'dividend' AND occurred_at = 1000`).Scan(&sameDay); err != nil {
		t.Fatalf("count: %v", err)
	}
	if sameDay == 0 {
		t.Fatal("fixture is wrong: no floor-kind row shares a day with a deposit")
	}

	cands, _, err := collectMerchantCandidates(ctx, db, incomeCategorizeFamily,
		config.SpendContextTransaction, 3, backlogAll, true, "spending.categorization")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(cands) == 0 {
		t.Fatal("no candidates, so no neighbour lists to judge")
	}
	sawAny := false
	for _, c := range cands {
		for _, sample := range c.Samples {
			for _, n := range sample.Neighbours {
				sawAny = true
				if n == "EXAMPLE DEPOT AG" {
					t.Errorf("%s: a dividend's signature reached a neighbour list: %v",
						c.Signature, sample.Neighbours)
				}
			}
		}
	}
	// Neighbours have to be attaching, or the assertion above is about
	// an empty list. Two unfenced deposits share a day for exactly this.
	if !sawAny {
		t.Fatal("no neighbour was attached to any candidate, so the kind gate is untested here")
	}
}

// TestCategorizeRunsBothFamiliesWhenNoneIsNamed pins the positional
// family selector that both commands resolve through — the arg alone,
// with no command run.
func TestCategorizeRunsBothFamiliesWhenNoneIsNamed(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	db, ctx := openIncomeCategorizeGold(t)
	rows := []categorization{
		{Signature: "BLUE HARBOUR PAYROLL", MerchantName: "Example Letting Agent", Detailed: "INCOME_RENT"},
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
         WHERE payer_signature = 'BLUE HARBOUR PAYROLL'`).Scan(&name, &detailed); err != nil {
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

// openIncomeCategorizeGold builds gold holding five rows, each with a
// job: a floored dividend (excluded by the KIND gate, not by the
// fence, since its signature carries an organisation marker), two
// unfenced deposits so a candidate has a neighbour at all, and two the
// fence refuses — a bare person's name, and a name beside an IBAN.
// Every name is invented.
func openIncomeCategorizeGold(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	db, err := gold.OpenFresh(filepath.Join(t.TempDir(), "gold.db"))
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources(silver_source_id, silver_kind, silver_path,
            high_watermark, first_loaded_at, last_loaded_at)
        VALUES ('bank', 'chase', '/tmp/b.db', -1, 0, 0);

        INSERT INTO accounts(silver_source_id, account_external_id, account_kind,
            first_seen_at, last_seen_at)
        VALUES ('bank', 'CASH1', 'cash', 1, 1), ('bank', 'BRK1', 'brokerage', 1, 1);

        INSERT INTO transactions(silver_source_id, transaction_external_id, occurred_at,
            account_external_id, kind, currency, net_amount, counterparty) VALUES
            -- 'AG' is an organisation marker, so this signature is not
            -- person-shaped: the KIND gate is the only thing that can
            -- keep it out of the candidate list, which is what the
            -- --all test below needs it to prove.
            ('bank', 'T-DIV',    1000, 'BRK1',  'dividend', 'USD', 40,  'Example Depot AG'),
            -- An employer: a payer worth naming, and one whose name
            -- says it is an organisation rather than a person.
            ('bank', 'T-WIRE',   1000, 'CASH1', 'deposit',  'USD', 900, 'Blue Harbour Payroll'),
            -- A bare person's name, arriving the way one does: a credit
            -- transfer whose narrative IS the sender, with no rail
            -- token, no IBAN and no masked number beside it.
            ('bank', 'T-BARE',   1000, 'CASH1', 'deposit',  'USD', 300, 'Jane Example'),
            -- A second unfenced deposit on the same day, so a candidate
            -- has a neighbour at all: without one, an assertion that
            -- the dividend is absent from the neighbour lists passes
            -- because the lists are empty.
            ('bank', 'T-RENT',   1000, 'CASH1', 'deposit',  'USD', 450, 'Example Letting Agent Ltd'),
            ('bank', 'T-PERSON', 1000, 'CASH1', 'deposit',  'USD', 250, 'John Example IBAN CH00');

        INSERT INTO income_txn_enrichment(silver_source_id, transaction_external_id,
            payer_signature, signature_version, income_detailed, provenance, assigned_at) VALUES
            ('bank', 'T-DIV',    'EXAMPLE DEPOT AG',       1, NULL, 'signature-only', 1),
            ('bank', 'T-WIRE',   'BLUE HARBOUR PAYROLL',   1, NULL, 'signature-only', 1),
            ('bank', 'T-BARE',   'JANE EXAMPLE',           1, NULL, 'signature-only', 1),
            ('bank', 'T-RENT',   'EXAMPLE LETTING AGENT LTD', 1, NULL, 'signature-only', 1),
            ('bank', 'T-PERSON', 'JOHN EXAMPLE IBAN CH00', 1, NULL, 'signature-only', 1);
    `); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db, ctx
}
