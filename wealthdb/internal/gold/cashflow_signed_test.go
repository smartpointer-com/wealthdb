package gold

import (
	"testing"
)

// TestASignPinnedCashLegJoinsInvesting covers migration 0161: a
// corporate action's cash leg, on a source whose kind the sign table
// pins, is investing and in the coverage ledger; the same kind moving
// shares, the same cash leg on an unpinned source, and an FX leg stay
// excluded and in the unsigned volume. Every figure is invented.
func TestASignPinnedCashLegJoinsInvesting(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path,
                                    high_watermark, first_loaded_at, last_loaded_at) VALUES
            ('cf', 'fidelity', '/tmp/cf.db', -1, 0, 0),
            ('xs', 'viac', '/tmp/xs.db', -1, 0, 0);
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              tax_wrapper, display_name, first_seen_at, last_seen_at) VALUES
            ('xs', 'FUND', 'brokerage', 'taxable_personal', 'Fund', 1, 1);
        INSERT INTO silver_sources (silver_source_id, silver_kind, silver_path,
                                    high_watermark, first_loaded_at, last_loaded_at) VALUES
            ('sw', 'schwab', '/tmp/sw.db', -1, 0, 0);
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              tax_wrapper, display_name, first_seen_at, last_seen_at) VALUES
            ('sw', 'BRK', 'brokerage', 'taxable_personal', 'Other broker', 1, 1);
        INSERT INTO cash_signed_kinds (silver_kind, kind) VALUES
            ('fidelity', 'corporate_action'), ('schwab', 'corporate_action');
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount, quantity, description) VALUES
            ('cf', 'S-LIEU',  1728000, 'BROK', 'corporate_action', 'USD', 12.5, NULL, 'IN LIEU OF FRACTION'),
            ('cf', 'S-SHARES', 1728000, 'BROK', 'corporate_action', 'USD', 900, 10, 'MERGER EXCHANGE'),
            ('cf', 'S-FX',    1728000, 'CASH', 'fx', 'USD', -30, NULL, 'FX'),
            ('xs', 'S-OTHER', 1728000, 'FUND', 'corporate_action', 'USD', -7, NULL, 'FUND EVENT'),
            ('cf', 'S-ZERO',  1728000, 'BROK', 'corporate_action', 'USD', 0, NULL, 'NAME CHANGE'),
            ('sw', 'S-SCHWAB', 1728000, 'BRK', 'corporate_action', 'USD', 25, NULL, 'UNISSUED RIGHTS');
        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, instrument_external_id, kind, currency,
                                  net_amount, quantity, description) VALUES
            ('cf', 'S-CASHMM', 1728000, 'BROK', 'MMF', 'corporate_action', 'USD', 3, NULL, 'FUND CREDIT');
        INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id,
                                   currency, balance_kind, amount) VALUES
            ('cf', 86400,   'BROK', 'USD', 'current', 100),
            ('cf', 3456000, 'BROK', 'USD', 'current', 112.5);`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	node := map[string]string{}
	rows, err := db.QueryContext(ctx, `SELECT transaction_external_id, disposition,
	        COALESCE(section, '') || '.' || COALESCE(class, '') || '.' || COALESCE(grp, '')
	          FROM cashflow_txn_nodes(0, 9999999999) WHERE transaction_external_id LIKE 'S-%'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, disp, n string
		if err := rows.Scan(&id, &disp, &n); err != nil {
			t.Fatal(err)
		}
		node[id] = disp + " " + n
	}
	rows.Close()
	want := map[string]string{
		// No instrument named: class `other`, never Untracked investments.
		"S-LIEU": "line investing.other.corporate_actions",
		// Into a cash-class holding: cash becoming cash, pool-internal.
		"S-CASHMM": "internal ..",
		"S-ZERO":   "excluded ..",
		"S-SCHWAB": "line investing.other.corporate_actions",
		"S-SHARES": "excluded ..",
		"S-FX":     "excluded ..",
		"S-OTHER":  "excluded ..",
	}
	for id, w := range want {
		if node[id] != w {
			t.Errorf("%s = %q, want %q", id, node[id], w)
		}
	}

	cov, err := CashflowCoverage(ctx, db, 0, 9999999999, "total")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range cov {
		if r.SourceID != "cf" || r.Account != "Brokerage" {
			continue
		}
		if show(r.Ledger) != "15.5" || show(r.UnsignedVolume) != "900" {
			t.Errorf("brokerage coverage: ledger %v, unsigned %v; want the two cash legs in the ledger, the share leg unsigned",
				show(r.Ledger), show(r.UnsignedVolume))
		}
		return
	}
	t.Error("no coverage row for the brokerage account")
}
