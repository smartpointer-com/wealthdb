package gold

import (
	"context"
	"database/sql"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The Stage A pins for migration 0078: the five values exist in the
// dimension, and both bases drop a row that resolves to one of them.
//
// TestSpendCategoriesMatchGoTable already compares the whole dimension
// against canonical.SpendCategories, so nothing here re-checks the
// seed's contents. What it checks is the consequence: a base names its
// exclusions one by one, so a value added to the taxonomy without a
// re-issue would stay IN the base and double-count against the
// cashflow statement that placed it.

// seedCashflowTaxonomyFixture lays down one transaction per new value,
// on its own account, with the overlay row a rule would have written.
// Amounts and ids are invented.
func seedCashflowTaxonomyFixture(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, first_seen_at, last_seen_at) VALUES
            ('cf-src', 'CASH1', 'cash', 'Everyday', 1, 1);

        INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
                                  account_external_id, kind, currency, net_amount) VALUES
            -- Outflows: an ordinary purchase to prove the base still
            -- holds one, then the five values that must leave it.
            ('cf-src', 'T-BUY',       1000, 'CASH1', 'purchase',   'USD',  -20),
            ('cf-src', 'T-LOAN-OUT',  1000, 'CASH1', 'withdrawal', 'USD', -400),
            ('cf-src', 'T-RET-OUT',   1000, 'CASH1', 'withdrawal', 'USD', -500),
            ('cf-src', 'T-EDU-OUT',   1000, 'CASH1', 'withdrawal', 'USD', -600),
            ('cf-src', 'T-HSA-OUT',   1000, 'CASH1', 'withdrawal', 'USD', -700),
            ('cf-src', 'T-TRU-OUT',   1000, 'CASH1', 'withdrawal', 'USD', -800),
            -- Inflows: a plain receipt, then the four crossings
            -- arriving, which is the same value read the other way.
            ('cf-src', 'T-PAID',      1000, 'CASH1', 'deposit',    'USD',  900),
            ('cf-src', 'T-RET-IN',    1000, 'CASH1', 'deposit',    'USD',  110),
            ('cf-src', 'T-EDU-IN',    1000, 'CASH1', 'deposit',    'USD',  120),
            ('cf-src', 'T-HSA-IN',    1000, 'CASH1', 'deposit',    'USD',  130),
            ('cf-src', 'T-TRU-IN',    1000, 'CASH1', 'deposit',    'USD',  140);

        INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
                                          merchant_signature, signature_version,
                                          spend_detailed, provenance, assigned_at) VALUES
            ('cf-src', 'T-BUY',      'sig-shop',  1, 'FOOD_AND_DRINK_GROCERIES', 'rule', 100),
            ('cf-src', 'T-LOAN-OUT', 'sig-loan',  1, 'debt_repayment',           'rule', 100),
            ('cf-src', 'T-RET-OUT',  'sig-plan',  1, 'retirement_transfer',      'rule', 100),
            ('cf-src', 'T-EDU-OUT',  'sig-plan',  1, 'education_transfer',       'rule', 100),
            ('cf-src', 'T-HSA-OUT',  'sig-plan',  1, 'health_transfer',          'rule', 100),
            ('cf-src', 'T-TRU-OUT',  'sig-plan',  1, 'trust_transfer',           'rule', 100);

        INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id,
                                           payer_signature, signature_version,
                                           income_detailed, provenance, assigned_at) VALUES
            ('cf-src', 'T-PAID',   'sig-payer', 1, 'INCOME_WAGES',        'rule', 100),
            ('cf-src', 'T-RET-IN', 'sig-plan',  1, 'retirement_transfer', 'rule', 100),
            ('cf-src', 'T-EDU-IN', 'sig-plan',  1, 'education_transfer',  'rule', 100),
            ('cf-src', 'T-HSA-IN', 'sig-plan',  1, 'health_transfer',     'rule', 100),
            ('cf-src', 'T-TRU-IN', 'sig-plan',  1, 'trust_transfer',      'rule', 100);
    `); err != nil {
		t.Fatalf("seed cashflow taxonomy fixture: %v", err)
	}
}

// TestCashflowDeltasLeaveBothBases is the whole of migration 0078's
// behavioural change: the five values are enriched and visible, and
// neither family's base charts them.
func TestCashflowDeltasLeaveBothBases(t *testing.T) {
	db, ctx := openMigrated(t)
	seedCashflowTaxonomyFixture(t, db, ctx)

	spend := macroTxnIDs(t, db, ctx, "spending_lines_base", 0, 5000)
	if _, ok := spend["T-BUY"]; !ok {
		t.Error("spending_lines_base dropped an ordinary purchase")
	}
	for id, why := range map[string]string{
		"T-LOAN-OUT": "a loan instalment reduces a liability rather than buying anything",
		"T-RET-OUT":  "a contribution to a retirement plan is the holder's money moving, not spend",
		"T-EDU-OUT":  "the same for an education plan",
		"T-HSA-OUT":  "the same for a health account",
		"T-TRU-OUT":  "the same for a trust that is a separate taxpayer",
	} {
		if _, ok := spend[id]; ok {
			t.Errorf("spending_lines_base still charts %s: %s", id, why)
		}
	}

	income := macroTxnIDs(t, db, ctx, "income_lines_base", 0, 5000)
	if _, ok := income["T-PAID"]; !ok {
		t.Error("income_lines_base dropped an ordinary receipt")
	}
	for id, why := range map[string]string{
		"T-RET-IN": "a plan payout is the holder's own capital arriving, not income",
		"T-EDU-IN": "the same for an education plan",
		"T-HSA-IN": "the same for a health account",
		"T-TRU-IN": "the same for a trust distribution",
	} {
		if _, ok := income[id]; ok {
			t.Errorf("income_lines_base still charts %s: %s", id, why)
		}
	}
}

// TestCashflowDeltasStayVisibleOnTheOverlay pins the other half of the
// decision: a value out of a base is still ENRICHED, so cashflow can
// read the verdict and `wealthdb transactions` can name the row. A base
// exclusion is about what a report charts, never about what gold knows.
func TestCashflowDeltasStayVisibleOnTheOverlay(t *testing.T) {
	db, ctx := openMigrated(t)
	seedCashflowTaxonomyFixture(t, db, ctx)

	for _, tc := range []struct{ macro, col, id, want string }{
		{"spend_txn_categories", "spend_detailed", "T-LOAN-OUT", "debt_repayment"},
		{"spend_txn_categories", "spend_detailed", "T-RET-OUT", "retirement_transfer"},
		{"income_txn_categories", "income_detailed", "T-HSA-IN", "health_transfer"},
		{"income_txn_categories", "income_detailed", "T-TRU-IN", "trust_transfer"},
	} {
		var got sql.NullString
		if err := db.QueryRowContext(ctx,
			`SELECT `+tc.col+` FROM `+tc.macro+`() WHERE transaction_external_id = ?`,
			tc.id).Scan(&got); err != nil {
			t.Fatalf("%s(%s): %v", tc.macro, tc.id, err)
		}
		if got.String != tc.want {
			t.Errorf("%s[%s] = %q, want %q", tc.macro, tc.id, got.String, tc.want)
		}
	}
}

// TestCashflowDeltaLabelsAreSeeded pins the display labels of the five
// new rows against the Go rule, which is what the diagram's node names
// are read from.
func TestCashflowDeltaLabelsAreSeeded(t *testing.T) {
	db, ctx := openMigrated(t)
	for _, v := range []string{
		canonical.SpendDetailedDebtRepayment,
		canonical.DetailedRetirementTransfer, canonical.DetailedEducationTransfer,
		canonical.DetailedHealthTransfer, canonical.DetailedTrustTransfer,
	} {
		var label, primaryLabel string
		var catchAll sql.NullBool
		if err := db.QueryRowContext(ctx,
			`SELECT label, primary_label, catch_all FROM spend_categories WHERE spend_detailed = ?`,
			v).Scan(&label, &primaryLabel, &catchAll); err != nil {
			t.Fatalf("read %s: %v", v, err)
		}
		if want := canonical.SpendLabel(v); label != want {
			t.Errorf("%s label = %q, want %q", v, label, want)
		}
		if want := canonical.SpendPrimaryLabel(v); primaryLabel != want {
			t.Errorf("%s primary_label = %q, want %q", v, primaryLabel, want)
		}
		if !catchAll.Valid || catchAll.Bool {
			t.Errorf("%s catch_all = %v, want false: a delta is a verdict", v, catchAll)
		}
	}
}

// TestMigration0078DDLIsRerunnable holds the seed and the two re-issued
// bases to the replay bar: OR REPLACE throughout, and the catch-all
// recompute guarded on NULL so a second application changes nothing.
func TestMigration0078DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedCashflowTaxonomyFixture(t, db, ctx)
	rerunMigrationDDL(t, db, ctx, "0078_cashflow_taxonomy.sql")

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM spend_categories WHERE spend_detailed = 'debt_repayment'`).
		Scan(&n); err != nil {
		t.Fatalf("count after replay: %v", err)
	}
	if n != 1 {
		t.Errorf("debt_repayment rows after replay = %d, want 1", n)
	}
	if _, ok := macroTxnIDs(t, db, ctx, "spending_lines_base", 0, 5000)["T-LOAN-OUT"]; ok {
		t.Error("the replayed base charts a debt repayment")
	}
}

