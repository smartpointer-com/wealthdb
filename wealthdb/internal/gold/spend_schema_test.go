package gold

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestSpendOverlayAnswers: each overlay table answers, and so does the
// spending base.
func TestSpendOverlayAnswers(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	for _, tbl := range []string{
		"spend_txn_enrichment", "spend_merchant_categories", "spend_account_scope",
	} {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tbl).Scan(&n); err != nil {
			t.Errorf("table %s: %v", tbl, err)
		}
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spending_lines_base(0, 9223372036854775807)`).Scan(&n); err != nil {
		t.Errorf("spending_lines_base: %v", err)
	}
}

// TestSpendMatcherPoolAnswers: the matcher pool answers over every
// account.
func TestSpendMatcherPoolAnswers(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spend_matcher_pool(0, 9223372036854775807)`).Scan(&n); err != nil {
		t.Errorf("spend_matcher_pool: %v", err)
	}
}

// TestInvestmentIsNotSpend: the dimension holds `investment` once, a row
// resolving to it is out of the spending base, and the backlog stays
// in.
func TestInvestmentIsNotSpend(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spend_categories WHERE spend_detailed = 'investment'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("investment rows = %d (%v), want 1", n, err)
	}
	seedSpendingFixture(t, db, ctx)
	got := macroTxnIDs(t, db, ctx, "spending_lines_base", 0, 5000)
	if _, ok := got["T-INVEST-TXN"]; ok {
		t.Error("T-INVEST-TXN is in spending_lines_base; capital deployed is not spend")
	}
	if _, ok := got["T-BACKLOG"]; !ok {
		t.Error("T-BACKLOG fell out of spending_lines_base; a NULL category must pass both exclusions")
	}
}

// TestCardSpendIsItsOwnPrimary: the dimension holds `card_spend` once,
// and a row resolving to it is in the spending base as its own primary,
// while the two excluded deltas still are not.
func TestCardSpendIsItsOwnPrimary(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spend_categories WHERE spend_detailed = 'card_spend'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("card_spend rows = %d (%v), want 1", n, err)
	}
	seedSpendingFixture(t, db, ctx)
	got := macroTxnIDs(t, db, ctx, "spending_lines_base", 0, 5000)
	if _, ok := got["T-CARD-BILL"]; !ok {
		t.Error("T-CARD-BILL is not in spending_lines_base; an unpaired card bill is spend on a card not itemised")
	}
	for _, id := range []string{"T-XFER-TXN", "T-INVEST-TXN"} {
		if _, ok := got[id]; ok {
			t.Errorf("%s is in spending_lines_base; the exclusions must survive the new delta", id)
		}
	}
	var primary sql.NullString
	if err := db.QueryRowContext(ctx, `
        SELECT spend_primary FROM spending_lines_base(0, 5000)
         WHERE transaction_external_id = 'T-CARD-BILL'`).Scan(&primary); err != nil {
		t.Fatalf("read T-CARD-BILL: %v", err)
	}
	if primary.String != "card_spend" {
		t.Errorf("T-CARD-BILL primary = %q, want card_spend (a delta is its own primary)", primary.String)
	}
}

// TestAGiftIsItsOwnPrimary: the dimension holds `gift` once, and a row
// resolving to it is in the spending base as its own primary, while the
// two excluded deltas still are not. The exclusion list names
// `internal_transfer` and `investment` alone, so the base admits a gift
// by construction. The assertion is live rather than structural: the same
// fixture and the same macro drop the two excluded rows in this very
// query.
func TestAGiftIsItsOwnPrimary(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spend_categories WHERE spend_detailed = 'gift'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("gift rows = %d (%v), want 1", n, err)
	}
	seedSpendingFixture(t, db, ctx)
	got := macroTxnIDs(t, db, ctx, "spending_lines_base", 0, 5000)
	if _, ok := got["T-GIFT"]; !ok {
		t.Error("T-GIFT is not in spending_lines_base; a cash gift is spending")
	}
	for _, id := range []string{"T-XFER-TXN", "T-INVEST-TXN"} {
		if _, ok := got[id]; ok {
			t.Errorf("%s is in spending_lines_base; the exclusions must survive the new delta", id)
		}
	}
	var primary sql.NullString
	if err := db.QueryRowContext(ctx, `
        SELECT spend_primary FROM spending_lines_base(0, 5000)
         WHERE transaction_external_id = 'T-GIFT'`).Scan(&primary); err != nil {
		t.Fatalf("read T-GIFT: %v", err)
	}
	if primary.String != "gift" {
		t.Errorf("T-GIFT primary = %q, want gift (a delta is its own primary)", primary.String)
	}
}

// TestADeltaLineCarriesNoMerchant: on this fixture the store names the
// grocery signature and carries a delta verdict on the transfer's. A gift
// whose signature the store names and a row the STORE resolved to an
// own-account move read blank; a grocery purchase reads its name,
// whichever scope answered.
func TestADeltaLineCarriesNoMerchant(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedSpendingFixture(t, db, ctx)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name) VALUES
            ('sig-person', 'Example Relative', 'GENERAL_SERVICES_OTHER_GENERAL_SERVICES', 1, 100, 'test-model')`); err != nil {
		t.Fatalf("name the gift's signature: %v", err)
	}
	want := map[string]struct{ merchant, detailed string }{
		"T-GIFT":        {"", "gift"},
		"T-XFER-MERCH":  {"", "internal_transfer"},
		"T-BY-MERCHANT": {"Corner Market", "FOOD_AND_DRINK_GROCERIES"},
		// The card bill shows the issuer its rule labels it with.
		"T-CARD-BILL": {"Example Card Issuer", "card_spend"},
	}
	for id, w := range want {
		var merchant, detailed sql.NullString
		if err := db.QueryRowContext(ctx, `
        SELECT merchant_name, spend_detailed FROM spend_txn_categories()
         WHERE transaction_external_id = ?`, id).Scan(&merchant, &detailed); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if merchant.String != w.merchant || detailed.String != w.detailed {
			t.Errorf("%s = (%q, %q), want (%q, %q)", id, merchant.String, detailed.String, w.merchant, w.detailed)
		}
	}
}

// TestAStorePlacedLineReadsModel: a line the merchant store placed reads
// 'model', which no tier writes to the overlay, while every line the pass
// placed keeps the provenance it was stamped with.
func TestAStorePlacedLineReadsModel(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedSpendingFixture(t, db, ctx)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, assigned_at)
        VALUES ('test-src', 'T-BACKLOG', 'sig-unknown', 1, NULL, 'signature-only', 100)`); err != nil {
		t.Fatalf("seed the backlog row: %v", err)
	}
	want := map[string]string{
		// The store answered a row the pass could not place.
		"T-BY-MERCHANT": "model",
		// The store answered it too, with a delta.
		"T-XFER-MERCH": "model",
		// The transaction scope answered, so the store is overruled and
		// so is its provenance.
		"T-BY-TXN": "rule",
		"T-GIFT":   "manual",
		// No scope answered: the backlog keeps the pass's own value,
		// which is what tells it apart from a store-placed line.
		"T-BACKLOG": "signature-only",
	}
	for id, w := range want {
		var provenance string
		if err := db.QueryRowContext(ctx, `
        SELECT provenance FROM spend_txn_categories()
         WHERE transaction_external_id = ?`, id).Scan(&provenance); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if provenance != w {
			t.Errorf("%s provenance = %q, want %q", id, provenance, w)
		}
	}
}

