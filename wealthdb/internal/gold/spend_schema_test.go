package gold

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// TestSpendCategoriesMatchGoTable is the generator-style pin between
// the seeded spend_categories dimension (migrations 0040, 0045-0047,
// 0056, 0065, 0069) and canonical.SpendCategories. The Go table is the
// source those seeds were generated from; if either side is edited
// alone — a taxonomy refresh that skips the migration, or a hand-edit
// of the SQL — this fails rather than letting gold and the enrichment
// pass disagree about what a valid category is.
//
// The family column is compared with the rest: it is what the two
// vocabularies are told apart by, and a row seeded into the wrong one
// would be admitted by the wrong rules, pins and conversation.
func TestSpendCategoriesMatchGoTable(t *testing.T) {
	db, ctx := openMigrated(t)

	rows, err := db.QueryContext(ctx,
		`SELECT spend_primary, spend_detailed, description, family FROM spend_categories`)
	if err != nil {
		t.Fatalf("read spend_categories: %v", err)
	}
	defer rows.Close()

	seeded := map[string]canonical.SpendCategory{}
	for rows.Next() {
		var c canonical.SpendCategory
		var family sql.NullString
		if err := rows.Scan(&c.Primary, &c.Detailed, &c.Description, &family); err != nil {
			t.Fatalf("scan spend_categories: %v", err)
		}
		if !family.Valid {
			t.Errorf("%s has no family; it belongs to neither vocabulary", c.Detailed)
		}
		c.Family = canonical.Family(family.String)
		seeded[c.Detailed] = c
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate spend_categories: %v", err)
	}

	if len(seeded) != len(canonical.SpendCategories) {
		t.Errorf("spend_categories rows = %d, want %d", len(seeded), len(canonical.SpendCategories))
	}
	for _, want := range canonical.SpendCategories {
		got, ok := seeded[want.Detailed]
		if !ok {
			t.Errorf("spend_categories is missing %q", want.Detailed)
			continue
		}
		if got != want {
			t.Errorf("spend_categories[%q] = %+v, want %+v", want.Detailed, got, want)
		}
		delete(seeded, want.Detailed)
	}
	for detailed := range seeded {
		t.Errorf("spend_categories has %q, which the Go table does not", detailed)
	}
}

// TestMigration0040DDLIsRerunnable proves the CREATE TABLE IF NOT
// EXISTS and the INSERT OR REPLACE seed both survive a replay — a bare
// INSERT would collide on spend_detailed — and that the re-run neither
// duplicates nor drops rows.
func TestMigration0040DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)

	rerunMigrationDDL(t, db, ctx, "0040_spend_taxonomy.sql")

	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM spend_categories`).Scan(&n); err != nil {
		t.Fatalf("count spend_categories: %v", err)
	}
	if n != len(canonical.SpendCategories) {
		t.Errorf("spend_categories = %d rows after re-run, want %d", n, len(canonical.SpendCategories))
	}
}

// TestMigration0041DDLIsRerunnable holds the overlay migration to the
// same bar, and confirms the macro still answers afterwards (CREATE OR
// REPLACE MACRO must not have been left in a half-replaced state).
func TestMigration0041DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)

	rerunMigrationDDL(t, db, ctx, "0041_spend_categorizations.sql")

	for _, tbl := range []string{
		"spend_txn_enrichment", "spend_merchant_categories", "spend_account_scope",
	} {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tbl).Scan(&n); err != nil {
			t.Errorf("table %s after re-run: %v", tbl, err)
		}
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spending_lines_base(0, 9223372036854775807)`).Scan(&n); err != nil {
		t.Errorf("spending_lines_base after re-run: %v", err)
	}
}

