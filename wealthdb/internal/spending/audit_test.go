package spending

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
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

	runPass(t, db, ctx, Options{})
	pairs, unmatched, err := MatchedPairs(ctx, db)
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

	runPass(t, db, ctx, Options{})
	_, unmatched, err := MatchedPairs(ctx, db)
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

// TestMatchedPairsCannotPairCrossCurrencyUnasserted pins what the run
// report still has to surface rather than fix. The amount pass
// partitions by native currency, so two legs that are obviously one
// movement to a human stay unpaired unless the SOURCE says they are
// one, and these legs carry neither a shared reference nor a stated
// counter leg, so neither road reaches them.
func TestMatchedPairsCannotPairCrossCurrencyUnasserted(t *testing.T) {
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

	runPass(t, db, ctx, Options{})
	pairs, unmatched, err := MatchedPairs(ctx, db)
	if err != nil {
		t.Fatalf("MatchedPairs: %v", err)
	}
	if len(pairs) != 0 {
		t.Errorf("pairs = %d, want 0 — the amount pass partitions by native currency "+
			"and these legs carry neither a reference nor a stated counter", len(pairs))
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
		txn{"other-bank", "T-BRK-IN", "CUST2", "transfer_in", day(40), 1000, "", "From Brokerage", ""},
		// A one-legged cash withdrawal, so the flag is seen on an
		// unmatched leg too.
		txn{"bank", "T-LONE", "CASH1", "withdrawal", day(60), -9000, "Wire Out", "", ""},
	)

	runPass(t, db, ctx, Options{})
	pairs, unmatched, err := MatchedPairs(ctx, db)
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

// TestSortPairsListsAssertedPhasesFirst pins the listing order the pair cap
// relies on: override, reference and stated-counter pairs lead, in that
// order and whatever their day, and the named and amount pairs follow
// together in day order. A named pair is banded on amount like an amount
// pair, so it must not outrank an asserted one.
func TestSortPairsListsAssertedPhasesFirst(t *testing.T) {
	type spec struct {
		id  string
		day int64
		by  gold.TransferMatchPhase
	}
	// Shuffled on purpose: neither the input order nor the day order is
	// the listing order.
	specs := []spec{
		{"AMOUNT-LATE", 9, gold.MatchedByAmount},
		{"STATED", 1, gold.MatchedByStatedCounter},
		{"NAMED-EARLY", 2, gold.MatchedByName},
		{"OVERRIDE", 8, gold.MatchedByOverride},
		{"AMOUNT-EARLY", 3, gold.MatchedByAmount},
		{"REFERENCE", 5, gold.MatchedByReference},
		{"NAMED-LATE", 7, gold.MatchedByName},
	}
	got := make([]Pair, 0, len(specs))
	for _, sp := range specs {
		got = append(got, Pair{
			Debit:  Leg{Source: "bank", TxID: sp.id + "-D", Account: "CASH1", Day: sp.day},
			Credit: Leg{Source: "bank", TxID: sp.id + "-C", Account: "CASH2", Day: sp.day},
			By:     sp.by,
		})
	}

	sortPairs(got)
	want := []string{
		"OVERRIDE-D", "REFERENCE-D", "STATED-D",
		"NAMED-EARLY-D", "AMOUNT-EARLY-D", "NAMED-LATE-D", "AMOUNT-LATE-D",
	}
	if len(got) != len(want) {
		t.Fatalf("pairs = %d, want %d", len(got), len(want))
	}
	for i, p := range got {
		if p.Debit.TxID != want[i] {
			t.Errorf("pair %d = %s (%s), want %s", i, p.Debit.TxID, p.By, want[i])
		}
	}
}

// linkRows reads transfer_links as comparable lines, pairs and
// one-legged rows alike.
func linkRows(t *testing.T, db *sql.DB, ctx context.Context) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
        SELECT silver_source_id, transaction_external_id,
               COALESCE(far_silver_source_id, '(null)'),
               COALESCE(far_account_external_id, '(null)'),
               COALESCE(far_transaction_external_id, '(null)'),
               COALESCE(far_class, '(null)'), evidence, measure
          FROM transfer_links
         ORDER BY silver_source_id, transaction_external_id`)
	if err != nil {
		t.Fatalf("read transfer links: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var src, id, farSrc, farAcct, farTx, farClass, evidence, measure string
		if err := rows.Scan(&src, &id, &farSrc, &farAcct, &farTx, &farClass, &evidence, &measure); err != nil {
			t.Fatalf("scan transfer links: %v", err)
		}
		out = append(out, fmt.Sprintf("%s/%s far=%s/%s tx=%s class=%s by=%s measure=%s",
			src, id, farSrc, farAcct, farTx, farClass, evidence, measure))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate transfer links: %v", err)
	}
	return out
}

// TestTransferLinksAreThePassesPairs holds the table to the matcher it
// stores: its pair rows are the matcher's pairs over the same pool,
// flattened to one row per leg, and every row holds what the spending
// overlay's far columns hold.
func TestTransferLinksAreThePassesPairs(t *testing.T) {
	db, ctx := openGold(t)
	opts := seedFrozenIncomeFixture(t, db, ctx)
	runPass(t, db, ctx, opts)

	legs, _, _, err := loadMatcherPool(ctx, db, nil)
	if err != nil {
		t.Fatalf("loadMatcherPool: %v", err)
	}
	rates, err := gold.LoadDailyRates(ctx, db)
	if err != nil {
		t.Fatalf("LoadDailyRates: %v", err)
	}
	var want []string
	for _, p := range matchTransferPairs(legs, 5, 0.5, gold.TransferOverrides{}, rates) {
		for _, leg := range [][2]gold.TransferLeg{{p.Debit, p.Credit}, {p.Credit, p.Debit}} {
			want = append(want, fmt.Sprintf("%s/%s far=%s/%s tx=%s class=(null) by=%s measure=cash",
				leg[0].Group, leg[0].ID, leg[1].Group, leg[1].Owner, leg[1].ID, phaseEvidence[p.By]))
		}
	}
	sort.Strings(want)
	var got []string
	for _, l := range linkRows(t, db, ctx) {
		if !strings.Contains(l, " tx=(null) ") {
			got = append(got, l)
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("pair links:\n%s\nwant the matcher's pairs:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Every link is an overlay row's far account or class, and back.
	overlay := scanPairs(t, db, `
        SELECT silver_source_id || '/' || transaction_external_id,
               COALESCE(far_silver_source_id, '(null)') || '/' ||
               COALESCE(far_account_external_id, '(null)') || ' ' || COALESCE(far_class, '(null)')
          FROM spend_txn_enrichment
         WHERE far_account_external_id IS NOT NULL OR far_class IS NOT NULL`)
	links := scanPairs(t, db, `
        SELECT silver_source_id || '/' || transaction_external_id,
               COALESCE(far_silver_source_id, '(null)') || '/' ||
               COALESCE(far_account_external_id, '(null)') || ' ' || COALESCE(far_class, '(null)')
          FROM transfer_links`)
	if fmt.Sprint(links) != fmt.Sprint(overlay) {
		t.Errorf("links hold %v, the overlay %v", links, overlay)
	}
}

// TestACategorizeRunMatchesOnce: a real categorize run is the pass and
// then the audit listing, and only the pass runs the matcher.
func TestACategorizeRunMatchesOnce(t *testing.T) {
	db, ctx := openGold(t)
	opts := seedFrozenFixture(t, db, ctx)
	runs := 0
	t.Cleanup(func() { matchTransferLegs = gold.MatchTransferLegs })
	matchTransferLegs = func(legs []gold.TransferLeg, o gold.TransferMatchOpts) []gold.TransferMatchPair {
		runs++
		return gold.MatchTransferLegs(legs, o)
	}

	runPass(t, db, ctx, opts)
	pairs, _, err := MatchedPairs(ctx, db)
	if err != nil {
		t.Fatalf("MatchedPairs: %v", err)
	}
	if runs != 1 {
		t.Errorf("the matcher ran %d time(s), want 1", runs)
	}
	if len(pairs) != 3 {
		t.Errorf("the listing holds %d pair(s), want the pass's 3", len(pairs))
	}
}

// TestMatchedPairsWithoutTheTable: a gold no build with transfer_links
// has migrated reads as one with no stored pairs. The listing goes on,
// every pool leg unpaired, and does not fail.
func TestMatchedPairsWithoutTheTable(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T-OUT", "CASH1", "withdrawal", day(20), -400, "Autopay", "", ""},
		txn{"bank", "T-IN", "CARD1", "card_payment", day(20), 400, "", "Card Payment", ""},
	)
	runPass(t, db, ctx, Options{})
	if _, err := db.ExecContext(ctx, `DROP TABLE transfer_links`); err != nil {
		t.Fatalf("drop the table: %v", err)
	}
	if stored, err := TransferLinksStored(ctx, db); err != nil || stored {
		t.Fatalf("TransferLinksStored = (%v, %v), want (false, nil)", stored, err)
	}
	pairs, unmatched, err := MatchedPairs(ctx, db)
	if err != nil {
		t.Fatalf("MatchedPairs: %v", err)
	}
	if len(pairs) != 0 || len(unmatched) != 2 {
		t.Errorf("pairs = %d, unmatched = %d, want 0 and both legs", len(pairs), len(unmatched))
	}
}
