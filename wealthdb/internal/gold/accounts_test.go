package gold

import (
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// TestAccountsAsOfAggregates is the core coverage: an account with
// positions in two currencies plus a third-currency cash balance,
// with FX rates seeded so every line converts to both the
// account's base (USD) and the output currency (CHF). Asserts
// positions_value + cash + total in both columns.
func TestAccountsAsOfAggregates(t *testing.T) {
	db, ctx := openMigrated(t)

	// Account is USD-based.
	usd := "USD"
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{
			SilverSourceID: "test-src", AccountExternalID: "ACC1",
			AccountKind: canonical.AccountKindBrokerage,
			BaseCurrency: &usd,
			FirstSeenAt:  1000, LastSeenAt: 1000,
		}})
	})

	// FX: 1 USD = 0.80 CHF, 1 EUR = 1.06 CHF.
	// Reciprocal/triangulation handles USD↔EUR and EUR↔USD.
	seedFX(t, db, 1000, "CHF", "USD", "0.80")
	seedFX(t, db, 1000, "CHF", "EUR", "1.06")

	// Positions: 1000 USD of AAPL, 500 EUR of SAP.
	usd1000 := canonical.NewDecimalFromInt(1000)
	eur500 := canonical.NewDecimalFromInt(500)
	// Cash: 200 CHF.
	chf200 := canonical.NewDecimalFromInt(200)

	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.InsertPositions(ctx, []canonical.PositionChange{
			{
				SilverSourceID: "test-src", SnapshotAt: 1000,
				AccountExternalID: "ACC1", PositionKey: "AAPL",
				AssetClass: canonical.AssetClassEquity,
				Currency:   "USD", MarketValue: &usd1000,
			},
			{
				SilverSourceID: "test-src", SnapshotAt: 1000,
				AccountExternalID: "ACC1", PositionKey: "SAP",
				AssetClass: canonical.AssetClassEquity,
				Currency:   "EUR", MarketValue: &eur500,
			},
		}); err != nil {
			return err
		}
		return w.InsertCashBalances(ctx, []canonical.CashBalanceChange{{
			SilverSourceID: "test-src", SnapshotAt: 1000,
			AccountExternalID: "ACC1",
			Currency:          "CHF",
			BalanceKind:       canonical.BalanceKindCurrent,
			Amount:            chf200,
		}})
	})

	rows, err := AccountsAsOf(ctx, db, 2000, "CHF", canonical.FxModeHistoric)
	if err != nil {
		t.Fatalf("AccountsAsOf: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	a := rows[0]

	// positions_value (base USD): 1000 USD + (500 EUR → 500 * 1.06 / 0.80 = 662.5 USD)
	//                            = 1662.5 USD
	if a.PositionsValueBase == nil || *a.PositionsValueBase != "1662.5" {
		t.Errorf("positions_value (USD) = %v, want 1662.5", a.PositionsValueBase)
	}
	// cash (base USD): 200 CHF → 200 / 0.80 = 250 USD
	if a.CashBalanceBase == nil || *a.CashBalanceBase != "250" {
		t.Errorf("cash_balance (USD) = %v, want 250", a.CashBalanceBase)
	}
	// total (base USD): 1662.5 + 250 = 1912.5
	if a.TotalValueBase == nil || *a.TotalValueBase != "1912.5" {
		t.Errorf("total_value (USD) = %v, want 1912.5", a.TotalValueBase)
	}

	// In output ccy CHF:
	// positions: 1000 USD → 800 CHF; 500 EUR → 530 CHF; total 1330 CHF
	if a.PositionsValueOutCcy == nil || *a.PositionsValueOutCcy != "1330" {
		t.Errorf("positions_value (CHF) = %v, want 1330", a.PositionsValueOutCcy)
	}
	// cash: 200 CHF stays 200 CHF
	if a.CashBalanceOutCcy == nil || *a.CashBalanceOutCcy != "200" {
		t.Errorf("cash_balance (CHF) = %v, want 200", a.CashBalanceOutCcy)
	}
	// total: 1530 CHF
	if a.TotalValueOutCcy == nil || *a.TotalValueOutCcy != "1530" {
		t.Errorf("total_value (CHF) = %v, want 1530", a.TotalValueOutCcy)
	}
}

// TestAccountsAsOfNoBaseCurrency asserts that an account with no
// base_currency leaves the unsuffixed base columns blank but
// still populates the _<CCY> output columns (the requested output
// currency is always known).
func TestAccountsAsOfNoBaseCurrency(t *testing.T) {
	db, ctx := openMigrated(t)

	// Account WITHOUT base_currency.
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{
			SilverSourceID: "test-src", AccountExternalID: "ACC1",
			AccountKind: canonical.AccountKindOther,
			FirstSeenAt: 1000, LastSeenAt: 1000,
		}})
	})

	seedFX(t, db, 1000, "CHF", "USD", "0.80")
	usd500 := canonical.NewDecimalFromInt(500)
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertPositions(ctx, []canonical.PositionChange{{
			SilverSourceID: "test-src", SnapshotAt: 1000,
			AccountExternalID: "ACC1", PositionKey: "AAPL",
			AssetClass: canonical.AssetClassEquity,
			Currency:   "USD", MarketValue: &usd500,
		}})
	})

	rows, err := AccountsAsOf(ctx, db, 2000, "CHF", canonical.FxModeHistoric)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].PositionsValueBase != nil ||
		rows[0].CashBalanceBase != nil ||
		rows[0].TotalValueBase != nil {
		t.Errorf("base columns should be nil for account without base_currency, got %+v", rows[0])
	}
	if rows[0].PositionsValueOutCcy == nil || *rows[0].PositionsValueOutCcy != "400" {
		t.Errorf("positions_value (CHF) = %v, want 400", rows[0].PositionsValueOutCcy)
	}
}

