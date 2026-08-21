package silver

import (
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// TestClosureMarkerBatch pins the exit-day zero snapshot: the previous held
// snapshot's positions replayed at zero value (quantity zeroed only where one
// existed), their instruments re-stamped at t, and the custody account carried
// along.
func TestClosureMarkerBatch(t *testing.T) {
	qty := canonical.NewDecimalFromInt(100)
	mv := canonical.NewDecimalFromInt(5000)
	instA, instB := "deal-A", "deal-B"
	nameA := "ACME-CO"
	prev := canonical.SnapshotBatch{
		Positions: []canonical.PositionChange{
			{SnapshotAt: 100, AccountExternalID: "ACCT", PositionKey: "deal-A",
				InstrumentExternalID: &instA, AssetClass: canonical.AssetClassSPV,
				Vehicle: canonical.VehicleSPV, Currency: "USD", Quantity: &qty, MarketValue: &mv},
			{SnapshotAt: 100, AccountExternalID: "ACCT", PositionKey: "deal-B",
				InstrumentExternalID: &instB, AssetClass: canonical.AssetClassPrivateEquity,
				Vehicle: canonical.VehicleFund, Currency: "USD", MarketValue: &mv},
		},
		Instruments: []canonical.InstrumentChange{
			{InstrumentExternalID: instA, AssetClass: canonical.AssetClassSPV,
				Vehicle: canonical.VehicleSPV, Name: &nameA, FirstSeenAt: 100, LastSeenAt: 100},
			{InstrumentExternalID: instB, AssetClass: canonical.AssetClassPrivateEquity,
				Vehicle: canonical.VehicleFund, FirstSeenAt: 100, LastSeenAt: 100},
		},
	}
	acct := canonical.AccountChange{AccountExternalID: "ACCT",
		AccountKind: canonical.AccountKindCustody, FirstSeenAt: 200, LastSeenAt: 200}

	got := ClosureMarkerBatch(prev, 200, acct)

	if len(got.Accounts) != 1 || got.Accounts[0].AccountExternalID != "ACCT" {
		t.Fatalf("accounts = %+v, want the custody account", got.Accounts)
	}
	if len(got.Positions) != 2 {
		t.Fatalf("positions = %d, want both former holdings replayed", len(got.Positions))
	}
	for _, p := range got.Positions {
		if p.SnapshotAt != 200 {
			t.Errorf("%s snapshot_at = %d, want the exit day", p.PositionKey, p.SnapshotAt)
		}
		if p.MarketValue == nil || !p.MarketValue.IsZero() {
			t.Errorf("%s market value = %v, want zero", p.PositionKey, p.MarketValue)
		}
		if p.BookValue != nil {
			t.Errorf("%s book value = %v, want nil (marker carries no basis)", p.PositionKey, p.BookValue)
		}
		if string(p.Payload) != `{"closure_marker": true}` {
			t.Errorf("%s payload = %s, want the closure marker", p.PositionKey, p.Payload)
		}
	}
	byKey := map[string]canonical.PositionChange{}
	for _, p := range got.Positions {
		byKey[p.PositionKey] = p
	}
	if q := byKey["deal-A"].Quantity; q == nil || !q.IsZero() {
		t.Errorf("deal-A quantity = %v, want explicit zero (previous snapshot had a count)", q)
	}
	if q := byKey["deal-B"].Quantity; q != nil {
		t.Errorf("deal-B quantity = %v, want nil (fund units have no count)", q)
	}
	if len(got.Instruments) != 2 {
		t.Fatalf("instruments = %d, want both re-emitted for the FK", len(got.Instruments))
	}
	for _, i := range got.Instruments {
		if i.FirstSeenAt != 200 || i.LastSeenAt != 200 {
			t.Errorf("%s seen range = [%d,%d], want the exit day", i.InstrumentExternalID, i.FirstSeenAt, i.LastSeenAt)
		}
	}
	if got.Instruments[0].Name == nil || *got.Instruments[0].Name != "ACME-CO" {
		t.Errorf("instrument name not carried through: %+v", got.Instruments[0])
	}
}
