package ubs

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
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
			Instruments: []canonical.InstrumentChange{
				{InstrumentExternalID: "fwd", Vehicle: canonical.VehicleForward, FirstSeenAt: at},
				{InstrumentExternalID: "XX0000000002", Vehicle: canonical.VehicleStock, FirstSeenAt: at},
			},
			FundCompositions: []canonical.FundCompositionChange{{FundRef: "fwd", AsOf: at}},
			FundLines:        []canonical.FundLineChange{{FundRef: "fwd", AsOf: at}},
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
	s := silver.MapSnapshots(inner, psnHoldingsGapFilter{firstHoldingsAt: firstHoldings, lastHoldingsAt: lastHoldings}.apply)
	defer s.Close()

	ctx := context.Background()
	posBySnap := map[int64]int{}
	cashBySnap := map[int64]int{}
	var kept []string
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
		for _, i := range batch.Instruments {
			kept = append(kept, i.InstrumentExternalID)
		}
		if len(batch.FundCompositions)+len(batch.FundLines) > 0 {
			kept = append(kept, "legs")
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
	// A gap day's forward keeps no instrument and no legs; any other
	// instrument it states still flows.
	if len(kept) != 2 || kept[0] != "XX0000000002" || kept[1] != "XX0000000002" {
		t.Errorf("gap days kept %v, want the stock's instrument alone, once a day", kept)
	}
	// Holdings-day positions kept.
	if posBySnap[firstHoldings] != 1 {
		t.Errorf("holdings-day positions = %d, want 1", posBySnap[firstHoldings])
	}
}

// The fold keeps web's row under `web` and states its Rating under the
// rating key, where the PSN row states none of its own; a base that is
// no object keeps the row alone.
func TestTheWebFoldStatesTheRating(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ base, web, want string }{
		{`{"fields":{}}`, `{"Rating":"A+","cost_price":1}`,
			`{"fields":{},"rating_sp":"A+","web":{"Rating":"A+","cost_price":1}}`},
		{`{"rating_sp":"B"}`, `{"Rating":"A+"}`, `{"rating_sp":"B","web":{"Rating":"A+"}}`},
		{`{"fields":{}}`, `{"Rating":null}`, `{"fields":{},"web":{"Rating":null}}`},
		{`{"fields":{}}`, `{"cost_price":1}`, `{"fields":{},"web":{"cost_price":1}}`},
		{``, `{"Rating":"BBB"}`, `{"rating_sp":"BBB","web":{"Rating":"BBB"}}`},
		{`[1]`, `{"cost_price": 1}`, `{"web":{"cost_price": 1}}`},
	} {
		if got := string(foldWebPayloadAsWebKey(json.RawMessage(tc.base), tc.web)); got != tc.want {
			t.Errorf("%s + %s:\n %s, want %s", tc.base, tc.web, got, tc.want)
		}
	}
}