// TestACardBillShowsItsIssuer: a card bill shows the issuer the rule
// labelled it with, while every other delta line still shows nothing and
// a vendored line still shows the store's name.
func TestACardBillShowsItsIssuer(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedSpendingFixture(t, db, ctx)
	// A store name for the card bill's signature and the gift's: on a
	// delta line neither is published, whatever the store holds.
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name) VALUES
            ('sig-issuer', 'Example Holder',   'GENERAL_SERVICES_OTHER_GENERAL_SERVICES', 1, 100, 'test-model'),
            ('sig-person', 'Example Relative', 'GENERAL_SERVICES_OTHER_GENERAL_SERVICES', 1, 100, 'test-model')`); err != nil {
		t.Fatalf("name the delta signatures: %v", err)
	}
	want := map[string]struct{ merchant, detailed string }{
		// The exception: the label, not the store's name for the key.
		"T-CARD-BILL": {"Example Card Issuer", "card_spend"},
		// The rule, unchanged: a delta line carries no merchant.
		"T-GIFT":       {"", "gift"},
		"T-XFER-TXN":   {"", "internal_transfer"},
		"T-INVEST-TXN": {"", "investment"},
		"T-XFER-MERCH": {"", "internal_transfer"},
		// A vendored line still reads the store.
		"T-BY-MERCHANT": {"Corner Market", "FOOD_AND_DRINK_GROCERIES"},
	}
	for id, w := range want {
		var merchant, detailed sql.NullString
		if err := db.QueryRowContext(ctx, `
        SELECT merchant_name, spend_detailed FROM spend_txn_categories()
         WHERE transaction_external_id = ?`, id).Scan(&merchant, &detailed); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if merchant.String != w.merchant || detailed.String != w.detailed {
			t.Errorf("%s = (%q, %q), want (%q, %q)", id, merchant.String, detailed.String, w.merchant, w.detailed)
		}
	}
}

// TestSpendPopulationProjectsTheScopeColumns: the scoped accounts and the
// enrichment population answer, and the population projects the columns
// a rule scope narrows by.
func TestSpendPopulationProjectsTheScopeColumns(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedSpendingFixture(t, db, ctx)

	for _, q := range []string{
		"SELECT COUNT(*) FROM spend_scoped_accounts()",
		"SELECT COUNT(*) FROM spend_enrichment_population(0, 9999999999)",
	} {
		var n int
		if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}

	// A column the population drops would fail the rule tier, not the
	// query.
	rows, err := db.QueryContext(ctx,
		"SELECT * FROM spend_enrichment_population(0, 9999999999) LIMIT 0")
	if err != nil {
		t.Fatalf("population columns: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("population columns: %v", err)
	}
	have := map[string]bool{}
	for _, c := range cols {
		have[c] = true
	}
	for _, c := range []string{"silver_source_id", "portfolio_external_id",
		"account_external_id", "occurred_at"} {
		if !have[c] {
			t.Errorf("population is missing %q, which a rule scope narrows by", c)
		}
	}
}

// TestAMerchantFallsBackToTheSignature: a line no store row covers falls
// back to its own signature, a line the store named still reads the
// store, a delta still reads its label or nothing, and an empty signature
// renders as nothing rather than as an empty name.
func TestAMerchantFallsBackToTheSignature(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedSpendingFixture(t, db, ctx)
	// A provider-placed line, which the model tier never sees, and a
	// line whose signature came out empty.
	if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount) VALUES
            ('test-src', 'T-BY-PROVIDER', 1000, 'CARD1', 'purchase', 'USD', -130),
            ('test-src', 'T-NO-SIG',      1000, 'CARD1', 'purchase', 'USD', -140);

        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, assigned_at) VALUES
            ('test-src', 'T-BY-PROVIDER', 'EXAMPLE TRANSIT AUTHORITY', 1,
             'TRANSPORTATION_PUBLIC_TRANSIT', 'provider', 100),
            ('test-src', 'T-NO-SIG',      '',                          1,
             'TRAVEL_LODGING',                'provider', 100)`); err != nil {
		t.Fatalf("seed the unstored lines: %v", err)
	}
	want := map[string]struct{ merchant, detailed string }{
		// The fallback: the line's own signature, verbatim.
		"T-BY-PROVIDER": {"EXAMPLE TRANSIT AUTHORITY", "TRANSPORTATION_PUBLIC_TRANSIT"},
		// Nothing to fall back to.
		"T-NO-SIG": {"", "TRAVEL_LODGING"},
		// The store outranks the fold, whichever tier placed the line.
		"T-BY-MERCHANT": {"Corner Market", "FOOD_AND_DRINK_GROCERIES"},
		"T-BY-TXN":      {"Corner Market", "TRAVEL_FLIGHTS"},
		// A delta reads its label, or nothing — never the fold.
		"T-CARD-BILL": {"Example Card Issuer", "card_spend"},
		"T-GIFT":      {"", "gift"},
		"T-XFER-TXN":  {"", "internal_transfer"},
	}
	for id, w := range want {
		var merchant, detailed sql.NullString
		if err := db.QueryRowContext(ctx, `
        SELECT merchant_name, spend_detailed FROM spend_txn_categories()
         WHERE transaction_external_id = ?`, id).Scan(&merchant, &detailed); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if merchant.String != w.merchant || detailed.String != w.detailed {
			t.Errorf("%s = (%q, %q), want (%q, %q)", id, merchant.String, detailed.String, w.merchant, w.detailed)
		}
	}
}

