package gold

import "testing"

func TestStatusSpendCounters(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, first_seen_at, last_seen_at) VALUES
            ('test-src', 'CASH1', 'cash',      'Everyday',  1, 1),
            ('test-src', 'CARD1', 'card',      'Card',      1, 1),
            ('test-src', 'CUST1', 'custody',   'Custody',   1, 1);

        INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id,
                                   currency, balance_kind, amount) VALUES
            ('test-src', 9000, 'CASH1', 'USD', 'settled', 100),
            ('test-src', 3000, 'CARD1', 'USD', 'settled', -50);

        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id,
                               position_key, asset_class, currency, market_value) VALUES
            ('test-src', 9000, 'CUST1', 'P1', 'equity', 'USD', 1000);

        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount) VALUES
            ('test-src', 'T-DEP',    9000, 'CASH1', 'deposit',    'USD', 500),
            ('test-src', 'T-WD',     9000, 'CASH1', 'withdrawal', 'USD', -80),
            ('test-src', 'T-BUY',    3000, 'CARD1', 'purchase',   'USD', -20),
            ('test-src', 'T-OTHER',  3000, 'CASH1', 'other',      'USD', -30),
            ('test-src', 'T-JRNL',   3000, 'CASH1', 'journal',    'USD', -35),
            ('test-src', 'T-OTHBRK', 3000, 'CUST1',  'other',      'USD', -40);

        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, assigned_at) VALUES
            ('test-src', 'T-WD',  'SIG A', 1, NULL, 'signature-only', 100),
            ('test-src', 'T-BUY', 'SIG B', 1, 'FOOD_AND_DRINK_GROCERIES', 'provider', 100);

        -- Since migration 0068 no account kind is out of scope, so the
        -- out-of-scope account this test needs is made one the only way
        -- there is.
        INSERT INTO spend_account_scope (silver_source_id, account_external_id, mode)
             VALUES ('test-src', 'CUST1', 'exclude');
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
	// cash `other` and `journal` rows are spending gaps, the excluded
	// account's `other` row is not.
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
	if got := byKind["custody"]; got.LatestSnapshotAt != 9000 || got.LatestTransactionAt != 3000 {
		t.Errorf("custody activity = %+v", got)
	}
}

// TestStatusPerKindSkipsSingleKindSources: a source holding one
// account kind gets no breakdown, because it would only repeat the
// source-wide extrema printed a few lines above it.
func TestStatusPerKindSkipsSingleKindSources(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// TestStatusCountsAKindGuessedBySign pins the drift counter for the
// adapters that do NOT fall to `other` on an unrecognised source kind:
// they park the raw value in payload.source_kind and kind the row by
// its sign instead, so the row reads as an ordinary purchase or bill
// everywhere downstream and no `other` counter can see it.
func TestStatusCountsAKindGuessedBySign(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, first_seen_at, last_seen_at) VALUES
            ('test-src', 'CARD1', 'card', 'Example Card', 1, 1);

        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount,
                                  payload) VALUES
            ('test-src', 'T-GUESSED', 3000, 'CARD1', 'purchase', 'USD', -20,
             '{"source_kind": "A DIRECTION THIS BUILD HAS NOT SEEN"}'),
            -- The other shape: an adapter that parks the raw value AND
            -- buckets the row as 'other'. The two counters must not both
            -- claim it, or status -v double-reports one row of drift.
            ('test-src', 'T-OTHER',   3000, 'CARD1', 'other', 'USD', -50,
             '{"source_kind": "A KIND THIS BUILD FILED AS OTHER"}'),
            -- A payload that names the key but holds no value: JSON null
            -- is not SQL NULL, so only a string-typed read excludes it.
            ('test-src', 'T-JSONNULL', 3000, 'CARD1', 'purchase', 'USD', -60,
             '{"source_kind": null}'),
            ('test-src', 'T-MAPPED',  3000, 'CARD1', 'purchase', 'USD', -30, '{}'),
            ('test-src', 'T-NOPAY',   3000, 'CARD1', 'purchase', 'USD', -40, NULL);
    `); err != nil {
		t.Fatalf("seed: %v", err)
	}
	st, err := StatusForSource(ctx, db, "test-src", true)
	if err != nil {
		t.Fatalf("StatusForSource: %v", err)
	}
	if st.GuessedTxKindCount != 1 {
		t.Errorf("GuessedTxKindCount = %d, want 1 — only the row carrying a raw source_kind under a kind that is not 'other'",
			st.GuessedTxKindCount)
	}
	// The guessed row is a purchase, which is exactly why it needs a
	// counter of its own; the `other` row belongs to the older counter
	// alone.
	if st.OtherTxKindCount != 1 {
		t.Errorf("OtherTxKindCount = %d, want 1", st.OtherTxKindCount)
	}
}

// TestStatusForSourcesCountsEachSourceApart: the drift counters run once
// for all the sources, grouped, and each count lands on its own source.
// A source with no such rows keeps 0, an unregistered id answers nil,
// and an id given twice answers the same both times.
func TestStatusForSourcesCountsEachSourceApart(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	execEach(t, db, `
        INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path,
                                    high_watermark, first_loaded_at, last_loaded_at)
             VALUES ('second-src', 'schwab', '/tmp/second.db', -1, 0, 0);
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, first_seen_at, last_seen_at) VALUES
            ('test-src',   'CASH1', 'cash', 'Everyday', 1, 1),
            ('second-src', 'CASH2', 'cash', 'Savings',  1, 1);
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount) VALUES
            ('test-src',   'T-WD1', 100, 'CASH1', 'withdrawal', 'USD', -80),
            ('test-src',   'T-WD2', 100, 'CASH1', 'withdrawal', 'USD', -15),
            ('test-src',   'T-OT',  100, 'CASH1', 'other',      'USD', -30),
            ('second-src', 'T-WD3', 100, 'CASH2', 'withdrawal', 'USD', -40);`)
	got := StatusForSources(ctx, db, []string{"test-src", "no-such-src", "second-src", "test-src"}, true)
	if len(got) != 4 {
		t.Fatalf("%d results for 4 ids", len(got))
	}
	if r := got[1]; r.Status != nil || r.Err != nil {
		t.Errorf("an unregistered id: %+v, want nil and no error", r)
	}
	for _, c := range []struct {
		i                     int
		uncategorized, mapped int
	}{{0, 2, 1}, {2, 1, 0}, {3, 2, 1}} {
		r := got[c.i]
		if r.Err != nil || r.Status == nil {
			t.Errorf("ids[%d]: %+v", c.i, r)
			continue
		}
		if r.Status.UncategorizedSpendCount != c.uncategorized || r.Status.ExcludedUnmappedCount != c.mapped {
			t.Errorf("%s: uncategorised %d, excluded-unmapped %d; want %d and %d", r.Status.SilverSourceID,
				r.Status.UncategorizedSpendCount, r.Status.ExcludedUnmappedCount, c.uncategorized, c.mapped)
		}
	}
}
