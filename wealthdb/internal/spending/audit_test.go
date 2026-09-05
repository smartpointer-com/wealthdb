package spending

import (
	"testing"
)

// TestMatchedPairsListsBothLegs is the audit surface's contract: the
// pairs come back whole. A listing that showed only the outgoing leg
// would be useless for the question it exists to answer — whether the
// thing the matcher removed from spending really was an own-account
// move — because that question is decided by what the money reached.
func TestMatchedPairsListsBothLegs(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T-OUT", "CASH1", "withdrawal", day(20), -400, "Autopay", "", ""},
		txn{"bank", "T-IN", "CARD1", "card_payment", day(20), 400, "", "Card Payment", ""},
		// A one-legged withdrawal: nothing in gold funds it.
		txn{"bank", "T-LONE", "CASH1", "withdrawal", day(30), -9000, "Wire Out", "", ""},
	)

	pairs, unmatched, err := MatchedPairs(ctx, db, 5, 0.5)
	if err != nil {
		t.Fatalf("MatchedPairs: %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("pairs = %d, want 1", len(pairs))
	}
	p := pairs[0]
	if p.Debit.TxID != "T-OUT" || p.Credit.TxID != "T-IN" {
		t.Errorf("pair = (%s, %s), want (T-OUT, T-IN)", p.Debit.TxID, p.Credit.TxID)
	}
	if p.Debit.Amount != -400 || p.Credit.Amount != 400 {
		t.Errorf("amounts = (%g, %g)", p.Debit.Amount, p.Credit.Amount)
	}
	if p.Debit.Signature != "AUTOPAY" {
		t.Errorf("debit signature = %q, want AUTOPAY", p.Debit.Signature)
	}
	if p.Debit.Account != "CASH1" || p.Credit.Account != "CARD1" {
		t.Errorf("accounts = (%s, %s)", p.Debit.Account, p.Credit.Account)
	}

	if len(unmatched) != 1 || unmatched[0].TxID != "T-LONE" {
		t.Fatalf("unmatched = %+v, want just T-LONE", unmatched)
	}
}

// TestMatchedPairsSortsUnmatchedByMagnitude pins the ordering the run
// report relies on: it prints the head of this slice, and the legs
// worth a human's attention are the big ones.
func TestMatchedPairsSortsUnmatchedByMagnitude(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T-SMALL", "CASH1", "withdrawal", day(10), -25, "Wire Out", "", ""},
		txn{"bank", "T-BIG", "CASH1", "withdrawal", day(11), -8000, "Wire Out", "", ""},
		txn{"bank", "T-MID", "CASH1", "deposit", day(40), 300, "", "Inbound", ""},
	)

	_, unmatched, err := MatchedPairs(ctx, db, 5, 0.5)
	if err != nil {
		t.Fatalf("MatchedPairs: %v", err)
	}
	want := []string{"T-BIG", "T-MID", "T-SMALL"}
	if len(unmatched) != len(want) {
		t.Fatalf("unmatched = %d rows, want %d", len(unmatched), len(want))
	}
	for i, id := range want {
		if unmatched[i].TxID != id {
			t.Errorf("unmatched[%d] = %s, want %s", i, unmatched[i].TxID, id)
		}
	}
}

// TestMatchedPairsCannotPairCrossCurrency pins the structural
// limitation the run report has to surface rather than fix: the shared
// core partitions by native currency, so two legs that are obviously
// one movement to a human stay unpaired.
func TestMatchedPairsCannotPairCrossCurrency(t *testing.T) {
	db, ctx := openGold(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount,
                                  counterparty, description, provider_category)
             VALUES ('bank', 'T-USD-OUT', ?, 'CASH1', 'withdrawal', 'USD', -1000, 'Wire Out', NULL, NULL),
                    ('bank', 'T-CHF-IN',  ?, 'CASH1', 'deposit',    'CHF',   950, NULL, 'Inbound', NULL)`,
		day(10), day(11)); err != nil {
		t.Fatalf("seed cross-currency legs: %v", err)
	}

	pairs, unmatched, err := MatchedPairs(ctx, db, 5, 0.5)
	if err != nil {
		t.Fatalf("MatchedPairs: %v", err)
	}
	if len(pairs) != 0 {
		t.Errorf("pairs = %d, want 0 — the core partitions by native currency", len(pairs))
	}
	if len(unmatched) != 2 {
		t.Fatalf("unmatched = %d, want both legs", len(unmatched))
	}
	if unmatched[0].Currency == unmatched[1].Currency {
		t.Error("the two unmatched legs should carry different currencies")
	}
}

// TestMatchedPairsFlagsPopulationLegs pins what lets a listing tell a
// pair that removed something from spending from one that did not.
// The pool is every account and the income-side kinds, so a card
// payment funded from a cash account has ONE leg in the population,
// and a move between two brokerage accounts has none. With
// InPopulation never set in MatchedPairs, both the flags and
// RemovedFromSpending fail here, so the test is not vacuous.
func TestMatchedPairsFlagsPopulationLegs(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		// A card paid off from the cash account: the withdrawal is a
		// spending row until the matcher says otherwise, the card
		// payment never was.
		txn{"bank", "T-OUT", "CASH1", "withdrawal", day(20), -400, "Autopay", "", ""},
		txn{"bank", "T-IN", "CARD1", "card_payment", day(20), 400, "", "Card Payment", ""},
		// Brokerage to brokerage, across sources: a correct pair that no
		// spending report ever charted either end of.
		txn{"bank", "T-BRK-OUT", "BRK1", "transfer_out", day(40), -1000, "To Invested", "", ""},
		txn{"other-bank", "T-BRK-IN", "BRK2", "transfer_in", day(40), 1000, "", "From Brokerage", ""},
		// A one-legged cash withdrawal, so the flag is seen on an
		// unmatched leg too.
		txn{"bank", "T-LONE", "CASH1", "withdrawal", day(60), -9000, "Wire Out", "", ""},
	)

	pairs, unmatched, err := MatchedPairs(ctx, db, 5, 0.5)
	if err != nil {
		t.Fatalf("MatchedPairs: %v", err)
	}
	if len(pairs) != 2 {
		t.Fatalf("pairs = %d, want 2", len(pairs))
	}
	byDebit := map[string]Pair{}
	for _, p := range pairs {
		byDebit[p.Debit.TxID] = p
	}

	cash, ok := byDebit["T-OUT"]
	if !ok {
		t.Fatalf("no pair led by T-OUT in %+v", pairs)
	}
	if !cash.Debit.InPopulation || cash.Credit.InPopulation {
		t.Errorf("cash-to-card flags = (%v, %v), want (true, false): the withdrawal was a spending row, the card payment never was",
			cash.Debit.InPopulation, cash.Credit.InPopulation)
	}
	if !cash.RemovedFromSpending() {
		t.Error("cash-to-card pair must count as removed from spending")
	}

	brk, ok := byDebit["T-BRK-OUT"]
	if !ok {
		t.Fatalf("no pair led by T-BRK-OUT in %+v", pairs)
	}
	if brk.Debit.InPopulation || brk.Credit.InPopulation {
		t.Errorf("brokerage-to-brokerage flags = (%v, %v), want (false, false)",
			brk.Debit.InPopulation, brk.Credit.InPopulation)
	}
	if brk.RemovedFromSpending() {
		t.Error("a pair with neither leg in the population removed nothing from spending")
	}

	if len(unmatched) != 1 || unmatched[0].TxID != "T-LONE" || !unmatched[0].InPopulation {
		t.Errorf("unmatched = %+v, want just T-LONE, flagged in the population", unmatched)
	}
}