// TestSpendOverlayCheckConstraints exercises the two CHECKs at the DDL
// level. 'manual' is the provenance the pins ledger writes, and is
// admitted like the four the enrichment pass derives. 'model' is not
// admitted and must not be: no tier writes it, because the model tier
// writes to the merchant store instead — spend_txn_categories()
// resolves it, and DuckDB cannot widen a CHECK in
// place anyway.
func TestSpendOverlayCheckConstraints(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)

	insertEnrichment := func(id, provenance string) error {
		_, err := db.ExecContext(ctx, `
            INSERT INTO spend_txn_enrichment (
                silver_source_id, transaction_external_id, merchant_signature,
                signature_version, spend_detailed, provenance, assigned_at
            ) VALUES ('test-src', ?, 'sig', 1, NULL, ?, 100)`, id, provenance)
		return err
	}
	for _, p := range []string{"matcher", "rule", "provider", "signature-only", "manual"} {
		if err := insertEnrichment("OK-"+p, p); err != nil {
			t.Errorf("provenance %q rejected by CHECK: %v", p, err)
		}
	}
	for _, p := range []string{"model", "llm", "override", "", "MANUAL"} {
		if err := insertEnrichment("REJ-"+p, p); err == nil {
			t.Errorf("provenance %q accepted, want CHECK violation", p)
		}
	}

	insertScope := func(id, mode string) error {
		_, err := db.ExecContext(ctx, `
            INSERT INTO spend_account_scope (silver_source_id, account_external_id, mode)
            VALUES ('test-src', ?, ?)`, id, mode)
		return err
	}
	for _, m := range []string{"include", "exclude"} {
		if err := insertScope("OK-"+m, m); err != nil {
			t.Errorf("scope mode %q rejected by CHECK: %v", m, err)
		}
	}
	for _, m := range []string{"only", "", "INCLUDE"} {
		if err := insertScope("REJ-"+m, m); err == nil {
			t.Errorf("scope mode %q accepted, want CHECK violation", m)
		}
	}
}

// seedSpendingFixture lays down the accounts, transactions and overlay
// rows the spending_lines_base tests read. Every row is there to pin
// one rule of the population definition; the comments say which.
func seedSpendingFixture(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, first_seen_at, last_seen_at) VALUES
            ('test-src', 'CASH1', 'cash',      'Everyday',   1, 1),
            ('test-src', 'CARD1', 'card',      'Card',       1, 1),
            ('test-src', 'BRK1',  'brokerage', 'Brokerage',  1, 1),
            ('test-src', 'CASH2', 'cash',      'Opted out',  1, 1),
            ('test-src', 'CUST1', 'custody',   'Opted in',   1, 1);

        -- Scope overrides both ways round. Since migration 0068 every
        -- account is in by default, so the exclude is what carries the
        -- proof and the include is a no-op held for symmetry.
        INSERT INTO spend_account_scope (silver_source_id, account_external_id, mode) VALUES
            ('test-src', 'CASH2', 'exclude'),
            ('test-src', 'CUST1', 'include');

        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount) VALUES
            ('test-src', 'T-PURCHASE',   1000, 'CARD1', 'purchase',   'USD',  -50),
            ('test-src', 'T-REFUND',     1000, 'CARD1', 'refund',     'USD',   20),
            ('test-src', 'T-FEE',        1000, 'CARD1', 'fee',        'USD',   -5),
            ('test-src', 'T-TAX',        1000, 'CASH1', 'tax',        'USD',   -7),
            ('test-src', 'T-WITHDRAWAL', 1000, 'CASH1', 'withdrawal', 'USD', -200),
            -- interest splits on sign: a finance charge is spend,
            -- credited interest is income.
            ('test-src', 'T-INT-NEG',    1000, 'CARD1', 'interest',   'USD',  -10),
            ('test-src', 'T-INT-POS',    1000, 'CASH1', 'interest',   'USD',    5),
            -- income side, and the unsigned catch-all kind. The reward
            -- kind joined them in migration 0070.
            ('test-src', 'T-REWARD',     1000, 'CARD1', 'reward',     'USD',    3),
            ('test-src', 'T-DEPOSIT',    1000, 'CASH1', 'deposit',    'USD',  100),
            ('test-src', 'T-OTHER',      1000, 'CASH1', 'other',      'USD',  -30),
            -- account scope.
            ('test-src', 'T-BRK',        1000, 'BRK1',  'purchase',   'USD',  -40),
            ('test-src', 'T-OPTOUT',     1000, 'CASH2', 'purchase',   'USD',  -60),
            ('test-src', 'T-OPTIN',      1000, 'CUST1', 'purchase',   'USD',  -70),
            -- transfer-eligible legs whose KIND the spending base
            -- never charts, one of them on an account fenced out by
            -- spend_account_scope as well. Both are matcher-pool
            -- candidates, because a movement's receiving half lands
            -- wherever the money went.
            ('test-src', 'T-BRK-XFER',   1000, 'BRK1',  'deposit',    'USD',  400),
            ('test-src', 'T-OPTOUT-XFER',1000, 'CASH2', 'withdrawal', 'USD', -400),
            -- category resolution.
            ('test-src', 'T-XFER-TXN',   1000, 'CASH1', 'withdrawal', 'USD', -800),
            ('test-src', 'T-XFER-MERCH', 1000, 'CASH1', 'withdrawal', 'USD', -900),
            -- capital deployed, pinned by hand: out of the base like an
            -- own-account move (migration 0045).
            ('test-src', 'T-INVEST-TXN', 1000, 'CASH1', 'withdrawal', 'USD', -950),
            -- a card bill with no counter-leg, placed by the built-in
            -- rule: IN the base, as generic card spend (migration 0046).
            ('test-src', 'T-CARD-BILL',  1000, 'CASH1', 'withdrawal', 'USD', -450),
            -- a cash gift, pinned by hand: IN the base, as spending with
            -- no merchant behind it (migration 0047).
            ('test-src', 'T-GIFT',       1000, 'CASH1', 'withdrawal', 'USD', -300),
            ('test-src', 'T-BY-MERCHANT',1000, 'CARD1', 'purchase',   'USD',  -90),
            ('test-src', 'T-BY-TXN',     1000, 'CARD1', 'purchase',   'USD', -100),
            ('test-src', 'T-BACKLOG',    1000, 'CARD1', 'purchase',   'USD', -110),
            -- outside every test window that ends before it.
            ('test-src', 'T-LATE',       9000, 'CARD1', 'purchase',   'USD', -120);

        -- merchant_label is the card rule's alone: the card bill carries
        -- the issuer it was paid to, every other row NULL.
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, merchant_label,
                                          assigned_at) VALUES
            ('test-src', 'T-XFER-TXN',    'sig-bank',   1, 'internal_transfer', 'rule',           NULL,                  100),
            ('test-src', 'T-INVEST-TXN',  'sig-sub',    1, 'investment',        'manual',         NULL,                  100),
            ('test-src', 'T-CARD-BILL',   'sig-issuer', 1, 'card_spend',        'rule',           'Example Card Issuer', 100),
            ('test-src', 'T-GIFT',        'sig-person', 1, 'gift',              'manual',         NULL,                  100),
            ('test-src', 'T-XFER-MERCH',  'sig-xfer',   1, NULL,                'signature-only', NULL,                  100),
            ('test-src', 'T-BY-MERCHANT', 'sig-market', 1, NULL,                'signature-only', NULL,                  100),
            ('test-src', 'T-BY-TXN',      'sig-market', 1, 'TRAVEL_FLIGHTS',    'rule',           NULL,                  100);

        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name) VALUES
            ('sig-market', 'Corner Market', 'FOOD_AND_DRINK_GROCERIES', 1, 100, 'test-model'),
            ('sig-xfer',   'Own Account',   'internal_transfer',        1, 100, 'test-model');
    `); err != nil {
		t.Fatalf("seed spending fixture: %v", err)
	}
}

// macroTxnIDs runs one of the layered population macros over a window
// and returns the transaction ids it admits.
func macroTxnIDs(t *testing.T, db *sql.DB, ctx context.Context, macro string, from, to int64) map[string]struct{} {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT transaction_external_id FROM `+macro+`(?, ?)`, from, to)
	if err != nil {
		t.Fatalf("%s(%d, %d): %v", macro, from, to, err)
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan %s: %v", macro, err)
		}
		out[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s: %v", macro, err)
	}
	return out
}

