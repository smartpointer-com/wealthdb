package spending

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// The far side of a move: the partner leg's account the matcher saw, or
// the account a rule or the source names, linked in transfer_links.
//
// Neither family's base reads it. An own-account move is not spending
// and not income whichever account it went to, so these tests assert
// the links and, just as deliberately, that the two families' reports
// do not move.

// farOf reads the far account the pass linked for one transaction. Each
// part comes back as the empty string where the pass linked none.
func farOf(t *testing.T, db *sql.DB, ctx context.Context, source, id string) (farSource, farAccount string) {
	t.Helper()
	var s, a sql.NullString
	if err := db.QueryRowContext(ctx, `
        SELECT l.far_silver_source_id, l.far_account_external_id
          FROM spend_txn_enrichment e
          LEFT JOIN transfer_links l USING (silver_source_id, transaction_external_id)
         WHERE e.silver_source_id = ? AND e.transaction_external_id = ?`,
		source, id).Scan(&s, &a); err != nil {
		t.Fatalf("read the far side of %s/%s: %v", source, id, err)
	}
	return s.String, a.String
}

// TestMatcherRecordsBothEndsOfAPair: each leg of a matched pair names
// the OTHER leg's account, so a reader can ask where the money went
// without re-running the matcher.
func TestMatcherRecordsBothEndsOfAPair(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		// A funding wire between two sources: the outgoing leg is in
		// the spending population, the incoming one is not.
		txn{source: "bank", id: "T-OUT", account: "CASH1", kind: "withdrawal",
			occurredAt: day(10), amount: -2500, description: "TRANSFER TO SAVINGS"},
		txn{source: "other-bank", id: "T-IN", account: "CASH2", kind: "deposit",
			occurredAt: day(11), amount: 2500, description: "INCOMING TRANSFER"},
	)
	runPass(t, db, ctx, Options{})

	if src, acct := farOf(t, db, ctx, "bank", "T-OUT"); src != "other-bank" || acct != "CASH2" {
		t.Errorf("outgoing leg far = (%q, %q), want (other-bank, CASH2)", src, acct)
	}
	// The receiving leg is outside the spending population and gets its
	// row anyway (emitOutsidePopulation) — which is the only reason a
	// fact about it can be recorded at all.
	if src, acct := farOf(t, db, ctx, "other-bank", "T-IN"); src != "bank" || acct != "CASH1" {
		t.Errorf("incoming leg far = (%q, %q), want (bank, CASH1)", src, acct)
	}
	for _, id := range []string{"T-OUT", "T-IN"} {
		src := "bank"
		if id == "T-IN" {
			src = "other-bank"
		}
		if detailed, prov := verdictOf(t, db, ctx, src, id); detailed != internalTransfer || prov != ProvenanceMatcher {
			t.Errorf("%s = (%q, %q), want the matcher's own-account verdict", id, detailed, prov)
		}
	}
}

// TestSameSourceCardBillRecordsTheCard pins the commonest own-account
// move of all — a cash account paying a card inside one bank — and that
// the card's own receipt names the paying account back.
func TestSameSourceCardBillRecordsTheCard(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{source: "bank", id: "T-PAY", account: "CASH1", kind: "withdrawal",
			occurredAt: day(20), amount: -430, description: "CREDIT CRD EPAY"},
		txn{source: "bank", id: "T-RCPT", account: "CARD1", kind: "card_payment",
			occurredAt: day(20), amount: 430, description: "PAYMENT THANK YOU"},
	)
	runPass(t, db, ctx, Options{})

	if src, acct := farOf(t, db, ctx, "bank", "T-PAY"); src != "bank" || acct != "CARD1" {
		t.Errorf("the bill's far account = (%q, %q), want (bank, CARD1)", src, acct)
	}
	if src, acct := farOf(t, db, ctx, "bank", "T-RCPT"); src != "bank" || acct != "CASH1" {
		t.Errorf("the receipt's far account = (%q, %q), want (bank, CASH1)", src, acct)
	}
}

