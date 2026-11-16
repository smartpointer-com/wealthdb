// Package enrichtest seeds the enrichment pass's tables by hand, for the
// tests of the macros that read them: the two family overlays and the
// far side of a move (transfer_links). It imports no wealthdb package,
// so the tests of gold and of spending can both use it.
package enrichtest

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// Execer is what the seeders write through: a database or a
// transaction.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Spend is one row of the spending overlay as a pass would write it. An
// empty text is NULL. Version 0 writes signature version 1, and At 0
// writes assigned_at 100.
type Spend struct {
	Source, Txn, Signature string
	Version                int
	Detailed, Provenance   string
	Label, Provider        string
	Stated                 string // stated_asset_class
	// The far side of the move, where the pass knows it: a link with
	// evidence `rule`, or FarEvidence, and the partner leg FarTxn.
	FarSource, FarAccount, FarTxn, FarEvidence string
	At                                         int64
}

// Income is one row of the income overlay as a pass would write it, on
// the same terms as Spend.
type Income struct {
	Source, Txn, Signature string
	Version                int
	Detailed, Provenance   string
	Provider               string
	Stated                 string // stated_asset_class
	At                     int64
}

// SeedSpend writes rows to the spending overlay, and the far side of a
// row that has one to transfer_links.
func SeedSpend(t testing.TB, db Execer, rows ...Spend) {
	t.Helper()
	for _, r := range rows {
		exec(t, db, `INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
		        merchant_signature, signature_version, spend_detailed, provenance, merchant_label,
		        provider_spend_detailed, stated_asset_class, assigned_at)
		    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.Source, r.Txn, text(r.Signature), version(r.Version), text(r.Detailed), r.Provenance,
			text(r.Label), text(r.Provider), text(r.Stated), at(r.At))
		if r.FarAccount == "" {
			continue
		}
		evidence := r.FarEvidence
		if evidence == "" {
			evidence = "rule"
		}
		exec(t, db, `INSERT INTO transfer_links (silver_source_id, transaction_external_id,
		        far_silver_source_id, far_account_external_id, far_transaction_external_id,
		        evidence, measure)
		    VALUES (?, ?, ?, ?, ?, ?, 'cash')`,
			r.Source, r.Txn, text(r.FarSource), text(r.FarAccount), text(r.FarTxn), evidence)
	}
}

// SeedIncome writes rows to the income overlay.
func SeedIncome(t testing.TB, db Execer, rows ...Income) {
	t.Helper()
	for _, r := range rows {
		exec(t, db, `INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id,
		        payer_signature, signature_version, income_detailed, provenance,
		        provider_income_detailed, stated_asset_class, assigned_at)
		    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.Source, r.Txn, text(r.Signature), version(r.Version), text(r.Detailed), r.Provenance,
			text(r.Provider), text(r.Stated), at(r.At))
	}
}

func exec(t testing.TB, db Execer, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("enrichtest: %s: %v", strings.Fields(query)[2], err)
	}
}

func text(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func version(v int) int {
	if v == 0 {
		return 1
	}
	return v
}

func at(a int64) int64 {
	if a == 0 {
		return 100
	}
	return a
}