// TestAccountsAsOfNoCrossAccountRollup confirms that
// `wealthdb accounts` reports each account's OWN positions+cash
// only. Migration 0004 moved portfolios into their own entity;
// the portfolio rollup is now `wealthdb portfolios`. Summing the
// accounts column must not double-count.
func TestAccountsAsOfNoCrossAccountRollup(t *testing.T) {
	db, ctx := openMigrated(t)

	chf := "CHF"
	port := "PORT1"
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{
			{SilverSourceID: "test-src", AccountExternalID: "CASH1",
				AccountKind: canonical.AccountKindCash, BaseCurrency: &chf,
				PortfolioExternalID: &port,
				FirstSeenAt:         1000, LastSeenAt: 1000},
			{SilverSourceID: "test-src", AccountExternalID: "SAFE1",
				AccountKind: canonical.AccountKindSafekeeping, BaseCurrency: &chf,
				PortfolioExternalID: &port,
				FirstSeenAt:         1000, LastSeenAt: 1000},
		})
	})

	chf1000 := canonical.NewDecimalFromInt(1000)
	chf500 := canonical.NewDecimalFromInt(500)
	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.InsertPositions(ctx, []canonical.PositionChange{{
			SilverSourceID: "test-src", SnapshotAt: 1000, AccountExternalID: "SAFE1",
			PositionKey: "X", AssetClass: canonical.AssetClassEquity,
			Currency: "CHF", MarketValue: &chf1000,
		}}); err != nil {
			return err
		}
		return w.InsertCashBalances(ctx, []canonical.CashBalanceChange{{
			SilverSourceID: "test-src", SnapshotAt: 1000, AccountExternalID: "CASH1",
			Currency: "CHF", BalanceKind: canonical.BalanceKindClosing, Amount: chf500,
		}})
	})

	rows, err := AccountsAsOf(ctx, db, 2000, "CHF", canonical.FxModeHistoric)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]AccountRow{}
	for _, r := range rows {
		byID[r.AccountExternalID] = r
	}

	// Each account reports its OWN values only — no cross-account
	// rollup. SAFE1 has only positions; CASH1 has only cash.
	if v := byID["SAFE1"].PositionsValueBase; v == nil || *v != "1000" {
		t.Errorf("SAFE1 positions_value = %v, want 1000", v)
	}
	if v := byID["SAFE1"].CashBalanceBase; v == nil || *v != "0" {
		t.Errorf("SAFE1 cash_balance = %v, want 0 (no cash on SAFE1)", v)
	}
	if v := byID["CASH1"].PositionsValueBase; v == nil || *v != "0" {
		t.Errorf("CASH1 positions_value = %v, want 0 (no positions on CASH1)", v)
	}
	if v := byID["CASH1"].CashBalanceBase; v == nil || *v != "500" {
		t.Errorf("CASH1 cash_balance = %v, want 500", v)
	}
	// And the portfolio_external_id round-trips through gold.
	if v := byID["CASH1"].PortfolioExternalID; v == nil || *v != "PORT1" {
		t.Errorf("CASH1 portfolio_external_id = %v, want PORT1", v)
	}
}

// TestAccountsAsOfEmptyAccount confirms that an account with no
// positions and no cash still appears, with zero aggregates (or
// nil for the base columns when base_currency is absent).
func TestAccountsAsOfEmptyAccount(t *testing.T) {
	db, ctx := openMigrated(t)

	usd := "USD"
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{
			SilverSourceID: "test-src", AccountExternalID: "EMPTY",
			AccountKind:  canonical.AccountKindBrokerage,
			BaseCurrency: &usd,
			FirstSeenAt:  1000, LastSeenAt: 1000,
		}})
	})

	rows, err := AccountsAsOf(ctx, db, 2000, "CHF", canonical.FxModeHistoric)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	for _, got := range []*string{
		rows[0].PositionsValueBase, rows[0].CashBalanceBase, rows[0].TotalValueBase,
		rows[0].PositionsValueOutCcy, rows[0].CashBalanceOutCcy, rows[0].TotalValueOutCcy,
	} {
		if got == nil || *got != "0" {
			t.Errorf("empty-account aggregate = %v, want \"0\"", got)
		}
	}
}