// TestTheMortgageRulePlacesItsValue pins the one built-in rule that
// places an own money movement. It matches a NARRATIVE, so it fires
// whether or not the lender is tracked, and its value reaches
// financing · Mortgage with no far account at all.
func TestTheMortgageRulePlacesItsValue(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{source: "bank", id: "T-MORT", account: "CASH1", kind: "withdrawal",
			occurredAt: day(30), amount: -1800, description: "MORTGAGE PAYMENT"},
		// A rule-placed own-account move: a wire the config names as the
		// holder's own account elsewhere.
		txn{source: "bank", id: "T-OWN", account: "CASH1", kind: "withdrawal",
			occurredAt: day(31), amount: -900, description: "WIRE TO OWN ACCOUNT"},
		// An ordinary purchase: no rule, no pairing, nothing to record.
		txn{source: "bank", id: "T-SHOP", account: "CASH1", kind: "purchase",
			occurredAt: day(32), amount: -40, description: "CORNER MARKET"},
	)
	runPass(t, db, ctx, Options{Rules: []Rule{{
		Match:    regexp.MustCompile(`(?i)WIRE TO OWN ACCOUNT`),
		Category: canonical.SpendDetailedInternalTransfer,
	}}})

	if detailed, prov := verdictOf(t, db, ctx, "bank", "T-MORT"); detailed != canonical.DetailedMortgageTransfer ||
		prov != ProvenanceRule {
		t.Errorf("mortgage payment = (%q, %q), want the rule tier's mortgage_transfer", detailed, prov)
	}
	// Nothing names where any of the three went, which is exactly the
	// hole the untracked-accounts node exists to show for the config
	// rule's move rather than to swallow.
	for _, id := range []string{"T-MORT", "T-OWN", "T-SHOP"} {
		if src, acct := farOf(t, db, ctx, "bank", id); src != "" || acct != "" {
			t.Errorf("%s far = (%q, %q), want none", id, src, acct)
		}
	}
}

// TestATierAboveTheMortgageRuleReplacesItsValue: a pairing and a pin
// both outrank the rule's value.
func TestATierAboveTheMortgageRuleReplacesItsValue(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		// A mortgage payment whose lender IS tracked: the rule places
		// its value, the matcher then pairs the legs and outranks it.
		txn{source: "bank", id: "T-MORT-PAID", account: "CASH1", kind: "withdrawal",
			occurredAt: day(40), amount: -1800, description: "MORTGAGE PAYMENT"},
		txn{source: "bank", id: "T-MORT-RCVD", account: "BRK1", kind: "deposit",
			occurredAt: day(40), amount: 1800, description: "LOAN INSTALMENT RECEIVED"},
		// A mortgage payment the holder pinned as something else.
		txn{source: "bank", id: "T-MORT-PIN", account: "CASH1", kind: "withdrawal",
			occurredAt: day(41), amount: -1200, description: "MORTGAGE PAYMENT"},
	)
	runPass(t, db, ctx, Options{Pins: []Pin{{
		Source: "bank", Account: "CASH1", Day: day(41),
		Amount: -1200, Currency: "USD",
		Detailed: canonical.SpendDetailedDebtRepayment,
	}}})

	if src, acct := farOf(t, db, ctx, "bank", "T-MORT-PAID"); src != "bank" || acct != "BRK1" {
		t.Errorf("paired mortgage far = (%q, %q), want (bank, BRK1)", src, acct)
	}
	if detailed, prov := verdictOf(t, db, ctx, "bank", "T-MORT-PAID"); detailed != internalTransfer ||
		prov != ProvenanceMatcher {
		t.Errorf("paired mortgage = (%q, %q), want the matcher's own-account move", detailed, prov)
	}
	if detailed, prov := verdictOf(t, db, ctx, "bank", "T-MORT-PIN"); detailed != canonical.SpendDetailedDebtRepayment || prov != ProvenanceManual {
		t.Errorf("pinned row = (%q, %q), want the pin's verdict", detailed, prov)
	}
}

