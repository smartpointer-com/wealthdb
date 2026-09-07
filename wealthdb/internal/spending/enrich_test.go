package spending

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
)

// day turns a day offset into the Unix-seconds timestamp gold stores.
func day(n int64) int64 { return n * gold.SecondsPerDay }

// openGold returns a migrated in-memory gold holding two sources: a
// `chase` one with a cash, a card and a brokerage account, and a
// second source with a cash and a brokerage account. `chase` is the
// silver kind with a provider-category map, so a fixture can exercise
// that tier; the second source's brokerage account is what a movement
// out of the first source can land on without being anywhere near the
// spending scope.
func openGold(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	db, err := gold.Open(":memory:", gold.ModeReadWrite)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if err := gold.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate gold: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path,
                                    high_watermark, first_loaded_at, last_loaded_at)
             VALUES ('bank', 'chase', '/tmp/test.db', -1, 0, 0),
                    ('other-bank', 'schwab', '/tmp/other.db', -1, 0, 0);

        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, first_seen_at, last_seen_at)
             VALUES ('bank', 'CASH1', 'cash',      'Everyday',  1, 1),
                    ('bank', 'CARD1', 'card',      'Card',      1, 1),
                    ('bank', 'BRK1',  'brokerage', 'Brokerage', 1, 1),
                    ('other-bank', 'CASH2', 'cash',      'Elsewhere', 1, 1),
                    ('other-bank', 'BRK2',  'brokerage', 'Invested',  1, 1);
    `); err != nil {
		t.Fatalf("seed dimensions: %v", err)
	}
	return db, ctx
}

// txn is one seeded transaction. Amounts are floats for brevity; gold
// stores them as DECIMAL(28,4) and the pass reads them back as doubles.
type txn struct {
	source, id, account, kind string
	occurredAt                int64
	amount                    float64
	counterparty              string
	description               string
	providerCategory          string
}

func seedTxns(t *testing.T, db *sql.DB, ctx context.Context, txns ...txn) {
	t.Helper()
	seedTxnsIn(t, db, ctx, "USD", txns...)
}

// seedTxnsIn seeds in a named currency. Only the tests that need two of
// them use it: the matcher partitions candidates by native currency, so
// a cross-currency movement is a distinct case from a same-currency one
// and cannot be expressed in the single-currency fixture.
func seedTxnsIn(t *testing.T, db *sql.DB, ctx context.Context,
	currency string, txns ...txn) {
	t.Helper()
	for _, x := range txns {
		if _, err := db.ExecContext(ctx, `
            INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                      account_external_id, kind, currency, net_amount,
                                      counterparty, description, provider_category)
                 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			x.source, x.id, x.occurredAt, x.account, x.kind, currency, x.amount,
			nullableString(x.counterparty), nullableString(x.description),
			nullableString(x.providerCategory)); err != nil {
			t.Fatalf("seed transaction %s: %v", x.id, err)
		}
	}
}

// enrichmentSnapshot reads the whole overlay as comparable text, which
// is what an idempotency check needs: not "the same number of rows"
// but "the same rows, with the same verdicts and the same provenance".
// assigned_at is excluded — the pass stamps wall-clock time, and
// pinning it would test the clock rather than the pass.
func enrichmentSnapshot(t *testing.T, db *sql.DB, ctx context.Context) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
        SELECT silver_source_id, transaction_external_id,
               COALESCE(merchant_signature, '(null)'), signature_version,
               COALESCE(spend_detailed, '(null)'), provenance,
               COALESCE(merchant_label, '(null)')
          FROM spend_txn_enrichment
         ORDER BY silver_source_id, transaction_external_id`)
	if err != nil {
		t.Fatalf("read enrichment overlay: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var src, id, sig, detailed, prov, label string
		var version int
		if err := rows.Scan(&src, &id, &sig, &version, &detailed, &prov, &label); err != nil {
			t.Fatalf("scan enrichment overlay: %v", err)
		}
		out = append(out, fmt.Sprintf("%s/%s sig=%q v%d cat=%s via=%s label=%s",
			src, id, sig, version, detailed, prov, label))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate enrichment overlay: %v", err)
	}
	return out
}

func runPass(t *testing.T, db *sql.DB, ctx context.Context, opts Options) *Result {
	t.Helper()
	if opts.MatchWindowDays == 0 {
		opts.MatchWindowDays = 5
	}
	if opts.MatchTolerancePct == 0 {
		opts.MatchTolerancePct = 0.5
	}
	if opts.Now == 0 {
		opts.Now = 1_700_000_000
	}
	res, err := RunDeterministicPass(ctx, db, opts)
	if err != nil {
		t.Fatalf("RunDeterministicPass: %v", err)
	}
	return res
}

// signatureOf reads the merchant signature the pass stored for one
// transaction.
func signatureOf(t *testing.T, db *sql.DB, ctx context.Context, source, id string) string {
	t.Helper()
	var sig sql.NullString
	if err := db.QueryRowContext(ctx, `
        SELECT merchant_signature FROM spend_txn_enrichment
         WHERE silver_source_id = ? AND transaction_external_id = ?`, source, id).Scan(&sig); err != nil {
		t.Fatalf("read signature of %s/%s: %v", source, id, err)
	}
	return sig.String
}

// verdictOf reads one row's category and provenance out of the overlay.
func verdictOf(t *testing.T, db *sql.DB, ctx context.Context, source, id string) (detailed, provenance string) {
	t.Helper()
	var d sql.NullString
	err := db.QueryRowContext(ctx, `
        SELECT spend_detailed, provenance FROM spend_txn_enrichment
         WHERE silver_source_id = ? AND transaction_external_id = ?`, source, id).
		Scan(&d, &provenance)
	if err != nil {
		t.Fatalf("read verdict for %s/%s: %v", source, id, err)
	}
	return d.String, provenance
}

// TestPassPrecedence pins matcher > rule > provider on rows that each
// have MORE than one tier's worth of evidence, which is the only way
// the ordering is observable at all.
func TestPassPrecedence(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		// Provider only.
		txn{"bank", "T-PROVIDER", "CARD1", "purchase", day(10), -50,
			"Corner Market", "", "Groceries"},
		// Rule AND provider: the issuer labelled an ATM withdrawal
		// "Shopping". A rule knows what the issuer cannot.
		txn{"bank", "T-RULE-OVER-PROVIDER", "CARD1", "purchase", day(10), -200,
			"ATM Withdrawal Main Street", "", "Shopping"},
		// Matcher AND rule AND provider: an autopay withdrawal whose
		// card leg IS in gold. Evidence outranks both inferences.
		txn{"bank", "T-MATCH-OUT", "CASH1", "withdrawal", day(20), -400,
			"Autopay Payment", "", "Shopping"},
		txn{"bank", "T-MATCH-IN", "CARD1", "card_payment", day(20), 400, "", "", ""},
	)

	runPass(t, db, ctx, Options{})

	for _, tc := range []struct{ id, detailed, provenance string }{
		{"T-PROVIDER", "FOOD_AND_DRINK_GROCERIES", ProvenanceProvider},
		{"T-RULE-OVER-PROVIDER", canonical.SpendDetailedCashWithdrawal, ProvenanceRule},
		{"T-MATCH-OUT", canonical.SpendDetailedInternalTransfer, ProvenanceMatcher},
		{"T-MATCH-IN", canonical.SpendDetailedInternalTransfer, ProvenanceMatcher},
	} {
		detailed, provenance := verdictOf(t, db, ctx, "bank", tc.id)
		if detailed != tc.detailed || provenance != tc.provenance {
			t.Errorf("%s = (%q, %q), want (%q, %q)",
				tc.id, detailed, provenance, tc.detailed, tc.provenance)
		}
	}
}

// TestPassUnpairedCardPaymentIsCardSpend pins the decision of record
// for a card bill paid from a cash account, and the precedence that
// makes it safe: matcher > rule.
//
// The same Chase-shaped narrative lands twice. Once with the card's
// own `card_payment` leg in gold — the card is collected, its
// purchases are itemised, and the matcher nets the bill out as
// `internal_transfer`. Once with no leg anywhere — a card wealthdb
// does not collect, or the deep era before the card's ledger begins —
// and the bill is the only trace of that spending, so the rule keeps
// it in the base as `card_spend`. Reverting the rule to
// `internal_transfer` fails the second half: the unpaired bill would
// vanish from the base.
//
// The last act is the day the card gets collected: its leg lands, the
// next pass pairs it, and the bill leaves the base — replaced by the
// purchases the card now itemises.
func TestPassUnpairedCardPaymentIsCardSpend(t *testing.T) {
	db, ctx := openGold(t)
	const bill = "PAYMENT TO CHASE CARD ENDING IN ####"
	seedTxns(t, db, ctx,
		txn{"bank", "T-BILL-PAIRED", "CASH1", "withdrawal", day(20), -400, "", bill, ""},
		txn{"bank", "T-CARD-LEG", "CARD1", "card_payment", day(20), 400, "", "", ""},
		txn{"bank", "T-BILL-UNPAIRED", "CASH1", "withdrawal", day(40), -950, "", bill, ""},
		// A purchase beside them, so the base is "kept what it should"
		// rather than empty.
		txn{"bank", "T-SPEND", "CARD1", "purchase", day(40), -60, "Corner Market", "", ""},
	)

	res := runPass(t, db, ctx, Options{})

	for _, tc := range []struct{ id, detailed, provenance string }{
		{"T-BILL-PAIRED", canonical.SpendDetailedInternalTransfer, ProvenanceMatcher},
		{"T-CARD-LEG", canonical.SpendDetailedInternalTransfer, ProvenanceMatcher},
		{"T-BILL-UNPAIRED", canonical.SpendDetailedCardSpend, ProvenanceRule},
	} {
		detailed, provenance := verdictOf(t, db, ctx, "bank", tc.id)
		if detailed != tc.detailed || provenance != tc.provenance {
			t.Errorf("%s = (%q, %q), want (%q, %q)", tc.id, detailed, provenance, tc.detailed, tc.provenance)
		}
	}
	if res.MatcherRows != 2 || res.RuleRows != 1 {
		t.Errorf("tier counts = matcher %d, rule %d; want 2, 1", res.MatcherRows, res.RuleRows)
	}

	base := spendingBaseIDs(t, db, ctx)
	if _, ok := base["T-BILL-PAIRED"]; ok {
		t.Error("the paired bill is in the spending base; its card is itemised, so it is an own-account move")
	}
	if _, ok := base["T-BILL-UNPAIRED"]; !ok {
		t.Error("the unpaired bill fell out of the spending base; it is the only trace of that card's spending")
	}
	if _, ok := base["T-SPEND"]; !ok {
		t.Error("the purchase fell out of the spending base")
	}
	if len(base) != 2 {
		t.Errorf("spending base = %d rows, want 2 (the unpaired bill and the purchase)", len(base))
	}

	// The card is collected: its leg lands, and the bill is replaced by
	// the purchases it now itemises.
	seedTxns(t, db, ctx,
		txn{"bank", "T-LATE-LEG", "CARD1", "card_payment", day(41), 950, "", "", ""})
	runPass(t, db, ctx, Options{})
	if detailed, provenance := verdictOf(t, db, ctx, "bank", "T-BILL-UNPAIRED"); detailed !=
		canonical.SpendDetailedInternalTransfer || provenance != ProvenanceMatcher {
		t.Errorf("once the card's leg is in gold, the bill = (%q, %q), want internal_transfer via matcher",
			detailed, provenance)
	}
	if _, ok := spendingBaseIDs(t, db, ctx)["T-BILL-UNPAIRED"]; ok {
		t.Error("the bill is still in the spending base after its card was collected")
	}
}

// TestPassCardBillCarriesItsIssuer pins the one exception to a delta
// line having no merchant: a card bill names the ISSUER it was paid
// to, because that is the only handle there is on which card the money
// went to. The bill's own signature names the payer's bank or the
// holder and is not a merchant, so the label comes from the built-in
// card rule's issuer table rather than from the merchant store.
//
// Every other line in the fixture is the boundary. A bill recognised
// only by a masked card number names no issuer. A gift and an
// own-account move are deltas and carry no merchant at all, whatever
// the store holds for their signatures — the own-account move here is
// the same Chase-shaped bill, paired, so a label that outlived the
// verdict that produced it would show up as an issuer on an internal
// transfer. A vendored line still shows the store's name. And the
// provider tier and a config rule label nothing: the exception is the
// card rule's alone.
//
// Every value is synthetic.

// TestPassAmexBillPairsWithTheCollectedCard is the same precedence, checked
// against the OTHER shape it has to hold for: a bill paid to an issuer whose
// card is collected as its own SOURCE, not as a sibling product of the paying
// bank.
//
// It matters because the two paths through the matcher differ. A Chase card
// paid from a Chase account pairs same-source; an Amex card paid from any bank
// pairs across sources, which is the commoner arrangement and the one the
// `card_spend` placeholder was written for. The card's leg is the collector's
// `card_payment` projection of an UNCATEGORISED credit — the tell that
// separates the monthly bill from a merchant refund — so this also pins that
// mapping: kind the bill `refund` instead and the pair never forms.
func TestPassAmexBillPairsWithTheCollectedCard(t *testing.T) {
	db, ctx := openGold(t)
	// The card is its own SOURCE here, which is the whole point: the shared
	// fixture's card is a sibling product of the paying bank.
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path,
                                    high_watermark, first_loaded_at, last_loaded_at)
             VALUES ('amex', 'amex', '/tmp/amex.db', -1, 0, 0);
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, first_seen_at, last_seen_at)
             VALUES ('amex', 'AMEXCARD', 'card', 'Example Card', 1, 1);
    `); err != nil {
		t.Fatalf("seed amex dimensions: %v", err)
	}
	const bill = "AMERICAN EXPRESS ACH PMT"
	seedTxns(t, db, ctx,
		txn{"bank", "T-AMEX-BILL", "CASH1", "withdrawal", day(20), -400, "", bill, ""},
		txn{"amex", "T-AMEX-LEG", "AMEXCARD", "card_payment", day(20), 400, "", "", ""},
		// A purchase on the card, carrying the provider category the amex
		// vocabulary translates — the spending the bill used to stand in for.
		txn{"amex", "T-AMEX-BUY", "AMEXCARD", "purchase", day(18), -60,
			"Corner Market", "", "Merchandise & Supplies"},
	)

	res := runPass(t, db, ctx, Options{})

	for _, tc := range []struct{ src, id, detailed, provenance string }{
		{"bank", "T-AMEX-BILL", canonical.SpendDetailedInternalTransfer, ProvenanceMatcher},
		{"amex", "T-AMEX-LEG", canonical.SpendDetailedInternalTransfer, ProvenanceMatcher},
		{"amex", "T-AMEX-BUY", "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE", ProvenanceProvider},
	} {
		detailed, provenance := verdictOf(t, db, ctx, tc.src, tc.id)
		if detailed != tc.detailed || provenance != tc.provenance {
			t.Errorf("%s = (%q, %q), want (%q, %q)", tc.id, detailed, provenance,
				tc.detailed, tc.provenance)
		}
	}
	// The placeholder is gone: no row is left as generic card spend.
	if res.RuleRows != 0 {
		t.Errorf("rule rows = %d, want 0 — the bill should pair, not fall to the rule", res.RuleRows)
	}
}