// TestSpendingLinesBasePopulation pins the shared population
// definition: that every account counts until spend_account_scope
// excludes it, which transaction kinds are spend, that `interest`
// splits on sign, that `other` and the income kinds stay out, and that
// an own-account move drops out however its category was resolved.
func TestSpendingLinesBasePopulation(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedSpendingFixture(t, db, ctx)

	got := macroTxnIDs(t, db, ctx, "spending_lines_base", 0, 5000)
	want := map[string]string{
		"T-PURCHASE":    "purchase is spend",
		"T-REFUND":      "a refund offsets spend and belongs to the same population",
		"T-FEE":         "fee is spend",
		"T-TAX":         "tax is spend",
		"T-WITHDRAWAL":  "withdrawal is spend",
		"T-INT-NEG":     "negative interest is a finance charge",
		"T-OPTIN":       "an account no spend_account_scope row excludes",
		"T-BY-MERCHANT": "resolved from the merchant store",
		"T-BY-TXN":      "resolved from the transaction overlay",
		"T-BACKLOG":     "uncategorised rows are the model tier's backlog",
		"T-CARD-BILL":   "card_spend is generic spend on a card not itemised, in the base like any primary",
		"T-GIFT":        "gift is a cash gift with no merchant behind it, in the base like any primary",
		"T-BRK":         "a brokerage account spends too — one product can be a brokerage and a chequing account at once (migration 0064)",
	}
	excluded := map[string]string{
		"T-REWARD":     "a rewards credit is income, never netted against the spending that earned it (migration 0070)",
		"T-INT-POS":    "positive interest is income",
		"T-DEPOSIT":    "deposit is income",
		"T-OTHER":      "the `other` kind carries no reliable sign",
		"T-OPTOUT":     "spend_account_scope fences an account out",
		"T-XFER-TXN":   "internal_transfer from the transaction overlay",
		"T-XFER-MERCH": "internal_transfer from the merchant store",
		"T-INVEST-TXN": "investment is capital deployed, excluded like an own-account move",
		"T-LATE":       "outside the window",
	}
	for id, why := range want {
		if _, ok := got[id]; !ok {
			t.Errorf("%s missing from spending_lines_base (%s)", id, why)
		}
	}
	for id, why := range excluded {
		if _, ok := got[id]; ok {
			t.Errorf("%s present in spending_lines_base (%s)", id, why)
		}
	}
	if len(got) != len(want) {
		t.Errorf("spending_lines_base returned %d rows, want %d", len(got), len(want))
	}
}

// TestSpendingLinesBaseCategoryPrecedence pins the resolution lattice:
// a per-transaction verdict beats the merchant-wide one, the merchant
// store is reached through the enrichment row's signature, and the
// primary comes from the seeded dimension rather than from string
// surgery on the detailed value.
func TestSpendingLinesBaseCategoryPrecedence(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedSpendingFixture(t, db, ctx)

	type resolved struct {
		detailed, primary, merchant sql.NullString
	}
	read := func(id string) resolved {
		t.Helper()
		var r resolved
		err := db.QueryRowContext(ctx, `
            SELECT spend_detailed, spend_primary, merchant_name
              FROM spending_lines_base(0, 5000)
             WHERE transaction_external_id = ?`, id).
			Scan(&r.detailed, &r.primary, &r.merchant)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return r
	}

	byMerchant := read("T-BY-MERCHANT")
	if byMerchant.detailed.String != "FOOD_AND_DRINK_GROCERIES" {
		t.Errorf("T-BY-MERCHANT detailed = %q, want FOOD_AND_DRINK_GROCERIES", byMerchant.detailed.String)
	}
	if byMerchant.primary.String != "FOOD_AND_DRINK" {
		t.Errorf("T-BY-MERCHANT primary = %q, want FOOD_AND_DRINK", byMerchant.primary.String)
	}
	if byMerchant.merchant.String != "Corner Market" {
		t.Errorf("T-BY-MERCHANT merchant = %q, want Corner Market", byMerchant.merchant.String)
	}

	byTxn := read("T-BY-TXN")
	if byTxn.detailed.String != "TRAVEL_FLIGHTS" {
		t.Errorf("T-BY-TXN detailed = %q, want TRAVEL_FLIGHTS (transaction scope wins)", byTxn.detailed.String)
	}
	if byTxn.primary.String != "TRAVEL" {
		t.Errorf("T-BY-TXN primary = %q, want TRAVEL", byTxn.primary.String)
	}

	backlog := read("T-BACKLOG")
	if backlog.detailed.Valid || backlog.primary.Valid {
		t.Errorf("T-BACKLOG resolved to (%v, %v), want NULL/NULL", backlog.detailed, backlog.primary)
	}
}

