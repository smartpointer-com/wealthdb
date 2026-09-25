package spending

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
)

// The reference road: the far account arriving as an IDENTITY rather than as
// an inference.
//
// The matcher's other road joins two legs by amount and day, which forces it
// to partition by native currency — converted amounts drift with each leg's
// FX, so the same movement would pair differently per output currency, and a
// report's display currency must not decide what counts as spending. The
// price of that partition is that a conversion between two of the holder's
// own accounts, whose two legs carry different figures in different
// currencies, can never pair on amount at all.
//
// A reference the source stamped on BOTH legs is not an inference from the
// figures, so it crosses the partition. These tests pin both halves of that:
// the pairing it now makes, and the pairings it must still refuse.

// stampMovementReference writes the reference a source stamped on a movement
// onto rows already seeded. It goes into the payload rather than a column
// because that is where it reaches gold: an adapter normalises its feed's own
// reference into one key, and the pass reads that key for every source.
func stampMovementReference(t *testing.T, db *sql.DB, ctx context.Context, source, id, ref string) {
	t.Helper()
	res, err := db.ExecContext(ctx, `
        UPDATE transactions
           SET payload = json_object('bank_ref', ?)
         WHERE silver_source_id = ? AND transaction_external_id = ?`, ref, source, id)
	if err != nil {
		t.Fatalf("stamp reference on %s/%s: %v", source, id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n != 1 {
		t.Fatalf("stamping %s/%s touched %d row(s), want 1", source, id, n)
	}
}

// A conversion between two own accounts pairs on the number the bank stamped
// on both legs, and each leg then names the other's account — which is the
// whole point downstream, because only the far account tells the cash flow
// statement whether the money stayed inside the household's pool.
func TestPassPairsACrossCurrencyMoveOnItsSourcesOwnReference(t *testing.T) {
	db, ctx := openGold(t)
	seedTxnsIn(t, db, ctx, "USD",
		txn{source: "bank", id: "T-FX-OUT", account: "CASH1", kind: "withdrawal",
			occurredAt: day(10), amount: -1000, description: "FX SELL"},
	)
	seedTxnsIn(t, db, ctx, "CHF",
		txn{source: "bank", id: "T-FX-IN", account: "BRK1", kind: "deposit",
			occurredAt: day(10), amount: 987.65, description: "FX BUY"},
	)
	stampMovementReference(t, db, ctx, "bank", "T-FX-OUT", "TXNO-1")
	stampMovementReference(t, db, ctx, "bank", "T-FX-IN", "TXNO-1")

	res := runPass(t, db, ctx, Options{})
	if res.MatcherRows != 2 {
		t.Errorf("MatcherRows = %d, want 2 (both legs of the conversion)", res.MatcherRows)
	}
	if res.Cashflow.ReferencePairs != 1 {
		t.Errorf("Cashflow.ReferencePairs = %d, want 1 — the road has to be countable "+
			"or a build that stops carrying traffic on it looks like a quiet month", res.Cashflow.ReferencePairs)
	}
	for _, c := range []struct{ id, farAccount string }{
		{"T-FX-OUT", "BRK1"}, {"T-FX-IN", "CASH1"},
	} {
		if detailed, prov := verdictOf(t, db, ctx, "bank", c.id); detailed != internalTransfer ||
			prov != ProvenanceMatcher {
			t.Errorf("%s = (%q, %q), want the matcher's own-account verdict", c.id, detailed, prov)
		}
		if src, acct, _ := farOf(t, db, ctx, "bank", c.id); src != "bank" || acct != c.farAccount {
			t.Errorf("%s far = (%q, %q), want (bank, %s)", c.id, src, acct, c.farAccount)
		}
	}
	// The outgoing leg is what the report would otherwise have charted, and
	// it is the reason the pairing matters rather than a detail of it.
	if _, charted := spendingBaseIDs(t, db, ctx)["T-FX-OUT"]; charted {
		t.Error("the outgoing leg of a paired conversion is still in the spending base")
	}
}

// The census that makes a reference safe is taken over the WHOLE of
// `transactions`, not over the matcher's pool. A reference the bank also
// stamped on a booking the pool admits no leg from — a charge beside the
// payment it belongs to, an FX leg, any unsigned kind — names something
// larger than the two legs in hand, and the pool's own census is blind to it
// by construction. Refusing costs a match; pairing on it costs a real
// spending line, because a false pair withdraws both legs.
func TestAReferenceSharedWithARowOutsideThePoolPairsNothing(t *testing.T) {
	db, ctx := openGold(t)
	seedTxnsIn(t, db, ctx, "USD",
		txn{source: "bank", id: "T-SHARED-OUT", account: "CASH1", kind: "withdrawal",
			occurredAt: day(10), amount: -1000, description: "PAYMENT"},
		// A `fee` is not a kind the matcher pool is made from, so no leg of
		// it ever reaches the matcher — only this query sees it.
		txn{source: "bank", id: "T-SHARED-FEE", account: "CASH1", kind: "fee",
			occurredAt: day(10), amount: -5, description: "CHARGE"},
	)
	seedTxnsIn(t, db, ctx, "CHF",
		txn{source: "bank", id: "T-SHARED-IN", account: "BRK1", kind: "deposit",
			occurredAt: day(10), amount: 987.65, description: "CREDIT"},
	)
	for _, id := range []string{"T-SHARED-OUT", "T-SHARED-FEE", "T-SHARED-IN"} {
		stampMovementReference(t, db, ctx, "bank", id, "TXNO-2")
	}

	refs, ambiguous, err := loadMovementReferences(ctx, db)
	if err != nil {
		t.Fatalf("loadMovementReferences: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("a reference on three rows was offered anyway: %v", refs)
	}
	if ambiguous != 1 {
		t.Errorf("ambiguous references = %d, want 1 — a refusal nobody counts is a road nobody can see silting up", ambiguous)
	}
	res := runPass(t, db, ctx, Options{})
	if res.MatcherRows != 0 {
		t.Errorf("MatcherRows = %d, want 0", res.MatcherRows)
	}
	if res.Cashflow.AmbiguousReferences != 1 {
		t.Errorf("Cashflow.AmbiguousReferences = %d, want 1", res.Cashflow.AmbiguousReferences)
	}
}

// A reference reaches the matcher only where the source minted exactly two
// rows under it, and only ever within that source. The query is where both
// rules live, so it is where they are pinned.
func TestMovementReferencesAreOfferedOnlyWhereTheyNameOneMovement(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{source: "bank", id: "T-PAIR-OUT", account: "CASH1", kind: "withdrawal",
			occurredAt: day(10), amount: -400},
		txn{source: "bank", id: "T-PAIR-IN", account: "BRK1", kind: "deposit",
			occurredAt: day(10), amount: 400},
		txn{source: "bank", id: "T-LONE", account: "CASH1", kind: "withdrawal",
			occurredAt: day(11), amount: -50},
		// Two sources minting the same string is a collision, not a
		// movement: each side has one row under it, so neither is offered.
		txn{source: "bank", id: "T-COLLIDE", account: "CASH1", kind: "withdrawal",
			occurredAt: day(12), amount: -75},
		txn{source: "other-bank", id: "T-COLLIDE-TOO", account: "CASH2", kind: "deposit",
			occurredAt: day(12), amount: 75},
	)
	stampMovementReference(t, db, ctx, "bank", "T-PAIR-OUT", "TXNO-3")
	stampMovementReference(t, db, ctx, "bank", "T-PAIR-IN", "TXNO-3")
	stampMovementReference(t, db, ctx, "bank", "T-LONE", "TXNO-4")
	stampMovementReference(t, db, ctx, "bank", "T-COLLIDE", "TXNO-5")
	stampMovementReference(t, db, ctx, "other-bank", "T-COLLIDE-TOO", "TXNO-5")

	refs, ambiguous, err := loadMovementReferences(ctx, db)
	if err != nil {
		t.Fatalf("loadMovementReferences: %v", err)
	}
	// None of the refusals here is a reference naming MORE than one
	// movement: the lone leg and each half of the cross-source collision
	// carry one row apiece, which is the ordinary shape of a row whose
	// other half belongs to a third party.
	if ambiguous != 0 {
		t.Errorf("ambiguous references = %d, want 0 — a reference on one row refuses nothing", ambiguous)
	}
	want := map[txKey]string{
		{source: "bank", txID: "T-PAIR-OUT"}: "TXNO-3",
		{source: "bank", txID: "T-PAIR-IN"}:  "TXNO-3",
	}
	if len(refs) != len(want) {
		t.Fatalf("offered %v, want %v", refs, want)
	}
	for k, v := range want {
		if refs[k] != v {
			t.Errorf("reference for %s/%s = %q, want %q", k.source, k.txID, refs[k], v)
		}
	}
}

// The pool is the one place the legs are built, so the reference travels with
// them and the pass and the audit surface cannot disagree about a pairing.
func TestTheMatcherPoolCarriesTheReferenceToEveryCaller(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{source: "bank", id: "T-REF-OUT", account: "CASH1", kind: "withdrawal",
			occurredAt: day(10), amount: -400},
		txn{source: "bank", id: "T-REF-IN", account: "BRK1", kind: "deposit",
			occurredAt: day(10), amount: 400},
	)
	stampMovementReference(t, db, ctx, "bank", "T-REF-OUT", "TXNO-6")
	stampMovementReference(t, db, ctx, "bank", "T-REF-IN", "TXNO-6")

	legs, _, _, err := loadMatcherPool(ctx, db, nil)
	if err != nil {
		t.Fatalf("loadMatcherPool: %v", err)
	}
	if len(legs) != 2 {
		t.Fatalf("pooled %d leg(s), want 2", len(legs))
	}
	for _, l := range legs {
		if l.Ref != "TXNO-6" {
			t.Errorf("leg %s reached the matcher with Ref %q, want TXNO-6", l.ID, l.Ref)
		}
	}
	pairs, _, err := MatchedPairs(ctx, db, 5, 0.5, nil, nil)
	if err != nil {
		t.Fatalf("MatchedPairs: %v", err)
	}
	if len(pairs) != 1 {
		t.Errorf("the audit surface saw %d pair(s), want 1", len(pairs))
	}
}

// The returns engine offers no reference and forbids same-source pairing, so
// the phase has nothing to act on there whatever a leg happens to carry.
func TestTheReturnsCallersSettingsLeaveTheReferencePhaseInert(t *testing.T) {
	legs := []gold.TransferLeg{
		{Group: "bank", Owner: "CASH1", ID: "out", Day: 10, Ccy: "USD", Amt: -1000, Ref: "TXNO-7"},
		{Group: "bank", Owner: "BRK1", ID: "in", Day: 10, Ccy: "CHF", Amt: 987.65, Ref: "TXNO-7"},
	}
	got := gold.MatchTransferLegs(legs, gold.TransferMatchOpts{
		WindowDays: 5, TolerancePct: 0.5, CrossGroupOnly: true,
	})
	if len(got) != 0 {
		t.Errorf("the returns caller's options paired a same-source reference twin: %+v", got)
	}
}