// TestMigration0044DDLIsRerunnable holds the matcher-pool re-issue to
// the same bar, and confirms the macro still answers afterwards.
func TestMigration0044DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)

	rerunMigrationDDL(t, db, ctx, "0044_spend_matcher_pool_all_accounts.sql")

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spend_matcher_pool(0, 9223372036854775807)`).Scan(&n); err != nil {
		t.Errorf("spend_matcher_pool after re-run: %v", err)
	}
}

// TestMigration0045DDLIsRerunnable holds the investment migration to
// the same bar: the INSERT OR REPLACE must not collide on the seeded
// row, the re-issued base must still answer, and — the point of the
// migration — a row resolving to `investment` must be out of it while
// the backlog stays in.
func TestMigration0045DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)

	rerunMigrationDDL(t, db, ctx, "0045_spend_investment.sql")

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spend_categories WHERE spend_detailed = 'investment'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("investment rows after re-run = %d (%v), want 1", n, err)
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

// TestMigration0046DDLIsRerunnable holds the card_spend migration to
// the same bar: the INSERT OR REPLACE must not collide on the seeded
// row, and — the point of the value — a row resolving to `card_spend`
// is IN the base, as its own primary, while the two excluded deltas
// still are not.
func TestMigration0046DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)

	rerunMigrationDDL(t, db, ctx, "0046_spend_card_spend.sql")

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spend_categories WHERE spend_detailed = 'card_spend'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("card_spend rows after re-run = %d (%v), want 1", n, err)
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

// TestMigration0047DDLIsRerunnable holds the gift migration to the
// same bar: the INSERT OR REPLACE must not collide on the seeded row,
// and — the point of the value — a row resolving to `gift` is IN the
// base, as its own primary, while the two excluded deltas still are
// not. The base admits it by construction (the migration does not
// re-issue it; the exclusion list names `internal_transfer` and
// `investment` alone), and the assertion is live rather than
// structural: the same fixture and the same macro drop the two
// excluded rows in this very query.
func TestMigration0047DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)

	rerunMigrationDDL(t, db, ctx, "0047_spend_gift.sql")

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spend_categories WHERE spend_detailed = 'gift'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("gift rows after re-run = %d (%v), want 1", n, err)
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

// TestMigration0048DDLIsRerunnable holds the merchant-column re-issue
// to the same bar, and pins its point on this fixture, whose store
// names the grocery signature and carries a delta verdict on the
// transfer's: a gift whose signature the store names, a row the STORE
// resolved to an own-account move, and a grocery purchase — the first
// two blank, the third named, whichever scope answered.
func TestMigration0048DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)

	rerunMigrationDDL(t, db, ctx, "0048_spend_delta_no_merchant.sql")

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
		// Under 0048's own issue every delta line is blank, the card
		// bill included: its label is what migration 0052 publishes.
		"T-CARD-BILL": {"", "card_spend"},
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

// TestMigration0050DDLIsRerunnable holds the provenance re-issue to the
// same bar and pins what it resolves: a line the merchant store placed
// reads 'model', which no tier writes to the overlay, while every line
// the pass placed keeps the provenance it was stamped with.
func TestMigration0050DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)

	rerunMigrationDDL(t, db, ctx, "0050_spend_model_provenance.sql")

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

// TestMigration0052DDLIsRerunnable holds the issuer re-issue to the
// same bar — the ADD COLUMN must survive a replay, and the macro must
// still answer — and pins what it publishes: a card bill shows the
// issuer the rule labelled it with, while every other delta line still
// shows nothing and a vendored line still shows the store's name.
func TestMigration0052DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)

	rerunMigrationDDL(t, db, ctx, "0052_spend_card_bill_issuer.sql")

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

// TestMigration0055DDLIsRerunnable holds the scope-projection re-issue to
// the replay bar. 0055 re-issues two macros, so a replay that dropped one
// would leave the enrichment pass with no population to read.
func TestMigration0055DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedSpendingFixture(t, db, ctx)

	rerunMigrationDDL(t, db, ctx, "0055_spend_rule_scope.sql")

	for _, q := range []string{
		"SELECT COUNT(*) FROM spend_scoped_accounts()",
		"SELECT COUNT(*) FROM spend_enrichment_population(0, 9999999999)",
	} {
		var n int
		if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Errorf("%s after re-run: %v", q, err)
		}
	}

	// The population projects the columns a scoped rule narrows by; a
	// re-issue that dropped one would fail the rule tier, not the query.
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

// TestMigration0056DDLIsRerunnable holds the extension-category seed to the
// replay bar. Its INSERT is OR REPLACE for exactly this reason, so a second
// application must neither fail nor duplicate the row.
func TestMigration0056DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)

	rerunMigrationDDL(t, db, ctx, "0056_spend_digital_services.sql")

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spend_categories WHERE spend_detailed = ?`,
		canonical.SpendDetailedDigitalServices).Scan(&n); err != nil {
		t.Fatalf("count the seeded row: %v", err)
	}
	if n != 1 {
		t.Errorf("got %d rows for the extension category after a re-run, want 1", n)
	}
}

