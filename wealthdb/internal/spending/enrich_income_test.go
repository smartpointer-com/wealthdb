package spending

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// incomeVerdictOf reads what the pass wrote into the income overlay for
// one transaction. A missing row is reported as such rather than as an
// empty verdict: on the income side "no row" and "a row nothing placed"
// are different answers, and only the second is the model's backlog.
func incomeVerdictOf(t *testing.T, db *sql.DB, ctx context.Context, source, id string) (detailed, provenance string, found bool) {
	t.Helper()
	var d sql.NullString
	err := db.QueryRowContext(ctx, `
        SELECT income_detailed, provenance FROM income_txn_enrichment
         WHERE silver_source_id = ? AND transaction_external_id = ?`, source, id).
		Scan(&d, &provenance)
	if err == sql.ErrNoRows {
		return "", "", false
	}
	if err != nil {
		t.Fatalf("read income overlay for %s/%s: %v", source, id, err)
	}
	return d.String, provenance, true
}

func incomeSignatureOf(t *testing.T, db *sql.DB, ctx context.Context, source, id string) string {
	t.Helper()
	var sig sql.NullString
	if err := db.QueryRowContext(ctx, `
        SELECT payer_signature FROM income_txn_enrichment
         WHERE silver_source_id = ? AND transaction_external_id = ?`, source, id).
		Scan(&sig); err != nil {
		t.Fatalf("read income signature for %s/%s: %v", source, id, err)
	}
	return sig.String
}

