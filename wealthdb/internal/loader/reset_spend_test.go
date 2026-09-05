package loader_test

import (
	"context"
	"testing"
)

// TestResetClearsEnrichmentKeepsMerchantStore pins the asymmetry in
// the spending overlay's reset semantics. The per-transaction
// enrichment is derived from the very transactions being deleted, is
// source-scoped, and is recomputed by the next pass — so it goes, and
// only for the source being reset. The merchant store is global
// knowledge keyed by merchant signature and its verdicts were paid
// for, so it stays; so does the account scope, which is configuration
// stamped into gold rather than source data.
func TestResetClearsEnrichmentKeepsMerchantStore(t *testing.T) {
	h := newHarness(t)

	h.silverExec(t, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES (1000, 'ACC1', '{}');
    `)
	h.load(t)

	if _, err := h.gold.Exec(`
        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, assigned_at) VALUES
            ('schwab-test', 'T1', 'sig-market', 1, 'FOOD_AND_DRINK_GROCERIES', 'rule', 100),
            ('other-src',   'T2', 'sig-market', 1, NULL,                       'signature-only', 100);

        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name) VALUES
            ('sig-market', 'Corner Market', 'FOOD_AND_DRINK_GROCERIES', 1, 100, 'test-model');

        INSERT INTO spend_account_scope (silver_source_id, account_external_id, mode) VALUES
            ('schwab-test', 'ACC1', 'include');
    `); err != nil {
		t.Fatalf("seed spending overlay: %v", err)
	}

	if err := h.loader.Reset(context.Background(), "schwab-test"); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if got := h.goldScalar(t, `SELECT CAST(COUNT(*) AS VARCHAR) FROM spend_txn_enrichment
                                WHERE silver_source_id = 'schwab-test'`); got != "0" {
		t.Errorf("enrichment rows for the reset source = %s, want 0", got)
	}
	if got := h.goldScalar(t, `SELECT CAST(COUNT(*) AS VARCHAR) FROM spend_txn_enrichment
                                WHERE silver_source_id = 'other-src'`); got != "1" {
		t.Errorf("enrichment rows for another source = %s, want 1 (reset is source-scoped)", got)
	}
	if got := h.goldCount(t, "spend_merchant_categories"); got != 1 {
		t.Errorf("merchant store rows after reset = %d, want 1 (paid verdicts survive)", got)
	}
	if got := h.goldCount(t, "spend_account_scope"); got != 1 {
		t.Errorf("account scope rows after reset = %d, want 1 (config, not source data)", got)
	}
}
