package gold

import "testing"

func TestStatusSpendCounters(t *testing.T) {
	db, ctx := openMigrated(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, first_seen_at, last_seen_at) VALUES
            ('test-src', 'CASH1', 'cash',      'Everyday',  1, 1),
            ('test-src', 'CARD1', 'card',      'Card',      1, 1),
            ('test-src', 'BRK1',  'brokerage', 'Brokerage', 1, 1);

        INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id,
                                   currency, balance_kind, amount) VALUES
            ('test-src', 9000, 'CASH1', 'USD', 'settled', 100),
            ('test-src', 3000, 'CARD1', 'USD', 'settled', -50);

        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id,
                               position_key, asset_class, currency, market_value) VALUES
            ('test-src', 9000, 'BRK1', 'P1', 'equity', 'USD', 1000);

        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount) VALUES
            ('test-src', 'T-DEP',    9000, 'CASH1', 'deposit',    'USD', 500),
            ('test-src', 'T-WD',     9000, 'CASH1', 'withdrawal', 'USD', -80),
            ('test-src', 'T-BUY',    3000, 'CARD1', 'purchase',   'USD', -20),
            ('test-src', 'T-OTHER',  3000, 'CASH1', 'other',      'USD', -30),
            ('test-src', 'T-JRNL',   3000, 'CASH1', 'journal',    'USD', -35),
            ('test-src', 'T-OTHBRK', 3000, 'BRK1',  'other',      'USD', -40);

        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, assigned_at) VALUES
            ('test-src', 'T-WD',  'SIG A', 1, NULL, 'signature-only', 100),
            ('test-src', 'T-BUY', 'SIG B', 1, 'FOOD_AND_DRINK_GROCERIES', 'provider', 100);
    `); err != nil {
		t.Fatalf("seed: %v", err)
	}

	st, err := StatusForSource(ctx, db, "test-src", true)
	if err != nil {
		t.Fatalf("StatusForSource: %v", err)
	}
	if st == nil {
		t.Fatal("StatusForSource returned nil for a registered source")
	}

	// T-WD is the only spending line with no resolved category.
	if st.UncategorizedSpendCount != 1 {
		t.Errorf("UncategorizedSpendCount = %d, want 1", st.UncategorizedSpendCount)
	}
	// Both CATCH-ALL kinds count, and only on an IN-SCOPE account: the
	// cash `other` and `journal` rows are spending gaps, the brokerage
	// `other` row is not.
	if st.ExcludedUnmappedCount != 2 {
		t.Errorf("ExcludedUnmappedCount = %d, want 2 (the in-scope 'other' and 'journal' rows)",
			st.ExcludedUnmappedCount)
	}

	// The per-kind breakdown is what shows the card population going
	// stale behind a current deposit population.
	byKind := map[string]AccountKindActivity{}
	for _, a := range st.PerKindActivity {
		byKind[a.AccountKind] = a
	}
	if len(byKind) != 3 {
		t.Fatalf("PerKindActivity = %+v, want one row per account kind", st.PerKindActivity)
	}
	if got := byKind["cash"]; got.LatestTransactionAt != 9000 || got.LatestSnapshotAt != 9000 || got.Accounts != 1 {
		t.Errorf("cash activity = %+v", got)
	}
	if got := byKind["card"]; got.LatestTransactionAt != 3000 || got.LatestSnapshotAt != 3000 {
		t.Errorf("card activity = %+v, want the stale extrema", got)
	}
	if got := byKind["brokerage"]; got.LatestSnapshotAt != 9000 || got.LatestTransactionAt != 3000 {
		t.Errorf("brokerage activity = %+v", got)
	}
}

// TestStatusPerKindSkipsSingleKindSources: a source holding one
// account kind gets no breakdown, because it would only repeat the
// source-wide extrema printed a few lines above it.
func TestStatusPerKindSkipsSingleKindSources(t *testing.T) {
	db, ctx := openMigrated(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, first_seen_at, last_seen_at) VALUES
            ('test-src', 'CASH1', 'cash', 'Everyday', 1, 1),
            ('test-src', 'CASH2', 'cash', 'Savings',  1, 1);
    `); err != nil {
		t.Fatalf("seed: %v", err)
	}
	st, err := StatusForSource(ctx, db, "test-src", true)
	if err != nil {
		t.Fatalf("StatusForSource: %v", err)
	}
	if len(st.PerKindActivity) != 0 {
		t.Errorf("PerKindActivity = %+v, want empty for a single-kind source", st.PerKindActivity)
	}
}

// TestStatusSpendCountersSkippedWithoutVerbose keeps the default
// status path off the spending macros: they join three overlay tables
// and the one-line overview runs them once per source.
func TestStatusSpendCountersSkippedWithoutVerbose(t *testing.T) {
	db, ctx := openMigrated(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, first_seen_at, last_seen_at) VALUES
            ('test-src', 'CASH1', 'cash', 'Everyday', 1, 1);
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount) VALUES
            ('test-src', 'T-WD', 100, 'CASH1', 'withdrawal', 'USD', -80),
            ('test-src', 'T-OT', 100, 'CASH1', 'other',      'USD', -30);
    `); err != nil {
		t.Fatalf("seed: %v", err)
	}
	st, err := StatusForSource(ctx, db, "test-src", false)
	if err != nil {
		t.Fatalf("StatusForSource: %v", err)
	}
	if st.UncategorizedSpendCount != 0 || st.ExcludedUnmappedCount != 0 || len(st.PerKindActivity) != 0 {
		t.Errorf("non-verbose status computed spending counters: %+v", st)
	}
}