// TestMigration0079DDLIsRerunnable holds the far-account columns to the
// replay bar: three IF NOT EXISTS ALTERs, so a second application
// neither fails nor drops what the first wrote.
func TestMigration0079DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedCashflowTaxonomyFixture(t, db, ctx)
	if _, err := db.ExecContext(ctx, `
        UPDATE spend_txn_enrichment SET far_class = 'mortgage'
         WHERE transaction_external_id = 'T-LOAN-OUT'`); err != nil {
		t.Fatalf("seed a far class: %v", err)
	}
	rerunMigrationDDL(t, db, ctx, "0079_spend_far_account.sql")

	var class sql.NullString
	if err := db.QueryRowContext(ctx, `
        SELECT far_class FROM spend_txn_enrichment
         WHERE transaction_external_id = 'T-LOAN-OUT'`).Scan(&class); err != nil {
		t.Fatalf("read far class after replay: %v", err)
	}
	if class.String != "mortgage" {
		t.Errorf("far_class after replay = %q, want mortgage", class.String)
	}
}

// TestMigration0080DDLIsRerunnable holds the boundary tables to the same
// bar: CREATE TABLE IF NOT EXISTS and an OR REPLACE macro, so a replay
// keeps the stamped rows and the pool still reads them.
func TestMigration0080DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              tax_wrapper, first_seen_at, last_seen_at) VALUES
            ('cf-src', 'POOLED', 'cash', 'taxable_personal', 1, 1),
            ('cf-src', 'PLAN',   'brokerage', 'roth_ira',     1, 1);
        INSERT INTO cashflow_wrapper_sides (tax_wrapper, side, class) VALUES
            ('taxable_personal', 'household', NULL),
            ('roth_ira', 'vehicle', 'retirement');`); err != nil {
		t.Fatalf("seed the boundary: %v", err)
	}
	rerunMigrationDDL(t, db, ctx, "0080_cashflow_boundary.sql")

	var pooled int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM cashflow_pool_accounts()`).Scan(&pooled); err != nil {
		t.Fatalf("read the pool after replay: %v", err)
	}
	if pooled != 1 {
		t.Errorf("pool holds %d accounts after replay, want 1", pooled)
	}
}