// TestAmexResidualCategoryIsNotDrift pins the third ProviderCategory outcome
// for the amex vocabulary: the issuer's own "Other" bucket is REVIEWED, so it
// is neither translated (which would pre-empt the model on exactly the rows
// the model exists for) nor counted as drift (which is supposed to mean the
// issuer said something this build does not understand).
func TestAmexResidualCategoryIsNotDrift(t *testing.T) {
	for _, tc := range []struct {
		category  string
		ok, drift bool
	}{
		{"Merchandise & Supplies", true, false},
		{"Other", false, false},
		{"A Category Amex Just Invented", false, true},
		{"", false, false},
	} {
		// Cards are the only product this source has, so its vocabulary is
		// registered source-wide and every account kind inherits it.
		_, ok, drift := ProviderCategory("amex", "", tc.category)
		if ok != tc.ok || drift != tc.drift {
			t.Errorf("ProviderCategory(amex, %q) = (ok %v, drift %v), want (%v, %v)",
				tc.category, ok, drift, tc.ok, tc.drift)
		}
	}
}

func TestPassCardBillCarriesItsIssuer(t *testing.T) {
	db, ctx := openGold(t)
	const chaseBill = "PAYMENT TO CHASE CARD ENDING IN ####"
	const maskedBill = "0000XXXXXXXX0000 03.09.26"
	seedTxns(t, db, ctx,
		// A bill whose narrative names its issuer, and one recognised
		// only by the masked card number it was topped up with.
		txn{"bank", "T-BILL-NAMED", "CASH1", "withdrawal", day(40), -950, "", chaseBill, ""},
		txn{"bank", "T-BILL-MASKED", "CASH1", "withdrawal", day(41), -120, maskedBill, "", ""},
		// The same named bill, paired with the card's own leg: the
		// matcher outranks the rule, and the verdict it replaces takes
		// the label with it.
		txn{"bank", "T-BILL-PAIRED", "CASH1", "withdrawal", day(20), -400, "", chaseBill, ""},
		txn{"bank", "T-CARD-LEG", "CARD1", "card_payment", day(20), 400, "", "", ""},
		// A gift, pinned, on a signature the store has named.
		txn{"bank", "T-GIFT", "CASH1", "withdrawal", day(42), -300, "Example Relative", "", ""},
		// A vendored line the store named: the merchant column is
		// unmoved for everything that is not a delta.
		txn{"bank", "T-GROCERY", "CARD1", "purchase", day(43), -60, "Corner Market", "", ""},
		// A config rule places a card bill from the narrative.
		txn{"bank", "T-CONFIG-BILL", "CASH1", "withdrawal", day(45), -260,
			"Example Card Services Ltd", "", ""},
	)
	// The provider tier places a card bill of its own, from a booking
	// type that names one — which only a bank's vocabulary carries, so
	// it needs a source of that kind.
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path,
                                    high_watermark, first_loaded_at, last_loaded_at)
             VALUES ('swiss-bank', 'ubs', '/tmp/ubs.db', -1, 0, 0);
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, first_seen_at, last_seen_at)
             VALUES ('swiss-bank', 'CASH3', 'cash', 'Swiss cash', 1, 1)`); err != nil {
		t.Fatalf("seed the bank source: %v", err)
	}
	seedTxns(t, db, ctx,
		txn{"swiss-bank", "T-PROVIDER-BILL", "CASH3", "withdrawal", day(44), -220,
			"", "Statement settlement", "Payment To Card"},
	)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name) VALUES
            ('EXAMPLE RELATIVE', 'Example Relative', 'GENERAL_SERVICES_OTHER_GENERAL_SERVICES',
             ?, 100, 'test-model'),
            ('CORNER MARKET', 'Corner Market', 'FOOD_AND_DRINK_GROCERIES', ?, 100, 'test-model')`,
		SignatureVersion, SignatureVersion); err != nil {
		t.Fatalf("seed the merchant store: %v", err)
	}

	runPass(t, db, ctx, Options{
		Rules: []Rule{{regexp.MustCompile(`(?i)EXAMPLE CARD SERVICES`), canonical.SpendDetailedCardSpend}},
		Pins: []Pin{{Source: "bank", Account: "CASH1", Day: day(42), Amount: -300,
			Currency: "USD", Detailed: canonical.SpendDetailedGift}},
	})

	for _, tc := range []struct{ source, id, detailed, provenance, merchant string }{
		{"bank", "T-BILL-NAMED", canonical.SpendDetailedCardSpend, ProvenanceRule, "Chase"},
		{"bank", "T-BILL-MASKED", canonical.SpendDetailedCardSpend, ProvenanceRule, ""},
		{"bank", "T-BILL-PAIRED", canonical.SpendDetailedInternalTransfer, ProvenanceMatcher, ""},
		{"bank", "T-GIFT", canonical.SpendDetailedGift, ProvenanceManual, ""},
		{"bank", "T-GROCERY", "FOOD_AND_DRINK_GROCERIES", "model", "Corner Market"},
		{"swiss-bank", "T-PROVIDER-BILL", canonical.SpendDetailedCardSpend, ProvenanceProvider, ""},
		{"bank", "T-CONFIG-BILL", canonical.SpendDetailedCardSpend, ProvenanceRule, ""},
	} {
		var merchant, detailed sql.NullString
		var provenance string
		if err := db.QueryRowContext(ctx, `
        SELECT merchant_name, spend_detailed, provenance FROM spend_txn_categories()
         WHERE silver_source_id = ? AND transaction_external_id = ?`, tc.source, tc.id).
			Scan(&merchant, &detailed, &provenance); err != nil {
			t.Fatalf("read %s: %v", tc.id, err)
		}
		if merchant.String != tc.merchant || detailed.String != tc.detailed || provenance != tc.provenance {
			t.Errorf("%s = (merchant %q, %q, %q), want (%q, %q, %q)", tc.id,
				merchant.String, detailed.String, provenance, tc.merchant, tc.detailed, tc.provenance)
		}
	}

	// The label is stored on the card rule's row and nowhere else, so
	// a tier that never sets one cannot inherit it from the row it
	// overruled.
	rows, err := db.QueryContext(ctx, `
        SELECT transaction_external_id FROM spend_txn_enrichment
         WHERE merchant_label IS NOT NULL ORDER BY 1`)
	if err != nil {
		t.Fatalf("read the labelled rows: %v", err)
	}
	defer rows.Close()
	var labelled []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan the labelled rows: %v", err)
		}
		labelled = append(labelled, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the labelled rows: %v", err)
	}
	if strings.Join(labelled, ",") != "T-BILL-NAMED" {
		t.Errorf("labelled rows = %v, want only T-BILL-NAMED", labelled)
	}
}