// TestIncomePassReadsTheSharedMatch is the decision of record about the
// matcher, expressed as one pass over one fixture: income pairs nothing
// and reads the verdicts spending's matcher already produced.
//
// A withdrawal on one source and its deposit on another. One pass runs
// the matcher ONCE and writes `internal_transfer` on both legs, into
// two different overlays — the outgoing leg into the spending one, the
// receiving leg into the income one. Two matchers with two bandings
// would be free to disagree about the same wire; there is one.
func TestIncomePassReadsTheSharedMatch(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T-OUT", "CASH1", "withdrawal", day(20), -400, "Own Transfer", "", ""},
		txn{"other-bank", "T-IN", "CASH2", "deposit", day(20), 400, "Own Transfer", "", ""},
	)
	runPass(t, db, ctx, Options{})

	detailed, provenance := verdictOf(t, db, ctx, "bank", "T-OUT")
	if detailed != canonical.SpendDetailedInternalTransfer || provenance != ProvenanceMatcher {
		t.Errorf("the outgoing leg = (%q, %q), want the matcher's internal_transfer", detailed, provenance)
	}
	detailed, provenance, found := incomeVerdictOf(t, db, ctx, "other-bank", "T-IN")
	if !found {
		t.Fatal("the receiving leg has no income overlay row: one pass writes both families")
	}
	if detailed != canonical.SpendDetailedInternalTransfer || provenance != ProvenanceMatcher {
		t.Errorf("the receiving leg = (%q, %q), want the matcher's internal_transfer", detailed, provenance)
	}

	// The receiving leg is in the income population, so it is income's
	// own row rather than a leg reached from outside — and it leaves
	// the income base, an own-account move being no more income than
	// it is spend.
	var inBase int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM income_lines_base(0, ?)
         WHERE transaction_external_id = 'T-IN'`, day(400)).Scan(&inBase); err != nil {
		t.Fatalf("read income base: %v", err)
	}
	if inBase != 0 {
		t.Error("the receiving leg is in the income base; an own-account move is not income")
	}
}

// TestIncomePassEnrichesItsOwnPopulation pins the shape of the income
// overlay after a pass with no config at all: every income-population
// row is recorded, whether or not a tier could place it, and rows that
// are spending's are not.
func TestIncomePassEnrichesItsOwnPopulation(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		// Income's: the floor places these at query time, and the pass
		// records them with their signature so the re-key can find them.
		txn{"bank", "T-DIVIDEND", "BRK1", "dividend", day(10), 40, "", "", ""},
		txn{"bank", "T-DEPOSIT", "CASH1", "deposit", day(10), 900, "Example Payer", "", ""},
		// A deposit reversal, which decision 12 admits so the pair nets.
		// Dated well outside the matcher's window on purpose: inside
		// it, a same-amount reversal pairs with its own booking — see
		// TestIncomeDepositReversalInsideTheMatcherWindow.
		txn{"bank", "T-DEP-REVERSAL", "CASH1", "deposit", day(40), -900, "Example Payer", "", ""},
		// Spending's.
		txn{"bank", "T-PURCHASE", "CARD1", "purchase", day(10), -50, "Corner Market", "", "Groceries"},
	)
	res := runPass(t, db, ctx, Options{})

	for _, id := range []string{"T-DIVIDEND", "T-DEPOSIT", "T-DEP-REVERSAL"} {
		detailed, provenance, found := incomeVerdictOf(t, db, ctx, "bank", id)
		if !found {
			t.Errorf("%s has no income overlay row", id)
			continue
		}
		if detailed != "" {
			t.Errorf("%s = %q; no deterministic tier places these, and the floor is gold's", id, detailed)
		}
		if provenance != ProvenanceSignatureOnly {
			t.Errorf("%s provenance = %q, want %q", id, provenance, ProvenanceSignatureOnly)
		}
	}
	if _, _, found := incomeVerdictOf(t, db, ctx, "bank", "T-PURCHASE"); found {
		t.Error("a purchase reached the income overlay; the two populations select different kinds")
	}

	// A reversal carries the same signature as the booking it reverses,
	// which is what lets the two net inside one payer's type once the
	// model has named that payer.
	if a, b := incomeSignatureOf(t, db, ctx, "bank", "T-DEPOSIT"),
		incomeSignatureOf(t, db, ctx, "bank", "T-DEP-REVERSAL"); a != b || a == "" {
		t.Errorf("reversal signature %q != booking signature %q", b, a)
	}

	if res.Income.Population != 3 || res.Income.Enriched != 3 {
		t.Errorf("income population/enriched = %d/%d, want 3/3",
			res.Income.Population, res.Income.Enriched)
	}
	if res.Population != 1 {
		t.Errorf("spending population = %d, want 1", res.Population)
	}
}

// TestIncomeCashDepositRule pins the income family's one built-in rule,
// and the argument for its being the only one: cash over a counter has
// no counter-leg for the matcher and no payer for the model, so a
// narrative is the only thing that can place it.
func TestIncomeCashDepositRule(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T-COUNTER", "CASH1", "deposit", day(10), 500, "CASH DEPOSIT BRANCH", "", ""},
		txn{"bank", "T-MACHINE", "CASH1", "deposit", day(11), 200, "ATM DEPOSIT", "", ""},
		txn{"bank", "T-GERMAN", "CASH1", "deposit", day(12), 300, "BAREINZAHLUNG", "", ""},
		// An ordinary inbound wire: the rule must not claim it.
		txn{"bank", "T-WIRE", "CASH1", "deposit", day(13), 700, "Example Payer", "", ""},
		// A machine token with no deposit phrasing: on the inflow side
		// that is not enough, and the row stays for the model.
		txn{"bank", "T-BARE-ATM", "CASH1", "deposit", day(14), 100, "ATM REVERSAL", "", ""},
	)
	runPass(t, db, ctx, Options{})

	for _, id := range []string{"T-COUNTER", "T-MACHINE", "T-GERMAN"} {
		detailed, provenance, _ := incomeVerdictOf(t, db, ctx, "bank", id)
		if detailed != canonical.IncomeDetailedCashDeposit || provenance != ProvenanceRule {
			t.Errorf("%s = (%q, %q), want (%q, rule)", id, detailed, provenance,
				canonical.IncomeDetailedCashDeposit)
		}
	}
	for _, id := range []string{"T-WIRE", "T-BARE-ATM"} {
		detailed, provenance, _ := incomeVerdictOf(t, db, ctx, "bank", id)
		if detailed != "" {
			t.Errorf("%s = (%q, %q), want unplaced: an inbound wire is the model tier's", id, detailed, provenance)
		}
	}
}

// TestIncomeProviderTier pins the second bank vocabulary the income
// side reads, and the two ways it declines.
func TestIncomeProviderTier(t *testing.T) {
	db, ctx := openGold(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path,
                                    high_watermark, first_loaded_at, last_loaded_at)
             VALUES ('swiss', 'ubs', '/tmp/ubs.db', -1, 0, 0);
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, first_seen_at, last_seen_at)
             VALUES ('swiss', 'CASH9', 'cash', 'Swiss', 1, 1),
                    ('swiss', 'CARD9', 'card', 'Swiss card', 1, 1);
    `); err != nil {
		t.Fatalf("seed a bank source: %v", err)
	}
	seedTxns(t, db, ctx,
		// The bank names the movement, and there is no payer to read.
		txn{"swiss", "T-SALARY", "CASH9", "deposit", day(10), 5000, "", "", "SALARY PAYMENT"},
		// The same booking type the spending map reads as a finance
		// charge, on the credited side.
		txn{"swiss", "T-INTEREST", "CASH9", "interest", day(11), 12, "", "", "INTEREST CALCULATION BALANCE"},
		// Capital the holder put in, coming back: the provider tier may
		// place a delta.
		txn{"swiss", "T-RETURN", "CASH9", "deposit", day(12), 4000, "", "", "RETURN OF CAPITAL"},
		// A generic credit type: names a rail, not a payer.
		txn{"swiss", "T-CREDIT", "CASH9", "deposit", day(13), 600, "Example Payer", "", "e-banking credit"},
	)
	runPass(t, db, ctx, Options{})

	for id, want := range map[string][2]string{
		"T-SALARY":   {"INCOME_WAGES", ProvenanceProvider},
		"T-INTEREST": {"INCOME_INTEREST_EARNED", ProvenanceProvider},
		"T-RETURN":   {canonical.IncomeDetailedCapitalReturn, ProvenanceProvider},
		"T-CREDIT":   {"", ProvenanceSignatureOnly},
	} {
		detailed, provenance, found := incomeVerdictOf(t, db, ctx, "swiss", id)
		if !found {
			t.Errorf("%s has no income overlay row", id)
			continue
		}
		if got := [2]string{detailed, provenance}; got != want {
			t.Errorf("%s = %v, want %v", id, got, want)
		}
	}

	// The same row read from the two sides: the spending map has its
	// own answer for the charged half of that booking type, and neither
	// map answers for the other's direction.
	if detailed, _, found := incomeVerdictOf(t, db, ctx, "swiss", "T-SALARY"); found && detailed == "" {
		t.Error("the income provider tier placed nothing where the bank named the movement")
	}
	var spendRow int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM spend_txn_enrichment
         WHERE silver_source_id = 'swiss' AND transaction_external_id = 'T-SALARY'`).Scan(&spendRow); err != nil {
		t.Fatalf("read spending overlay: %v", err)
	}
	if spendRow != 0 {
		t.Error("a salary credit reached the spending overlay")
	}
}

// TestIncomePassIsIdempotent holds the income overlay to the same bar
// the spending one is held to: the pass re-derives everything it owns,
// so running it twice must leave the same rows.
func TestIncomePassIsIdempotent(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T-DIVIDEND", "BRK1", "dividend", day(10), 40, "", "", ""},
		txn{"bank", "T-COUNTER", "CASH1", "deposit", day(11), 500, "CASH DEPOSIT BRANCH", "", ""},
		txn{"bank", "T-WIRE", "CASH1", "deposit", day(12), 700, "Example Payer", "", ""},
	)
	snapshot := func() string {
		t.Helper()
		rows, err := db.QueryContext(ctx, `
            SELECT silver_source_id, transaction_external_id,
                   COALESCE(payer_signature, '(null)'), signature_version,
                   COALESCE(income_detailed, '(null)'), provenance,
                   COALESCE(provider_income_detailed, '(null)')
              FROM income_txn_enrichment
             ORDER BY silver_source_id, transaction_external_id`)
		if err != nil {
			t.Fatalf("read income overlay: %v", err)
		}
		defer rows.Close()
		out := ""
		for rows.Next() {
			var src, id, sig, detailed, prov, provider string
			var version int
			if err := rows.Scan(&src, &id, &sig, &version, &detailed, &prov, &provider); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out += fmt.Sprintf("%s/%s %q v%d %s %s %s\n", src, id, sig, version, detailed, prov, provider)
		}
		return out
	}

	runPass(t, db, ctx, Options{})
	first := snapshot()
	runPass(t, db, ctx, Options{})
	if second := snapshot(); second != first {
		t.Errorf("the income overlay moved on a second pass.\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if first == "" {
		t.Fatal("the income overlay is empty; the snapshot compares nothing")
	}
}

// TestIncomeDepositReversalInsideTheMatcherWindow pins an interaction
// worth knowing about rather than discovering: a deposit and its
// same-amount reversal, days apart, are a transfer pair as far as the
// matcher is concerned.
//
// The matcher pool has never had a sign guard (migration 0044): it
// admits `deposit` rows of either sign, and a negative one is the
// outgoing shape it looks for. So the pair is matched, both legs read
// `internal_transfer`, and both leave the income base — where decision
// 12 would have had them net inside the payer's type instead.
//
// The NET is the same either way, which is why this is a note and not
// a defect: a booking and its reversal contribute nothing to income
// under either reading. What differs is the account of it — "money
// moved between two tracked accounts" is not what happened — and the
// possibility, shared with every matcher false positive, of pairing a
// reversal with an unrelated movement of the same size. The remedy is
// the one that already exists for that: the transfer-override ledger.
func TestIncomeDepositReversalInsideTheMatcherWindow(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T-BOOKED", "CASH1", "deposit", day(10), 900, "Example Payer", "", ""},
		txn{"bank", "T-REVERSED", "CASH1", "deposit", day(11), -900, "Example Payer", "", ""},
	)
	runPass(t, db, ctx, Options{})

	for _, id := range []string{"T-BOOKED", "T-REVERSED"} {
		detailed, provenance, found := incomeVerdictOf(t, db, ctx, "bank", id)
		if !found {
			t.Fatalf("%s has no income overlay row", id)
		}
		if detailed != canonical.SpendDetailedInternalTransfer || provenance != ProvenanceMatcher {
			t.Errorf("%s = (%q, %q), want the matcher's internal_transfer", id, detailed, provenance)
		}
	}
	var inBase int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM income_lines_base(0, ?)`, day(400)).Scan(&inBase); err != nil {
		t.Fatalf("read income base: %v", err)
	}
	if inBase != 0 {
		t.Errorf("income base holds %d rows; the matcher removed both legs of the pair", inBase)
	}
}