// TestSpendPopulationLayering pins the three populations AGAINST EACH
// OTHER, which is the property the layering exists for and which no
// test of any one macro alone can express:
//
//   - the enrichment population is the spending base BEFORE the
//     internal-transfer exclusion, because the pass is what decides
//     which rows are internal — reading the filtered base would be
//     circular;
//   - the matcher pool is not a subset of either. It admits the
//     income-side rows the spending base excludes, because a movement
//     is only recognisable through its counter-leg, and it spans every
//     account rather than the scoped ones, because the counter-leg
//     lands wherever the money went;
//   - the account scope is shared by the two POPULATIONS, so an
//     account fenced out is fenced out of what the pass writes and
//     what a report charts — but not out of what the matcher may pair.
func TestSpendPopulationLayering(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedSpendingFixture(t, db, ctx)

	base := macroTxnIDs(t, db, ctx, "spending_lines_base", 0, 5000)
	population := macroTxnIDs(t, db, ctx, "spend_enrichment_population", 0, 5000)
	pool := macroTxnIDs(t, db, ctx, "spend_matcher_pool", 0, 5000)

	// The population is the base plus exactly the rows the base drops
	// for resolving to internal_transfer or investment.
	for id := range base {
		if _, ok := population[id]; !ok {
			t.Errorf("%s is in spending_lines_base but not in the enrichment population", id)
		}
	}
	for _, id := range []string{"T-XFER-TXN", "T-XFER-MERCH", "T-INVEST-TXN"} {
		if _, ok := population[id]; !ok {
			t.Errorf("%s missing from the enrichment population; the pass cannot re-decide a row it never sees", id)
		}
	}
	if len(population) != len(base)+3 {
		t.Errorf("enrichment population = %d rows, want %d (the base plus its two internal transfers and its investment)",
			len(population), len(base)+3)
	}

	// The kind and scope rules the population shares with the base.
	for id, why := range map[string]string{
		"T-REWARD":  "a rewards credit is income (migration 0070)",
		"T-DEPOSIT": "deposit is income",
		"T-INT-POS": "credited interest is income",
		"T-OTHER":   "the `other` kind carries no reliable sign",
		"T-OPTOUT":  "spend_account_scope fences an account out",
		"T-LATE":    "outside the window",
	} {
		if _, ok := population[id]; ok {
			t.Errorf("%s reached the enrichment population (%s)", id, why)
		}
	}

	// The pool is bound by transaction KIND and by the window, and by
	// nothing else — not the account's kind, not the account scope. A
	// leg the matcher cannot see is a pair it cannot form, and an
	// unpairable outgoing leg reads as spending.
	for id, why := range map[string]string{
		"T-WITHDRAWAL":  "a withdrawal is the outgoing half of a movement",
		"T-DEPOSIT":     "the income side: without it an outgoing leg has nothing to pair with",
		"T-BRK-XFER":    "a transaction kind the spending base never charts still holds counter-legs",
		"T-OPTOUT-XFER": "an account fenced out of spending still holds counter-legs",
	} {
		if _, ok := pool[id]; !ok {
			t.Errorf("%s missing from the matcher pool (%s)", id, why)
		}
	}
	for id, why := range map[string]string{
		"T-PURCHASE": "a purchase is not a transfer-eligible kind",
		"T-FEE":      "a fee is not a transfer-eligible kind",
		"T-OTHER":    "`other` is not a transfer-eligible kind",
		"T-LATE":     "outside the window",
	} {
		if _, ok := pool[id]; ok {
			t.Errorf("%s reached the matcher pool (%s)", id, why)
		}
	}

	// The account scope, shared by the two populations and read once.
	var scoped int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spend_scoped_accounts()`).Scan(&scoped); err != nil {
		t.Fatalf("count spend_scoped_accounts: %v", err)
	}
	if scoped != 4 {
		t.Errorf("spend_scoped_accounts = %d, want 4 (every seeded account, minus the one fenced out)", scoped)
	}
}

// TestProviderSpendColumns: the enrichment overlay holds
// provider_spend_detailed once, and spend_txn_categories projects the two
// provider columns beside merchant, category and provenance.
func TestProviderSpendColumns(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)

	var n int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM information_schema.columns
         WHERE table_name = 'spend_txn_enrichment'
           AND column_name = 'provider_spend_detailed'`).Scan(&n); err != nil {
		t.Fatalf("column check: %v", err)
	}
	if n != 1 {
		t.Errorf("provider_spend_detailed columns = %d, want 1", n)
	}

	rows, err := db.QueryContext(ctx, "SELECT * FROM spend_txn_categories() LIMIT 0")
	if err != nil {
		t.Fatalf("spend_txn_categories: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("macro columns: %v", err)
	}
	have := map[string]bool{}
	for _, c := range cols {
		have[c] = true
	}
	// The two provider columns, and the four the reports read.
	for _, c := range []string{"provider_spend_detailed", "provider_spend_primary",
		"merchant_name", "spend_detailed", "spend_primary", "provenance"} {
		if !have[c] {
			t.Errorf("spend_txn_categories() lacks %q", c)
		}
	}
}

