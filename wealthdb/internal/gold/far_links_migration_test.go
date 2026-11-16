package gold

import (
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/enrichtest"
)

// farSideMigration is the migration that moves the far side of a move
// off the spending overlay and into transfer_links.
func farSideMigration(t *testing.T) (prev int, m migration) {
	t.Helper()
	ms, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ms, func(m migration) bool { return strings.HasSuffix(m.name, "_far_side_in_links.sql") })
	if i < 1 {
		t.Fatal("no far side migration above another")
	}
	return ms[i-1].version, ms[i]
}

// A gold whose overlay carries the far sides, migrated, reads every far
// side from transfer_links as a fresh pass writes it: a link the pass
// wrote is kept as it is, a far side with no link gains one, and the
// cash flow nodes are those of a gold the new engine seeded. The
// overlay keeps every other column, in its place, and its index.
func TestTheFarSideMovesIntoTheLinks(t *testing.T) {
	t.Parallel()
	prev, m := farSideMigration(t)
	old, ctx := openAtVersion(t, prev)
	fresh, _ := openMigrated(t)
	for _, db := range []*sql.DB{old, fresh} {
		seedResolutionFixture(t, db, ctx)
		mustExec(t, db, `INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
		        account_external_id, kind, currency, net_amount, description) VALUES
		    ('cf', 'L-PAIR-OUT', 1728000, 'CASH', 'withdrawal', 'USD', -500, 'to plan'),
		    ('cf', 'L-PAIR-IN',  1728000, 'IRA',  'deposit',    'USD',  500, 'from cash'),
		    ('cf', 'L-MORT',     1728000, 'CASH', 'withdrawal', 'USD', -900, 'instalment'),
		    ('cf', 'L-DECL',     1728000, 'CASH', 'withdrawal', 'USD', -300, 'to a declared account'),
		    ('cf', 'L-STATED',   1728000, 'CASH', 'withdrawal', 'USD', -200, 'to a stated account'),
		    ('cf', 'L-SHOP',     1728000, 'CASH', 'purchase',   'USD',  -20, 'shop')`)
	}
	// The old shape: the far sides on the overlay, and one link a pass
	// wrote beside them.
	mustExec(t, old, `INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
	        merchant_signature, signature_version, spend_detailed, provenance,
	        far_silver_source_id, far_account_external_id, far_class, assigned_at) VALUES
	    ('cf', 'L-PAIR-OUT', 'sig', 1, 'internal_transfer', 'matcher', 'cf', 'IRA', NULL, 100),
	    ('cf', 'L-PAIR-IN',  'sig', 1, 'internal_transfer', 'matcher', 'cf', 'CASH', NULL, 100),
	    ('cf', 'L-MORT',     'sig', 1, 'internal_transfer', 'rule', NULL, NULL, 'mortgage', 100),
	    ('cf', 'L-DECL',     'sig', 1, 'internal_transfer', 'rule', 'declared', 'DECL1', NULL, 100),
	    ('cf', 'L-STATED',   'sig', 1, 'GENERAL_SERVICES_OTHER_GENERAL_SERVICES', 'rule', 'cf', 'SAVE', NULL, 100),
	    ('cf', 'L-SHOP',     'sig', 1, 'FOOD_AND_DRINK_GROCERIES', 'rule', NULL, NULL, NULL, 100);
	    INSERT INTO transfer_links VALUES
	    ('cf', 'L-PAIR-OUT', 'cf', 'IRA', 'L-PAIR-IN', NULL, 'amount', 'cash')`)
	// The new shape, as a pass writes it.
	enrichtest.SeedSpend(t, fresh,
		enrichtest.Spend{Source: "cf", Txn: "L-PAIR-OUT", Signature: "sig", Detailed: "internal_transfer",
			Provenance: "matcher", FarSource: "cf", FarAccount: "IRA", FarTxn: "L-PAIR-IN", FarEvidence: "amount"},
		enrichtest.Spend{Source: "cf", Txn: "L-PAIR-IN", Signature: "sig", Detailed: "internal_transfer",
			Provenance: "matcher", FarSource: "cf", FarAccount: "CASH", FarTxn: "L-PAIR-OUT", FarEvidence: "amount"},
		enrichtest.Spend{Source: "cf", Txn: "L-MORT", Signature: "sig", Detailed: "internal_transfer",
			Provenance: "rule", FarClass: "mortgage"},
		enrichtest.Spend{Source: "cf", Txn: "L-DECL", Signature: "sig", Detailed: "internal_transfer",
			Provenance: "rule", FarSource: "declared", FarAccount: "DECL1"},
		enrichtest.Spend{Source: "cf", Txn: "L-STATED", Signature: "sig",
			Detailed: "GENERAL_SERVICES_OTHER_GENERAL_SERVICES", Provenance: "rule", FarSource: "cf",
			FarAccount: "SAVE", FarEvidence: "stated-account"},
		enrichtest.Spend{Source: "cf", Txn: "L-SHOP", Signature: "sig", Detailed: "FOOD_AND_DRINK_GROCERIES",
			Provenance: "rule"})

	if err := applyMigration(ctx, old, m); err != nil {
		t.Fatalf("apply %s: %v", m.name, err)
	}
	links := func(db *sql.DB) map[string]string {
		return scanPairs(t, db, `SELECT transaction_external_id,
		        concat_ws(' ', far_silver_source_id, far_account_external_id, far_transaction_external_id,
		                  far_class, evidence, measure)
		      FROM transfer_links`)
	}
	want := map[string]string{
		// The link a pass wrote, as it wrote it.
		"L-PAIR-OUT": "cf IRA L-PAIR-IN amount cash",
		// A far side with no link: the partner leg and its phase are not
		// known, so it reads as a one-legged move until the next pass.
		"L-PAIR-IN": "cf CASH stated-account cash",
		"L-MORT":    "mortgage rule cash",
		"L-DECL":    "declared DECL1 rule cash",
		"L-STATED":  "cf SAVE stated-account cash",
	}
	if got := links(old); !maps.Equal(got, want) {
		t.Errorf("migrated links = %v, want %v", got, want)
	}

	// The overlay's shape is a fresh gold's.
	if err := Migrate(ctx, old); err != nil {
		t.Fatal(err)
	}
	shape := func(db *sql.DB) string {
		var s string
		if err := db.QueryRowContext(ctx, `SELECT sql FROM duckdb_tables() WHERE table_name = 'spend_txn_enrichment'`).
			Scan(&s); err != nil {
			t.Fatal(err)
		}
		var ix int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM duckdb_indexes()
		        WHERE index_name = 'ix_spend_txn_enrichment_signature'`).Scan(&ix); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%s, %d signature index", s, ix)
	}
	if o, f := shape(old), shape(fresh); o != f {
		t.Errorf("migrated overlay %s\nfresh overlay %s", o, f)
	}
	// What the cash flow statement reads off the far side is a fresh
	// gold's.
	nodes := func(db *sql.DB) map[string]string {
		return scanPairs(t, db, `SELECT transaction_external_id, concat_ws(' ', disposition, place, section,
		        class, grp, far_silver_source_id, far_account_external_id, far_place)
		      FROM cashflow_txn_nodes(0, 9999999999) WHERE transaction_external_id LIKE 'L-%'`)
	}
	if o, f := nodes(old), nodes(fresh); !maps.Equal(o, f) {
		t.Errorf("migrated nodes %v\nfresh nodes %v", o, f)
	}
}