// TestIncomePassPrecedence pins the income family's tier ladder, which
// is the spending one written once and read with a second vocabulary:
// pin > matcher > rule > provider.
func TestIncomePassPrecedence(t *testing.T) {
	db, ctx := openGold(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path,
                                    high_watermark, first_loaded_at, last_loaded_at)
             VALUES ('swiss', 'ubs', '/tmp/ubs.db', -1, 0, 0);
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, first_seen_at, last_seen_at)
             VALUES ('swiss', 'CASH9', 'cash', 'Swiss', 1, 1);
    `); err != nil {
		t.Fatalf("seed a bank source: %v", err)
	}
	seedTxns(t, db, ctx,
		// provider alone.
		txn{"swiss", "T-PROVIDER", "CASH9", "deposit", day(10), 5000, "", "", "SALARY PAYMENT"},
		// built-in rule over provider: the bank called a counter
		// deposit a salary, which a narrative naming the counter beats.
		txn{"swiss", "T-RULE", "CASH9", "deposit", day(11), 400, "CASH DEPOSIT BRANCH", "", "SALARY PAYMENT"},
		// config rule over provider, on a row no built-in reaches.
		txn{"swiss", "T-CONFIG", "CASH9", "deposit", day(12), 900, "EXAMPLE LETTING AGENT", "", "SALARY PAYMENT"},
		// matcher over everything below it.
		txn{"swiss", "T-MATCH-IN", "CASH9", "deposit", day(13), 700, "Own Transfer", "", "SALARY PAYMENT"},
		txn{"bank", "T-MATCH-OUT", "CASH1", "withdrawal", day(13), -700, "Own Transfer", "", ""},
		// pin over everything, including the matcher.
		txn{"swiss", "T-PIN", "CASH9", "deposit", day(14), 250, "Looks Internal", "", "SALARY PAYMENT"},
		txn{"bank", "T-PIN-PAIR", "CASH1", "withdrawal", day(14), -250, "Looks Internal", "", ""},
	)
	runPass(t, db, ctx, Options{Income: IncomeOptions{
		Rules: []Rule{{Match: regexp.MustCompile(`(?i)EXAMPLE LETTING AGENT`), Category: "INCOME_RENT"}},
		Pins: []Pin{{Source: "swiss", Account: "CASH9", Day: day(14), Amount: 250, Currency: "USD",
			Detailed: canonical.IncomeDetailedInheritance}},
	}})

	for id, want := range map[string][2]string{
		"T-PROVIDER": {"INCOME_WAGES", ProvenanceProvider},
		"T-RULE":     {canonical.IncomeDetailedCashDeposit, ProvenanceRule},
		"T-CONFIG":   {"INCOME_RENT", ProvenanceRule},
		"T-MATCH-IN": {canonical.SpendDetailedInternalTransfer, ProvenanceMatcher},
		"T-PIN":      {canonical.IncomeDetailedInheritance, ProvenanceManual},
	} {
		detailed, provenance, found := incomeVerdictOf(t, db, ctx, "swiss", id)
		if !found {
			t.Errorf("%s has no income overlay row", id)
			continue
		}
		if got := [2]string{detailed, provenance}; got != want {
			t.Errorf("%s = %v, want %v", id, got, want)
		}
	}
}

// TestIncomePinLedgerColumn pins the one difference between the two
// ledgers: the value column, and the vocabulary it is read against.
func TestIncomePinLedgerColumn(t *testing.T) {
	const header = "silver_source_id,account,occurred_at,amount,currency,"
	good := header + "income_detailed\nbank,CASH1,2024-01-02,900.00,USD,INCOME_WAGES\n"
	pins, err := parsePinLedger(strings.NewReader(good), "income", "income_detailed", canonical.ValidIncomeDetailed)
	if err != nil {
		t.Fatalf("parse the income ledger: %v", err)
	}
	if len(pins) != 1 || pins[0].Detailed != "INCOME_WAGES" {
		t.Fatalf("pins = %+v", pins)
	}

	// A spending value in the income ledger is refused where a person
	// can still fix it cheaply.
	bad := header + "income_detailed\nbank,CASH1,2024-01-02,900.00,USD,FOOD_AND_DRINK_GROCERIES\n"
	if _, err := parsePinLedger(strings.NewReader(bad), "income", "income_detailed", canonical.ValidIncomeDetailed); err == nil ||
		!strings.Contains(err.Error(), "income.pins") {
		t.Errorf("a spending value in the income ledger: err = %v", err)
	}
	// ...and the spending ledger's own column name is not accepted for
	// it, so a file cannot be half one family's and half the other's.
	wrongCol := header + "spend_detailed\nbank,CASH1,2024-01-02,900.00,USD,INCOME_WAGES\n"
	if _, err := parsePinLedger(strings.NewReader(wrongCol), "income", "income_detailed", canonical.ValidIncomeDetailed); err == nil ||
		!strings.Contains(err.Error(), "income_detailed") {
		t.Errorf("the spending column in an income ledger: err = %v", err)
	}
}

// TestIncomeRekeysPayerVerdicts holds the payer store to the same
// carry-forward the merchant store gets: a SignatureVersion bump moves
// a paid-for verdict onto the signature the current rules compute,
// rather than orphaning it.
func TestIncomeRekeysPayerVerdicts(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T-WIRE", "CASH1", "deposit", day(10), 900, "Example Payer", "", ""},
	)
	runPass(t, db, ctx, Options{})
	sig := incomeSignatureOf(t, db, ctx, "bank", "T-WIRE")
	if sig == "" {
		t.Fatal("no signature computed for the deposit")
	}

	// A verdict paid for under an OLDER signature version, hanging off
	// a signature the current rules no longer compute.
	if _, err := db.ExecContext(ctx, `
        INSERT INTO income_payer_categories (payer_signature, payer_name, income_detailed,
                                             signature_version, assigned_at, model_name)
        VALUES ('EXAMPLE PAYER OLD FOLD', 'Example Payer', 'INCOME_WAGES', ?, 1, 'test-model')`,
		SignatureVersion-1); err != nil {
		t.Fatalf("seed an older-version verdict: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
        UPDATE income_txn_enrichment SET payer_signature = 'EXAMPLE PAYER OLD FOLD',
                                         signature_version = ?
         WHERE transaction_external_id = 'T-WIRE'`, SignatureVersion-1); err != nil {
		t.Fatalf("age the overlay row: %v", err)
	}

	res := runPass(t, db, ctx, Options{})
	if res.Income.RekeyedVerdicts != 1 {
		t.Errorf("Income.RekeyedVerdicts = %d, want 1", res.Income.RekeyedVerdicts)
	}
	var name, detailed string
	if err := db.QueryRowContext(ctx, `
        SELECT payer_name, income_detailed FROM income_payer_categories
         WHERE payer_signature = ?`, sig).Scan(&name, &detailed); err != nil {
		t.Fatalf("the verdict did not land on the new signature: %v", err)
	}
	if name != "Example Payer" || detailed != "INCOME_WAGES" {
		t.Errorf("carried verdict = (%q, %q)", name, detailed)
	}
	// The merchant store is untouched: two stores, two questions.
	var merchants int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spend_merchant_categories`).Scan(&merchants); err != nil {
		t.Fatalf("count: %v", err)
	}
	if merchants != 0 {
		t.Errorf("the payer re-key wrote %d merchant verdict(s)", merchants)
	}
}