// TestFarLinksAreRewrittenEveryPass pins the re-assert: the pass owns
// these links as it owns every other derived row, so a pairing that
// stops holding — a counter-leg that never loads, an override that
// unmatches it — leaves nothing behind.
func TestFarLinksAreRewrittenEveryPass(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{source: "bank", id: "T-OUT", account: "CASH1", kind: "withdrawal",
			occurredAt: day(10), amount: -2500, description: "TRANSFER TO SAVINGS"},
		txn{source: "other-bank", id: "T-IN", account: "CASH2", kind: "deposit",
			occurredAt: day(11), amount: 2500, description: "INCOMING TRANSFER"},
	)
	runPass(t, db, ctx, Options{})
	if _, acct := farOf(t, db, ctx, "bank", "T-OUT"); acct != "CASH2" {
		t.Fatalf("far account after the first pass = %q, want CASH2", acct)
	}

	// The same pass again is idempotent.
	runPass(t, db, ctx, Options{})
	if _, acct := farOf(t, db, ctx, "bank", "T-OUT"); acct != "CASH2" {
		t.Errorf("far account after a re-run = %q, want CASH2", acct)
	}

	// An override that refuses the pair takes the record with it.
	runPass(t, db, ctx, Options{TransferOverrides: []gold.TransferOverrideRule{{
		Verb: "unmatch",
		A: gold.TransferOverrideSelector{Source: "bank", Account: "CASH1",
			Day: day(10), Amount: -2500, Currency: "USD"},
	}}})
	if src, acct := farOf(t, db, ctx, "bank", "T-OUT"); src != "" || acct != "" {
		t.Errorf("far account survives an unmatch: (%q, %q)", src, acct)
	}
}

// TestOnlyTheSpendingFamilyLinksFarSides pins the family split. The far
// record belongs to the family that writes a row for every matched
// leg; the income one holds its own population plus pins, so a card
// bill's own leg — a pair's far side as often as not — has no row there
// to carry the fact. Neither overlay holds it: transfer_links does.
func TestOnlyTheSpendingFamilyLinksFarSides(t *testing.T) {
	if !spendingFamily.linksFar || incomeFamily.linksFar {
		t.Errorf("links far sides: spending %v, income %v; want spending alone",
			spendingFamily.linksFar, incomeFamily.linksFar)
	}
	db, ctx := openGold(t)
	var n int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM duckdb_columns()
         WHERE table_name IN ('spend_txn_enrichment', 'income_txn_enrichment')
           AND column_name LIKE 'far\_%' ESCAPE '\'`).Scan(&n); err != nil {
		t.Fatalf("inspect the overlays: %v", err)
	}
	if n != 0 {
		t.Errorf("the overlays carry %d far columns", n)
	}
}

// TestFarAccountChangesNoReport: the two families' reports are
// byte-identical with the links written and with them deleted.
// Recording where money went must not move a verdict.
func TestFarAccountChangesNoReport(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{source: "bank", id: "T-OUT", account: "CASH1", kind: "withdrawal",
			occurredAt: day(10), amount: -2500, description: "TRANSFER TO SAVINGS"},
		txn{source: "other-bank", id: "T-IN", account: "CASH2", kind: "deposit",
			occurredAt: day(11), amount: 2500, description: "INCOMING TRANSFER"},
		txn{source: "bank", id: "T-MORT", account: "CASH1", kind: "withdrawal",
			occurredAt: day(12), amount: -1800, description: "MORTGAGE PAYMENT"},
		txn{source: "bank", id: "T-SHOP", account: "CASH1", kind: "purchase",
			occurredAt: day(13), amount: -40, description: "CORNER MARKET"},
		txn{source: "bank", id: "T-WAGE", account: "CASH1", kind: "deposit",
			occurredAt: day(14), amount: 5000, description: "SALARY"},
	)
	runPass(t, db, ctx, Options{})

	before := reportShape(t, db, ctx)
	if _, err := db.ExecContext(ctx, `DELETE FROM transfer_links`); err != nil {
		t.Fatalf("drop the far sides: %v", err)
	}
	after := reportShape(t, db, ctx)
	if len(before) != len(after) {
		t.Fatalf("report rows = %d with the far record, %d without", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("row %d moved: %q vs %q", i, before[i], after[i])
		}
	}
}

// reportShape reads both families' bases as comparable text: the line,
// its resolved verdict and its amount. What a report SHOWS, at the
// finest grain at which a changed verdict could hide.
func reportShape(t *testing.T, db *sql.DB, ctx context.Context) []string {
	t.Helper()
	var out []string
	for _, q := range []struct{ name, sql string }{
		{"spending", `SELECT silver_source_id, transaction_external_id,
                             COALESCE(spend_detailed, '(null)'), COALESCE(provenance, ''),
                             CAST(net_amount AS VARCHAR)
                        FROM spending_lines_base(0, 9223372036854775807)`},
		{"income", `SELECT silver_source_id, transaction_external_id,
                           COALESCE(income_detailed, '(null)'), COALESCE(provenance, ''),
                           CAST(net_amount AS VARCHAR)
                      FROM income_lines_base(0, 9223372036854775807)`},
	} {
		rows, err := db.QueryContext(ctx, q.sql)
		if err != nil {
			t.Fatalf("read %s base: %v", q.name, err)
		}
		for rows.Next() {
			var src, id, detailed, prov, amount string
			if err := rows.Scan(&src, &id, &detailed, &prov, &amount); err != nil {
				rows.Close()
				t.Fatalf("scan %s base: %v", q.name, err)
			}
			out = append(out, fmt.Sprintf("%s %s/%s %s via=%s %s", q.name, src, id, detailed, prov, amount))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatalf("iterate %s base: %v", q.name, err)
		}
	}
	return out
}

