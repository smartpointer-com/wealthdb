package spending

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// seedSwissBank adds a source of silver kind `ubs` with one cash
// account to the gold openGold returns, so a test can seed rows whose
// provider_category is a bank's booking type.
func seedSwissBank(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path,
                                    high_watermark, first_loaded_at, last_loaded_at)
             VALUES ('swiss-bank', 'ubs', '/tmp/swiss.db', -1, 0, 0);
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, first_seen_at, last_seen_at)
             VALUES ('swiss-bank', 'CASH3', 'cash', 'Swiss cash', 1, 1);
    `); err != nil {
		t.Fatalf("seed ubs source: %v", err)
	}
}

// hasEnrichment reports whether the pass wrote an enrichment row for
// the transaction at all.
func hasEnrichment(t *testing.T, db *sql.DB, ctx context.Context, source, id string) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM spend_txn_enrichment
         WHERE silver_source_id = ? AND transaction_external_id = ?`, source, id).Scan(&n); err != nil {
		t.Fatalf("count enrichment for %s/%s: %v", source, id, err)
	}
	return n > 0
}

// TestPassProviderTierPlacesBankBookingTypes pins the provider tier on
// a booking-type vocabulary — the UBS adapter's, whose provider_category
// is the bank's own booking type in every era — and the two policies it
// carries. The tier may place a DELTA when the provider's word names the
// movement: an ATM withdrawal whose narrative is a bare bank tag is
// `cash_withdrawal`, an FX conversion between the holder's own currency
// accounts `internal_transfer`, a bill paid to a card `card_spend`, each
// with provenance `provider`. And a value the map does not translate is
// a rail, not drift: it is not counted, where the same miss on a card
// issuer's categorical vocabulary is. The tiers above still overrule it:
// a narrative that names the machine is the rule's, and a leg the
// matcher pairs is the matcher's. Of the FX spellings only the MT940
// `NFEX` shape — a withdrawal whose narrative is a bare tag — reaches
// the tier: the adapter classifies the web and PDF eras' FX bookings as
// `fx` kinds, which the population excludes by kind, so such a row is
// never enriched at all and the map's entry for it places nothing.
// Every value is synthetic.
func TestPassProviderTierPlacesBankBookingTypes(t *testing.T) {
	db, ctx := openGold(t)
	seedSwissBank(t, db, ctx)
	seedTxns(t, db, ctx,
		// Web era: cash out of an ATM, narrative a bare code the atm
		// rule cannot see.
		txn{"swiss-bank", "T-ATM", "CASH3", "withdrawal", day(10), -200, "", "KH", "ATM Withdrawal"},
		// PSN era: a charge and an FX leg whose narrative is nothing
		// but a bank tag; the :61: code is all there is.
		txn{"swiss-bank", "T-FEE", "CASH3", "fee", day(11), -12, "", "N21?", "NCHG"},
		txn{"swiss-bank", "T-FX", "CASH3", "withdrawal", day(12), -1000, "", "N21?", "NFEX"},
		// PDF era: interest charged, admitted to the population by its
		// sign; and a card bill with no counter-leg in gold.
		txn{"swiss-bank", "T-INTEREST", "CASH3", "interest", day(13), -3, "", "INTEREST CALCULATION BALANCE", "INTEREST CALCULATION BALANCE"},
		txn{"swiss-bank", "T-CARD-BILL", "CASH3", "withdrawal", day(14), -350, "Example Card Services", "", "PAYMENT TO CARD"},
		// A payment order: the rail says nothing about what was bought,
		// and the row is backlog for the model — not drift.
		txn{"swiss-bank", "T-RAIL", "CASH3", "withdrawal", day(15), -80,
			"EXAMPLE PAYEE", "EXAMPLE PAYEE; EXAMPLE STREET 1; 9999 EXAMPLETOWN", "e-banking payment order"},
		// The rule outranks the provider: the narrative names the
		// machine, the booking type is a rail.
		txn{"swiss-bank", "T-RULE", "CASH3", "withdrawal", day(16), -100, "ATM Main Street", "", "NTRF"},
		// Web era: an FX leg the adapter classified as an `fx` kind.
		// Outside the population by kind — the tier never sees it.
		txn{"swiss-bank", "T-FX-WEB", "CASH3", "fx", day(17), -500, "", "Sale FX Spot", "Sale FX Spot"},
		// The matcher outranks the provider's delta: a card bill whose
		// card leg IS in gold is an own-account move, not card spend.
		txn{"swiss-bank", "T-MATCH-OUT", "CASH3", "withdrawal", day(20), -400, "Example Card Services", "", "Payment to card"},
		txn{"bank", "T-MATCH-IN", "CARD1", "card_payment", day(20), 400, "", "", ""},
		// A categorical vocabulary's miss still counts.
		txn{"bank", "T-UNSEEN", "CARD1", "purchase", day(21), -60, "Ferry Road Depot", "", "Automotive & Transit"},
	)

	res := runPass(t, db, ctx, Options{})

	for _, tc := range []struct{ source, id, detailed, provenance string }{
		{"swiss-bank", "T-ATM", canonical.SpendDetailedCashWithdrawal, ProvenanceProvider},
		{"swiss-bank", "T-FEE", "BANK_FEES_OTHER_BANK_FEES", ProvenanceProvider},
		{"swiss-bank", "T-FX", canonical.SpendDetailedInternalTransfer, ProvenanceProvider},
		{"swiss-bank", "T-INTEREST", "BANK_FEES_INTEREST_CHARGE", ProvenanceProvider},
		{"swiss-bank", "T-CARD-BILL", canonical.SpendDetailedCardSpend, ProvenanceProvider},
		{"swiss-bank", "T-RAIL", "", ProvenanceSignatureOnly},
		{"swiss-bank", "T-RULE", canonical.SpendDetailedCashWithdrawal, ProvenanceRule},
		{"swiss-bank", "T-MATCH-OUT", canonical.SpendDetailedInternalTransfer, ProvenanceMatcher},
		{"bank", "T-MATCH-IN", canonical.SpendDetailedInternalTransfer, ProvenanceMatcher},
		{"bank", "T-UNSEEN", "", ProvenanceSignatureOnly},
	} {
		detailed, provenance := verdictOf(t, db, ctx, tc.source, tc.id)
		if detailed != tc.detailed || provenance != tc.provenance {
			t.Errorf("%s = (%q, %q), want (%q, %q)", tc.id, detailed, provenance, tc.detailed, tc.provenance)
		}
	}
	if hasEnrichment(t, db, ctx, "swiss-bank", "T-FX-WEB") {
		t.Error("T-FX-WEB was enriched; an fx-kind row is outside the population and no tier may reach it")
	}
	if res.UnmappedProviderCategories != 1 {
		t.Errorf("UnmappedProviderCategories = %d, want 1: a bank's untranslated rail is not drift, an issuer's unseen category is",
			res.UnmappedProviderCategories)
	}
	if res.ProviderRows != 5 {
		t.Errorf("ProviderRows = %d, want 5", res.ProviderRows)
	}
	base := spendingBaseIDs(t, db, ctx)
	for _, id := range []string{"T-ATM", "T-FEE", "T-INTEREST", "T-CARD-BILL", "T-RAIL", "T-RULE"} {
		if _, ok := base[id]; !ok {
			t.Errorf("%s left the spending base", id)
		}
	}
	for _, id := range []string{"T-FX", "T-MATCH-OUT"} {
		if _, ok := base[id]; ok {
			t.Errorf("%s is still in the spending base; an own-account move is not spend", id)
		}
	}
	if _, ok := base["T-FX-WEB"]; ok {
		t.Error("T-FX-WEB is in the spending base; an fx-kind row is not in the population")
	}
}
