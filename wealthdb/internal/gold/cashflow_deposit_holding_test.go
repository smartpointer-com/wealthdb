package gold

import "testing"

// TestTheMemoSeesADepositStatedAsAHolding covers migration 0163: an
// account whose source states its balance as a demand-deposit holding,
// and no cash balance, joins the memo's balance pool and is measured by
// coverage; an account with a cash balance keeps reading that alone.
// Every figure is invented.
func TestTheMemoSeesADepositStatedAsAHolding(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               asset_class, vehicle, currency, market_value) VALUES
            ('cf', 86400,   'SAVE', 'BAL', 'cash', 'demand_deposit', 'USD', 1000),
            ('cf', 3456000, 'SAVE', 'BAL', 'cash', 'demand_deposit', 'USD', 1200),
            ('cf', 3456000, 'SAVE', 'BAL2', 'cash', 'demand_deposit', 'USD', 50),
            ('cf', 86400,   'CASH', 'BAL', 'cash', 'demand_deposit', 'USD', 7);
        INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id,
                                   currency, balance_kind, amount) VALUES
            ('cf', 86400, 'CASH', 'USD', 'current', 500);
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount, description) VALUES
            ('cf', 'H-IN', 1728000, 'SAVE', 'deposit', 'USD', 250, 'interest and savings');`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM cashflow_balance_pool()
	        WHERE account_external_id = 'SAVE'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("SAVE in the balance pool: %d (%v), want it there", n, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM cashflow_cash_balances()
	        WHERE account_external_id = 'CASH'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("CASH balances: %d (%v), want its cash balance alone, no holding beside it", n, err)
	}
	// The memo measures it: two holdings on one snapshot sum to its
	// balance, so its 250 change is in cash_measured.
	var memo, measured float64
	if err := db.QueryRowContext(ctx, `SELECT CAST(cash_measured AS DOUBLE)
	        FROM report_cashflow_reconciliation(172800, 3500000, 'USD', 'total')`).Scan(&memo); err != nil || memo != 250 {
		t.Errorf("the memo's cash_measured = %v (%v), want SAVE's 250 change", memo, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT CAST(amount AS DOUBLE) FROM cashflow_cash_balances()
	        WHERE account_external_id = 'SAVE' AND snapshot_at = 3456000`).Scan(&measured); err != nil || measured != 1250 {
		t.Errorf("SAVE's closing balance = %v (%v), want the two holdings summed, 1250", measured, err)
	}
	cov, err := CashflowCoverage(ctx, db, 172800, 3500000, "total")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range cov {
		if r.Account == "Savings" {
			if r.Status != "measured" || show(r.Measured) != "250" {
				t.Errorf("Savings coverage: status %s, measured %s; want measured 250", r.Status, show(r.Measured))
			}
			return
		}
	}
	t.Error("no coverage row for the savings account")
}