// TestMigration0054DDLIsRerunnable holds the signature-fallback
// re-issue to the same bar and pins what it publishes: a line no store
// row covers falls back to its own signature, a line the store named
// still reads the store, a delta still reads its label or nothing, and
// an empty signature renders as nothing rather than as an empty name.
func TestMigration0054DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)

	rerunMigrationDDL(t, db, ctx, "0054_spend_merchant_signature_fallback.sql")

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
// resolves it (migration 0050), and DuckDB cannot widen a CHECK in
// place anyway.
func TestSpendOverlayCheckConstraints(t *testing.T) {
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
            ('test-src', 'T-REWARD',     1000, 'CARD1', 'reward',     'USD',    3),
            ('test-src', 'T-FEE',        1000, 'CARD1', 'fee',        'USD',   -5),
            ('test-src', 'T-TAX',        1000, 'CASH1', 'tax',        'USD',   -7),
            ('test-src', 'T-WITHDRAWAL', 1000, 'CASH1', 'withdrawal', 'USD', -200),
            -- interest splits on sign: a finance charge is spend,
            -- credited interest is income.
            ('test-src', 'T-INT-NEG',    1000, 'CARD1', 'interest',   'USD',  -10),
            ('test-src', 'T-INT-POS',    1000, 'CASH1', 'interest',   'USD',    5),
            -- income side, and the unsigned catch-all kind.
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
	db, ctx := openMigrated(t)
	seedSpendingFixture(t, db, ctx)

	got := macroTxnIDs(t, db, ctx, "spending_lines_base", 0, 5000)
	want := map[string]string{
		"T-PURCHASE":    "purchase is spend",
		"T-REFUND":      "a refund offsets spend and belongs to the same population",
		"T-REWARD":      "a rewards credit offsets card spend",
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

// TestMigration0057DDLIsRerunnable holds the issuer-view migration to the
// replay bar: the ADD COLUMN must be IF NOT EXISTS, and the macro re-issue
// must keep every refinement made since 0042 — a re-issue replaces the
// whole body, so a carried-forward clause dropped here is a silent
// regression in what every spending report reads.
func TestMigration0057DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	rerunMigrationDDL(t, db, ctx, "0057_spend_provider_view.sql")

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
		t.Fatalf("macro after re-run: %v", err)
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
	// The two new columns, and the three refinements 0042 did not have.
	for _, c := range []string{"provider_spend_detailed", "provider_spend_primary",
		"merchant_name", "spend_detailed", "spend_primary", "provenance"} {
		if !have[c] {
			t.Errorf("spend_txn_categories() lost %q in the re-issue", c)
		}
	}
}

// TestProviderViewRecordsWithoutDeciding pins the two halves of the
// policy at once: the issuer's mapped value is recorded on every row it
// translates, and a catch-all among them does not become the verdict.
func TestProviderViewRecordsWithoutDeciding(t *testing.T) {
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

// TestSpendCategoryLabelsMatchGoTable pins the seeded display labels to
// canonical.SpendLabel. The migration seeds them literally so one can be
// corrected by hand; this is what stops a correction there from silently
// disagreeing with the rule in Go, and what catches a new taxonomy value
// seeded without a label at all.
func TestSpendCategoryLabelsMatchGoTable(t *testing.T) {
	db, ctx := openMigrated(t)
	rows, err := db.QueryContext(ctx,
		`SELECT spend_primary, spend_detailed, label, primary_label FROM spend_categories`)
	if err != nil {
		t.Fatalf("read labels: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var prim, det string
		var label, primLabel sql.NullString
		if err := rows.Scan(&prim, &det, &label, &primLabel); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
		if !label.Valid || !primLabel.Valid {
			t.Errorf("%s has no label; every value must read as something", det)
			continue
		}
		if want := canonical.SpendLabel(det); label.String != want {
			t.Errorf("%s label = %q, want %q", det, label.String, want)
		}
		if want := canonical.SpendPrimaryLabel(prim); primLabel.String != want {
			t.Errorf("%s primary_label = %q, want %q", det, primLabel.String, want)
		}
	}
	if n != len(canonical.SpendCategories) {
		t.Errorf("labelled %d rows, want %d", n, len(canonical.SpendCategories))
	}
}

// TestMigration0058DDLIsRerunnable holds the label seed to the replay
// bar: the ALTERs are IF NOT EXISTS and the UPDATE is idempotent, so a
// second application neither fails nor changes a label.
func TestMigration0058DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	rerunMigrationDDL(t, db, ctx, "0058_spend_category_labels.sql")
	var label string
	if err := db.QueryRowContext(ctx,
		`SELECT label FROM spend_categories WHERE spend_detailed = 'BANK_FEES_ATM_FEES'`).
		Scan(&label); err != nil {
		t.Fatalf("read label after re-run: %v", err)
	}
	if label != "ATM fees" {
		t.Errorf("label = %q after a re-run, want %q", label, "ATM fees")
	}
}

// TestSpendCategoryCatchAllMatchesGoTable pins the seeded catch-all flag
// to the Go predicates. The rule is expressed twice — once in Go for the
// enrichment pass, once in SQL for the seed — and this is what stops the
// two drifting.
//
// The seed reads a value's own spelling and knows nothing of families,
// while Go asks the question inside one vocabulary: a row is a catch-all
// if it is its own family's. That is the same rule read from two places,
// and the OR below is where they meet — no value is a catch-all in one
// family and an ordinary value in the other, because no value is in both
// unless it is a delta, and a delta is never one.
func TestSpendCategoryCatchAllMatchesGoTable(t *testing.T) {
	db, ctx := openMigrated(t)
	rows, err := db.QueryContext(ctx,
		`SELECT spend_detailed, catch_all FROM spend_categories`)
	if err != nil {
		t.Fatalf("read catch_all: %v", err)
	}
	defer rows.Close()
	seen, flagged := 0, 0
	for rows.Next() {
		var det string
		var flag sql.NullBool
		if err := rows.Scan(&det, &flag); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		if !flag.Valid {
			t.Errorf("%s has no catch_all flag", det)
			continue
		}
		if flag.Bool {
			flagged++
		}
		want := canonical.CatchAllSpendDetailed(det) || canonical.CatchAllIncomeDetailed(det)
		if flag.Bool != want {
			t.Errorf("%s catch_all = %v, want %v", det, flag.Bool, want)
		}
	}
	if seen != len(canonical.SpendCategories) {
		t.Errorf("checked %d rows, want %d", seen, len(canonical.SpendCategories))
	}
	if flagged == 0 {
		t.Error("no value is a catch-all; the seed is not doing anything")
	}
}

// TestMigration0069DDLIsRerunnable holds the income seed to the replay
// bar and, in doing so, pins the three things that migration decides.
//
// The family of every row: a value seeded before 0069 is spending's, a
// value seeded by it is income's, and the three deltas both families
// read are 'both'. The catch-all flag, which nothing set by hand — the
// twenty rows 0069 adds would carry none at all, 0060's UPDATE having
// run once at version 60, and the re-issued rule is what gives them
// one and marks income's single catch-all. And that a replay neither
// duplicates a row nor changes a family.
//
// What it does NOT pin is the NULL guard on the blanket family stamp:
// this migration re-asserts its own rows a few statements later, so
// removing the guard leaves every assertion here green. The guard is
// for rows it does not re-assert, and
// TestMigration0069LeavesALaterFamilyStampAlone is what holds it.
func TestMigration0069DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)

	rerunMigrationDDL(t, db, ctx, "0069_income_taxonomy.sql")

	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM spend_categories`).Scan(&n); err != nil {
		t.Fatalf("count spend_categories: %v", err)
	}
	if n != len(canonical.SpendCategories) {
		t.Errorf("spend_categories = %d rows after re-run, want %d", n, len(canonical.SpendCategories))
	}

	for _, tc := range []struct{ detailed, family, label string }{
		{"FOOD_AND_DRINK_GROCERIES", "spending", "Groceries"},
		{canonical.SpendDetailedCardSpend, "spending", "Uncategorized card spend"},
		{"INCOME_WAGES", "income", "Wages"},
		{canonical.IncomeDetailedCapitalReturn, "income", "Capital return"},
		{canonical.SpendDetailedInternalTransfer, "both", "Internal transfer"},
		{canonical.SpendDetailedGift, "both", "Gift"},
		{canonical.SpendDetailedOther, "both", "Other"},
	} {
		var family, label string
		if err := db.QueryRowContext(ctx,
			`SELECT family, label FROM spend_categories WHERE spend_detailed = ?`,
			tc.detailed).Scan(&family, &label); err != nil {
			t.Errorf("read %s after re-run: %v", tc.detailed, err)
			continue
		}
		if family != tc.family {
			t.Errorf("%s family = %q after a re-run, want %q", tc.detailed, family, tc.family)
		}
		if label != tc.label {
			t.Errorf("%s label = %q after a re-run, want %q", tc.detailed, label, tc.label)
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
	if len(catchAlls) != 1 || catchAlls[0] != "INCOME_OTHER_INCOME" {
		t.Errorf("income catch-alls = %v, want [INCOME_OTHER_INCOME]", catchAlls)
	}
}

// TestMigration0069LeavesALaterFamilyStampAlone pins the one thing in
// 0069 nothing else can reach: the `WHERE family IS NULL` guard on the
// blanket 'spending' stamp.
//
// The guard is idle for the rows 0069 seeds itself — the INSERTs below
// it re-assert their family in the same script — so every other
// assertion about this migration stays green without it. What it
// protects is a row a LATER migration seeds into a family of its own,
// which 0069 knows nothing about and cannot restore. A replay against a
// database holding one is exactly what the rerun tests do, and an
// unguarded stamp would demote it to spending silently.
//
// The stand-in row is synthetic and local to this database: the point is
// the stamp, not the value.
func TestMigration0069LeavesALaterFamilyStampAlone(t *testing.T) {
	db, ctx := openMigrated(t)

	const later = "INCOME_EXAMPLE_LATER_VALUE"
	if _, err := db.ExecContext(ctx, `
INSERT INTO spend_categories
    (spend_primary, spend_detailed, description, label, primary_label, catch_all, family)
VALUES ('INCOME', ?, 'A value a later migration seeds', 'Example later value', 'Income', FALSE, 'income')`,
		later); err != nil {
		t.Fatalf("seed a later value: %v", err)
	}

	rerunMigrationDDL(t, db, ctx, "0069_income_taxonomy.sql")

	var family string
	if err := db.QueryRowContext(ctx,
		`SELECT family FROM spend_categories WHERE spend_detailed = ?`, later).Scan(&family); err != nil {
		t.Fatalf("read %s after re-run: %v", later, err)
	}
	if family != "income" {
		t.Errorf("%s family = %q after a 0069 replay, want %q: the blanket stamp must skip a row that already has one",
			later, family, "income")
	}
}

// TestSpendKindFloorPlacesWhatNothingElseCould pins migration 0066's
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
func TestSpendKindFloorPlacesWhatNothingElseCould(t *testing.T) {
	db, ctx := openMigrated(t)
	seedSpendingFixture(t, db, ctx)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount) VALUES
            ('test-src', 'T-ADR',   1000, 'CASH1', 'fee',      'USD',  -1.50),
            ('test-src', 'T-WHT',   1000, 'CASH1', 'tax',      'USD', -12.00),
            ('test-src', 'T-FXFEE', 1000, 'CARD1', 'fee',      'USD',  -3.00),
            ('test-src', 'T-NOCAT', 1000, 'CASH1', 'purchase', 'USD', -20.00),
            ('test-src', 'T-MARGIN', 1000, 'CASH1', 'interest', 'USD',  -6.66),
            ('test-src', 'T-CREDIT', 1000, 'CASH1', 'interest', 'USD',   4.00);

        -- Nothing placed any of them; the first three carry a signature.
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, assigned_at) VALUES
            ('test-src', 'T-ADR',   'EXAMPLE HOLDINGS ADR', 1, NULL, 'signature-only', 100),
            ('test-src', 'T-WHT',   'EXAMPLE TREASURY ETF', 1, NULL, 'signature-only', 100),
            ('test-src', 'T-FXFEE', 'FOREIGN TRANSACTION FEE', 1, NULL, 'signature-only', 100),
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
		"T-WHT": {canonical.SpendDetailedWithholdingTax, "kind"},
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
	db, ctx := openMigrated(t)
	seedSpendingFixture(t, db, ctx)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount)
        VALUES ('test-src', 'T-ADR', 1000, 'CASH1', 'fee', 'USD', -1.50);
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
