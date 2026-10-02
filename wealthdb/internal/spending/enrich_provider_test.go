package spending

import (
	"context"
	"database/sql"
	"regexp"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
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
		// A BOOKING-TYPE catch-all is a verdict: the bank is naming the
		// movement as a fee of its own, there is no merchant name for a
		// later tier to read, and the row's signature is fenced out of
		// model candidacy anyway. It claims. The card-issuer case, which
		// declines, is TestProviderTierDeclinesACatchAll below.
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

// TestProviderTierDeclinesACatchAll pins the policy directly: a value the
// vocabulary translates to a real category claims the row, a value it can
// only translate to that primary's catch-all does not — and both are
// recorded, so the issuer's view survives either way.
//
// The point is not tidiness. The provider tier outranks the model, so a
// claimed catch-all means the model is never asked about a merchant whose
// NAME would have placed it — the issuer knew the row was "shopping", and
// the descriptor said Apple.
func TestProviderTierDeclinesACatchAll(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		// The issuer named a line of business: it decides.
		txn{"bank", "T-SPECIFIC", "CARD1", "purchase", day(10), -30,
			"Example Grocer", "", "Groceries"},
		// The issuer managed only its own coarse bucket: recorded, declined,
		// and the row is left for the tier that reads the merchant name.
		txn{"bank", "T-CATCHALL", "CARD1", "purchase", day(11), -40,
			"Example Software Shop", "", "Shopping"},
	)

	res := runPass(t, db, ctx, Options{})

	if d, p := verdictOf(t, db, ctx, "bank", "T-SPECIFIC"); d != "FOOD_AND_DRINK_GROCERIES" || p != ProvenanceProvider {
		t.Errorf("T-SPECIFIC = (%q, %q), want the issuer's specific value, claimed", d, p)
	}
	if d, p := verdictOf(t, db, ctx, "bank", "T-CATCHALL"); d != "" || p != ProvenanceSignatureOnly {
		t.Errorf("T-CATCHALL = (%q, %q), want no verdict: a catch-all does not claim a row", d, p)
	}
	// Recorded either way — the faithful issuer view does not depend on
	// whether the tier went on to decide.
	for id, want := range map[string]string{
		"T-SPECIFIC": "FOOD_AND_DRINK_GROCERIES",
		"T-CATCHALL": "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE",
	} {
		if v, ok := providerViewOf(t, db, ctx, "bank", id); !ok || v != want {
			t.Errorf("%s provider view = (%q, %v), want %q recorded", id, v, ok, want)
		}
	}
	// A declined row is not drift: the vocabulary knew the value.
	if res.UnmappedProviderCategories != 0 {
		t.Errorf("UnmappedProviderCategories = %d, want 0: a declined value is mapped, not missing",
			res.UnmappedProviderCategories)
	}
}

// TestBookingTypeCatchAllStillClaims is the other half of the decline
// rule, and the one a regression is invisible in.
//
// A card issuer's catch-all declines because the row still carries a
// merchant name the model can read. A bank's booking type has no
// merchant name at all — and the signature of such a row is fenced out
// of model candidacy by Uninformative or FilingOnly — so declining
// would not defer the verdict to a better tier, it would throw it away
// and the row would read "(uncategorized)" for ever.
func TestBookingTypeCatchAllStillClaims(t *testing.T) {
	db, ctx := openGold(t)
	seedSwissBank(t, db, ctx)
	seedTxns(t, db, ctx,
		// The bank's own booking type for a fee of its own. It resolves
		// to the BANK_FEES catch-all, and that IS the verdict.
		txn{"swiss-bank", "T-CUSTODY", "CASH3", "fee", day(10), -40, "", "", "CUSTODY PRICE"},
	)
	res := runPass(t, db, ctx, Options{})

	if d, p := verdictOf(t, db, ctx, "swiss-bank", "T-CUSTODY"); d != "BANK_FEES_OTHER_BANK_FEES" || p != ProvenanceProvider {
		t.Errorf("T-CUSTODY = (%q, %q), want the bank's own filing to claim", d, p)
	}
	if res.ProviderRows != 1 {
		t.Errorf("ProviderRows = %d, want 1", res.ProviderRows)
	}
	// The row could not be rescued by the model if the tier declined:
	// its signature is nothing but the bank's own filing.
	sig := Normalize("", "CUSTODY PRICE")
	if !FilingOnly(sig, "CUSTODY PRICE") && !Uninformative(sig) {
		t.Errorf("signature %q is model-candidate; this test's premise needs revisiting", sig)
	}
}

