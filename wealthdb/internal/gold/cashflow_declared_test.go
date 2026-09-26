package gold

import (
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// TestADeclaredAccountPlacesTheMoveByItsWrapper: a rule-placed
// own-account move whose far side is a declared account resolves by
// the declared wrapper, exactly as a collected far account does — a
// household wrapper makes the move pool-internal, a vehicle wrapper
// draws it as that vehicle's crossing — in both directions. The same
// move with no far account stays the residual.
func TestADeclaredAccountPlacesTheMoveByItsWrapper(t *testing.T) {
	db, ctx := openMigrated(t)
	seedResolutionFixture(t, db, ctx)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              tax_wrapper, display_name, first_seen_at, last_seen_at) VALUES
            (?, 'savings-bank', 'cash',      'taxable_personal', 'Example Savings', 1, 1),
            (?, 'old-plan',     'brokerage', '401k',             'Old Plan',        1, 1)`,
		canonical.DeclaredSourceID, canonical.DeclaredSourceID); err != nil {
		t.Fatalf("declare accounts: %v", err)
	}
	type row struct {
		id, kind string
		amount   float64
		far      string
		want     string
	}
	rows := []row{
		{"D-SAVE-OUT", "withdrawal", -1000, "savings-bank", "internal"},
		{"D-SAVE-IN", "deposit", 1000, "savings-bank", "internal"},
		{"D-PLAN-OUT", "withdrawal", -1000, "old-plan", "vehicles.retirement.retirement"},
		{"D-PLAN-IN", "deposit", 1000, "old-plan", "vehicles.retirement.retirement"},
		{"D-NONE-OUT", "withdrawal", -1000, "", "vehicles.unpaired.unnamed"},
	}
	for _, r := range rows {
		if _, err := db.ExecContext(ctx, `
            INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                      account_external_id, kind, currency, net_amount, description)
                 VALUES ('cf', ?, 1728000, 'CASH', ?, 'USD', ?, ?)`,
			r.id, r.kind, r.amount, r.id); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
		if _, err := db.ExecContext(ctx, `
            INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                    merchant_signature, signature_version, spend_detailed, provenance,
                    far_silver_source_id, far_account_external_id, assigned_at)
                 VALUES ('cf', ?, 'sig', 1, 'internal_transfer', 'rule', ?, ?, 100)`,
			r.id, nullable(map[bool]string{true: canonical.DeclaredSourceID, false: ""}[r.far != ""]),
			nullable(r.far)); err != nil {
			t.Fatalf("seed overlay for %s: %v", r.id, err)
		}
		if r.kind == "deposit" {
			if _, err := db.ExecContext(ctx, `
                INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id,
                        payer_signature, signature_version, income_detailed, provenance, assigned_at)
                     VALUES ('cf', ?, 'sig', 1, 'internal_transfer', 'rule', 100)`, r.id); err != nil {
				t.Fatalf("seed income overlay for %s: %v", r.id, err)
			}
		}
	}
	got := seedLines(t, db, ctx, nil)
	for _, r := range rows {
		if got[r.id] != r.want {
			t.Errorf("%s (%s, far %q): %s, want %s", r.id, r.kind, r.far, got[r.id], r.want)
		}
	}
}
