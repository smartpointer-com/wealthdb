package spending

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// The UBS card bill, end to end.
//
// Gold files a card bill paid from a cash account as `card_spend` while
// the card is not itemised, and the matcher replaces that verdict once
// the card's own settlement leg arrives. Collecting UBS cards is what
// makes the second half true for this source, and these tests pin it
// against the descriptors UBS actually prints — the generic mechanism is
// covered by TestPassUnpairedCardPaymentIsCardSpend.
//
// Every value is synthetic; the descriptor SHAPES are what matter.

// openUBSGold is a gold with one UBS relationship: a CHF cash account,
// a CHF card and a EUR card. The third is not decoration — a
// relationship really can hold cards in several currencies, and the
// matcher cannot pair across two.
func openUBSGold(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	db, err := gold.OpenFresh(filepath.Join(t.TempDir(), "gold.db"))
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path,
                                    high_watermark, first_loaded_at, last_loaded_at)
             VALUES ('ubs', 'ubs', '/tmp/ubs.db', -1, 0, 0);

        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, first_seen_at, last_seen_at)
             VALUES ('ubs', 'CH-CASH', 'cash', 'Everyday',    1, 1),
                    ('ubs', 'CARD-CHF', 'card', 'Card CHF',   1, 1),
                    ('ubs', 'CARD-EUR', 'card', 'Card EUR',   1, 1);
    `); err != nil {
		t.Fatalf("seed dimensions: %v", err)
	}
	return db, ctx
}

// The descriptors a UBS deposit export prints on a card bill, in the
// two shapes the built-in rule's issuer table carries.
const (
	ubsBillPaymentOrder = "UBS SWITZERLAND AG;C/O UBS CARD CENTER"
	ubsBillDirectDebit  = "DIRECT DEBIT; UBS CARD CENTER; CREDIT CARD STATEMENT 0000"
)

// TestUBSCardBillNetsOutOnceTheCardIsCollected is the phase's own
// acceptance test: with the card in gold, the bill stops being a
// `card_spend` placeholder and the card's real purchases carry the
// spending instead.
func TestUBSCardBillNetsOutOnceTheCardIsCollected(t *testing.T) {
	db, ctx := openUBSGold(t)
	// Before the card is collected: the bill is the only trace of that
	// spending, so it stays in the base as a placeholder.
	seedTxns(t, db, ctx,
		txn{"ubs", "T-BILL", "CH-CASH", "withdrawal", day(20), -400,
			"UBS SWITZERLAND AG", ubsBillPaymentOrder, "e-banking payment order"},
	)
	runPass(t, db, ctx, Options{})
	if detailed, provenance := verdictOf(t, db, ctx, "ubs", "T-BILL"); detailed !=
		canonical.SpendDetailedCardSpend || provenance != ProvenanceRule {
		t.Fatalf("uncollected bill = (%q, %q), want card_spend via rule",
			detailed, provenance)
	}

	// The card is collected: its settlement leg and its purchases land.
	seedTxns(t, db, ctx,
		txn{"ubs", "T-CARD-SETTLE", "CARD-CHF", "card_payment", day(20), 400,
			"DIRECT DEBIT", "DIRECT DEBIT", ""},
		txn{"ubs", "T-CARD-BUY-1", "CARD-CHF", "purchase", day(12), -250,
			"EXAMPLE GROCER EXAMPLETOWN CHE", "EXAMPLE GROCER EXAMPLETOWN CHE",
			"Grocery stores"},
		txn{"ubs", "T-CARD-BUY-2", "CARD-CHF", "purchase", day(15), -150,
			"EXAMPLE CAFE EXAMPLETOWN CHE", "EXAMPLE CAFE EXAMPLETOWN CHE",
			"Restaurants"},
	)
	runPass(t, db, ctx, Options{})

	// The bill and the card's leg are now two halves of one movement.
	for _, id := range []string{"T-BILL", "T-CARD-SETTLE"} {
		detailed, provenance := verdictOf(t, db, ctx, "ubs", id)
		if detailed != canonical.SpendDetailedInternalTransfer ||
			provenance != ProvenanceMatcher {
			t.Errorf("%s = (%q, %q), want internal_transfer via matcher",
				id, detailed, provenance)
		}
	}

	// And the spending is the purchases, in real categories, placed for
	// free by the provider tier from the MCC description.
	base := spendingBaseIDs(t, db, ctx)
	if _, ok := base["T-BILL"]; ok {
		t.Error("the bill is still in the spending base; its card is itemised now")
	}
	for id, want := range map[string]string{
		"T-CARD-BUY-1": "FOOD_AND_DRINK_GROCERIES",
		"T-CARD-BUY-2": "FOOD_AND_DRINK_RESTAURANT",
	} {
		if _, ok := base[id]; !ok {
			t.Errorf("%s is not in the spending base", id)
		}
		detailed, provenance := verdictOf(t, db, ctx, "ubs", id)
		if detailed != want || provenance != ProvenanceProvider {
			t.Errorf("%s = (%q, %q), want (%q, %q)", id, detailed, provenance,
				want, ProvenanceProvider)
		}
	}
	if len(base) != 2 {
		t.Errorf("spending base = %d rows, want the two purchases", len(base))
	}
}

// TestUBSDirectDebitBillAlsoNetsOut covers the other descriptor shape:
// the LSV rail prints the mandate notice before the creditor, so the
// bill's counterparty is the bank's own notice and the creditor sits in
// the description.
func TestUBSDirectDebitBillAlsoNetsOut(t *testing.T) {
	db, ctx := openUBSGold(t)
	seedTxns(t, db, ctx,
		txn{"ubs", "T-BILL", "CH-CASH", "withdrawal", day(30), -700,
			"CRD1W OBJECTION TO UBS", ubsBillDirectDebit, "DIRECT DEBIT"},
		txn{"ubs", "T-CARD-SETTLE", "CARD-CHF", "card_payment", day(30), 700,
			"DIRECT DEBIT", "DIRECT DEBIT", ""},
	)
	runPass(t, db, ctx, Options{})
	for _, id := range []string{"T-BILL", "T-CARD-SETTLE"} {
		detailed, provenance := verdictOf(t, db, ctx, "ubs", id)
		if detailed != canonical.SpendDetailedInternalTransfer ||
			provenance != ProvenanceMatcher {
			t.Errorf("%s = (%q, %q), want internal_transfer via matcher",
				id, detailed, provenance)
		}
	}
}

// TestUBSCrossCurrencyCardBillStaysCardSpend records a real limitation
// rather than a defect, and is the reason the placeholder does not
// retire for every collected card.
//
// The matcher's amount pass partitions candidates by NATIVE currency and
// cannot pair across two — converting them would make the same movement
// pair differently per report currency. Its reference pass can, but only
// where one source stamped one reference on both legs, and a card ledger
// mints its own ids: the bank-side payment order and the card's record of
// being settled share no number. So a card billed in one currency and
// settled from an account in another leaves both legs one-legged however
// well the card is projected, and the built-in rule keeps placing
// `card_spend` on the bill.
//
// That is the honest verdict for the row: the purchases on that card
// ARE itemised, so the bill double-counts them — but a matcher that
// paired across currencies would be wrong in a worse way. The
// correction surface for it is a pin or a config rule
// (docs/SPENDING.md §3), and `wealthdb categorize` surfaces such rows
// as cross-currency near-pairs.
func TestUBSCrossCurrencyCardBillStaysCardSpend(t *testing.T) {
	db, ctx := openUBSGold(t)
	seedTxnsIn(t, db, ctx, "CHF",
		txn{"ubs", "T-BILL-CHF", "CH-CASH", "withdrawal", day(50), -300,
			"UBS SWITZERLAND AG", ubsBillPaymentOrder, "e-banking payment order"},
	)
	seedTxnsIn(t, db, ctx, "EUR",
		txn{"ubs", "T-CARD-SETTLE-EUR", "CARD-EUR", "card_payment", day(50), 300,
			"DIRECT DEBIT", "DIRECT DEBIT", ""},
	)
	runPass(t, db, ctx, Options{})

	detailed, provenance := verdictOf(t, db, ctx, "ubs", "T-BILL-CHF")
	if detailed != canonical.SpendDetailedCardSpend || provenance != ProvenanceRule {
		t.Errorf("cross-currency bill = (%q, %q), want card_spend via rule — "+
			"no reference joins the two feeds and the amount pass cannot cross currencies",
			detailed, provenance)
	}
	// The card's own leg is not spending either way: `card_payment` is
	// outside the enrichment population's kinds.
	if _, ok := spendingBaseIDs(t, db, ctx)["T-CARD-SETTLE-EUR"]; ok {
		t.Error("a card payment leg must never be a spending line")
	}
}

// TestUBSCardCategoryComesFromTheProviderTier pins the free half of the
// win: an MCC description places the row deterministically, so a
// collected card's purchases need no paid model verdict to be charted.
func TestUBSCardCategoryComesFromTheProviderTier(t *testing.T) {
	db, ctx := openUBSGold(t)
	seedTxns(t, db, ctx,
		txn{"ubs", "T-1", "CARD-CHF", "purchase", day(10), -20,
			"EXAMPLE PHARMACY EXAMPLETOWN CHE", "EXAMPLE PHARMACY EXAMPLETOWN CHE",
			"Pharmacies"},
		// The bank's own catch-all for a card row that moved money is
		// deliberately untranslated: it names no line of business.
		txn{"ubs", "T-2", "CARD-CHF", "purchase", day(11), -50,
			"MOBILE PAYMENT TO A PERSON", "MOBILE PAYMENT TO A PERSON",
			"Banks - merchandise and services"},
		// A description the vocabulary has never seen — the drift case.
		txn{"ubs", "T-3", "CARD-CHF", "purchase", day(12), -30,
			"EXAMPLE GROOMERS EXAMPLETOWN CHE", "EXAMPLE GROOMERS EXAMPLETOWN CHE",
			"Llama grooming"},
	)
	res := runPass(t, db, ctx, Options{})

	if detailed, provenance := verdictOf(t, db, ctx, "ubs", "T-1"); detailed !=
		"MEDICAL_PHARMACIES_AND_SUPPLEMENTS" || provenance != ProvenanceProvider {
		t.Errorf("T-1 = (%q, %q), want the pharmacy value via provider",
			detailed, provenance)
	}
	if detailed, provenance := verdictOf(t, db, ctx, "ubs", "T-2"); detailed != "" ||
		provenance != ProvenanceSignatureOnly {
		t.Errorf("T-2 = (%q, %q), want no provider verdict for the bank's "+
			"own catch-all", detailed, provenance)
	}
	// An UNREVIEWED card category is drift — the vocabulary is
	// categorical, unlike the same source's booking types — and it is
	// the only row counted here: the catch-all was reviewed and listed
	// untranslatable, so it contributes nothing to the canary.
	if res.UnmappedProviderCategories != 1 {
		t.Errorf("unmapped provider categories = %d, want 1 (T-3 alone)",
			res.UnmappedProviderCategories)
	}
}
