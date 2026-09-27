package gold

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestPortfoliosAsOfRollupAndSentinel covers the core
// PortfoliosAsOf contract:
//
//   - Each portfolio row aggregates positions+cash from every
//     account whose portfolio_external_id matches.
//   - Each silver_source gets a sentinel row (PortfolioExternalID="")
//     aggregating accounts with no portfolio.
//   - sum(portfolios.total) == sum(accounts.total).
func TestPortfoliosAsOfRollupAndSentinel(t *testing.T) {
	db, ctx := openMigrated(t)

	chf := "CHF"
	usd := "USD"
	port := "PORT1"

	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.UpsertPortfolios(ctx, []canonical.PortfolioChange{{
			SilverSourceID:      "test-src",
			PortfolioExternalID: "PORT1",
			BaseCurrency:        &chf,
			FirstSeenAt:         1000, LastSeenAt: 1000,
		}}); err != nil {
			return err
		}
		return w.UpsertAccounts(ctx, []canonical.AccountChange{
			// Two accounts in the portfolio.
			{SilverSourceID: "test-src", AccountExternalID: "CASH1",
				AccountKind: canonical.AccountKindCash, BaseCurrency: &chf,
				PortfolioExternalID: &port,
				FirstSeenAt:         1000, LastSeenAt: 1000},
			{SilverSourceID: "test-src", AccountExternalID: "SAFE1",
				AccountKind: canonical.AccountKindSafekeeping, BaseCurrency: &chf,
				PortfolioExternalID: &port,
				FirstSeenAt:         1000, LastSeenAt: 1000},
			// One orphan account with no portfolio — sentinel target.
			{SilverSourceID: "test-src", AccountExternalID: "ORPHAN",
				AccountKind: canonical.AccountKindBrokerage, BaseCurrency: &usd,
				FirstSeenAt: 1000, LastSeenAt: 1000},
		})
	})

	chf1000 := canonical.NewDecimalFromInt(1000)
	chf500 := canonical.NewDecimalFromInt(500)
	usd200 := canonical.NewDecimalFromInt(200)
	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.InsertPositions(ctx, []canonical.PositionChange{
			{SilverSourceID: "test-src", SnapshotAt: 1000, AccountExternalID: "SAFE1",
				PositionKey: "X", AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock,
				Currency: "CHF", MarketValue: &chf1000},
			{SilverSourceID: "test-src", SnapshotAt: 1000, AccountExternalID: "ORPHAN",
				PositionKey: "Y", AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock,
				Currency: "USD", MarketValue: &usd200},
		}); err != nil {
			return err
		}
		return w.InsertCashBalances(ctx, []canonical.CashBalanceChange{{
			SilverSourceID: "test-src", SnapshotAt: 1000, AccountExternalID: "CASH1",
			Currency: "CHF", BalanceKind: canonical.BalanceKindClosing, Amount: chf500,
		}})
	})

	rows, err := PortfoliosAsOf(ctx, db, 2000, "CHF")
	if err != nil {
		t.Fatalf("PortfoliosAsOf: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (one portfolio + one sentinel)", len(rows))
	}

	byID := map[string]PortfolioRow{}
	for _, r := range rows {
		byID[r.PortfolioExternalID] = r
	}

	// Portfolio rollup: positions=1000 CHF, cash=500 CHF, total=1500 CHF.
	port1 := byID["PORT1"]
	if v := port1.PositionsValueBase; v == nil || *v != "1000" {
		t.Errorf("PORT1 positions_value = %v, want 1000", v)
	}
	if v := port1.CashBalanceBase; v == nil || *v != "500" {
		t.Errorf("PORT1 cash_balance = %v, want 500", v)
	}
	if v := port1.TotalValueBase; v == nil || *v != "1500" {
		t.Errorf("PORT1 total_value = %v, want 1500", v)
	}

	// Sentinel: one orphan account with BaseCurrency=USD and one
	// $200 USD position. Per the agree-or-NULL rollup, the
	// sentinel inherits BaseCurrency=USD (single qualifying
	// account, no disagreement). Positions / cash get summed in
	// that base — 200 / 0 / 200.
	sentinel, ok := byID[""]
	if !ok {
		t.Fatal("sentinel row missing")
	}
	if sentinel.SilverSourceID != "test-src" {
		t.Errorf("sentinel src = %q, want test-src", sentinel.SilverSourceID)
	}
	if sentinel.BaseCurrency == nil || *sentinel.BaseCurrency != "USD" {
		t.Errorf("sentinel base_currency = %v, want USD", sentinel.BaseCurrency)
	}
	if v := sentinel.PositionsValueBase; v == nil || *v != "200" {
		t.Errorf("sentinel positions_value = %v, want 200", v)
	}
	if v := sentinel.TotalValueBase; v == nil || *v != "200" {
		t.Errorf("sentinel total_value = %v, want 200", v)
	}

	// Invariant in this fixture (USD output ccy with FX seeded
	// CHF→USD=0.80): sum(portfolios.total_USD) should equal
	// sum(accounts.total_USD).
	seedFX(t, db, 1000, "CHF", "USD", "0.80")
	pRows, _ := PortfoliosAsOf(ctx, db, 2000, "USD")
	aRows, _ := AccountsAsOf(ctx, db, 2000, "USD")

	sum := func(getter func(idx int) *string, n int) string {
		var total canonical.Decimal
		for i := 0; i < n; i++ {
			s := getter(i)
			if s == nil {
				continue
			}
			d, _ := canonical.NewDecimalFromString(*s)
			total = total.Add(d)
		}
		return total.String()
	}
	pSum := sum(func(i int) *string { return pRows[i].TotalValueOutCcy }, len(pRows))
	aSum := sum(func(i int) *string { return aRows[i].TotalValueOutCcy }, len(aRows))
	if pSum != aSum {
		t.Errorf("sum(portfolios.total_USD)=%s != sum(accounts.total_USD)=%s", pSum, aSum)
	}
}