// TestProviderViewRecordsWithoutDeciding pins the two halves of the
// policy at once: the issuer's mapped value is recorded on every row it
// translates, and a catch-all among them does not become the verdict.
func TestProviderViewRecordsWithoutDeciding(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
              merchant_signature, signature_version, spend_detailed, provenance,
              provider_spend_detailed, assigned_at) VALUES
            -- the issuer said something specific, and it decided
            ('s', 'T-SPECIFIC', 'SIG-A', 1, 'FOOD_AND_DRINK_GROCERIES', 'provider',
             'FOOD_AND_DRINK_GROCERIES', 1),
            -- the issuer said only "somewhere in general merchandise": recorded,
            -- but the row is left for a tier that can read the merchant name
            ('s', 'T-CATCHALL', 'SIG-B', 1, NULL, 'signature-only',
             'GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE', 1),
            -- the issuer said nothing at all: NULL, which is not the same
            ('s', 'T-SILENT', 'SIG-C', 1, NULL, 'signature-only', NULL, 1)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	type got struct{ ours, theirs, theirPrim sql.NullString }
	for id, want := range map[string]got{
		"T-SPECIFIC": {ours: sql.NullString{String: "FOOD_AND_DRINK_GROCERIES", Valid: true},
			theirs:    sql.NullString{String: "FOOD_AND_DRINK_GROCERIES", Valid: true},
			theirPrim: sql.NullString{String: "FOOD_AND_DRINK", Valid: true}},
		"T-CATCHALL": {theirs: sql.NullString{String: "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE", Valid: true},
			theirPrim: sql.NullString{String: "GENERAL_MERCHANDISE", Valid: true}},
		"T-SILENT": {},
	} {
		var g got
		if err := db.QueryRowContext(ctx, `
            SELECT spend_detailed, provider_spend_detailed, provider_spend_primary
              FROM spend_txn_categories() WHERE transaction_external_id = ?`, id).
			Scan(&g.ours, &g.theirs, &g.theirPrim); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if g != want {
			t.Errorf("%s = %+v, want %+v", id, g, want)
		}
	}
}

// TestTheATMFeesLabel: the dimension labels a seeded value by its
// display label.
func TestTheATMFeesLabel(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	var label string
	if err := db.QueryRowContext(ctx,
		`SELECT label FROM spend_categories WHERE spend_detailed = 'BANK_FEES_ATM_FEES'`).
		Scan(&label); err != nil {
		t.Fatalf("read label: %v", err)
	}
	if label != "ATM fees" {
		t.Errorf("label = %q, want %q", label, "ATM fees")
	}
}

// TestSpendCategoryFamilies: every row of the dimension is in the Go
// table, and each row's family is the one it is told apart by. A value of
// spending is spending's, a value of income is income's, and the three
// deltas both families read are 'both'. Income has one catch-all,
// INCOME_OTHER.
func TestSpendCategoryFamilies(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)

	current := map[string]bool{}
	for _, c := range canonical.SpendCategories {
		current[c.Detailed] = true
	}
	all, err := db.QueryContext(ctx, `SELECT spend_detailed FROM spend_categories`)
	if err != nil {
		t.Fatalf("read spend_categories: %v", err)
	}
	defer all.Close()
	n := 0
	for all.Next() {
		var det string
		if err := all.Scan(&det); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
		if !current[det] {
			t.Errorf("%s is in the dimension; the Go table does not hold it", det)
		}
	}
	if err := all.Err(); err != nil {
		t.Fatalf("iterate spend_categories: %v", err)
	}
	if n != len(canonical.SpendCategories) {
		t.Errorf("spend_categories = %d rows, want %d", n, len(canonical.SpendCategories))
	}

	for _, tc := range []struct{ detailed, family, label string }{
		{"FOOD_AND_DRINK_GROCERIES", "spending", "Groceries"},
		{canonical.SpendDetailedCardSpend, "spending", "Uncategorized card spend"},
		{"INCOME_SALARY", "income", "Salary"},
		{"INCOME_OTHER", "income", "Other income"},
		{canonical.IncomeDetailedCapitalReturn, "income", "Capital return"},
		{canonical.SpendDetailedInternalTransfer, "both", "Internal transfer"},
		{canonical.SpendDetailedGift, "both", "Gift"},
		{canonical.SpendDetailedOther, "both", "Other"},
	} {
		var family, label string
		if err := db.QueryRowContext(ctx,
			`SELECT family, label FROM spend_categories WHERE spend_detailed = ?`,
			tc.detailed).Scan(&family, &label); err != nil {
			t.Errorf("read %s: %v", tc.detailed, err)
			continue
		}
		if family != tc.family {
			t.Errorf("%s family = %q, want %q", tc.detailed, family, tc.family)
		}
		if label != tc.label {
			t.Errorf("%s label = %q, want %q", tc.detailed, label, tc.label)
		}
	}

	var catchAlls []string
	rows, err := db.QueryContext(ctx,
		`SELECT spend_detailed FROM spend_categories WHERE family = 'income' AND catch_all ORDER BY 1`)
	if err != nil {
		t.Fatalf("read income catch-alls: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var det string
		if err := rows.Scan(&det); err != nil {
			t.Fatalf("scan: %v", err)
		}
		catchAlls = append(catchAlls, det)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate income catch-alls: %v", err)
	}
	if len(catchAlls) != 1 || catchAlls[0] != "INCOME_OTHER" {
		t.Errorf("income catch-alls = %v, want [INCOME_OTHER]", catchAlls)
	}
}