// TestPassRuleTierReadsNarrative pins the wiring behind the two
// changes SignatureVersion 3 records, on rows shaped like the UBS
// adapter's. Its counterparty is silver's promoted first narrative
// segment: on a direct debit that is the mandate notice, on a transfer
// the bank's own name, and the creditor sits in the description. The
// pass must key the first two by the creditor — the notice reduces to
// a bare code, the bank's name is a truncation — and place all three
// bills as card spend by rule, the third from the description alone,
// since a PDF-era description leads with the booking type and the
// counterparty is then no truncation of it. None has a counter-leg,
// so nothing outranks the rule. Every value is synthetic.
func TestPassRuleTierReadsNarrative(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T-DD", "CASH1", "withdrawal", day(20), -700,
			"CRD1W OBJECTION TO UBS",
			"CRD1W OBJECTION TO UBS; WITHIN 30 DAYS; UBS CARD CENTER; CREDIT CARD STATEMENT 03/2026", ""},
		txn{"bank", "T-XFER", "CASH1", "withdrawal", day(21), -800,
			"UBS SWITZERLAND AG", "UBS SWITZERLAND AG;C/O UBS CARD CENTER", ""},
		txn{"bank", "T-PDF", "CASH1", "withdrawal", day(22), -900,
			"UBS SWITZERLAND AG", "E-BANKING PAYMENT ORDER; UBS SWITZERLAND AG; C/O UBS CARD CENTER", ""},
		// A purchase beside them, keyed by its counterparty as before.
		txn{"bank", "T-SPEND", "CARD1", "purchase", day(20), -60, "Corner Market", "", ""},
	)

	res := runPass(t, db, ctx, Options{})

	for _, tc := range []struct{ id, signature string }{
		{"T-DD", "UBS CARD CENTER CREDIT CARD STATEMENT 03"},
		{"T-XFER", "UBS SWITZERLAND AG C O UBS CARD CENTER"},
		{"T-PDF", "UBS SWITZERLAND AG"},
	} {
		detailed, provenance := verdictOf(t, db, ctx, "bank", tc.id)
		if detailed != canonical.SpendDetailedCardSpend || provenance != ProvenanceRule {
			t.Errorf("%s = (%q, %q), want (card_spend, rule)", tc.id, detailed, provenance)
		}
		if sig := signatureOf(t, db, ctx, "bank", tc.id); sig != tc.signature {
			t.Errorf("%s signature = %q, want %q", tc.id, sig, tc.signature)
		}
	}
	if sig := signatureOf(t, db, ctx, "bank", "T-SPEND"); sig != "CORNER MARKET" {
		t.Errorf("T-SPEND signature = %q, want CORNER MARKET", sig)
	}
	if res.RuleRows != 3 {
		t.Errorf("rule rows = %d, want 3", res.RuleRows)
	}
}

// TestPassIsIdempotent is the guarantee the whole full-re-assert design
// rests on: running the pass again over unchanged gold must reproduce
// the overlay exactly, not merely a similar one.
func TestPassIsIdempotent(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T1", "CARD1", "purchase", day(10), -50, "Corner Market", "", "Groceries"},
		txn{"bank", "T2", "CARD1", "purchase", day(11), -20, "Blue Harbour Cafe", "", "Beverages"},
		txn{"bank", "T3", "CASH1", "withdrawal", day(12), -100, "ATM Main Street", "", ""},
		txn{"bank", "T4", "CASH1", "withdrawal", day(20), -400, "Autopay", "", ""},
		txn{"bank", "T5", "CARD1", "card_payment", day(20), 400, "", "", ""},
		txn{"bank", "T6", "CASH1", "deposit", day(21), 900, "", "Payroll", ""},
	)

	first := runPass(t, db, ctx, Options{})
	before := enrichmentSnapshot(t, db, ctx)
	second := runPass(t, db, ctx, Options{})
	after := enrichmentSnapshot(t, db, ctx)

	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("overlay changed on a second pass over unchanged gold:\nfirst:\n%s\nsecond:\n%s",
			strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
	if *first != *second {
		t.Errorf("pass result changed on re-run: %+v then %+v", *first, *second)
	}
}

// TestPassMarksBothLegsOfAMatch pins the matcher tier's two contracts:
// both halves of a pair are marked, and the incoming half is marked
// even though it sits outside the spending population entirely — a
// card payment is never a spending line, but it is still an
// own-account move.
func TestPassMarksBothLegsOfAMatch(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		// Cross-source: a withdrawal here funds a deposit there. The
		// spending matcher permits same-source pairs too, which the
		// returns matcher forbids; this fixture covers the other axis.
		txn{"bank", "T-OUT", "CASH1", "withdrawal", day(30), -1500, "", "Transfer to savings", ""},
		txn{"other-bank", "T-IN", "CASH2", "deposit", day(32), 1500, "", "Incoming transfer", ""},
	)
	res := runPass(t, db, ctx, Options{})

	if res.MatcherRows != 2 {
		t.Errorf("MatcherRows = %d, want 2 (both legs)", res.MatcherRows)
	}
	for _, leg := range []struct{ source, id string }{{"bank", "T-OUT"}, {"other-bank", "T-IN"}} {
		detailed, provenance := verdictOf(t, db, ctx, leg.source, leg.id)
		if detailed != canonical.SpendDetailedInternalTransfer || provenance != ProvenanceMatcher {
			t.Errorf("%s/%s = (%q, %q), want internal_transfer via matcher",
				leg.source, leg.id, detailed, provenance)
		}
	}

	// And the outgoing leg drops out of the spending base, which is
	// the whole point of marking it.
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spending_lines_base(0, 9223372036854775807)`).Scan(&n); err != nil {
		t.Fatalf("count spending lines: %v", err)
	}
	if n != 0 {
		t.Errorf("spending_lines_base = %d rows, want 0 (an own-account move is not spend)", n)
	}
}

// spendingBaseIDs is the set of transaction ids a report would chart.
func spendingBaseIDs(t *testing.T, db *sql.DB, ctx context.Context) map[string]struct{} {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT transaction_external_id FROM spending_lines_base(0, ?)`, gold.MaxEpoch)
	if err != nil {
		t.Fatalf("read spending base: %v", err)
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan spending base: %v", err)
		}
		out[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate spending base: %v", err)
	}
	return out
}

