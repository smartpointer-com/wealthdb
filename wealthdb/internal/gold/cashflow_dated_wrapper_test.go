package gold

import (
	"slices"
	"testing"
)

// TestADatedWrapperMovesTheBoundaryOnItsDay covers migration 0162: a trust
// that was the household's (grantor) until a day and a separate taxpayer
// (non-grantor) after it. Before the day a move into it is pool-internal
// and its own spending is a line; after it the move is a vehicles
// crossing and its spending is the vehicle's. Every figure is invented.
func TestADatedWrapperMovesTheBoundaryOnItsDay(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	const day = 86400
	change := int64(100 * day)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account_wrapper_history (silver_source_id, account_external_id, tax_wrapper, until_at)
		       VALUES ('cf', 'TRUST', 'trust_grantor', ?)`, []any{change}},
		{`INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
		                           account_external_id, kind, currency, net_amount, description) VALUES
		     ('cf', 'D-IN-BEFORE', ?, 'CASH',  'withdrawal', 'USD', -500, 'to trust'),
		     ('cf', 'D-IN-AFTER',  ?, 'CASH',  'withdrawal', 'USD', -600, 'to trust'),
		     ('cf', 'D-OWN-BEFORE', ?, 'TRUST', 'purchase', 'USD', -40, 'trust expense'),
		     ('cf', 'D-OWN-AFTER',  ?, 'TRUST', 'purchase', 'USD', -50, 'trust expense'),
		     ('cf', 'D-OWN-ON',     ?, 'TRUST', 'purchase', 'USD', -60, 'trust expense')`,
			[]any{change - 10*day, change + 10*day, change - 5*day, change + 5*day, change}},
		{`INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id, merchant_signature,
		         signature_version, spend_detailed, provenance, far_silver_source_id, far_account_external_id,
		         assigned_at) VALUES
		     ('cf', 'D-IN-BEFORE', 'sig', 1, 'internal_transfer', 'rule', 'cf', 'TRUST', 100),
		     ('cf', 'D-IN-AFTER',  'sig', 1, 'internal_transfer', 'rule', 'cf', 'TRUST', 100),
		     ('cf', 'D-OWN-BEFORE', 'sig', 1, 'GENERAL_SERVICES_OTHER_GENERAL_SERVICES', 'rule', NULL, NULL, 100),
		     ('cf', 'D-OWN-AFTER',  'sig', 1, 'GENERAL_SERVICES_OTHER_GENERAL_SERVICES', 'rule', NULL, NULL, 100),
		     ('cf', 'D-OWN-ON',     'sig', 1, 'GENERAL_SERVICES_OTHER_GENERAL_SERVICES', 'rule', NULL, NULL, 100)`, nil},
	} {
		if _, err := db.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	check := func(when string) {
		t.Helper()
		place := scanPairs(t, db, `SELECT transaction_external_id, disposition || ' ' || COALESCE(section, '')
		        FROM cashflow_txn_nodes(0, 9999999999) WHERE transaction_external_id LIKE 'D-IN-%'`)
		if place["D-IN-BEFORE"] != "internal " || place["D-IN-AFTER"] != "line vehicles" {
			t.Errorf("%s: moves into the trust = %v; want pool-internal before the day, a vehicles crossing after",
				when, place)
		}
		lines := scanStrings(t, db, `SELECT DISTINCT transaction_external_id FROM cashflow_lines_base(0, 9999999999)
		        WHERE transaction_external_id LIKE 'D-OWN-%'`)
		// The day itself is the override's: the spans are half-open.
		if !slices.Equal(lines, []string{"D-OWN-BEFORE"}) {
			t.Errorf("%s: the trust's own lines = %v; want the one before the day alone", when, lines)
		}
		var nodes, txns int
		if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM cashflow_txn_nodes(0, 9999999999)),
		        (SELECT count(*) FROM transactions)`).Scan(&nodes, &txns); err != nil || nodes != txns {
			t.Errorf("%s: %d nodes for %d transactions (%v): a span join fanned out", when, nodes, txns, err)
		}
	}
	check("at head")
	rerunMigrationDDL(t, db, ctx, "0162_dated_wrapper_override.sql")
	check("after replaying 0162")

	// Without a history row the account has one span: the current wrapper
	// throughout, as before the migration.
	if _, err := db.ExecContext(ctx, `DELETE FROM account_wrapper_history`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM cashflow_lines_base(0, 9999999999)
	        WHERE transaction_external_id LIKE 'D-OWN-%'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("undated: %d of the trust's rows are lines (%v), want none", n, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM account_wrapper_spans() WHERE account_external_id = 'TRUST'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("undated: the trust has %d spans (%v), want one", n, err)
	}
}