// seedCounterAccount stamps a stated counter account into a seeded row's
// payload, which is where an adapter or collector puts the one its source
// named.
func seedCounterAccount(t *testing.T, db *sql.DB, ctx context.Context, source, id, counter string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
        UPDATE transactions SET payload = ?
         WHERE silver_source_id = ? AND transaction_external_id = ?`,
		`{"counter_account":"`+counter+`"}`, source, id); err != nil {
		t.Fatalf("stamp counter account on %s/%s: %v", id, source, err)
	}
}

// TestASourceStatedCounterAccountIsTheFarAccount pins the second road to
// the far account: the source naming it outright, for the movement whose
// other leg the product does not collect.
//
// This is the shape that has no pairing to find. A wire to an account of
// the holder's own that no feed reports transactions for leaves ONE row
// in gold, and the matcher — which needs two — can say nothing about it.
// The bank's own narrative can.
func TestASourceStatedCounterAccountIsTheFarAccount(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{source: "bank", id: "T-STATED", account: "CASH1", kind: "withdrawal",
			occurredAt: day(10), amount: -5000, description: "TRANSFER"},
	)
	// The far account is held by gold and contributes no transactions of
	// its own — the case the matcher cannot reach.
	seedCounterAccount(t, db, ctx, "bank", "T-STATED", "BRK1")
	runPass(t, db, ctx, Options{})

	if src, acct := farOf(t, db, ctx, "bank", "T-STATED"); src != "bank" || acct != "BRK1" {
		t.Errorf("stated far = (%q, %q), want (bank, BRK1)", src, acct)
	}
}

// TestAStatedCounterAccountResolvesInAnotherSourceOnlyWhenUnique pins the
// cross-source arm of the stated road. One collector can write several
// sources — a deposit ledger in one, the loan it pays in another — so an
// account the row's own source does not hold is looked for elsewhere, and
// taken only when exactly one account anywhere answers. The row's own
// source wins wherever it holds the id, and a row never names itself,
// even where another source happens to hold its id.
func TestAStatedCounterAccountResolvesInAnotherSourceOnlyWhenUnique(t *testing.T) {
	db, ctx := openGold(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path,
                                    high_watermark, first_loaded_at, last_loaded_at)
             VALUES ('third-bank', 'chase', '/tmp/third.db', -1, 0, 0);
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, first_seen_at, last_seen_at)
             VALUES ('other-bank', 'BRK1',    'brokerage', 'Same id elsewhere', 1, 1),
                    ('other-bank', 'SHARED9', 'cash',      'Twice held',        1, 1),
                    ('third-bank', 'SHARED9', 'cash',      'Twice held',        1, 1),
                    ('other-bank', 'CASH1',   'cash',      'Own id elsewhere',  1, 1);
    `); err != nil {
		t.Fatalf("seed accounts: %v", err)
	}
	seedTxns(t, db, ctx,
		txn{source: "bank", id: "T-ELSEWHERE", account: "CASH1", kind: "withdrawal",
			occurredAt: day(10), amount: -100, description: "TRANSFER"},
		txn{source: "bank", id: "T-OWN-SOURCE", account: "CASH1", kind: "withdrawal",
			occurredAt: day(11), amount: -200, description: "TRANSFER"},
		txn{source: "bank", id: "T-AMBIGUOUS", account: "CASH1", kind: "withdrawal",
			occurredAt: day(12), amount: -300, description: "TRANSFER"},
		txn{source: "bank", id: "T-ITSELF", account: "CASH1", kind: "withdrawal",
			occurredAt: day(13), amount: -400, description: "TRANSFER"},
	)
	seedCounterAccount(t, db, ctx, "bank", "T-ELSEWHERE", "CUST2")
	seedCounterAccount(t, db, ctx, "bank", "T-OWN-SOURCE", "BRK1")
	seedCounterAccount(t, db, ctx, "bank", "T-AMBIGUOUS", "SHARED9")
	seedCounterAccount(t, db, ctx, "bank", "T-ITSELF", "CASH1")
	runPass(t, db, ctx, Options{})

	for id, want := range map[string][2]string{
		"T-ELSEWHERE":  {"other-bank", "CUST2"},
		"T-OWN-SOURCE": {"bank", "BRK1"},
		"T-AMBIGUOUS":  {"", ""},
		"T-ITSELF":     {"", ""},
	} {
		if src, acct := farOf(t, db, ctx, "bank", id); src != want[0] || acct != want[1] {
			t.Errorf("%s: far = (%q, %q), want (%q, %q)", id, src, acct, want[0], want[1])
		}
	}
}