// TestMigration0111MovesEveryStoredVerdictToItsSuccessor pins what 0111
// does to a database that still stores the retired spellings. The
// database is built at version 110, every retired spelling is stored in
// each column that holds an income verdict — the payer store, the
// overlay's verdict and the provider's own filing — and Migrate carries
// it forward as the next read-write open would.
//
// The retired spellings are read off the version-110 dimension, as the
// rows the Go table does not hold, rather than listed here. So a row
// 0111 deletes with no canonical.RetiredDetailed entry fails, and so
// does a successor on which the SQL and the Go table disagree.
func TestMigration0111MovesEveryStoredVerdictToItsSuccessor(t *testing.T) {
	t.Parallel()
	db, ctx := openAtVersion(t, 110)

	current := map[string]bool{}
	for _, c := range canonical.SpendCategories {
		current[c.Detailed] = true
	}
	rows, err := db.QueryContext(ctx, `SELECT spend_detailed FROM spend_categories ORDER BY 1`)
	if err != nil {
		t.Fatalf("read the version-110 dimension: %v", err)
	}
	successor := map[string]string{}
	var retired []string
	for rows.Next() {
		var det string
		if err := rows.Scan(&det); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if current[det] {
			continue
		}
		r, ok := canonical.RetiredDetailed(det)
		if !ok {
			t.Errorf("0111 retires %s, and canonical.RetiredDetailed names no successor for it", det)
			continue
		}
		retired = append(retired, det)
		successor[det] = r.Successor
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the version-110 dimension: %v", err)
	}
	rows.Close()
	if len(retired) == 0 {
		t.Fatal("the version-110 dimension holds no retired spelling; there is nothing to move")
	}

	for i, old := range retired {
		if _, err := db.ExecContext(ctx, `
            INSERT INTO income_payer_categories (payer_signature, payer_name, income_detailed,
                                                 signature_version, assigned_at, model_name)
                 VALUES (?, 'Example Payer', ?, 1, 100, 'test-model')`,
			fmt.Sprintf("sig-%d", i), old); err != nil {
			t.Fatalf("store %s: %v", old, err)
		}
		if _, err := db.ExecContext(ctx, `
            INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id,
                    payer_signature, signature_version, income_detailed, provenance,
                    provider_income_detailed, assigned_at)
                 VALUES ('s', ?, ?, 1, ?, 'rule', ?, 100)`,
			"T-"+old, fmt.Sprintf("sig-%d", i), old, old); err != nil {
			t.Fatalf("overlay %s: %v", old, err)
		}
		// The two overlay columns apart: a verdict beside a different
		// filing, each way round, so a rewrite that read one column to
		// change the other would show.
		if _, err := db.ExecContext(ctx, `
            INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id,
                    payer_signature, signature_version, income_detailed, provenance,
                    provider_income_detailed, assigned_at) VALUES
                ('s', ?, ?, 1, 'INCOME_DIVIDENDS', 'rule', ?, 100),
                ('s', ?, ?, 1, ?, 'rule', 'INCOME_DIVIDENDS', 100)`,
			"T-FILED-"+old, fmt.Sprintf("sig-f-%d", i), old,
			"T-VERDICT-"+old, fmt.Sprintf("sig-v-%d", i), old); err != nil {
			t.Fatalf("mixed overlay rows for %s: %v", old, err)
		}
	}
	// Two rows the renames must leave alone: a current value, and a row
	// with neither column set.
	if _, err := db.ExecContext(ctx, `
        INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id,
                payer_signature, signature_version, income_detailed, provenance,
                provider_income_detailed, assigned_at) VALUES
            ('s', 'T-KEEP', 'sig-keep', 1, 'INCOME_INTEREST_EARNED', 'rule', 'INCOME_INTEREST_EARNED', 100),
            ('s', 'T-NONE', 'sig-none', 1, NULL, 'signature-only', NULL, 100)`); err != nil {
		t.Fatalf("seed the controls: %v", err)
	}

	verify := func(when string) {
		t.Helper()
		for i, old := range retired {
			var store, verdict, filed sql.NullString
			if err := db.QueryRowContext(ctx,
				`SELECT income_detailed FROM income_payer_categories WHERE payer_signature = ?`,
				fmt.Sprintf("sig-%d", i)).Scan(&store); err != nil {
				t.Fatalf("read the store for %s: %v", old, err)
			}
			if err := db.QueryRowContext(ctx, `
                SELECT income_detailed, provider_income_detailed
                  FROM income_txn_enrichment WHERE transaction_external_id = ?`,
				"T-"+old).Scan(&verdict, &filed); err != nil {
				t.Fatalf("read the overlay for %s: %v", old, err)
			}
			for _, got := range []struct {
				column string
				value  sql.NullString
			}{
				{"income_payer_categories.income_detailed", store},
				{"income_txn_enrichment.income_detailed", verdict},
				{"income_txn_enrichment.provider_income_detailed", filed},
			} {
				if got.value.String != successor[old] {
					t.Errorf("%s, %s holds %q where %s was stored, want %q",
						when, got.column, got.value.String, old, successor[old])
				}
			}
			for id, want := range map[string][2]string{
				"T-FILED-" + old:   {"INCOME_DIVIDENDS", successor[old]},
				"T-VERDICT-" + old: {successor[old], "INCOME_DIVIDENDS"},
			} {
				if err := db.QueryRowContext(ctx, `
                    SELECT income_detailed, provider_income_detailed
                      FROM income_txn_enrichment WHERE transaction_external_id = ?`,
					id).Scan(&verdict, &filed); err != nil {
					t.Fatalf("read %s: %v", id, err)
				}
				if verdict.String != want[0] || filed.String != want[1] {
					t.Errorf("%s, %s = (%q, %q), want (%q, %q)",
						when, id, verdict.String, filed.String, want[0], want[1])
				}
			}
		}
		for id, want := range map[string]sql.NullString{
			"T-KEEP": {String: "INCOME_INTEREST_EARNED", Valid: true},
			"T-NONE": {},
		} {
			var verdict, filed sql.NullString
			if err := db.QueryRowContext(ctx, `
                SELECT income_detailed, provider_income_detailed
                  FROM income_txn_enrichment WHERE transaction_external_id = ?`,
				id).Scan(&verdict, &filed); err != nil {
				t.Fatalf("read %s: %v", id, err)
			}
			if verdict != want || filed != want {
				t.Errorf("%s, %s = (%v, %v), want %v in both columns", when, id, verdict, filed, want)
			}
		}
	}

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migrate past version 110: %v", err)
	}
	verify("after the migration")
}