// inEnrichmentPopulation reports whether a row is one the pass may
// write a verdict for on its own account — as opposed to one it only
// ever reaches through the matcher pool.
func inEnrichmentPopulation(t *testing.T, db *sql.DB, ctx context.Context, source, id string) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM spend_enrichment_population(0, ?)
         WHERE silver_source_id = ? AND transaction_external_id = ?`,
		gold.MaxEpoch, source, id).Scan(&n); err != nil {
		t.Fatalf("read enrichment population for %s/%s: %v", source, id, err)
	}
	return n > 0
}

// TestPassMatchesOntoAnAccountOutsideTheSpendingScope is the
// regression the widened matcher pool (migration 0044) exists for.
//
// Money moved out of a cash account into an investment account is
// booked as a withdrawal on one side and a credit on the other, and
// the receiving side is an account kind no spending report will ever
// chart. While the pool drew from spend_scoped_accounts() the
// receiving leg was not a candidate at all, so the pair could never
// form; the withdrawal stayed one-legged, which is indistinguishable
// from spending, and the movement was counted as spend at whatever
// size it was.
//
// Narrowing spend_matcher_pool back to the scoped accounts fails this
// test twice over: no pair forms, and the withdrawal reappears in the
// spending base.
func TestPassMatchesOntoAnAccountOutsideTheSpendingScope(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T-FUND-OUT", "CASH1", "withdrawal", day(40), -25000, "", "Outgoing transfer", ""},
		// Different source, different account kind, same day, same
		// amount: the receiving half.
		txn{"other-bank", "T-FUND-IN", "BRK2", "deposit", day(40), 25000, "", "Funds received", ""},
		// A real purchase alongside it, so the assertion below is "the
		// base kept what it should" rather than "the base is empty".
		txn{"bank", "T-SPEND", "CARD1", "purchase", day(40), -60, "Corner Market", "", ""},
	)

	res := runPass(t, db, ctx, Options{})

	// The property that matters, asserted first: the movement is out of
	// the spending base and the purchase beside it is not.
	base := spendingBaseIDs(t, db, ctx)
	if _, ok := base["T-FUND-OUT"]; ok {
		t.Error("the outgoing leg is still in the spending base; an own-account move is not spend")
	}
	if _, ok := base["T-SPEND"]; !ok {
		t.Error("the purchase fell out of the spending base")
	}
	if len(base) != 1 {
		t.Errorf("spending base = %d rows, want 1 (the purchase alone)", len(base))
	}

	if res.MatcherRows != 2 {
		t.Errorf("MatcherRows = %d, want 2 (both legs of the movement)", res.MatcherRows)
	}
	if detailed, provenance := verdictOf(t, db, ctx, "bank", "T-FUND-OUT"); detailed !=
		canonical.SpendDetailedInternalTransfer || provenance != ProvenanceMatcher {
		t.Errorf("the outgoing leg = (%q, %q), want internal_transfer via matcher", detailed, provenance)
	}

	// The receiving leg is outside the enrichment population entirely —
	// a credit on a brokerage account is not a spending row and never
	// becomes one — and still carries the verdict. An out-of-population
	// leg has a home in the overlay because the table is keyed by
	// transaction alone.
	if inEnrichmentPopulation(t, db, ctx, "other-bank", "T-FUND-IN") {
		t.Error("the receiving leg reached the enrichment population; only the pool should hold it")
	}
	if detailed, provenance := verdictOf(t, db, ctx, "other-bank", "T-FUND-IN"); detailed !=
		canonical.SpendDetailedInternalTransfer || provenance != ProvenanceMatcher {
		t.Errorf("the receiving leg = (%q, %q), want internal_transfer via matcher", detailed, provenance)
	}

	// The wider pool must not cost the pass its idempotence: the legs
	// it reaches only through the pool are re-derived like every other.
	before := enrichmentSnapshot(t, db, ctx)
	second := runPass(t, db, ctx, Options{})
	if after := enrichmentSnapshot(t, db, ctx); strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("overlay changed on a second pass:\nfirst:\n%s\nsecond:\n%s",
			strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
	if *second != *res {
		t.Errorf("pass result changed on re-run: %+v then %+v", *res, *second)
	}
}

// TestPassExcludesASameAccountRoundTrip pins the knob the spending
// caller turns on and the returns caller never does. A withdrawal and a
// deposit of the same amount, on the same day, on the same scoped
// account is a round trip that nets to zero — a transfer bounced back,
// a reversal booked as its own line — and left unpaired its outgoing
// half counts as spending. The second half of the test runs the shared
// core over the very same pool with AllowSameOwner off, which is the
// proof that the knob, and nothing else, is what pairs them.
func TestPassExcludesASameAccountRoundTrip(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T-RT-OUT", "CASH1", "withdrawal", day(60), -800, "", "Outgoing transfer", ""},
		txn{"bank", "T-RT-BACK", "CASH1", "deposit", day(60), 800, "", "Returned transfer", ""},
		// A real purchase beside it, so the base is "kept what it
		// should" rather than empty.
		txn{"bank", "T-SPEND", "CARD1", "purchase", day(60), -45, "Corner Market", "", ""},
	)

	res := runPass(t, db, ctx, Options{})

	base := spendingBaseIDs(t, db, ctx)
	if _, ok := base["T-RT-OUT"]; ok {
		t.Error("the outgoing half of a same-account round trip is still in the spending base")
	}
	if _, ok := base["T-SPEND"]; !ok {
		t.Error("the purchase fell out of the spending base")
	}
	if res.MatcherRows != 2 {
		t.Errorf("MatcherRows = %d, want 2 (both halves of the round trip)", res.MatcherRows)
	}
	for _, id := range []string{"T-RT-OUT", "T-RT-BACK"} {
		if detailed, prov := verdictOf(t, db, ctx, "bank", id); detailed !=
			canonical.SpendDetailedInternalTransfer || prov != ProvenanceMatcher {
			t.Errorf("%s = (%q, %q), want internal_transfer via matcher", id, detailed, prov)
		}
	}

	// Same pool, same knobs, AllowSameOwner off: no pair. This is the
	// returns caller's setting, and it is why that engine is unchanged.
	legs, _, err := loadMatcherPool(ctx, db)
	if err != nil {
		t.Fatalf("loadMatcherPool: %v", err)
	}
	off := gold.TransferMatchOpts{WindowDays: 5, TolerancePct: 0.5}
	if got := gold.MatchTransferLegs(legs, off); len(got) != 0 {
		t.Errorf("with AllowSameOwner off the round trip paired anyway: %+v", got)
	}
	on := off
	on.AllowSameOwner = true
	if got := gold.MatchTransferLegs(legs, on); len(got) != 1 {
		t.Errorf("with AllowSameOwner on the round trip must pair once, got %+v", got)
	}
}

// TestPassAppliesConfigRules pins the config-supplied rules end to
// end: a matching withdrawal is placed by the rule tier with the
// category the rule names — leaving the spending base for a delta,
// staying in it for a consumption category; a row that matches
// nothing is untouched; the matcher still outranks a rule; a built-in
// rule is not overridden by one; and the pass stays idempotent with
// rules in play. Without rules the same rows are ordinary backlog,
// which is the "empty list changes nothing" half. Every name and
// entity is invented.
func TestPassAppliesConfigRules(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		// Own-name wire to an untracked bank: the description carries
		// the holder, the counterparty is empty. Mixed case on purpose.
		txn{"bank", "T-OWN-WIRE", "CASH1", "withdrawal", day(70), -3000, "", "Wire transfer to Sample Holder", ""},
		// A transfer to a tracked exchange: the counterparty carries
		// the legal entity.
		txn{"bank", "T-EXCHANGE", "CASH1", "withdrawal", day(71), -500, "Example Exchange Ltd", "", ""},
		// A subscription the bank books as a plain withdrawal: capital
		// deployed to a destination gold does not track.
		txn{"bank", "T-SUBSCR", "CASH1", "withdrawal", day(71), -2500, "", "Subscription Example Ventures Fund II", ""},
		// A real purchase with a provider label: matches no rule.
		txn{"bank", "T-SPEND", "CARD1", "purchase", day(70), -60, "Corner Market", "", "Groceries"},
		// A withdrawal that matches a rule AND has its counter-leg in
		// gold: evidence outranks the rule.
		txn{"bank", "T-MATCH-OUT", "CASH1", "withdrawal", day(72), -1200, "", "Transfer to Sample Holder savings", ""},
		txn{"other-bank", "T-MATCH-IN", "CASH2", "deposit", day(72), 1200, "", "Incoming transfer", ""},
		// An ATM withdrawal that happens to carry the holder's name: the
		// built-in rule fires first and the config rule does not
		// re-label it.
		txn{"bank", "T-ATM", "CASH1", "withdrawal", day(73), -100, "ATM Main Street Sample Holder", "", ""},
		// Consumption paid by wire — a lawyer's invoice. The narrative
		// is rail-shaped, so the fence keeps it from the model; a
		// config rule placing a vendored category is its local route,
		// and the row stays in the spending base.
		txn{"bank", "T-LAWYER", "CASH1", "withdrawal", day(74), -800, "", "SEPA transfer Example Law Office invoice", ""},
	)
	rules := []Rule{
		{regexp.MustCompile(`(?i)SAMPLE HOLDER`), canonical.SpendDetailedInternalTransfer},
		{regexp.MustCompile(`(?i)EXAMPLE EXCHANGE LTD`), canonical.SpendDetailedInternalTransfer},
		{regexp.MustCompile(`(?i)EXAMPLE VENTURES FUND`), canonical.SpendDetailedInvestment},
		{regexp.MustCompile(`(?i)EXAMPLE LAW OFFICE`), "GENERAL_SERVICES_CONSULTING_AND_LEGAL"},
	}

	// No rules: the own-name wire, the exchange transfer and the
	// subscription are unplaced backlog and count as spending.
	runPass(t, db, ctx, Options{})
	base := spendingBaseIDs(t, db, ctx)
	for _, id := range []string{"T-OWN-WIRE", "T-EXCHANGE", "T-SUBSCR", "T-SPEND", "T-LAWYER"} {
		if _, ok := base[id]; !ok {
			t.Errorf("without rules %s should be in the spending base", id)
		}
	}
	if detailed, prov := verdictOf(t, db, ctx, "bank", "T-OWN-WIRE"); detailed != "" || prov != ProvenanceSignatureOnly {
		t.Errorf("without rules T-OWN-WIRE = (%q, %q), want unplaced backlog", detailed, prov)
	}

	res := runPass(t, db, ctx, Options{Rules: rules})

	base = spendingBaseIDs(t, db, ctx)
	if _, ok := base["T-SPEND"]; !ok {
		t.Error("the purchase fell out of the spending base")
	}
	if _, ok := base["T-LAWYER"]; !ok {
		t.Error("the wire to the lawyer fell out of the spending base; a consumption category placed by a rule is still spend")
	}
	for _, id := range []string{"T-OWN-WIRE", "T-EXCHANGE", "T-SUBSCR", "T-MATCH-OUT"} {
		if _, ok := base[id]; ok {
			t.Errorf("%s is still in the spending base; own-money movement and capital deployed are not spend", id)
		}
	}
	if _, ok := base["T-ATM"]; !ok {
		t.Error("the ATM withdrawal left the spending base; a config rule must not override a built-in rule")
	}
	for _, tc := range []struct{ source, id, detailed, provenance string }{
		{"bank", "T-OWN-WIRE", canonical.SpendDetailedInternalTransfer, ProvenanceRule},
		{"bank", "T-EXCHANGE", canonical.SpendDetailedInternalTransfer, ProvenanceRule},
		{"bank", "T-SUBSCR", canonical.SpendDetailedInvestment, ProvenanceRule},
		{"bank", "T-LAWYER", "GENERAL_SERVICES_CONSULTING_AND_LEGAL", ProvenanceRule},
		{"bank", "T-SPEND", "FOOD_AND_DRINK_GROCERIES", ProvenanceProvider},
		{"bank", "T-MATCH-OUT", canonical.SpendDetailedInternalTransfer, ProvenanceMatcher},
		{"other-bank", "T-MATCH-IN", canonical.SpendDetailedInternalTransfer, ProvenanceMatcher},
		{"bank", "T-ATM", canonical.SpendDetailedCashWithdrawal, ProvenanceRule},
	} {
		detailed, provenance := verdictOf(t, db, ctx, tc.source, tc.id)
		if detailed != tc.detailed || provenance != tc.provenance {
			t.Errorf("%s = (%q, %q), want (%q, %q)", tc.id, detailed, provenance, tc.detailed, tc.provenance)
		}
	}
	if res.RuleRows != 5 || res.MatcherRows != 2 || res.ProviderRows != 1 {
		t.Errorf("tier counts = rule %d, matcher %d, provider %d; want 5, 2, 1",
			res.RuleRows, res.MatcherRows, res.ProviderRows)
	}

	before := enrichmentSnapshot(t, db, ctx)
	second := runPass(t, db, ctx, Options{Rules: rules})
	if after := enrichmentSnapshot(t, db, ctx); strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("overlay changed on a second pass with rules:\nfirst:\n%s\nsecond:\n%s",
			strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
	if *second != *res {
		t.Errorf("pass result changed on re-run: %+v then %+v", *res, *second)
	}
}

// TestPassAppliesPins pins the ledger end to end. A pin resolves its
// account by nickname or by id; it applies to every row it describes,
// so two identical bare withdrawals both take it; it beats a matcher
// verdict, since it sits above every tier; a pin that describes
// nothing — a day with no such row, a source gold has not seen — is
// counted and is not an error; the pass is idempotent with pins in
// play; and removing a pin from the ledger removes its effect on the
// next pass, because the pass owns `manual` rows the way it owns the
// derived ones. Every id and amount is invented.
func TestPassAppliesPins(t *testing.T) {
	db, ctx := openGold(t)
	if _, err := db.ExecContext(ctx, `
        UPDATE accounts SET nickname = 'Everyday Cash'
         WHERE silver_source_id = 'bank' AND account_external_id = 'CASH1'`); err != nil {
		t.Fatalf("nickname the cash account: %v", err)
	}
	seedTxns(t, db, ctx,
		// Two identical bare withdrawals: the two legs of a roll, no
		// descriptor, no counter-leg. Indistinguishable by design.
		txn{"bank", "T-ROLL-1", "CASH1", "withdrawal", day(80), -2500, "", "", ""},
		txn{"bank", "T-ROLL-2", "CASH1", "withdrawal", day(80), -2500, "", "", ""},
		// A withdrawal the matcher pairs with a deposit elsewhere — and
		// the holder knows it was a payment that merely coincided.
		txn{"bank", "T-PAID", "CASH1", "withdrawal", day(81), -1200, "", "Transfer", ""},
		txn{"other-bank", "T-LANDED", "CASH2", "deposit", day(81), 1200, "", "Incoming", ""},
		// Untouched by any pin.
		txn{"bank", "T-SPEND", "CARD1", "purchase", day(80), -45, "Corner Market", "", "Groceries"},
	)
	pins := []Pin{
		// By nickname. The amount is what a statement shows.
		{Source: "bank", Account: "Everyday Cash", Day: day(80), Amount: -2500, Currency: "USD",
			Detailed: canonical.SpendDetailedInvestment},
		// By id, a vendored category, and within a cent of the stored
		// amount rather than equal to it.
		{Source: "bank", Account: "CASH1", Day: day(81), Amount: -1200.004, Currency: "USD",
			Detailed: "GENERAL_SERVICES_CONSULTING_AND_LEGAL"},
		// Nothing on that day.
		{Source: "bank", Account: "CASH1", Day: day(90), Amount: -1, Currency: "USD",
			Detailed: canonical.SpendDetailedOther},
		// A source gold has not seen: no accounts, nothing to resolve.
		{Source: "not-loaded", Account: "Whatever", Day: day(80), Amount: -1, Currency: "USD",
			Detailed: canonical.SpendDetailedOther},
	}

	res := runPass(t, db, ctx, Options{Pins: pins})

	base := spendingBaseIDs(t, db, ctx)
	for _, id := range []string{"T-ROLL-1", "T-ROLL-2"} {
		if _, ok := base[id]; ok {
			t.Errorf("%s is still in the spending base; a pinned investment is not spend", id)
		}
	}
	for _, id := range []string{"T-PAID", "T-SPEND"} {
		if _, ok := base[id]; !ok {
			t.Errorf("%s fell out of the spending base", id)
		}
	}
	for _, tc := range []struct{ source, id, detailed, provenance string }{
		{"bank", "T-ROLL-1", canonical.SpendDetailedInvestment, ProvenanceManual},
		{"bank", "T-ROLL-2", canonical.SpendDetailedInvestment, ProvenanceManual},
		{"bank", "T-PAID", "GENERAL_SERVICES_CONSULTING_AND_LEGAL", ProvenanceManual},
		// The far leg keeps the matcher's verdict: a pin is per transaction.
		{"other-bank", "T-LANDED", canonical.SpendDetailedInternalTransfer, ProvenanceMatcher},
		{"bank", "T-SPEND", "FOOD_AND_DRINK_GROCERIES", ProvenanceProvider},
	} {
		detailed, provenance := verdictOf(t, db, ctx, tc.source, tc.id)
		if detailed != tc.detailed || provenance != tc.provenance {
			t.Errorf("%s = (%q, %q), want (%q, %q)", tc.id, detailed, provenance, tc.detailed, tc.provenance)
		}
	}
	if res.PinRows != 3 || res.UnmatchedPins != 2 || res.MatcherRows != 1 || res.ProviderRows != 1 {
		t.Errorf("counts = pinned %d, unmatched %d, matcher %d, provider %d; want 3, 2, 1, 1",
			res.PinRows, res.UnmatchedPins, res.MatcherRows, res.ProviderRows)
	}

	before := enrichmentSnapshot(t, db, ctx)
	second := runPass(t, db, ctx, Options{Pins: pins})
	if after := enrichmentSnapshot(t, db, ctx); strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("overlay changed on a second pass with pins:\nfirst:\n%s\nsecond:\n%s",
			strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
	if *second != *res {
		t.Errorf("pass result changed on re-run: %+v then %+v", *res, *second)
	}

	// The ledger is emptied: every pinned verdict is gone on the next
	// pass, and the rows fall back to what the tiers below say.
	runPass(t, db, ctx, Options{})
	base = spendingBaseIDs(t, db, ctx)
	if _, ok := base["T-ROLL-1"]; !ok {
		t.Error("with the pin removed T-ROLL-1 should be back in the spending base")
	}
	if detailed, prov := verdictOf(t, db, ctx, "bank", "T-ROLL-1"); detailed != "" || prov != ProvenanceSignatureOnly {
		t.Errorf("with the pin removed T-ROLL-1 = (%q, %q), want unplaced backlog", detailed, prov)
	}
	if detailed, prov := verdictOf(t, db, ctx, "bank", "T-PAID"); detailed != canonical.SpendDetailedInternalTransfer || prov != ProvenanceMatcher {
		t.Errorf("with the pin removed T-PAID = (%q, %q), want the matcher's verdict back", detailed, prov)
	}
	var manual int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spend_txn_enrichment WHERE provenance = ?`, ProvenanceManual).Scan(&manual); err != nil {
		t.Fatalf("count manual rows: %v", err)
	}
	if manual != 0 {
		t.Errorf("%d manual row(s) survived a pass with no pins; the pass must own them", manual)
	}

	// An ambiguous nickname is a configuration fault, not "not loaded
	// yet", and the pass says so rather than guessing an account.
	if _, err := db.ExecContext(ctx, `
        UPDATE accounts SET nickname = 'Everyday Cash'
         WHERE silver_source_id = 'bank' AND account_external_id = 'CARD1'`); err != nil {
		t.Fatalf("duplicate the nickname: %v", err)
	}
	if _, err := RunDeterministicPass(ctx, db, Options{MatchWindowDays: 5, MatchTolerancePct: 0.5, Now: 1, Pins: pins[:1]}); err == nil {
		t.Error("a pin by an ambiguous nickname must fail the pass")
	}
}

