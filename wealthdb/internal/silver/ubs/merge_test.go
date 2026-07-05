package ubs

import (
	"context"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// TestPSNHoldingsGapFilter locks in the cutover-gap fix at BOTH edges.
// PSN's cash and forward-contract feeds can bracket its MT535 holdings
// batches: they go live a day before the first batch, and (after a
// nightly run) can arrive before that day's holdings land. On such a
// leading OR trailing gap day PSN emits a positions snapshot with
// forwards but no securities; left alone it wins gold's "latest
// snapshot per source" and blanks the real portfolio to ~0. The filter
// drops PSN position rows dated outside the [first, last] holdings
// window while leaving cash balances (and dimensions) intact, so the
// nearest complete securities snapshot stays authoritative and PSN cash
// still flows.
func TestPSNHoldingsGapFilter(t *testing.T) {
	const firstHoldings, lastHoldings = 200, 200
	mv := canonical.NewDecimalFromFloat(1)
	fwdDay := func(at int64) canonical.SnapshotBatch {
		return canonical.SnapshotBatch{
			Positions: []canonical.PositionChange{{
				SnapshotAt: at, AccountExternalID: "fwd", PositionKey: "fwd",
				AssetClass: canonical.AssetClassFxForward, MarketValue: &mv,
			}},
			CashBalances: []canonical.CashBalanceChange{{
				SnapshotAt: at, AccountExternalID: "iban", Currency: "CHF",
			}},
		}
	}
	leadingDay := fwdDay(100) // before the first holdings batch
	holdingsDay := canonical.SnapshotBatch{
		Positions: []canonical.PositionChange{{
			SnapshotAt: firstHoldings, AccountExternalID: "sk", PositionKey: "CH0000000001",
			AssetClass: canonical.AssetClassOther, MarketValue: &mv,
		}},
		CashBalances: []canonical.CashBalanceChange{{
			SnapshotAt: firstHoldings, AccountExternalID: "iban", Currency: "CHF",
		}},
	}
	trailingDay := fwdDay(300) // a nightly captured before that day's holdings
	inner := silver.NewSnapshotStream([]canonical.SnapshotBatch{leadingDay, holdingsDay, trailingDay})
	s := &psnHoldingsGapFilter{inner: inner, firstHoldingsAt: firstHoldings, lastHoldingsAt: lastHoldings}
	defer s.Close()

	ctx := context.Background()
	posBySnap := map[int64]int{}
	cashBySnap := map[int64]int{}
	for {
		batch, more, err := s.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range batch.Positions {
			posBySnap[p.SnapshotAt]++
		}
		for _, cb := range batch.CashBalances {
			cashBySnap[cb.SnapshotAt]++
		}
		if !more {
			break
		}
	}

	// Bracket-day positions (before first / after last holdings) dropped;
	// cash kept.
	for _, at := range []int64{100, 300} {
		if posBySnap[at] != 0 {
			t.Errorf("gap-day %d positions = %d, want 0 (must not supersede the portfolio)", at, posBySnap[at])
		}
		if cashBySnap[at] != 1 {
			t.Errorf("gap-day %d cash = %d, want 1 (PSN cash must still flow)", at, cashBySnap[at])
		}
	}
	// Holdings-day positions kept.
	if posBySnap[firstHoldings] != 1 {
		t.Errorf("holdings-day positions = %d, want 1", posBySnap[firstHoldings])
	}
}