// TestSpendKindFloorPlacesWhatNothingElseCould pins the kind floor's
// last-resort arm, and above all WHERE it sits.
//
// A brokerage books security-level fees and tax withheld at source
// whose narrative is the SECURITY, or on some sources nothing at all —
// there is no payee for a rule to key on. The transaction's KIND says
// what the row is, and each adapter derives that from whatever
// evidence its own source gives, so the floor reads that verdict
// rather than re-deriving it from prose.
//
// Under the merchant store, never over it: the model files a
// "Foreign Transaction Fee" as the vendored FOREIGN_TRANSACTION_FEES,
// which is finer than any floor, and a floor that outranked it would
// quietly coarsen every such row.
//
// A fee's floor depends on its account (0103, 0110). On a card or on a
// bank account it is a bank fee. A bank account here is a cash account in
// no portfolio. Everywhere else it is an investment fee, the cash side of
// an investment mandate included.
func TestSpendKindFloorPlacesWhatNothingElseCould(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedSpendingFixture(t, db, ctx)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, portfolio_external_id, first_seen_at, last_seen_at)
        VALUES ('test-src', 'MANDATE-CASH', 'cash', 'Mandate cash', 'PF-1', 1, 1),
               ('test-src', 'MANDATE-CARD', 'card', 'Mandate card', 'PF-1', 1, 1);

        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount) VALUES
            ('test-src', 'T-ADR',   1000, 'BRK1',  'fee',      'USD',  -1.50),
            ('test-src', 'T-BANKFEE', 1000, 'CASH1', 'fee',    'USD',  -2.00),
            ('test-src', 'T-MANDATEFEE', 1000, 'MANDATE-CASH', 'fee', 'USD', -9.00),
            ('test-src', 'T-ORPHANFEE', 1000, 'NOT-IN-GOLD', 'fee', 'USD', -1.00),
            ('test-src', 'T-WHT',   1000, 'CASH1', 'tax',      'USD', -12.00),
            ('test-src', 'T-FXFEE', 1000, 'CARD1', 'fee',      'USD',  -3.00),
            ('test-src', 'T-CARDFEE', 1000, 'CARD1', 'fee',    'USD', -100.00),
            ('test-src', 'T-PFCARDFEE', 1000, 'MANDATE-CARD', 'fee', 'USD', -40.00),
            ('test-src', 'T-NOCAT', 1000, 'CASH1', 'purchase', 'USD', -20.00),
            ('test-src', 'T-MARGIN', 1000, 'CASH1', 'interest', 'USD',  -6.66),
            ('test-src', 'T-CREDIT', 1000, 'CASH1', 'interest', 'USD',   4.00);

        -- Nothing placed any of them; the first three carry a signature.
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, assigned_at) VALUES
            ('test-src', 'T-ADR',   'EXAMPLE HOLDINGS ADR', 1, NULL, 'signature-only', 100),
            ('test-src', 'T-BANKFEE', 'ACCOUNT MAINTENANCE', 1, NULL, 'signature-only', 100),
            ('test-src', 'T-MANDATEFEE', 'MANDATE FEE', 1, NULL, 'signature-only', 100),
            ('test-src', 'T-ORPHANFEE', 'SOME FEE', 1, NULL, 'signature-only', 100),
            ('test-src', 'T-WHT',   'EXAMPLE TREASURY ETF', 1, NULL, 'signature-only', 100),
            ('test-src', 'T-FXFEE', 'FOREIGN TRANSACTION FEE', 1, NULL, 'signature-only', 100),
            ('test-src', 'T-CARDFEE', 'ANNUAL MEMBERSHIP FEE', 1, NULL, 'signature-only', 100),
            ('test-src', 'T-PFCARDFEE', 'LATE FEE', 1, NULL, 'signature-only', 100),
            ('test-src', 'T-NOCAT',  'SOMETHING UNPLACED', 1, NULL, 'signature-only', 100),
            ('test-src', 'T-MARGIN', 'MARGIN INTEREST', 1, NULL, 'signature-only', 100),
            ('test-src', 'T-CREDIT', 'CREDITED INTEREST', 1, NULL, 'signature-only', 100);

        -- ...except that the MODEL has a verdict for the fx-fee signature,
        -- and it is finer than the floor could ever be.
        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name,
                                               spend_detailed, signature_version,
                                               assigned_at, model_name)
        VALUES ('FOREIGN TRANSACTION FEE', 'Foreign Transaction Fee',
                'BANK_FEES_FOREIGN_TRANSACTION_FEES', 1, 100, 'test-model');
    `); err != nil {
		t.Fatalf("seed the floor fixture: %v", err)
	}

	rows, err := db.QueryContext(ctx, `
        SELECT transaction_external_id, COALESCE(spend_detailed, ''), provenance
          FROM spend_txn_categories()
         WHERE transaction_external_id LIKE 'T-%'`)
	if err != nil {
		t.Fatalf("spend_txn_categories: %v", err)
	}
	defer rows.Close()
	got := map[string][2]string{}
	for rows.Next() {
		var id, detailed, prov string
		if err := rows.Scan(&id, &detailed, &prov); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[id] = [2]string{detailed, prov}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}

	for id, want := range map[string][2]string{
		"T-ADR": {canonical.SpendDetailedInvestmentFees, "kind"},
		// a bank account's fee is a bank fee, and so is any card's, in a
		// portfolio or not; a mandate's cash account, though also kind
		// cash, sits in a portfolio and keeps the investment floor, as
		// does a fee whose account gold lacks
		"T-BANKFEE":    {"BANK_FEES_OTHER_BANK_FEES", "kind"},
		"T-CARDFEE":    {"BANK_FEES_OTHER_BANK_FEES", "kind"},
		"T-PFCARDFEE":  {"BANK_FEES_OTHER_BANK_FEES", "kind"},
		"T-MANDATEFEE": {canonical.SpendDetailedInvestmentFees, "kind"},
		"T-ORPHANFEE":  {canonical.SpendDetailedInvestmentFees, "kind"},
		"T-WHT":        {canonical.SpendDetailedWithholdingTax, "kind"},
		// the model's finer verdict survives the floor
		"T-FXFEE": {"BANK_FEES_FOREIGN_TRANSACTION_FEES", "model"},
		// interest joins them, but only when it was CHARGED: the
		// same macro is read at transaction grain, where credited
		// interest reaches it and is income, not a fee (0067)
		"T-MARGIN": {"BANK_FEES_INTEREST_CHARGE", "kind"},
		"T-CREDIT": {"", "signature-only"},
		// and the floor covers nothing beyond those three kinds
		"T-NOCAT": {"", "signature-only"},
	} {
		if got[id] != want {
			t.Errorf("%s = %v, want %v", id, got[id], want)
		}
	}
}

// TestSpendKindFloorLabelsResolve: a floored row must read as words
// like any other. The label join keys on the resolved value, so a
// floor added to the COALESCE without being added to that join would
// leave the category legible in the id column and blank in the one a
// report prints.
func TestSpendKindFloorLabelsResolve(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedSpendingFixture(t, db, ctx)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount)
        VALUES ('test-src', 'T-ADR', 1000, 'BRK1', 'fee', 'USD', -1.50);
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, assigned_at)
        VALUES ('test-src', 'T-ADR', 'EXAMPLE HOLDINGS ADR', 1, NULL, 'signature-only', 100);
    `); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var primary, label, primaryLabel sql.NullString
	if err := db.QueryRowContext(ctx, `
        SELECT spend_primary, spend_label, spend_primary_label
          FROM spend_txn_categories() WHERE transaction_external_id = 'T-ADR'`,
	).Scan(&primary, &label, &primaryLabel); err != nil {
		t.Fatalf("read the floored row: %v", err)
	}
	if primary.String != "BANK_FEES" || label.String != "Investment fees" ||
		primaryLabel.String != "Bank fees" {
		t.Errorf("floored row = (%q, %q, %q), want (BANK_FEES, Investment fees, Bank fees)",
			primary.String, label.String, primaryLabel.String)
	}
}
