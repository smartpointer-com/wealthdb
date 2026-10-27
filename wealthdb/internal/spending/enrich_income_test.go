package spending

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
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

// TestIncomeBuiltinRuleIsGatedOnDeposit pins the gate, which is the
// thing that keeps the rule from reaching past the one kind it can
// speak for.
//
// The rule tier is ABOVE the kind floor and stays there — a config rule
// promoting a `distribution` depends on it — so a phrase match on a
// kind the floor already answers does not merely add a verdict, it
// REPLACES a right one. `interest` is where the collision is real: a
// call or money-market account books credited interest and a bank
// narrates it "INTEREST ON CASH DEPOSIT", which the rule's phrase
// matches exactly. Ungated, interest earned would read as cash paid in
// over a counter.
func TestIncomeBuiltinRuleIsGatedOnDeposit(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		// The phrase, on the kind the rule exists for.
		txn{"bank", "T-REAL", "CASH1", "deposit", day(10), 500, "CASH DEPOSIT BRANCH", "", ""},
		// The same phrase, on a kind the floor answers. The narrative
		// is a bank's ordinary wording for a credit-interest line.
		txn{"bank", "T-INT", "CASH1", "interest", day(11), 12, "INTEREST ON CASH DEPOSIT", "", ""},
		// ...and on two more floored kinds, so the gate is not read as
		// a special case about interest.
		txn{"bank", "T-DIV", "CASH1", "dividend", day(12), 40, "CASH DEPOSIT DIVIDEND", "", ""},
		txn{"bank", "T-RWD", "CASH1", "reward", day(13), 5, "BAREINZAHLUNG BONUS", "", ""},
	)
	runPass(t, db, ctx, Options{})

	if detailed, provenance, _ := incomeVerdictOf(t, db, ctx, "bank", "T-REAL"); detailed !=
		canonical.IncomeDetailedCashDeposit || provenance != ProvenanceRule {
		t.Errorf("T-REAL = (%q, %q), want the rule to fire on a deposit", detailed, provenance)
	}
	for _, id := range []string{"T-INT", "T-DIV", "T-RWD"} {
		detailed, provenance, _ := incomeVerdictOf(t, db, ctx, "bank", id)
		if detailed != "" {
			t.Errorf("%s = (%q, %q): the built-in rule reached a kind the floor answers, "+
				"and the rule tier outranks the floor", id, detailed, provenance)
		}
	}

	// The gate is the function's own, not the caller's, so a direct
	// call cannot get past it either.
	if _, ok := IncomeRuleCategory("interest", "CASH DEPOSIT BRANCH", "", "", ""); ok {
		t.Error("IncomeRuleCategory fired on an interest row")
	}
	if _, ok := IncomeRuleCategory("deposit", "CASH DEPOSIT BRANCH", "", "", ""); !ok {
		t.Error("IncomeRuleCategory did not fire on a deposit row")
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
		"T-SALARY":   {"INCOME_SALARY", ProvenanceProvider},
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

// TestIncomeDepositReversalInsideTheMatcherWindow pins what a deposit
// and its same-amount reversal on one account read as when they sit a
// day apart, well inside the matcher's window.
//
// The matcher pool has no sign guard (migration 0044): it admits
// `deposit` rows of either sign, and a negative one is the outgoing
// shape it looks for. With no partner on another account, the reversal
// phase pairs the two: both read `internal_transfer` and leave the
// income base, rather than netting inside the payer's type (INCOME.md
// §11). The net is the same either way.
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
		Rules: []Rule{{Match: regexp.MustCompile(`(?i)EXAMPLE LETTING AGENT`), Category: "INCOME_RENTAL"}},
		Pins: []Pin{{Source: "swiss", Account: "CASH9", Day: day(14), Amount: 250, Currency: "USD",
			Detailed: canonical.IncomeDetailedInheritance}},
	}})

	for id, want := range map[string][2]string{
		"T-PROVIDER": {"INCOME_SALARY", ProvenanceProvider},
		"T-RULE":     {canonical.IncomeDetailedCashDeposit, ProvenanceRule},
		"T-CONFIG":   {"INCOME_RENTAL", ProvenanceRule},
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
	good := header + "income_detailed\nbank,CASH1,2024-01-02,900.00,USD,INCOME_SALARY\n"
	pins, err := parsePinLedger(strings.NewReader(good), "income", "income_detailed", canonical.IncomeDetailedCapitalReturn, canonical.ValidIncomeDetailed)
	if err != nil {
		t.Fatalf("parse the income ledger: %v", err)
	}
	if len(pins) != 1 || pins[0].Detailed != "INCOME_SALARY" {
		t.Fatalf("pins = %+v", pins)
	}

	// A spending value in the income ledger is refused where a person
	// can still fix it cheaply.
	bad := header + "income_detailed\nbank,CASH1,2024-01-02,900.00,USD,FOOD_AND_DRINK_GROCERIES\n"
	if _, err := parsePinLedger(strings.NewReader(bad), "income", "income_detailed", canonical.IncomeDetailedCapitalReturn, canonical.ValidIncomeDetailed); err == nil ||
		!strings.Contains(err.Error(), "income.pins") {
		t.Errorf("a spending value in the income ledger: err = %v", err)
	}
	// ...and the spending ledger's own column name is not accepted for
	// it, so a file cannot be half one family's and half the other's.
	wrongCol := header + "spend_detailed\nbank,CASH1,2024-01-02,900.00,USD,INCOME_SALARY\n"
	if _, err := parsePinLedger(strings.NewReader(wrongCol), "income", "income_detailed", canonical.IncomeDetailedCapitalReturn, canonical.ValidIncomeDetailed); err == nil ||
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
        VALUES ('EXAMPLE PAYER OLD FOLD', 'Example Payer', 'INCOME_SALARY', ?, 1, 'test-model')`,
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
	if name != "Example Payer" || detailed != "INCOME_SALARY" {
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

// TestIncomeCustodyDepositIsCapital pins the built-in that reads the
// account kind: a deposit on a custody account is the holder's capital
// going in, `capital_return`, whatever its narrative; the same deposit
// on a cash account stays the narrative tiers' to place. Like every
// built-in it sits above the config rules; the matcher and a pin outrank
// it, and it reads `deposit` alone.
func TestIncomeCustodyDepositIsCapital(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "FUNDING", "CUST1", "deposit", day(10), 2500, "Wire received", "", ""},
		txn{"bank", "WAGES", "CASH1", "deposit", day(10), 2600, "Wire received", "", ""},
		txn{"other-bank", "RULED", "CUST2", "deposit", day(11), 900, "Example Fund Return", "", ""},
	)
	runPass(t, db, ctx, Options{Income: IncomeOptions{Rules: []Rule{{
		Match: regexp.MustCompile(`(?i)example fund return`), Category: "INCOME_DIVIDENDS"}}}})
	if d, p, _ := incomeVerdictOf(t, db, ctx, "bank", "FUNDING"); d != canonical.IncomeDetailedCapitalReturn || p != ProvenanceRule {
		t.Errorf("custody deposit = (%q, %q), want the built-in's capital_return", d, p)
	}
	if d, _, _ := incomeVerdictOf(t, db, ctx, "bank", "WAGES"); d == canonical.IncomeDetailedCapitalReturn {
		t.Error("a deposit on a cash account is not the custody rule's")
	}
	if d, _, _ := incomeVerdictOf(t, db, ctx, "other-bank", "RULED"); d != canonical.IncomeDetailedCapitalReturn {
		t.Errorf("the built-in sits above a config rule: got %q", d)
	}
}

// TestIncomeCustodyRuleGivesWay pins the tiers above the custody rule
// and its gate: a funding leg the matcher pairs stays an own-account
// move, a pin outranks the rule, and a custody row of another kind is
// not the rule's.
func TestIncomeCustodyRuleGivesWay(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "FUND-OUT", "CASH1", "withdrawal", day(20), -700, "To platform", "", ""},
		txn{"other-bank", "FUND-IN", "CUST2", "deposit", day(20), 700, "Funds received", "", ""},
		txn{"bank", "PINNED", "CUST1", "deposit", day(30), 55, "Funds received", "", ""},
		txn{"bank", "PAYOUT", "CUST1", "distribution", day(40), 80, "Distribution", "", ""},
	)
	runPass(t, db, ctx, Options{Income: IncomeOptions{Pins: []Pin{{
		Source: "bank", Account: "CUST1", Day: day(30), Amount: 55, Currency: "USD", Detailed: "INCOME_OTHER"}}}})
	if d, p, _ := incomeVerdictOf(t, db, ctx, "other-bank", "FUND-IN"); d != canonical.SpendDetailedInternalTransfer || p != ProvenanceMatcher {
		t.Errorf("a paired funding leg = (%q, %q), want the matcher's internal_transfer", d, p)
	}
	if d, p, _ := incomeVerdictOf(t, db, ctx, "bank", "PINNED"); d != "INCOME_OTHER" || p != ProvenanceManual {
		t.Errorf("a pinned custody deposit = (%q, %q), want the pin", d, p)
	}
	if d, _, _ := incomeVerdictOf(t, db, ctx, "bank", "PAYOUT"); d == canonical.IncomeDetailedCapitalReturn {
		t.Error("the custody rule reads deposits alone; a distribution is the floor's")
	}
}
