package loader

import (
	"context"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
)

// TestResetClearsBothOverlays pins what a per-source reset owns. Both
// enrichment overlays are derived from the transactions being deleted
// and are re-asserted by the next pass; both verdict stores are global,
// were paid for, and must survive.
func TestResetClearsBothOverlays(t *testing.T) {
	db, err := gold.Open(":memory:", gold.ModeReadWrite)
	if err != nil {
		t.Fatalf("open gold: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := gold.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources(silver_source_id, silver_kind, silver_path,
            high_watermark, first_loaded_at, last_loaded_at)
        VALUES ('gone', 'chase', '/tmp/a.db', -1, 0, 0),
               ('kept', 'chase', '/tmp/b.db', -1, 0, 0);

        INSERT INTO spend_txn_enrichment(silver_source_id, transaction_external_id,
            merchant_signature, signature_version, spend_detailed, provenance, assigned_at)
        VALUES ('gone', 'T1', 'SIG', 1, NULL, 'signature-only', 1),
               ('kept', 'T2', 'SIG', 1, NULL, 'signature-only', 1);

        INSERT INTO income_txn_enrichment(silver_source_id, transaction_external_id,
            payer_signature, signature_version, income_detailed, provenance, assigned_at)
        VALUES ('gone', 'T3', 'SIG', 1, NULL, 'signature-only', 1),
               ('kept', 'T4', 'SIG', 1, NULL, 'signature-only', 1);

        INSERT INTO spend_merchant_categories(merchant_signature, merchant_name,
            spend_detailed, signature_version, assigned_at, model_name)
        VALUES ('SIG', 'Example Merchant', 'FOOD_AND_DRINK_GROCERIES', 1, 1, 'm');

        INSERT INTO income_payer_categories(payer_signature, payer_name,
            income_detailed, signature_version, assigned_at, model_name)
        VALUES ('SIG', 'Example Payer', 'INCOME_WAGES', 1, 1, 'm');

        INSERT INTO income_account_scope(silver_source_id, account_external_id, mode)
        VALUES ('gone', 'ACC1', 'exclude');
    `); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := New(db).Reset(ctx, "gone"); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	count := func(q string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	for _, tc := range []struct {
		name, query string
		want        int
	}{
		{"the reset source's spending overlay",
			`SELECT COUNT(*) FROM spend_txn_enrichment WHERE silver_source_id = 'gone'`, 0},
		{"the reset source's income overlay",
			`SELECT COUNT(*) FROM income_txn_enrichment WHERE silver_source_id = 'gone'`, 0},
		{"the other source's spending overlay",
			`SELECT COUNT(*) FROM spend_txn_enrichment WHERE silver_source_id = 'kept'`, 1},
		{"the other source's income overlay",
			`SELECT COUNT(*) FROM income_txn_enrichment WHERE silver_source_id = 'kept'`, 1},
		// Global, paid for, and keyed by signature rather than source.
		{"the merchant store", `SELECT COUNT(*) FROM spend_merchant_categories`, 1},
		{"the payer store", `SELECT COUNT(*) FROM income_payer_categories`, 1},
		// Configuration stamped into gold, re-stamped from config.
		{"the income account scope", `SELECT COUNT(*) FROM income_account_scope`, 1},
	} {
		if got := count(tc.query); got != tc.want {
			t.Errorf("%s = %d rows after the reset, want %d", tc.name, got, tc.want)
		}
	}
}
