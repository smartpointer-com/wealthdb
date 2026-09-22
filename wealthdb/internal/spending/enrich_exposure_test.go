package spending

import (
	"context"
	"database/sql"
	"regexp"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// What the holder said the capital went INTO (migration 0102).
//
// The column describes the VERDICT, not the row, exactly as
// merchantLabel and farClass do — so the whole of its contract is where
// it travels when a tier above replaces the verdict it was written
// beside. These tests pin the four transitions; the resolution's half,
// which turns the word into a node, is in internal/gold.

// exposureOf reads the stated exposure for one transaction. Empty where
// the pass wrote NULL, which is what "nobody said" looks like.
func exposureOf(t *testing.T, db *sql.DB, ctx context.Context, table, source, id string) string {
	t.Helper()
	var c sql.NullString
	if err := db.QueryRowContext(ctx, `
        SELECT stated_asset_class FROM `+table+`
         WHERE silver_source_id = ? AND transaction_external_id = ?`,
		source, id).Scan(&c); err != nil {
		t.Fatalf("read stated_asset_class for %s/%s: %v", source, id, err)
	}
	return c.String
}

// TestAnExposureTravelsWithTheVerdictItDescribes walks all four
// transitions in one pass, because they are one contract and a test per
// arm would let the lattice drift between them.
func TestAnExposureTravelsWithTheVerdictItDescribes(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		// WRITTEN: a config rule places the verdict and the exposure.
		txn{source: "bank", id: "T-RULE", account: "CASH1", kind: "withdrawal",
			occurredAt: day(10), amount: -5000, description: "EXAMPLE VENTURE FUND CALL"},
		// REPLACED: a pin outranks that rule and carries its own word.
		txn{source: "bank", id: "T-PIN-OVER-RULE", account: "CASH1", kind: "withdrawal",
			occurredAt: day(11), amount: -6000, description: "EXAMPLE VENTURE FUND CALL"},
		// REPLACED WITH NOTHING: the same, by a pin that states none.
		txn{source: "bank", id: "T-PIN-SILENT", account: "CASH1", kind: "withdrawal",
			occurredAt: day(12), amount: -7000, description: "EXAMPLE VENTURE FUND CALL"},
		// CLEARED: the matcher pairs the row, so the verdict becomes
		// internal_transfer and an exposure on it would mean nothing.
		txn{source: "bank", id: "T-PAIRED", account: "CASH1", kind: "withdrawal",
			occurredAt: day(13), amount: -8000, description: "EXAMPLE VENTURE FUND CALL"},
		txn{source: "other-bank", id: "T-PAIRED-IN", account: "CASH2", kind: "deposit",
			occurredAt: day(13), amount: 8000, description: "INCOMING"},
	)
	rules := []Rule{{
		Match:      regexp.MustCompile(`(?i)example venture fund`),
		Category:   canonical.SpendDetailedInvestment,
		AssetClass: "private_equity",
	}}
	pins := []Pin{
		{Source: "bank", Account: "CASH1", Day: day(11), Amount: -6000, Currency: "USD",
			Detailed: canonical.SpendDetailedInvestment, AssetClass: "real_estate"},
		{Source: "bank", Account: "CASH1", Day: day(12), Amount: -7000, Currency: "USD",
			Detailed: canonical.SpendDetailedInvestment},
	}
	res := runPass(t, db, ctx, Options{Rules: rules, Pins: pins})

	for _, c := range []struct{ id, want, why string }{
		{"T-RULE", "private_equity", "written by the rule that placed the verdict"},
		{"T-PIN-OVER-RULE", "real_estate", "replaced by the pin above it"},
		{"T-PIN-SILENT", "", "a pin that states none clears the rule's word with the verdict"},
		{"T-PAIRED", "", "the matcher replaced the verdict, so the exposure goes with it"},
	} {
		if got := exposureOf(t, db, ctx, "spend_txn_enrichment", "bank", c.id); got != c.want {
			t.Errorf("%s exposure = %q, want %q — %s", c.id, got, c.want, c.why)
		}
	}

	// The counters are read off the finished rows, so they must agree
	// with the column above: two rows still carry a word, and the one
	// the pin silenced is the backlog. A tally taken at the tier that
	// wrote each word would say three and one.
	if res.FamilyResult.StatedExposures != 2 {
		t.Errorf("StatedExposures = %d, want 2 — one per row that still carries one",
			res.FamilyResult.StatedExposures)
	}
	if res.FamilyResult.UnstatedInvesting != 1 {
		t.Errorf("UnstatedInvesting = %d, want 1 — the pin that stated none",
			res.FamilyResult.UnstatedInvesting)
	}
}

// TestTheIncomeFamilyCarriesAnExposureToo drives the other half. The
// return leg of a private holding is the income family's row, and a
// spending-only column could never have reached it — which is why
// there are two, one per overlay. Same contract, same clearing.
func TestTheIncomeFamilyCarriesAnExposureToo(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{source: "bank", id: "T-BACK", account: "CASH1", kind: "deposit",
			occurredAt: day(10), amount: 9000, description: "EXAMPLE VENTURE FUND RETURN"},
	)
	res := runPass(t, db, ctx, Options{Income: IncomeOptions{Rules: []Rule{{
		Match:      regexp.MustCompile(`(?i)example venture fund return`),
		Category:   canonical.IncomeDetailedCapitalReturn,
		AssetClass: "private_debt",
	}}}})

	if got := exposureOf(t, db, ctx, "income_txn_enrichment", "bank", "T-BACK"); got != "private_debt" {
		t.Errorf("income exposure = %q, want private_debt", got)
	}
	if res.Income.StatedExposures != 1 || res.Income.UnstatedInvesting != 0 {
		t.Errorf("income counters = (%d stated, %d unstated), want (1, 0)",
			res.Income.StatedExposures, res.Income.UnstatedInvesting)
	}
}

// TestAStatedFarAccountKeepsTheExposure pins the one place this column
// departs from farClass on purpose. The source naming the counter
// account answers WHERE the money went and never WHAT the movement was,
// so the verdict it was written beside is still standing — where
// farClass, which is a stand-in for that same account, correctly gives
// way to it.
func TestAStatedFarAccountKeepsTheExposure(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{source: "bank", id: "T-STATED", account: "CASH1", kind: "withdrawal",
			occurredAt: day(10), amount: -5000, description: "EXAMPLE VENTURE FUND CALL"},
	)
	// An account gold holds and the matcher cannot reach, which is the
	// only shape this road exists for.
	seedCounterAccount(t, db, ctx, "bank", "T-STATED", "BRK1")
	rules := []Rule{{
		Match:      regexp.MustCompile(`(?i)example venture fund`),
		Category:   canonical.SpendDetailedInvestment,
		AssetClass: "private_equity",
	}}
	runPass(t, db, ctx, Options{Rules: rules})

	// Without this the test passes when the stated-far road never runs,
	// since an untouched exposure survives either way.
	if _, acct, _ := farOf(t, db, ctx, "bank", "T-STATED"); acct != "BRK1" {
		t.Fatalf("far account = %q, want BRK1 — the stated-far road did not fire, so this proves nothing", acct)
	}
	if got := exposureOf(t, db, ctx, "spend_txn_enrichment", "bank", "T-STATED"); got != "private_equity" {
		t.Errorf("exposure = %q, want private_equity — a stated far account places no verdict", got)
	}
}
