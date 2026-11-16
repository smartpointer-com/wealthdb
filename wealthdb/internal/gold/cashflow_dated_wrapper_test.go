package gold

import (
	"maps"
	"slices"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/enrichtest"
)

// TestADatedWrapperMovesTheBoundaryOnItsDay covers a dated wrapper: an account
// whose wrapper changes on a day, inside the household's pool before it and
// a vehicle after it. Before the day a move into it is pool-internal
// and its own spending is a line; after it the move is a vehicles
// crossing and its spending is the vehicle's. Its income and its rows
// cash flow excludes by kind are the pool's before the day alone. Every
// figure is invented.
func TestADatedWrapperMovesTheBoundaryOnItsDay(t *testing.T) {
	t.Parallel()
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
	} {
		if _, err := db.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	for _, id := range []string{"D-IN-BEFORE", "D-IN-AFTER"} {
		enrichtest.SeedSpend(t, db, enrichtest.Spend{Source: "cf", Txn: id, Signature: "sig",
			Detailed: "internal_transfer", Provenance: "rule", FarSource: "cf", FarAccount: "TRUST"})
	}
	for _, id := range []string{"D-OWN-BEFORE", "D-OWN-AFTER", "D-OWN-ON"} {
		enrichtest.SeedSpend(t, db, enrichtest.Spend{Source: "cf", Txn: id, Signature: "sig",
			Detailed: "GENERAL_SERVICES_OTHER_GENERAL_SERVICES", Provenance: "rule"})
	}
	excludedByKind := func() int {
		t.Helper()
		s := &SourceStatus{SilverSourceID: "cf"}
		if label, err := driftCounts(ctx, db, map[string]*SourceStatus{"cf": s}); err != nil {
			t.Fatalf("drift counts (%s): %v", label, err)
		}
		return s.CashflowExcludedByKindCount
	}
	excludedBefore := excludedByKind()
	if _, err := db.ExecContext(ctx, `
	    INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
	                              account_external_id, kind, currency, net_amount, description) VALUES
	        ('cf', 'D-EARN-BEFORE', ?, 'TRUST', 'interest', 'USD', 7, 'trust interest'),
	        ('cf', 'D-EARN-ON',     ?, 'TRUST', 'interest', 'USD', 8, 'trust interest'),
	        ('cf', 'D-EARN-AFTER',  ?, 'TRUST', 'interest', 'USD', 9, 'trust interest'),
	        ('cf', 'D-JRNL-BEFORE', ?, 'TRUST', 'journal', 'USD', -3, 'trust journal'),
	        ('cf', 'D-JRNL-ON',     ?, 'TRUST', 'journal', 'USD', -4, 'trust journal'),
	        ('cf', 'D-JRNL-AFTER',  ?, 'TRUST', 'journal', 'USD', -5, 'trust journal')`,
		change-5*day, change, change+5*day, change-5*day, change, change+5*day); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, id := range []string{"D-EARN-BEFORE", "D-EARN-ON", "D-EARN-AFTER"} {
		enrichtest.SeedIncome(t, db, enrichtest.Income{Source: "cf", Txn: id, Signature: "sig-earn",
			Detailed: "INCOME_INTEREST_EARNED", Provenance: "rule"})
	}
	// The income side reads the boundary by the row's own day, half-open
	// as the spending side does.
	inPool := scanPairs(t, db, `SELECT transaction_external_id, in_pool::VARCHAR
	        FROM income_lines_base(0, 9999999999) WHERE transaction_external_id LIKE 'D-EARN-%'`)
	if want := map[string]string{"D-EARN-BEFORE": "true", "D-EARN-ON": "false", "D-EARN-AFTER": "false"}; !maps.Equal(inPool, want) {
		t.Errorf("the trust's income in_pool = %v, want %v", inPool, want)
	}
	// status counts an excluded row where its account is in the pool on
	// the row's day: the journal before the day alone.
	if got := excludedByKind() - excludedBefore; got != 1 {
		t.Errorf("cashflow excluded by kind counts %d of the trust's journals, want the one before the day", got)
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
	check("dated")

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