// TestPinNearMisses fences the pin key on all four of its dimensions.
// A pin sits at the top of the precedence lattice and may place a
// category that DELETES the row from the spending base, so a key that
// reached one row too far would make arbitrary spending vanish with
// nothing to show for it. The ledger's amount tolerance is a cent and
// its day is one day, and neither the account nor the currency is
// negotiable at all: a row two cents off, a day later, on a sibling
// account or in another currency is a different transaction, and the
// pin must leave every one of them in the backlog. Every value is
// invented.
func TestPinNearMisses(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T-HIT", "CASH1", "withdrawal", day(80), -2500, "", "", ""},
		txn{"bank", "T-NEXTDAY", "CASH1", "withdrawal", day(81), -2500, "", "", ""},
		txn{"bank", "T-TWOCENTS", "CASH1", "withdrawal", day(80), -2500.02, "", "", ""},
		txn{"bank", "T-SIBLING", "CARD1", "withdrawal", day(80), -2500, "", "", ""},
	)
	// seedTxns books every row in USD, so the currency near miss needs
	// its own insert.
	if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount)
             VALUES ('bank', 'T-CHF', ?, 'CASH1', 'withdrawal', 'CHF', -2500)`, day(80)); err != nil {
		t.Fatalf("seed the second-currency row: %v", err)
	}
	pins := []Pin{{Source: "bank", Account: "CASH1", Day: day(80), Amount: -2500, Currency: "USD",
		Detailed: canonical.SpendDetailedInvestment}}

	res := runPass(t, db, ctx, Options{Pins: pins})
	if res.PinRows != 1 {
		t.Errorf("PinRows = %d, want 1 (a pin describes one identity, not its neighbourhood)", res.PinRows)
	}
	base := spendingBaseIDs(t, db, ctx)
	if _, ok := base["T-HIT"]; ok {
		t.Error("T-HIT is still in the spending base; a pinned investment is not spend")
	}
	for _, id := range []string{"T-NEXTDAY", "T-TWOCENTS", "T-SIBLING", "T-CHF"} {
		if _, ok := base[id]; !ok {
			t.Errorf("%s fell out of the spending base; the pin reached a near miss", id)
		}
		if d, p := verdictOf(t, db, ctx, "bank", id); p != ProvenanceSignatureOnly || d != "" {
			t.Errorf("%s = (%q, %q), want an unplaced backlog row", id, d, p)
		}
	}
}

// TestPassKeepsSpendingWithALookalikeCredit fences the widened pool.
// The pool spans every account, so a coincidence that would once have
// been invisible now sits right next to a real purchase: same amount,
// same day, a credit on another source's account. It must not pair,
// because a purchase is not a transfer-eligible kind — and a false
// pair does not show up as a wrong category, it deletes a spending
// line.
func TestPassKeepsSpendingWithALookalikeCredit(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T-SPEND", "CARD1", "purchase", day(50), -1200, "Ferry Road Depot", "", ""},
		txn{"other-bank", "T-COINCIDENCE", "BRK2", "deposit", day(50), 1200, "", "Unrelated credit", ""},
	)

	res := runPass(t, db, ctx, Options{})
	if res.MatcherRows != 0 {
		t.Errorf("MatcherRows = %d, want 0 (a purchase cannot be a transfer leg)", res.MatcherRows)
	}
	if detailed, prov := verdictOf(t, db, ctx, "bank", "T-SPEND"); detailed != "" || prov != ProvenanceSignatureOnly {
		t.Errorf("the purchase = (%q, %q), want unplaced backlog", detailed, prov)
	}
	if _, ok := spendingBaseIDs(t, db, ctx)["T-SPEND"]; !ok {
		t.Error("a real purchase was excluded from the spending base by a coincidental credit elsewhere")
	}
}

// TestMatcherPoolKinds pins the transfer-eligible set, which the
// spend_matcher_pool macro's WHERE clause is the only definition of.
// These five mean "cash moved into or out of an account" AND carry a
// pinned canonical sign, so every leg can be oriented; the catch-all
// and unsigned kinds are out because their sign is whatever the source
// supplied, and contribution/distribution because both are booked from
// the funding account's own perspective. A kind added here buys false
// pairs, and a false pair deletes a real spending line rather than
// mis-labelling one; a kind dropped leaves own-account moves
// one-legged, and a one-legged outgoing leg is indistinguishable from
// spending. The fixture puts one transaction of every canonical kind
// on a single account, so the pool's answer is about the kind and
// nothing else.
func TestMatcherPoolKinds(t *testing.T) {
	db, ctx := openGold(t)
	all := []canonical.TxKind{
		canonical.TxKindBuy, canonical.TxKindSell, canonical.TxKindDividend,
		canonical.TxKindCoupon, canonical.TxKindCapitalGain, canonical.TxKindInterest,
		canonical.TxKindStaking, canonical.TxKindContribution, canonical.TxKindDistribution,
		canonical.TxKindFee, canonical.TxKindTax, canonical.TxKindDeposit,
		canonical.TxKindWithdrawal, canonical.TxKindPurchase, canonical.TxKindRefund,
		canonical.TxKindCardPayment, canonical.TxKindReward, canonical.TxKindFx,
		canonical.TxKindFxForward, canonical.TxKindFxSwap, canonical.TxKindCorporateAction,
		canonical.TxKindTransferIn, canonical.TxKindTransferOut, canonical.TxKindJournal,
		canonical.TxKindOther,
	}
	rows := make([]txn, 0, len(all))
	for i, k := range all {
		rows = append(rows, txn{
			source: "bank", id: "T-" + string(k), account: "CASH1", kind: string(k),
			occurredAt: day(int64(i)), amount: -100,
		})
	}
	seedTxns(t, db, ctx, rows...)

	want := map[canonical.TxKind]bool{
		canonical.TxKindDeposit:     true,
		canonical.TxKindWithdrawal:  true,
		canonical.TxKindCardPayment: true,
		canonical.TxKindTransferIn:  true,
		canonical.TxKindTransferOut: true,
	}
	poolRows, err := db.QueryContext(ctx,
		`SELECT DISTINCT kind FROM spend_matcher_pool(0, ?)`, gold.MaxEpoch)
	if err != nil {
		t.Fatalf("read matcher pool kinds: %v", err)
	}
	defer poolRows.Close()
	got := map[canonical.TxKind]bool{}
	for poolRows.Next() {
		var kind string
		if err := poolRows.Scan(&kind); err != nil {
			t.Fatalf("scan matcher pool kind: %v", err)
		}
		got[canonical.TxKind(kind)] = true
	}
	if err := poolRows.Err(); err != nil {
		t.Fatalf("iterate matcher pool kinds: %v", err)
	}
	for k := range want {
		if !got[k] {
			t.Errorf("kind %q is transfer-eligible but the macro keeps it out of the pool", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("kind %q reaches the pool and is not transfer-eligible", k)
		}
	}
}

// TestMatcherPoolDropsALegWithNoAmount pins what happens to a
// transfer-eligible row whose net_amount is NULL. The pool projects
// net_amount straight out of gold, where it is nullable, so an
// adapter that books a movement without one lands a leg here that
// cannot be oriented: it can neither fund nor be funded, and it is
// dropped before pairing. It still belongs to the enrichment
// population, so it keeps its signature and its place in the backlog.
//
// The counter-leg is a debit a cent away from zero, which is what
// makes the test sensitive: admit the null leg as an amount of zero
// and it reads as a credit whose gap to that debit is exactly the
// matcher's absolute floor, so the pair forms and both rows leave the
// spending base. Every value is invented.
func TestMatcherPoolDropsALegWithNoAmount(t *testing.T) {
	db, ctx := openGold(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount,
                                  counterparty, description)
             VALUES ('bank',       'W-NULL', ?, 'CASH1', 'withdrawal', 'USD', NULL,  NULL, 'TRANSFER TO SAVINGS'),
                    ('other-bank', 'W-TINY', ?, 'CASH2', 'withdrawal', 'USD', -0.01, NULL, 'DUST')`,
		day(0), day(0)); err != nil {
		t.Fatalf("seed legs: %v", err)
	}

	res := runPass(t, db, ctx, Options{})
	if res.MatcherRows != 0 {
		t.Errorf("MatcherRows = %d, want 0: a leg with no amount cannot be oriented", res.MatcherRows)
	}
	if got := signatureOf(t, db, ctx, "bank", "W-NULL"); got != "TRANSFER TO SAVINGS" {
		t.Errorf("W-NULL signature = %q, want the narrative's own key", got)
	}
	if detailed, prov := verdictOf(t, db, ctx, "bank", "W-NULL"); detailed != "" || prov != ProvenanceSignatureOnly {
		t.Errorf("W-NULL = (%q, %q), want an unplaced backlog row", detailed, prov)
	}
	base := spendingBaseIDs(t, db, ctx)
	for _, id := range []string{"W-NULL", "W-TINY"} {
		if _, ok := base[id]; !ok {
			t.Errorf("%s fell out of the spending base; the unoriented leg paired", id)
		}
	}
}

