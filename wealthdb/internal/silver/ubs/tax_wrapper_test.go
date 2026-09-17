package ubs

import (
	"context"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// wantRelationshipWrapper asserts that an account the adapter
// CONSTRUCTS — rather than reads a product code for — carries the
// relationship's tax wrapper.
//
// An unset wrapper is not neutral downstream. The cash flow statement
// reads it as the household's, which is the right answer for these
// three kinds and the wrong one for a retirement account nobody mapped,
// and `wealthdb status -v` cannot tell the two apart: it reports both
// as the boundary's coverage gap. Stating the wrapper moves no number
// and empties the canary of the accounts that were never a question.
func wantRelationshipWrapper(t *testing.T, kind canonical.AccountKind, got *canonical.TaxWrapper) {
	t.Helper()
	if got == nil {
		t.Errorf("a %s account reaches gold with no tax wrapper", kind)
		return
	}
	if *got != canonical.TaxWrapperTaxablePersonal {
		t.Errorf("a %s account carries tax wrapper %q, want %q",
			kind, *got, canonical.TaxWrapperTaxablePersonal)
	}
}

// TestConstructedAccountsCarryTheRelationshipWrapper walks the three
// kinds the adapter builds itself, each through the reader that builds
// it. The safekeeping and cash accounts are deliberately NOT here: their
// wrapper is the AcctTpCd tables' answer, and an unknown product code
// leaving it unset is the alarm those tables exist to raise.
func TestConstructedAccountsCarryTheRelationshipWrapper(t *testing.T) {
	t.Run("card", func(t *testing.T) {
		r := newCardFixture(t)
		if _, err := r.db.Exec(`
INSERT INTO card_accounts VALUES
 (1000, 'ACCT-1', '0000 0000', 'CHF', -250.0, 4750.0, 5000.0, NULL, NULL,
  'Example Card', 'EXCA', 'ACTIVE', 'COMPLEX_TLA', '{}')`); err != nil {
			t.Fatal(err)
		}
		byTime := map[int64]*canonical.SnapshotBatch{1000: {}}
		if err := r.appendWebCards(context.Background(), fullWindow(), byTime); err != nil {
			t.Fatal(err)
		}
		assertEveryAccountOfKind(t, byTime[1000].Accounts, canonical.AccountKindCard)
	})

	// The web feed's mortgage, the second of the two paths that build
	// one (the PDF era's is pinned in historical_mortgage_test.go).
	t.Run("mortgage", func(t *testing.T) {
		r := newCardFixture(t)
		if _, err := r.db.Exec(`
CREATE TABLE mortgages (
    snapshot_at INTEGER, account_external_id TEXT, banking_relationship_id TEXT,
    portfolio_external_id TEXT, currency_iso TEXT, description TEXT, payload TEXT);
INSERT INTO mortgages VALUES
 (1000, 'MORT-1', 'REL-1', 'P1', 'CHF', 'Example Mortgage', '{}')`); err != nil {
			t.Fatal(err)
		}
		byTime := map[int64]*canonical.SnapshotBatch{1000: {}}
		if err := r.appendWebMortgages(context.Background(), fullWindow(), byTime); err != nil {
			t.Fatal(err)
		}
		assertEveryAccountOfKind(t, byTime[1000].Accounts, canonical.AccountKindMortgage)
	})
}

// assertEveryAccountOfKind fails unless the batch holds at least one
// account of `kind` and every one of them carries the wrapper. The
// at-least-one guard is what keeps this from passing vacuously if a
// fixture stops producing the kind.
func assertEveryAccountOfKind(t *testing.T, accounts []canonical.AccountChange,
	kind canonical.AccountKind) {
	t.Helper()
	var seen int
	for _, a := range accounts {
		if a.AccountKind != kind {
			continue
		}
		seen++
		wantRelationshipWrapper(t, a.AccountKind, a.TaxWrapper)
	}
	if seen == 0 {
		t.Fatalf("the fixture projected no %s account", kind)
	}
}