// TestProviderCategoryClaimsTurnsOnVocabularyShape pins the predicate
// directly, on the same catch-all value under both shapes.
func TestProviderCategoryClaimsTurnsOnVocabularyShape(t *testing.T) {
	const catchAll = "BANK_FEES_OTHER_BANK_FEES"
	if !ProviderCategoryClaims("ubs", "cash", catchAll) {
		t.Error("a booking-type vocabulary's catch-all is the bank naming the movement; it claims")
	}
	if ProviderCategoryClaims("ubs", "card", catchAll) {
		t.Error("a card issuer's catch-all leaves a merchant name unread; it declines")
	}
	// A specific value claims under either shape.
	for _, kind := range []string{"cash", "card"} {
		if !ProviderCategoryClaims("ubs", kind, "FOOD_AND_DRINK_GROCERIES") {
			t.Errorf("%s: a specific value always claims", kind)
		}
	}
	// A source with no vocabulary claims nothing.
	if ProviderCategoryClaims("no-such-source", "", "FOOD_AND_DRINK_GROCERIES") {
		t.Error("a source with no reviewed vocabulary must contribute no verdict")
	}
}

// TestConfigRuleReachesTheIssuersFilingThroughThePass is the end-to-end
// half of the rule-tier change. ConfigRuleCategory's own test proves the
// field is matched; this proves the pass actually hands it over — a
// missing struct field there would leave the feature silently inert.
func TestConfigRuleReachesTheIssuersFilingThroughThePass(t *testing.T) {
	db, ctx := openGold(t)
	seedSwissBank(t, db, ctx)
	seedTxns(t, db, ctx,
		// A descriptor a rule cannot key on; the issuer's filing is the
		// only thing that identifies the row.
		txn{"swiss-bank", "T-BY-FILING", "CASH3", "withdrawal", day(10), -55,
			"", "REF 4711", "Club Membership"},
	)
	res := runPass(t, db, ctx, Options{
		Rules: []Rule{{
			Match:    regexp.MustCompile(`(?i)^Club Membership$`),
			Category: "PERSONAL_CARE_GYMS_AND_FITNESS_CENTERS",
		}},
	})
	if d, p := verdictOf(t, db, ctx, "swiss-bank", "T-BY-FILING"); d != "PERSONAL_CARE_GYMS_AND_FITNESS_CENTERS" || p != ProvenanceRule {
		t.Errorf("T-BY-FILING = (%q, %q), want the config rule keyed on the issuer's filing to place it", d, p)
	}
	if res.RuleRows != 1 {
		t.Errorf("RuleRows = %d, want 1", res.RuleRows)
	}
}

// TestIssuerViewSurvivesBeingOverruled pins the half of the record that
// only shows when a tier above wins: the issuer's own verdict is kept on
// the row even when ours replaces it, which is the whole reason the
// disagreement is a number rather than an overwrite.
func TestIssuerViewSurvivesBeingOverruled(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		// The issuer filed it as groceries; a rule of ours says otherwise.
		txn{"bank", "T-OVERRULED", "CARD1", "purchase", day(10), -30,
			"Example Grocer", "", "Groceries"},
	)
	runPass(t, db, ctx, Options{
		Rules: []Rule{{
			Match:    regexp.MustCompile(`(?i)example grocer`),
			Category: "GENERAL_MERCHANDISE_SUPERSTORES",
		}},
	})
	if d, p := verdictOf(t, db, ctx, "bank", "T-OVERRULED"); d != "GENERAL_MERCHANDISE_SUPERSTORES" || p != ProvenanceRule {
		t.Fatalf("T-OVERRULED = (%q, %q), want our rule to win", d, p)
	}
	if v, ok := providerViewOf(t, db, ctx, "bank", "T-OVERRULED"); !ok || v != "FOOD_AND_DRINK_GROCERIES" {
		t.Errorf("issuer view = (%q, %v), want it kept under the tier that overruled it", v, ok)
	}
}