// TestPassAbsorbsALateCounterLeg is the case a per-source incremental
// pass would get wrong: a withdrawal loaded on its own reads as
// spending, and only becomes an own-account move when the OTHER
// source's leg arrives. A full re-assert changes its mind; an
// incremental one would never revisit it.
func TestPassAbsorbsALateCounterLeg(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T-OUT", "CASH1", "withdrawal", day(30), -1500, "Ferry Road Depot", "", ""})
	runPass(t, db, ctx, Options{})
	if detailed, prov := verdictOf(t, db, ctx, "bank", "T-OUT"); detailed != "" || prov != ProvenanceSignatureOnly {
		t.Fatalf("one-legged withdrawal = (%q, %q), want unplaced backlog", detailed, prov)
	}

	seedTxns(t, db, ctx,
		txn{"other-bank", "T-IN", "CASH2", "deposit", day(31), 1500, "", "", ""})
	runPass(t, db, ctx, Options{})
	if detailed, prov := verdictOf(t, db, ctx, "bank", "T-OUT"); detailed != canonical.SpendDetailedInternalTransfer ||
		prov != ProvenanceMatcher {
		t.Errorf("after the counter-leg landed, T-OUT = (%q, %q), want internal_transfer via matcher",
			detailed, prov)
	}
}

// TestPassRekeysMerchantVerdicts pins the one thing in the overlay that
// cost money. The fixture stands in for a SignatureVersion bump: an
// older-version enrichment row hangs a verdict off a signature the
// current Normalize no longer produces. The verdict must move to the
// new key, keeping the model that produced it, instead of being
// orphaned and bought again.
func TestPassRekeysMerchantVerdicts(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T1", "CARD1", "purchase", day(10), -50, "Corner Market", "", ""},
		txn{"bank", "T2", "CARD1", "purchase", day(11), -60, "Blue Harbour Cafe", "", ""})
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, assigned_at)
             VALUES ('bank', 'T1', 'CORNER MKT OLD', 0, NULL, 'signature-only', 100),
                    ('bank', 'T2', 'HARBOUR OLD',    0, NULL, 'signature-only', 100);

        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name)
             VALUES ('CORNER MKT OLD', 'Corner Market', 'FOOD_AND_DRINK_GROCERIES', 0, 100, 'test-model'),
                    -- Already current-version: a moved signature here is a
                    -- data change, not an algorithm change, so nothing is
                    -- carried forward.
                    ('HARBOUR OLD', 'Blue Harbour Cafe', 'FOOD_AND_DRINK_COFFEE',
                     `+fmt.Sprint(SignatureVersion)+`, 100, 'test-model');
    `); err != nil {
		t.Fatalf("seed old-version overlay: %v", err)
	}

	res := runPass(t, db, ctx, Options{})
	if res.RekeyedMerchants != 1 {
		t.Errorf("RekeyedMerchants = %d, want 1", res.RekeyedMerchants)
	}

	newSig := Normalize("Corner Market", "")
	var (
		name, detailed, model string
		version               int
	)
	if err := db.QueryRowContext(ctx, `
        SELECT merchant_name, spend_detailed, signature_version, model_name
          FROM spend_merchant_categories WHERE merchant_signature = ?`, newSig).
		Scan(&name, &detailed, &version, &model); err != nil {
		t.Fatalf("carried verdict at %q: %v", newSig, err)
	}
	if name != "Corner Market" || detailed != "FOOD_AND_DRINK_GROCERIES" || model != "test-model" {
		t.Errorf("carried verdict = (%q, %q, model %q), want the original preserved", name, detailed, model)
	}
	if version != SignatureVersion {
		t.Errorf("carried verdict signature_version = %d, want %d", version, SignatureVersion)
	}

	var carriedHarbour int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spend_merchant_categories WHERE merchant_signature = ?`,
		Normalize("Blue Harbour Cafe", "")).Scan(&carriedHarbour); err != nil {
		t.Fatalf("count harbour verdicts: %v", err)
	}
	if carriedHarbour != 0 {
		t.Errorf("a current-version verdict was carried forward; only OLDER versions re-key")
	}

	// The row now resolves through the carried verdict, which is what
	// makes the carry worth doing at all.
	var resolved sql.NullString
	if err := db.QueryRowContext(ctx, `
        SELECT spend_detailed FROM spending_lines_base(0, 9223372036854775807)
         WHERE transaction_external_id = 'T1'`).Scan(&resolved); err != nil {
		t.Fatalf("resolve T1: %v", err)
	}
	if resolved.String != "FOOD_AND_DRINK_GROCERIES" {
		t.Errorf("T1 resolves to %q, want the carried verdict", resolved.String)
	}
}

// TestPassRekeyLeavesAnExistingNewKeyVerdictAlone pins the carry's
// last condition: a verdict already standing at the NEW key is never
// overwritten, so a real re-categorisation is not undone by a stale
// copy left at an old key. The condition is also what keeps the
// re-key from raising: the merchant store is keyed by signature, so
// inserting a second row at a key that already holds one violates the
// primary key, the pass rolls back and the load fails outright. With
// the guard removed this fixture is that failure, and runPass fails
// the test on it. Every value is invented.
func TestPassRekeyLeavesAnExistingNewKeyVerdictAlone(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T1", "CARD1", "purchase", day(10), -50, "Corner Market", "", ""})
	newSig := Normalize("Corner Market", "")
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, assigned_at)
             VALUES ('bank', 'T1', 'CORNER MKT OLD', 1, NULL, 'signature-only', 100)`); err != nil {
		t.Fatalf("seed old-version overlay: %v", err)
	}
	// The standing verdict at the new key is current-version; the
	// stale one at the old key is what a bump leaves behind.
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name)
             VALUES ('CORNER MKT OLD', 'Corner Market', 'FOOD_AND_DRINK_GROCERIES', 1, 100, 'old-model'),
                    (?, 'Corner Market', 'FOOD_AND_DRINK_COFFEE', ?, 200, 'new-model')`,
		newSig, SignatureVersion); err != nil {
		t.Fatalf("seed merchant store: %v", err)
	}

	res := runPass(t, db, ctx, Options{})
	if res.RekeyedMerchants != 0 {
		t.Errorf("RekeyedMerchants = %d, want 0: the new key already holds a verdict", res.RekeyedMerchants)
	}
	var model, detailed string
	if err := db.QueryRowContext(ctx, `
        SELECT model_name, spend_detailed FROM spend_merchant_categories
         WHERE merchant_signature = ?`, newSig).Scan(&model, &detailed); err != nil {
		t.Fatalf("verdict at the new key: %v", err)
	}
	if model != "new-model" || detailed != "FOOD_AND_DRINK_COFFEE" {
		t.Errorf("verdict at the new key = (%q, %q); a standing verdict is never overwritten", model, detailed)
	}
	// The stale copy stays where it was, for --forget to clear.
	if got := storedVerdictCount(t, db, ctx, "CORNER MKT OLD"); got != 1 {
		t.Errorf("the old-key verdict holds %d row(s), want it left behind untouched", got)
	}
	// And the row resolves through the standing verdict, not the stale one.
	var resolved sql.NullString
	if err := db.QueryRowContext(ctx, `
        SELECT spend_detailed FROM spending_lines_base(0, ?)
         WHERE transaction_external_id = 'T1'`, gold.MaxEpoch).Scan(&resolved); err != nil {
		t.Fatalf("resolve T1: %v", err)
	}
	if resolved.String != "FOOD_AND_DRINK_COFFEE" {
		t.Errorf("T1 resolves to %q, want the standing verdict", resolved.String)
	}
}

