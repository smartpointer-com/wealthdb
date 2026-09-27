package loader

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// The exclusion sweep's own semantics. What it removes, what it leaves,
// and the invariant it exists to hold: no fact may survive the account
// it belongs to.
//
// The wiring — that Load actually runs this — is pinned separately in
// loader_test.go, where the harness that drives a real load lives.
//
// Every id below is invented.

func excludeFixture(t *testing.T) (context.Context, *sql.Tx, func(string) int) {
	t.Helper()
	ctx := context.Background()
	db, err := gold.OpenFresh(filepath.Join(t.TempDir(), "gold.db"))
	if err != nil {
		t.Fatalf("gold.OpenFresh: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() })

	acct := func(id, portfolio string) canonical.AccountChange {
		a := canonical.AccountChange{
			SilverSourceID: "src", AccountExternalID: id,
			AccountKind: canonical.AccountKindBrokerage,
			FirstSeenAt: 1000, LastSeenAt: 2000,
		}
		if portfolio != "" {
			p := portfolio
			a.PortfolioExternalID = &p
		}
		return a
	}
	pos := func(acct, key string) canonical.PositionChange {
		return canonical.PositionChange{
			SilverSourceID: "src", SnapshotAt: 1000, AccountExternalID: acct,
			PositionKey: key, Currency: "USD",
			AssetClass: "public_equity", Vehicle: "stock",
		}
	}
	w := gold.NewWriter(tx)
	if err := w.UpsertPortfolios(ctx, []canonical.PortfolioChange{
		{SilverSourceID: "src", PortfolioExternalID: "PF_DROP", FirstSeenAt: 1000, LastSeenAt: 2000},
		{SilverSourceID: "src", PortfolioExternalID: "PF_KEEP", FirstSeenAt: 1000, LastSeenAt: 2000},
	}); err != nil {
		t.Fatalf("seed portfolios: %v", err)
	}
	if err := w.UpsertAccounts(ctx, []canonical.AccountChange{
		acct("IN_PF", "PF_DROP"),   // dropped by its portfolio
		acct("BY_NAME", "PF_KEEP"), // dropped by name, inside a kept portfolio
		acct("KEEP", "PF_KEEP"),
		acct("LOOSE", ""), // no portfolio at all
	}); err != nil {
		t.Fatalf("seed accounts: %v", err)
	}
	if err := w.InsertPositions(ctx, []canonical.PositionChange{
		pos("IN_PF", "p1"), pos("BY_NAME", "p2"), pos("KEEP", "p3"), pos("LOOSE", "p4"),
	}); err != nil {
		t.Fatalf("seed positions: %v", err)
	}
	if err := w.InsertCashBalances(ctx, []canonical.CashBalanceChange{
		{SilverSourceID: "src", SnapshotAt: 1000, AccountExternalID: "IN_PF", Currency: "USD", BalanceKind: canonical.BalanceKindCurrent},
		{SilverSourceID: "src", SnapshotAt: 1000, AccountExternalID: "KEEP", Currency: "USD", BalanceKind: canonical.BalanceKindCurrent},
	}); err != nil {
		t.Fatalf("seed cash: %v", err)
	}
	if err := w.InsertTransactions(ctx, []canonical.TransactionChange{
		{SilverSourceID: "src", TransactionExternalID: "T1", AccountExternalID: "IN_PF", OccurredAt: 1000, Kind: canonical.TxKindBuy, Currency: "USD"},
		{SilverSourceID: "src", TransactionExternalID: "T2", AccountExternalID: "BY_NAME", OccurredAt: 1000, Kind: canonical.TxKindBuy, Currency: "USD"},
		{SilverSourceID: "src", TransactionExternalID: "T3", AccountExternalID: "KEEP", OccurredAt: 1000, Kind: canonical.TxKindBuy, Currency: "USD"},
	}); err != nil {
		t.Fatalf("seed transactions: %v", err)
	}

	count := func(q string) int {
		var c int
		if err := tx.QueryRowContext(ctx, q).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	return ctx, tx, count
}

// TestBothGrainsTakeTheirAccountsAndEveryFact: the two grains reach the
// same place by different routes — one named in the config, one found
// through the account dimension — and both must take the facts with
// them. The orphan assertion is the point: a fact whose account gold has
// no record of sits outside every account-scoped filter.
func TestBothGrainsTakeTheirAccountsAndEveryFact(t *testing.T) {
	ctx, tx, count := excludeFixture(t)

	if err := deleteExcluded(ctx, tx, "src", exclusions{
		accounts:   []string{"BY_NAME"},
		portfolios: []string{"PF_DROP"},
	}); err != nil {
		t.Fatalf("deleteExcluded: %v", err)
	}

	if c := count(`SELECT COUNT(*) FROM portfolios`); c != 1 {
		t.Errorf("portfolios = %d, want PF_KEEP alone", c)
	}
	// BY_NAME's portfolio survives it: excluding an account does not
	// touch the portfolio it sat in.
	if c := count(`SELECT COUNT(*) FROM portfolios WHERE portfolio_external_id = 'PF_KEEP'`); c != 1 {
		t.Error("excluding an account took its portfolio with it")
	}
	if c := count(`SELECT COUNT(*) FROM accounts`); c != 2 {
		t.Errorf("accounts = %d, want KEEP and LOOSE", c)
	}
	for _, id := range []string{"IN_PF", "BY_NAME"} {
		if c := count(`SELECT COUNT(*) FROM accounts WHERE account_external_id = '` + id + `'`); c != 0 {
			t.Errorf("%s survived", id)
		}
	}
	for _, table := range []string{"positions", "cash_balances", "transactions"} {
		if c := count(`SELECT COUNT(*) FROM ` + table + ` f
            WHERE NOT EXISTS (SELECT 1 FROM accounts a
                               WHERE a.silver_source_id = f.silver_source_id
                                 AND a.account_external_id = f.account_external_id)`); c != 0 {
			t.Errorf("%s left %d orphan(s)", table, c)
		}
	}
	// The untouched side is untouched, by identity and not by count
	// alone — a sweep that took the wrong rows would also leave two.
	if c := count(`SELECT COUNT(*) FROM positions WHERE account_external_id IN ('KEEP','LOOSE')`); c != 2 {
		t.Errorf("kept positions = %d, want KEEP's and LOOSE's", c)
	}
	if c := count(`SELECT COUNT(*) FROM transactions WHERE account_external_id = 'KEEP'`); c != 1 {
		t.Error("the kept account's transaction did not survive")
	}
}

// Each grain alone, because the sweep builds its WHERE clause from
// whichever grains have ids behind them: with one grain empty the other
// must still produce valid SQL and remove exactly its own rows.
func TestEitherGrainWorksWithoutTheOther(t *testing.T) {
	t.Run("accounts only", func(t *testing.T) {
		ctx, tx, count := excludeFixture(t)
		if err := deleteExcluded(ctx, tx, "src", exclusions{accounts: []string{"BY_NAME"}}); err != nil {
			t.Fatalf("deleteExcluded: %v", err)
		}
		if c := count(`SELECT COUNT(*) FROM accounts`); c != 3 {
			t.Errorf("accounts = %d, want BY_NAME alone removed", c)
		}
		if c := count(`SELECT COUNT(*) FROM portfolios`); c != 2 {
			t.Error("an account-only exclusion removed a portfolio")
		}
	})
	t.Run("portfolios only", func(t *testing.T) {
		ctx, tx, count := excludeFixture(t)
		if err := deleteExcluded(ctx, tx, "src", exclusions{portfolios: []string{"PF_DROP"}}); err != nil {
			t.Fatalf("deleteExcluded: %v", err)
		}
		if c := count(`SELECT COUNT(*) FROM accounts`); c != 3 {
			t.Errorf("accounts = %d, want IN_PF alone removed", c)
		}
		if c := count(`SELECT COUNT(*) FROM transactions WHERE account_external_id = 'BY_NAME'`); c != 1 {
			t.Error("a portfolio-only exclusion reached an account named in the other grain")
		}
	})
}

// TestTheSweepIsScopedToItsSource: ids are only unique within a source,
// so a sweep that forgot its source scoping would delete another
// source's rows that happen to share an id.
func TestTheSweepIsScopedToItsSource(t *testing.T) {
	ctx, tx, count := excludeFixture(t)
	other := canonical.AccountChange{
		SilverSourceID: "other", AccountExternalID: "IN_PF",
		AccountKind: canonical.AccountKindBrokerage, FirstSeenAt: 1000, LastSeenAt: 2000,
	}
	if err := gold.NewWriter(tx).UpsertAccounts(ctx, []canonical.AccountChange{other}); err != nil {
		t.Fatalf("seed other source: %v", err)
	}
	if err := deleteExcluded(ctx, tx, "src", exclusions{
		accounts: []string{"IN_PF"}, portfolios: []string{"PF_DROP"},
	}); err != nil {
		t.Fatalf("deleteExcluded: %v", err)
	}
	if c := count(`SELECT COUNT(*) FROM accounts WHERE silver_source_id = 'other'`); c != 1 {
		t.Error("the sweep crossed into another source")
	}
}

// Nothing configured, or ids this source has none of, must leave gold
// exactly as it was — the sweep is reached on every load of every
// source.
func TestASweepWithNothingToDoTouchesNothing(t *testing.T) {
	for name, e := range map[string]exclusions{
		"nothing excluded": {},
		"absent ids":       {accounts: []string{"NO_SUCH"}, portfolios: []string{"NO_SUCH_PF"}},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, tx, count := excludeFixture(t)
			before := count(`SELECT COUNT(*) FROM accounts`) + count(`SELECT COUNT(*) FROM positions`) +
				count(`SELECT COUNT(*) FROM portfolios`) + count(`SELECT COUNT(*) FROM transactions`)
			if err := deleteExcluded(ctx, tx, "src", e); err != nil {
				t.Fatalf("deleteExcluded: %v", err)
			}
			after := count(`SELECT COUNT(*) FROM accounts`) + count(`SELECT COUNT(*) FROM positions`) +
				count(`SELECT COUNT(*) FROM portfolios`) + count(`SELECT COUNT(*) FROM transactions`)
			if before != after {
				t.Errorf("rows went from %d to %d", before, after)
			}
		})
	}
}

// configExclusions sorts both grains, so the SQL the sweep builds is
// stable run to run: Go map iteration order is not, and an IN list that
// reorders itself makes a query log unreadable and a plan cache useless.
func TestConfigExclusionsCollectsBothGrainsInOrder(t *testing.T) {
	got := configExclusions(
		map[string]AccountOverride{
			"zz": {Exclude: true}, "aa": {Exclude: true},
			"nope": {Nickname: "kept"}, // not an exclusion
		},
		map[string]PortfolioOverride{
			"PF_Z": {Exclude: true}, "PF_A": {Exclude: true},
			"PF_N": {TaxWrapper: "roth_ira"}, // not an exclusion
		})
	if want := []string{"aa", "zz"}; !equalStrings(got.accounts, want) {
		t.Errorf("accounts = %v, want %v", got.accounts, want)
	}
	if want := []string{"PF_A", "PF_Z"}; !equalStrings(got.portfolios, want) {
		t.Errorf("portfolios = %v, want %v", got.portfolios, want)
	}
	if (exclusions{}).empty() != true || got.empty() {
		t.Error("empty() disagrees with the sets it reports on")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