// TestRaiffeisenVocabularyIsRegistered pins the vocabulary that had none:
// its registry key, its shape, and that its untranslatable tokens decline
// without being counted as drift.
func TestRaiffeisenVocabularyIsRegistered(t *testing.T) {
	for token, want := range map[string]string{
		"supermarket":       "FOOD_AND_DRINK_GROCERIES",
		"tv_phone_internet": "RENT_AND_UTILITIES_INTERNET_AND_CABLE",
		"atm_withdrawal":    canonical.SpendDetailedCashWithdrawal,
	} {
		got, ok, drift := ProviderCategory("raiffeisen_at", "cash", token)
		if !ok || got != want || drift {
			t.Errorf("ProviderCategory(raiffeisen_at, %q) = (%q, %v, drift %v), want %q",
				token, got, ok, drift, want)
		}
	}
	// The bank's own "I placed nothing" tokens: reviewed, so not drift.
	for _, token := range []string{"not_categorized", "payment_other", "income_other", "real_estate_other"} {
		got, ok, drift := ProviderCategory("raiffeisen_at", "cash", token)
		if ok || drift {
			t.Errorf("ProviderCategory(raiffeisen_at, %q) = (%q, %v, drift %v), want declined and uncounted",
				token, got, ok, drift)
		}
	}
	// It is categorical, so a token nobody reviewed IS drift.
	if _, ok, drift := ProviderCategory("raiffeisen_at", "cash", "no_such_token"); ok || !drift {
		t.Errorf("an unseen token = (ok %v, drift %v), want (false, true)", ok, drift)
	}
}

// seedPlaidItem adds a source of silver kind `plaid` with one cash
// account, so a test can seed rows filed under Plaid's categories.
func seedPlaidItem(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path,
                                    high_watermark, first_loaded_at, last_loaded_at)
             VALUES ('item', 'plaid', '/tmp/item.db', -1, 0, 0);
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, first_seen_at, last_seen_at)
             VALUES ('item', 'CHK', 'cash', 'Checking', 1, 1);
    `); err != nil {
		t.Fatalf("seed plaid source: %v", err)
	}
}

// TestCardRuleStandsDownForAProviderFiling runs the card rule's second
// refusal through the pass. A loan instalment or a utility bill paid by
// autopay keeps the provider's verdict. A row the provider filed under a
// catch-all is left to the model. The mortgage rule still reads its
// narrative. A row the provider filed as a card bill, or left
// untranslated, is the card rule's. Every value is synthetic.
func TestCardRuleStandsDownForAProviderFiling(t *testing.T) {
	db, ctx := openGold(t)
	seedPlaidItem(t, db, ctx)
	seedTxns(t, db, ctx,
		txn{"item", "T-LOAN", "CHK", "withdrawal", day(10), -250, "",
			"EXAMPLE LENDER AUTOPAY 000", "LOAN_PAYMENTS_PERSONAL_LOAN_PAYMENT"},
		txn{"item", "T-POWER", "CHK", "withdrawal", day(11), -90, "Example Energy",
			"EXAMPLE ENERGY AUTOPAY", "RENT_AND_UTILITIES_GAS_AND_ELECTRICITY"},
		txn{"item", "T-CITY", "CHK", "withdrawal", day(11), -60, "",
			"EXAMPLE UTILITY AUTOPAY", "RENT_AND_UTILITIES_OTHER_UTILITIES"},
		txn{"item", "T-HOME", "CHK", "withdrawal", day(12), -1500, "",
			"EXAMPLE HOME MORTGAGE AUTOPAY", "LOAN_PAYMENTS_MORTGAGE_PAYMENT"},
		txn{"item", "T-CARD", "CHK", "withdrawal", day(13), -400, "",
			"EXAMPLE CARD AUTOPAY", "LOAN_PAYMENTS_CREDIT_CARD_PAYMENT"},
		txn{"item", "T-UNSURE", "CHK", "withdrawal", day(14), -300, "",
			"EXAMPLE CARD AUTOPAY", "LOAN_PAYMENTS_OTHER_PAYMENT"},
	)

	runPass(t, db, ctx, Options{})

	for _, tc := range []struct{ id, detailed, provenance string }{
		{"T-LOAN", canonical.SpendDetailedDebtRepayment, ProvenanceProvider},
		{"T-POWER", "RENT_AND_UTILITIES_GAS_AND_ELECTRICITY", ProvenanceProvider},
		{"T-CITY", "", ProvenanceSignatureOnly},
		{"T-HOME", canonical.SpendDetailedInternalTransfer, ProvenanceRule},
		{"T-CARD", canonical.SpendDetailedCardSpend, ProvenanceRule},
		{"T-UNSURE", canonical.SpendDetailedCardSpend, ProvenanceRule},
	} {
		detailed, provenance := verdictOf(t, db, ctx, "item", tc.id)
		if detailed != tc.detailed || provenance != tc.provenance {
			t.Errorf("%s = (%q, %q), want (%q, %q)", tc.id, detailed, provenance, tc.detailed, tc.provenance)
		}
	}
}