// TestPassRekeyLeavesASplitVerdictBehind pins the carry's one-to-one
// condition, on the case that motivates it. One old-version signature —
// the direct-debit notice version 2 strips — covered rows from two
// creditors, and a verdict was bought at the notice itself. Under
// the current rules those rows split onto two real merchants, and the
// notice's verdict must follow NEITHER: it was about the notice, not about
// a telecom. A third row whose old key moves whole is carried beside
// the split, so the two cases are seen to be decided per old key. The
// split verdict stays in the store, unreachable, for --forget to clear.
func TestPassRekeyLeavesASplitVerdictBehind(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T1", "CASH1", "withdrawal", day(10), -120, "Northwind Telecom", "", ""},
		txn{"bank", "T2", "CASH1", "withdrawal", day(11), -300, "Harbour Insurance", "", ""},
		txn{"bank", "T3", "CARD1", "purchase", day(12), -50, "Corner Market", "", ""})
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, assigned_at)
             VALUES ('bank', 'T1', 'NOTICE OLD',     1, NULL, 'signature-only', 100),
                    ('bank', 'T2', 'NOTICE OLD',     1, NULL, 'signature-only', 100),
                    ('bank', 'T3', 'CORNER MKT OLD', 1, NULL, 'signature-only', 100);

        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name)
             VALUES ('NOTICE OLD',     'Bank Notice',   'BANK_FEES_OTHER_BANK_FEES', 1, 100, 'test-model'),
                    ('CORNER MKT OLD', 'Corner Market', 'FOOD_AND_DRINK_GROCERIES',  1, 100, 'test-model');
    `); err != nil {
		t.Fatalf("seed old-version overlay: %v", err)
	}

	res := runPass(t, db, ctx, Options{})
	if res.RekeyedMerchants != 1 {
		t.Errorf("RekeyedMerchants = %d, want 1 (the one-to-one move beside the split)", res.RekeyedMerchants)
	}
	if res.SplitMerchants != 1 {
		t.Errorf("SplitMerchants = %d, want 1", res.SplitMerchants)
	}

	for _, creditor := range []string{"Northwind Telecom", "Harbour Insurance"} {
		if got := storedVerdictCount(t, db, ctx, Normalize(creditor, "")); got != 0 {
			t.Errorf("%q holds %d verdict(s); the notice's verdict must be carried onto neither creditor", creditor, got)
		}
	}
	if got := storedVerdictCount(t, db, ctx, Normalize("Corner Market", "")); got != 1 {
		t.Errorf("Corner Market holds %d verdict(s), want 1: a whole move still carries", got)
	}
	if got := storedVerdictCount(t, db, ctx, "NOTICE OLD"); got != 1 {
		t.Errorf("the split verdict was removed from the store; it is left behind, not deleted")
	}

	// Both creditors are unplaced, which is what puts them back in the
	// backlog for the next categorize.
	for _, id := range []string{"T1", "T2"} {
		var resolved sql.NullString
		if err := db.QueryRowContext(ctx, `
            SELECT spend_detailed FROM spending_lines_base(0, 9223372036854775807)
             WHERE transaction_external_id = ?`, id).Scan(&resolved); err != nil {
			t.Fatalf("resolve %s: %v", id, err)
		}
		if resolved.Valid {
			t.Errorf("%s resolves to %q, want unplaced", id, resolved.String)
		}
	}
}

// TestPassRekeyLeavesTheEbillMarkersBehind pins the split guard on
// the case SignatureVersion 4 records. Version 3 keys an e-bill on
// the UBS adapter's statement era by its counterparty, the rail
// marker, so its rows share one key per spelling of the marker, and
// a verdict bought at such a key is about the rail, not about any
// creditor behind it. Under the current rules
// those rows split onto their creditors, so each marker verdict is
// carried onto none of them and left where it is, counted; a
// purchase whose old key moves whole is carried beside them, so the
// two cases are seen to be decided per old key. Every value is
// synthetic; the STRUCTURE is the bank's.
func TestPassRekeyLeavesTheEbillMarkersBehind(t *testing.T) {
	db, ctx := openGold(t)
	const tail = "; CH EXAMPLETOWN 9999; QRR; 000000000000000000000000000; 1 times E-Banking domestic"
	ebill := func(id string, d int64, marker, creditor string) txn {
		return txn{"bank", id, "CASH1", "withdrawal", day(d), -100 - float64(d), marker,
			"PAYNET ORDER; " + marker + "; " + creditor + tail, ""}
	}
	seedTxns(t, db, ctx,
		ebill("T1", 10, "EBILL-RECHNUNG", "Northwind Telecom AG"),
		ebill("T2", 11, "EBILL-RECHNUNG", "Harbour Insurance AG"),
		ebill("T3", 12, "EBILL INVOICE", "Example Energy AG"),
		ebill("T4", 13, "EBILL INVOICE", "Example Fuel AG"),
		ebill("T5", 14, "E-BILL", "Example Water AG"),
		ebill("T6", 15, "E-BILL", "Example Clinic AG"),
		txn{"bank", "T7", "CARD1", "purchase", day(16), -50, "Corner Market", "", ""})
	// The version 3 keys: the marker, reduced, whatever the creditor.
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, assigned_at)
             VALUES ('bank', 'T1', 'EBILL RECHNUNG', 3, NULL, 'signature-only', 100),
                    ('bank', 'T2', 'EBILL RECHNUNG', 3, NULL, 'signature-only', 100),
                    ('bank', 'T3', 'EBILL INVOICE',  3, NULL, 'signature-only', 100),
                    ('bank', 'T4', 'EBILL INVOICE',  3, NULL, 'signature-only', 100),
                    ('bank', 'T5', 'E BILL',         3, NULL, 'signature-only', 100),
                    ('bank', 'T6', 'E BILL',         3, NULL, 'signature-only', 100),
                    ('bank', 'T7', 'CORNER MKT OLD', 3, NULL, 'signature-only', 100);

        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name)
             VALUES ('EBILL RECHNUNG', 'E-Bill',        'BANK_FEES_OTHER_BANK_FEES', 3, 100, 'test-model'),
                    ('EBILL INVOICE',  'E-Bill',        'BANK_FEES_OTHER_BANK_FEES', 3, 100, 'test-model'),
                    ('E BILL',         'E-Bill',        'BANK_FEES_OTHER_BANK_FEES', 3, 100, 'test-model'),
                    ('CORNER MKT OLD', 'Corner Market', 'FOOD_AND_DRINK_GROCERIES',  3, 100, 'test-model');
    `); err != nil {
		t.Fatalf("seed old-version overlay: %v", err)
	}

	res := runPass(t, db, ctx, Options{})
	if res.RekeyedMerchants != 1 {
		t.Errorf("RekeyedMerchants = %d, want 1 (the one-to-one move beside the splits)", res.RekeyedMerchants)
	}
	if res.SplitMerchants != 3 {
		t.Errorf("SplitMerchants = %d, want 3 (one per spelling of the marker)", res.SplitMerchants)
	}

	for _, tc := range []struct{ id, creditor string }{
		{"T1", "NORTHWIND TELECOM AG"}, {"T2", "HARBOUR INSURANCE AG"},
		{"T3", "EXAMPLE ENERGY AG"}, {"T4", "EXAMPLE FUEL AG"},
		{"T5", "EXAMPLE WATER AG"}, {"T6", "EXAMPLE CLINIC AG"},
	} {
		want := tc.creditor + " CH EXAMPLETOWN"
		if sig := signatureOf(t, db, ctx, "bank", tc.id); sig != want {
			t.Errorf("%s signature = %q, want %q", tc.id, sig, want)
		}
		if got := storedVerdictCount(t, db, ctx, want); got != 0 {
			t.Errorf("%q holds %d verdict(s); the marker's verdict must be carried onto no creditor", want, got)
		}
		// Unplaced, which is what puts the creditor back in the backlog.
		var resolved sql.NullString
		if err := db.QueryRowContext(ctx, `
            SELECT spend_detailed FROM spending_lines_base(0, 9223372036854775807)
             WHERE transaction_external_id = ?`, tc.id).Scan(&resolved); err != nil {
			t.Fatalf("resolve %s: %v", tc.id, err)
		}
		if resolved.Valid {
			t.Errorf("%s resolves to %q, want unplaced", tc.id, resolved.String)
		}
	}
	for _, marker := range []string{"EBILL RECHNUNG", "EBILL INVOICE", "E BILL"} {
		if got := storedVerdictCount(t, db, ctx, marker); got != 1 {
			t.Errorf("the verdict at %q was removed from the store; it is left behind, not deleted", marker)
		}
	}
	if got := storedVerdictCount(t, db, ctx, Normalize("Corner Market", "")); got != 1 {
		t.Errorf("Corner Market holds %d verdict(s), want 1: a whole move still carries", got)
	}
}

// TestPassRekeyTreatsAPartialMoveAsASplit: an old key some of whose
// rows stay put while others leave is a split as well. The rows that
// stayed keep the verdict at the key it was bought at — nothing
// touches it — and the rows that left get nothing.
func TestPassRekeyTreatsAPartialMoveAsASplit(t *testing.T) {
	db, ctx := openGold(t)
	stay := Normalize("Corner Market", "")
	seedTxns(t, db, ctx,
		txn{"bank", "T1", "CARD1", "purchase", day(10), -50, "Corner Market", "", ""},
		txn{"bank", "T2", "CARD1", "purchase", day(11), -60, "Blue Harbour Cafe", "", ""})
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, assigned_at)
             VALUES ('bank', 'T1', ?, 1, NULL, 'signature-only', 100),
                    ('bank', 'T2', ?, 1, NULL, 'signature-only', 100)`, stay, stay); err != nil {
		t.Fatalf("seed old-version overlay: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name)
             VALUES (?, 'Corner Market', 'FOOD_AND_DRINK_GROCERIES', 1, 100, 'test-model')`, stay); err != nil {
		t.Fatalf("seed old-version verdict: %v", err)
	}

	res := runPass(t, db, ctx, Options{})
	if res.RekeyedMerchants != 0 || res.SplitMerchants != 1 {
		t.Errorf("(Rekeyed, Split) = (%d, %d), want (0, 1)", res.RekeyedMerchants, res.SplitMerchants)
	}
	if got := storedVerdictCount(t, db, ctx, Normalize("Blue Harbour Cafe", "")); got != 0 {
		t.Errorf("the row that left took the verdict with it")
	}
	var version int
	if err := db.QueryRowContext(ctx,
		`SELECT signature_version FROM spend_merchant_categories WHERE merchant_signature = ?`, stay).
		Scan(&version); err != nil {
		t.Fatalf("the verdict at the key that stayed: %v", err)
	}
	if version != 1 {
		t.Errorf("the verdict at the key that stayed was rewritten (version %d); it must be untouched", version)
	}
}

// TestPassWritesPastOneInsertChunk pins the overlay write across a
// chunk boundary. The rows go in as multi-row VALUES statements, and a
// bind list that drifted by a column — or a chunk that dropped or
// repeated a row at its edge — is invisible in every fixture small
// enough to fit one statement. Each seeded row carries its own
// merchant, so a shifted argument shows up as a signature filed under
// the wrong transaction.
func TestPassWritesPastOneInsertChunk(t *testing.T) {
	db, ctx := openGold(t)
	const n = gold.InsertChunkRows + 2
	rows := make([]txn, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, txn{
			source: "bank", id: fmt.Sprintf("T-%04d", i), account: "CARD1", kind: "purchase",
			occurredAt: day(int64(i % 30)), amount: -10,
			counterparty: "Example Merchant " + merchantLabel(i),
		})
	}
	seedTxns(t, db, ctx, rows...)

	res := runPass(t, db, ctx, Options{})
	if res.Population != n || res.Enriched != n {
		t.Fatalf("(Population, Enriched) = (%d, %d), want (%d, %d)",
			res.Population, res.Enriched, n, n)
	}
	// The first row, both sides of the boundary, and the last row.
	for _, i := range []int{0, gold.InsertChunkRows - 1, gold.InsertChunkRows, n - 1} {
		id := fmt.Sprintf("T-%04d", i)
		want := Normalize("Example Merchant "+merchantLabel(i), "")
		if got := signatureOf(t, db, ctx, "bank", id); got != want {
			t.Errorf("%s signature = %q, want %q", id, got, want)
		}
		if detailed, prov := verdictOf(t, db, ctx, "bank", id); detailed != "" || prov != ProvenanceSignatureOnly {
			t.Errorf("%s = (%q, %q), want unplaced backlog", id, detailed, prov)
		}
	}
}

// merchantLabel names one invented merchant per index in letters:
// digits would be read as a reference number and folded out of the
// signature, which is exactly the distinctness the test needs.
func merchantLabel(i int) string {
	return string([]rune{rune('A' + i/26%26), rune('A' + i%26)})
}

// TestPassRekeyCollapsesSeveralOldKeysOntoOne is the split's mirror
// image: several old keys whose rows now share ONE new signature. It
// is the shape a memo-split normalisation produces, where narratives
// that differed only by the payer's own words collapse onto the bank's
// booking type. Each old key moved whole, so none is a split, and the
// new key can hold only one verdict: the first in row order carries,
// the others stay where they are, uncounted and untouched, and the
// carry is not counted once per loser.
func TestPassRekeyCollapsesSeveralOldKeysOntoOne(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T1", "CARD1", "purchase", day(10), -50, "Corner Market", "", ""},
		txn{"bank", "T2", "CARD1", "purchase", day(11), -60, "Corner Market", "", ""})
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, assigned_at)
             VALUES ('bank', 'T1', 'CORNER MKT OLD A', 3, NULL, 'signature-only', 100),
                    ('bank', 'T2', 'CORNER MKT OLD B', 3, NULL, 'signature-only', 100);

        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name)
             VALUES ('CORNER MKT OLD A', 'Corner Market A', 'FOOD_AND_DRINK_GROCERIES', 3, 100, 'model-a'),
                    ('CORNER MKT OLD B', 'Corner Market B', 'FOOD_AND_DRINK_COFFEE',    3, 100, 'model-b');
    `); err != nil {
		t.Fatalf("seed old-version overlay: %v", err)
	}

	res := runPass(t, db, ctx, Options{})
	if res.RekeyedMerchants != 1 || res.SplitMerchants != 0 {
		t.Errorf("(Rekeyed, Split) = (%d, %d), want (1, 0): a collapse carries once and splits nothing",
			res.RekeyedMerchants, res.SplitMerchants)
	}

	newSig := Normalize("Corner Market", "")
	var (
		name, detailed, model string
		version               int
	)
	if err := db.QueryRowContext(ctx, `
        SELECT merchant_name, spend_detailed, signature_version, model_name
          FROM spend_merchant_categories WHERE merchant_signature = ?`, newSig).
		Scan(&name, &detailed, &version, &model); err != nil {
		t.Fatalf("carried verdict at %q: %v", newSig, err)
	}
	if name != "Corner Market A" || detailed != "FOOD_AND_DRINK_GROCERIES" || model != "model-a" {
		t.Errorf("carried verdict = (%q, %q, model %q), want the first old key's verdict verbatim",
			name, detailed, model)
	}
	if version != SignatureVersion {
		t.Errorf("carried verdict signature_version = %d, want %d", version, SignatureVersion)
	}

	// The losers stay behind, unreachable but intact — not deleted, not
	// merged, not restamped.
	for _, old := range []string{"CORNER MKT OLD A", "CORNER MKT OLD B"} {
		if got := storedVerdictCount(t, db, ctx, old); got != 1 {
			t.Errorf("%q holds %d verdict(s), want the old row left where it was", old, got)
		}
	}
	if err := db.QueryRowContext(ctx,
		`SELECT signature_version FROM spend_merchant_categories WHERE merchant_signature = 'CORNER MKT OLD B'`).
		Scan(&version); err != nil {
		t.Fatalf("the verdict that stayed behind: %v", err)
	}
	if version != 3 {
		t.Errorf("the verdict left behind was rewritten (version %d); it must be untouched", version)
	}

	// Both rows now resolve through the one carried verdict.
	for _, id := range []string{"T1", "T2"} {
		var resolved sql.NullString
		if err := db.QueryRowContext(ctx, `
            SELECT spend_detailed FROM spending_lines_base(0, 9223372036854775807)
             WHERE transaction_external_id = ?`, id).Scan(&resolved); err != nil {
			t.Fatalf("resolve %s: %v", id, err)
		}
		if resolved.String != "FOOD_AND_DRINK_GROCERIES" {
			t.Errorf("%s resolves to %q, want the carried verdict", id, resolved.String)
		}
	}

	// Safe to run every pass: the collapse is not carried a second time.
	again := runPass(t, db, ctx, Options{})
	if again.RekeyedMerchants != 0 || again.SplitMerchants != 0 {
		t.Errorf("second pass (Rekeyed, Split) = (%d, %d), want (0, 0)",
			again.RekeyedMerchants, again.SplitMerchants)
	}
}

// storedVerdictCount is how many merchant-store rows hang off one
// signature: 0 or 1, the store being keyed by it.
func storedVerdictCount(t *testing.T, db *sql.DB, ctx context.Context, signature string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spend_merchant_categories WHERE merchant_signature = ?`, signature).Scan(&n); err != nil {
		t.Fatalf("count verdicts at %q: %v", signature, err)
	}
	return n
}