// TestWrapperSidesRefusesAnIncoherentRow pins the CHECK constraints the
// stamped table carries: a side the vocabulary does not hold, a class
// outside the four pools, and the pairing rule — a class exactly on the
// vehicle side, never beside a household or a giving one.
func TestWrapperSidesRefusesAnIncoherentRow(t *testing.T) {
	db, ctx := openMigrated(t)
	for _, tc := range []struct{ name, values string }{
		{"unknown side", `('other', 'pool', NULL)`},
		{"unknown class", `('other', 'vehicle', 'unpaired')`},
		{"a vehicle with no class", `('other', 'vehicle', NULL)`},
		{"a household with a class", `('other', 'household', 'retirement')`},
		{"a giving side with a class", `('other', 'giving', 'trusts')`},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO cashflow_wrapper_sides (tax_wrapper, side, class) VALUES `+tc.values); err == nil {
			t.Errorf("%s was accepted: %s", tc.name, tc.values)
			if _, err := db.ExecContext(ctx, `DELETE FROM cashflow_wrapper_sides`); err != nil {
				t.Fatalf("clear: %v", err)
			}
		}
	}
}

// TestEveryCashflowClassReadsAsSomething is the generator-style pin
// between canonical's class vocabulary and the two macros that render
// it. A class gold can emit with no label reads as its bare enum
// spelling on every chart, and one with no rank sorts with the asset
// classes — both silent, and both a single forgotten CASE arm away.
func TestEveryCashflowClassReadsAsSomething(t *testing.T) {
	db, ctx := openMigrated(t)
	for _, c := range canonical.CashflowClasses() {
		var label string
		var rank int
		if err := db.QueryRowContext(ctx,
			`SELECT cashflow_class_label(?), cashflow_class_rank(?)`,
			string(c), string(c)).Scan(&label, &rank); err != nil {
			t.Fatalf("read %s: %v", c, err)
		}
		if label == string(c) {
			t.Errorf("class %q has no label and would draw as its own enum spelling", c)
		}
		// 40 is the fallback the asset classes share, so an invented
		// class landing on it has no rank of its own.
		if rank == 40 {
			t.Errorf("class %q has no rank and would sort with the asset classes", c)
		}
	}
}

// TestEveryAssetClassGoldHoldsReadsAsSomething is the same pin for the
// investing section, whose classes ARE the instrument taxonomy: a new
// exposure value would otherwise draw as `private_debt` rather than as
// "Private debt". `cash` is excluded because a cash-class instrument is
// pool-internal and never reaches a node.
func TestEveryAssetClassGoldHoldsReadsAsSomething(t *testing.T) {
	db, ctx := openMigrated(t)
	for _, a := range canonical.AssetClasses() {
		if a == canonical.AssetClassCash {
			continue
		}
		var label string
		if err := db.QueryRowContext(ctx,
			`SELECT cashflow_class_label(?)`, string(a)).Scan(&label); err != nil {
			t.Fatalf("read %s: %v", a, err)
		}
		if label == string(a) {
			t.Errorf("asset class %q has no cashflow label and would draw as its enum spelling", a)
		}
	}
}