// TestAStatedCounterAccountGoldDoesNotHoldIsIgnored is the guard that
// keeps the road narrow. Most payments name a third party, and a third
// party's account is not the household's — so a stated account that
// resolves to nothing must leave the row exactly as it was rather than
// inventing a destination.
func TestAStatedCounterAccountGoldDoesNotHoldIsIgnored(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{source: "bank", id: "T-THIRD-PARTY", account: "CASH1", kind: "withdrawal",
			occurredAt: day(10), amount: -300, description: "PAYMENT"},
	)
	seedCounterAccount(t, db, ctx, "bank", "T-THIRD-PARTY", "AN-ACCOUNT-GOLD-DOES-NOT-HOLD")
	runPass(t, db, ctx, Options{})

	if src, acct := farOf(t, db, ctx, "bank", "T-THIRD-PARTY"); src != "" || acct != "" {
		t.Errorf("far = (%q, %q), want none: a third party is not a destination", src, acct)
	}
}

// TestAPairingOutranksAStatedCounterAccount pins the precedence between
// the two roads. A pairing is two collected legs agreeing; a statement is
// one row's narrative. Where both exist they agree anyway, and the
// pairing is the one that also names the SOURCE the far account lives on
// — which a bare account id in a narrative cannot.
func TestAPairingOutranksAStatedCounterAccount(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{source: "bank", id: "T-PAIRED-OUT", account: "CASH1", kind: "withdrawal",
			occurredAt: day(10), amount: -2500, description: "TRANSFER TO SAVINGS"},
		txn{source: "other-bank", id: "T-PAIRED-IN", account: "CASH2", kind: "deposit",
			occurredAt: day(11), amount: 2500, description: "INCOMING TRANSFER"},
	)
	// The narrative names a DIFFERENT own account from the one the
	// matcher pairs, so the test can tell which road was taken.
	seedCounterAccount(t, db, ctx, "bank", "T-PAIRED-OUT", "BRK1")
	runPass(t, db, ctx, Options{})

	if src, acct := farOf(t, db, ctx, "bank", "T-PAIRED-OUT"); src != "other-bank" || acct != "CASH2" {
		t.Errorf("far = (%q, %q), want the matcher's partner (other-bank, CASH2)", src, acct)
	}
}

// TestAStatedCounterAccountPlacesNoVerdict is the boundary of what this
// evidence answers. Where the money went and what the movement WAS are
// different questions; the tiers own the second one, and a far account
// arriving from the narrative must not quietly re-file a row as an
// own-account move.
func TestAStatedCounterAccountPlacesNoVerdict(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{source: "bank", id: "T-NO-VERDICT", account: "CASH1", kind: "withdrawal",
			occurredAt: day(10), amount: -5000, description: "TRANSFER"},
	)
	seedCounterAccount(t, db, ctx, "bank", "T-NO-VERDICT", "BRK1")
	runPass(t, db, ctx, Options{})

	var detailed sql.NullString
	var provenance string
	if err := db.QueryRowContext(ctx, `
        SELECT spend_detailed, provenance FROM spend_txn_enrichment
         WHERE silver_source_id = 'bank' AND transaction_external_id = 'T-NO-VERDICT'`).
		Scan(&detailed, &provenance); err != nil {
		t.Fatalf("read the overlay row: %v", err)
	}
	if detailed.Valid && detailed.String == string(canonical.SpendDetailedInternalTransfer) {
		t.Error("a stated counter account placed the matcher's verdict; it answers only where the money went")
	}
	if provenance == string(ProvenanceMatcher) {
		t.Errorf("provenance = %q, want the tier that actually placed the row", provenance)
	}
}