// TestPassOwnsManualRows: a `manual` row is what the pins ledger
// wrote, and the ledger is config. A full re-assert therefore owns it
// like every derived row — a manual verdict with no pin behind it is a
// ghost, and the pass clears it rather than carrying a decision whose
// source is gone.
func TestPassOwnsManualRows(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T1", "CARD1", "purchase", day(10), -50, "Corner Market", "", "Groceries"})
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, assigned_at)
             VALUES ('bank', 'T1', 'HAND MADE', 1, 'TRAVEL_FLIGHTS', 'manual', 100)`); err != nil {
		t.Fatalf("seed ghost manual verdict: %v", err)
	}

	res := runPass(t, db, ctx, Options{})
	if res.PinRows != 0 {
		t.Errorf("PinRows = %d, want 0: no ledger, no pins", res.PinRows)
	}
	if detailed, provenance := verdictOf(t, db, ctx, "bank", "T1"); detailed != "FOOD_AND_DRINK_GROCERIES" || provenance != ProvenanceProvider {
		t.Errorf("T1 = (%q, %q), want the provider's verdict; a ghost manual row must not survive", detailed, provenance)
	}
}

// TestPassStampsAccountScope pins the SetFxPriorities precedent: the
// config's scope is stamped into gold, a whole re-stamp so a removed
// entry disappears, and it takes effect in the SAME pass that applies
// it — the population macros read the table the pass just wrote.
func TestPassStampsAccountScope(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T-BRK", "BRK1", "purchase", day(10), -70, "Corner Market", "", "Groceries"},
		txn{"bank", "T-CARD", "CARD1", "purchase", day(10), -50, "Corner Market", "", "Groceries"})

	res := runPass(t, db, ctx, Options{
		Include: map[string][]string{"bank": {"BRK1"}},
		Exclude: map[string][]string{"bank": {"CARD1"}},
	})
	if res.ScopeRows != 2 {
		t.Errorf("ScopeRows = %d, want 2", res.ScopeRows)
	}
	if res.Population != 1 {
		t.Errorf("Population = %d, want 1 (the included brokerage row only)", res.Population)
	}
	if got := enrichmentSnapshot(t, db, ctx); len(got) != 1 || !strings.Contains(got[0], "T-BRK") {
		t.Errorf("overlay = %v, want the pulled-in account's row alone", got)
	}

	// Dropping the overrides re-stamps the table empty and the default
	// account-kind scope applies again.
	res = runPass(t, db, ctx, Options{})
	if res.ScopeRows != 0 {
		t.Errorf("ScopeRows after clearing the config = %d, want 0", res.ScopeRows)
	}
	if res.Population != 1 {
		t.Errorf("Population after clearing the config = %d, want 1 (the card row)", res.Population)
	}
	if got := enrichmentSnapshot(t, db, ctx); len(got) != 1 || !strings.Contains(got[0], "T-CARD") {
		t.Errorf("overlay = %v, want the card row alone", got)
	}
}

// TestPassCountsUnresolvedScopeAccounts: `spending.accounts` keys on
// the account id, and the scope table joins to `accounts` on that id.
// An entry naming anything else — a nickname, a typo — is still
// stamped (the table is the config's whole state) but fences nothing,
// so it has to be counted rather than disappear.
func TestPassCountsUnresolvedScopeAccounts(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T-BRK", "BRK1", "purchase", day(10), -70, "Corner Market", "", "Groceries"},
		txn{"bank", "T-CARD", "CARD1", "purchase", day(10), -50, "Corner Market", "", "Groceries"})

	res := runPass(t, db, ctx, Options{
		// "Brokerage" is BRK1's display name, not its id; the source
		// has no account 'CARD9' at all.
		Include: map[string][]string{"bank": {"BRK1", "Brokerage"}},
		Exclude: map[string][]string{"bank": {"CARD9"}},
	})
	if res.ScopeRows != 3 {
		t.Errorf("ScopeRows = %d, want 3: every entry is stamped, resolved or not", res.ScopeRows)
	}
	if res.UnresolvedScopeAccounts != 2 {
		t.Errorf("UnresolvedScopeAccounts = %d, want 2 (the display name and the unknown id)",
			res.UnresolvedScopeAccounts)
	}
	// The resolved entries still do their work, and the unresolved
	// ones changed nothing: the pulled-in brokerage row and the
	// card row by default.
	if res.Population != 2 {
		t.Errorf("Population = %d, want 2 (the included brokerage row and the card row)", res.Population)
	}

	// An entry that resolves is not counted.
	res = runPass(t, db, ctx, Options{Include: map[string][]string{"bank": {"BRK1"}}})
	if res.UnresolvedScopeAccounts != 0 {
		t.Errorf("UnresolvedScopeAccounts = %d, want 0: every entry names a known account",
			res.UnresolvedScopeAccounts)
	}
}

// TestPassCountsUnmappedProviderCategories: the provider vocabulary
// moves, and a value nobody reviewed must be counted rather than
// guessed. The row still lands as backlog, with its signature.
func TestPassCountsUnmappedProviderCategories(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T-SEEN", "CARD1", "purchase", day(10), -50, "Corner Market", "", "Groceries"},
		txn{"bank", "T-UNSEEN", "CARD1", "purchase", day(11), -60, "Ferry Road Depot", "", "Automotive & Transit"},
		// A source with no provider map at all is not vocabulary drift.
		txn{"other-bank", "T-NOMAP", "CASH2", "fee", day(12), -5, "Account Fee", "", "Whatever"})

	res := runPass(t, db, ctx, Options{})
	if res.UnmappedProviderCategories != 1 {
		t.Errorf("UnmappedProviderCategories = %d, want 1", res.UnmappedProviderCategories)
	}
	detailed, provenance := verdictOf(t, db, ctx, "bank", "T-UNSEEN")
	if detailed != "" || provenance != ProvenanceSignatureOnly {
		t.Errorf("unmapped row = (%q, %q), want unplaced backlog with a signature", detailed, provenance)
	}
	if sig := signatureOf(t, db, ctx, "bank", "T-UNSEEN"); sig != Normalize("Ferry Road Depot", "") {
		t.Errorf("unmapped row signature = %q, want the computed signature", sig)
	}
}

// TestPassPopulationExcludesOtherKind pins the exclusion the status
// counter exists to make loud: `other` carries no reliable sign, so it
// never reaches the pass at all.
func TestPassPopulationExcludesOtherKind(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "T-OTHER", "CASH1", "other", day(10), -30, "Corner Market", "", ""},
		txn{"bank", "T-INT-POS", "CASH1", "interest", day(10), 5, "", "", ""},
		txn{"bank", "T-INT-NEG", "CARD1", "interest", day(10), -10, "", "", ""})

	res := runPass(t, db, ctx, Options{})
	if res.Population != 1 {
		t.Errorf("Population = %d, want 1 (the negative interest only)", res.Population)
	}
	ids := seenIDs(t, db, ctx)
	if _, ok := ids["T-OTHER"]; ok {
		t.Error("kind='other' reached the enrichment population")
	}
	if _, ok := ids["T-INT-POS"]; ok {
		t.Error("credited interest reached the enrichment population; it is income")
	}
	if _, ok := ids["T-INT-NEG"]; !ok {
		t.Error("a finance charge did not reach the enrichment population")
	}
}

func seenIDs(t *testing.T, db *sql.DB, ctx context.Context) map[string]struct{} {
	t.Helper()
	out := map[string]struct{}{}
	for _, line := range enrichmentSnapshot(t, db, ctx) {
		fields := strings.SplitN(strings.Fields(line)[0], "/", 2)
		out[fields[1]] = struct{}{}
	}
	return out
}

// TestSyncAccountScopeIsDeterministic guards the ordering the whole
// pass claims: two runs from the same config stamp the same rows in
// the same order, so a diff of gold after a load is empty when nothing
// changed. The rows come back in the order they were STAMPED — by
// rowid, never re-sorted here — which is the only way that order is
// observable at all: sorting the snapshot would erase the very
// property the test is named for, and the sorts in syncAccountScope
// could then be dropped with nothing failing.
func TestSyncAccountScopeIsDeterministic(t *testing.T) {
	db, ctx := openGold(t)
	include := map[string][]string{"bank": {"CARD1", "BRK1"}, "other-bank": {"CASH2"}}
	var snapshots []string
	for i := 0; i < 2; i++ {
		runPass(t, db, ctx, Options{Include: include})
		rows, err := db.QueryContext(ctx, `
            SELECT silver_source_id, account_external_id, mode FROM spend_account_scope
             ORDER BY rowid`)
		if err != nil {
			t.Fatalf("read scope: %v", err)
		}
		var got []string
		for rows.Next() {
			var src, acct, mode string
			if err := rows.Scan(&src, &acct, &mode); err != nil {
				t.Fatalf("scan scope: %v", err)
			}
			got = append(got, src+"/"+acct+"="+mode)
		}
		rows.Close()
		snapshots = append(snapshots, strings.Join(got, ","))
	}
	if snapshots[0] != snapshots[1] {
		t.Errorf("scope changed across identical passes: %q then %q", snapshots[0], snapshots[1])
	}
	want := "bank/BRK1=include,bank/CARD1=include,other-bank/CASH2=include"
	if snapshots[0] != want {
		t.Errorf("scope = %q, want %q", snapshots[0], want)
	}
}
