package gold

import (
	"testing"

	"github.com/ptu/wealthdb/internal/canonical"
)

// TestGlobalAsOfRollup seeds two USD accounts captured at different
// snapshots and asserts that GlobalAsOf sums their output-currency
// aggregates into one row and spans their snapshot dates.
func TestGlobalAsOfRollup(t *testing.T) {
	db, ctx := openMigrated(t)
	usd := "USD"

	// Two accounts in DIFFERENT sources captured at different
	// snapshots: each source contributes its own latest snapshot ≤
	// the as-of date, so both survive (and span the date range).
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{
			{SilverSourceID: "src1", AccountExternalID: "ACC1", AccountKind: canonical.AccountKindBrokerage, BaseCurrency: &usd, FirstSeenAt: 1000, LastSeenAt: 1000},
			{SilverSourceID: "src2", AccountExternalID: "ACC2", AccountKind: canonical.AccountKindBrokerage, BaseCurrency: &usd, FirstSeenAt: 2000, LastSeenAt: 2000},
		})
	})

	// 1 USD = 0.80 CHF (flat-extrapolates to the snap-2000 lines).
	seedFX(t, db, 1000, "CHF", "USD", "0.80")

	pos1 := canonical.NewDecimalFromInt(1000) // ACC1: 1000 USD -> 800 CHF
	pos2 := canonical.NewDecimalFromInt(500)  // ACC2:  500 USD -> 400 CHF
	cash1 := canonical.NewDecimalFromInt(200) // 200 CHF
	cash2 := canonical.NewDecimalFromInt(100) // 100 CHF

	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.InsertPositions(ctx, []canonical.PositionChange{
			{SilverSourceID: "src1", SnapshotAt: 1000, AccountExternalID: "ACC1", PositionKey: "AAPL", AssetClass: canonical.AssetClassEquity, Currency: "USD", MarketValue: &pos1},
			{SilverSourceID: "src2", SnapshotAt: 2000, AccountExternalID: "ACC2", PositionKey: "MSFT", AssetClass: canonical.AssetClassEquity, Currency: "USD", MarketValue: &pos2},
		}); err != nil {
			return err
		}
		return w.InsertCashBalances(ctx, []canonical.CashBalanceChange{
			{SilverSourceID: "src1", SnapshotAt: 1000, AccountExternalID: "ACC1", Currency: "CHF", BalanceKind: canonical.BalanceKindCurrent, Amount: cash1},
			{SilverSourceID: "src2", SnapshotAt: 2000, AccountExternalID: "ACC2", Currency: "CHF", BalanceKind: canonical.BalanceKindCurrent, Amount: cash2},
		})
	})

	g, err := GlobalAsOf(ctx, db, 3000, "CHF", canonical.FxModeHistoric)
	if err != nil {
		t.Fatalf("GlobalAsOf: %v", err)
	}

	if g.MinSnapshotAt != 1000 {
		t.Errorf("MinSnapshotAt = %d, want 1000", g.MinSnapshotAt)
	}
	if g.MaxSnapshotAt != 2000 {
		t.Errorf("MaxSnapshotAt = %d, want 2000", g.MaxSnapshotAt)
	}
	// positions: 800 + 400 = 1200 CHF; cash: 200 + 100 = 300 CHF; total: 1500 CHF.
	if got := derefOr(g.PositionsValueOutCcy); got != "1200" {
		t.Errorf("positions_value (CHF) = %q, want 1200", got)
	}
	if got := derefOr(g.CashBalanceOutCcy); got != "300" {
		t.Errorf("cash_balance (CHF) = %q, want 300", got)
	}
	if got := derefOr(g.TotalValueOutCcy); got != "1500" {
		t.Errorf("total_value (CHF) = %q, want 1500", got)
	}
}

// TestGlobalAsOfEmpty confirms an empty gold DB yields a zeroed row
// (sums "0", no snapshot dates) rather than an error.
func TestGlobalAsOfEmpty(t *testing.T) {
	db, ctx := openMigrated(t)
	g, err := GlobalAsOf(ctx, db, 3000, "USD", canonical.FxModeHistoric)
	if err != nil {
		t.Fatalf("GlobalAsOf: %v", err)
	}
	if g.MinSnapshotAt != 0 || g.MaxSnapshotAt != 0 {
		t.Errorf("snapshot span = [%d,%d], want [0,0]", g.MinSnapshotAt, g.MaxSnapshotAt)
	}
	if got := derefOr(g.TotalValueOutCcy); got != "0" {
		t.Errorf("total_value = %q, want 0", got)
	}
}

func derefOr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}
