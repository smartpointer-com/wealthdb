package ubs

import (
	"context"
	"testing"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

// TestPSNHoldingsGapFilter locks in the cutover-gap fix: PSN's cash
// and forward-contract feeds can go live a day before its first MT535
// holdings batch. On that gap day PSN emits a positions snapshot with
// forwards but no securities; left alone it wins gold's "latest
// snapshot per source" and blanks out web's carried-forward
// historical securities (a one-day dip to ~0). The filter drops PSN
// position rows dated before the first holdings snapshot while leaving
// cash balances (and dimensions) intact, so web stays authoritative
// for securities until PSN actually holds them and PSN cash still
// takes over immediately.
func TestPSNHoldingsGapFilter(t *testing.T) {
	const firstHoldings = 200
	mv := canonical.NewDecimalFromFloat(1)
	gapDay := canonical.SnapshotBatch{
		Positions: []canonical.PositionChange{{
			SnapshotAt: 100, AccountExternalID: "fwd", PositionKey: "fwd",
			AssetClass: canonical.AssetClassFxForward, MarketValue: &mv,
		}},
		CashBalances: []canonical.CashBalanceChange{{
			SnapshotAt: 100, AccountExternalID: "iban", Currency: "CHF",
		}},
	}
	holdingsDay := canonical.SnapshotBatch{
		Positions: []canonical.PositionChange{{
			SnapshotAt: firstHoldings, AccountExternalID: "sk", PositionKey: "CH0000000001",
			AssetClass: canonical.AssetClassOther, MarketValue: &mv,
		}},
		CashBalances: []canonical.CashBalanceChange{{
			SnapshotAt: firstHoldings, AccountExternalID: "iban", Currency: "CHF",
		}},
	}
	inner := silver.NewSnapshotStream([]canonical.SnapshotBatch{gapDay, holdingsDay})
	s := &psnHoldingsGapFilter{inner: inner, firstHoldingsAt: firstHoldings}
	defer s.Close()

	ctx := context.Background()
	var posBySnap = map[int64]int{}
	var cashBySnap = map[int64]int{}
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

	// Gap-day (pre-holdings) positions dropped; cash kept.
	if posBySnap[100] != 0 {
		t.Errorf("gap-day positions = %d, want 0 (must not supersede web securities)", posBySnap[100])
	}
	if cashBySnap[100] != 1 {
		t.Errorf("gap-day cash = %d, want 1 (PSN cash must still flow)", cashBySnap[100])
	}
	// Holdings-day positions kept.
	if posBySnap[firstHoldings] != 1 {
		t.Errorf("holdings-day positions = %d, want 1", posBySnap[firstHoldings])
	}
}
