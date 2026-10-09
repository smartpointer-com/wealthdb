package synthetic

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

func TestBasisFor(t *testing.T) {
	stamp := func(m canonical.BasisMethod, f canonical.BasisFees) canonical.Basis {
		return canonical.Basis{Origin: canonical.BasisStated, Method: m, Fees: f}
	}
	cases := []struct {
		ac   canonical.AssetClass
		veh  canonical.Vehicle
		lots bool
		want canonical.Basis
	}{
		{canonical.AssetClassPublicEquity, canonical.VehicleETF, false, stamp(canonical.BasisMethodAverage, canonical.BasisFeesNone)},
		{canonical.AssetClassPublicEquity, canonical.VehicleETF, true, stamp(canonical.BasisMethodLots, canonical.BasisFeesNone)},
		{canonical.AssetClassMultiAsset, canonical.VehicleFund, false, stamp(canonical.BasisMethodAverage, canonical.BasisFeesNone)},
		{canonical.AssetClassPrivateEquity, canonical.VehicleFund, false, stamp(canonical.BasisMethodPaidIn, canonical.BasisFeesNone)},
		{canonical.AssetClassPrivateEquity, canonical.VehicleFund, true, stamp(canonical.BasisMethodPaidIn, canonical.BasisFeesNone)},
		{canonical.AssetClassPrivateEquity, canonical.VehicleSPV, false, stamp(canonical.BasisMethodPaidIn, canonical.BasisFeesNone)},
		{canonical.AssetClassCrypto, canonical.VehiclePhysical, false, stamp(canonical.BasisMethodAverage, canonical.BasisFeesExcluded)},
		{canonical.AssetClassCrypto, canonical.VehiclePhysical, true, stamp(canonical.BasisMethodLots, canonical.BasisFeesIncluded)},
		{canonical.AssetClassRealEstate, canonical.VehiclePhysical, false, stamp(canonical.BasisMethodAcquisitionValue, canonical.BasisFeesNone)},
		{canonical.AssetClassRealEstate, canonical.VehicleMortgage, false, stamp(canonical.BasisMethodAcquisitionValue, canonical.BasisFeesNone)},
		{canonical.AssetClassRealEstate, canonical.VehicleETF, false, stamp(canonical.BasisMethodAverage, canonical.BasisFeesNone)},
		// A realized lot whose instrument has no row: a lot, no fee.
		{"", "", true, stamp(canonical.BasisMethodLots, canonical.BasisFeesNone)},
	}
	for _, c := range cases {
		if got := basisFor(c.ac, c.veh, c.lots); got != c.want {
			t.Errorf("basisFor(%s, %s, %t) = %+v, want %+v", c.ac, c.veh, c.lots, got, c.want)
		}
		if err := c.want.Validate(); err != nil {
			t.Errorf("test bug: %v", err)
		}
	}
}

// A position's book value carries its stamp; one without a book value
// carries none.
func TestPositionsCarryTheBasisStamp(t *testing.T) {
	path, db := newFixture(t)
	exec(t, db, `INSERT INTO dump_runs VALUES (1, ?, ?, '2031-01-01')`, day(1), day(1))
	exec(t, db, `INSERT INTO accounts (account_id, account_kind, payload) VALUES ('acct-pm', 'brokerage', '{}')`)
	exec(t, db, `INSERT INTO instruments (instrument_id, valid_from, asset_class, vehicle, payload) VALUES
        ('inst-fund', ?, 'private_equity', 'fund', '{}'),
        ('inst-etf',  ?, 'public_equity',  'etf',  '{}')`, day(1), day(1))
	exec(t, db, insertPosition, day(1), "acct-pm", "inst-fund", "inst-fund", "private_equity", "fund",
		"USD", nil, "1100", "1000", nil, nil, "{}")
	exec(t, db, insertPosition, day(1), "acct-pm", "inst-etf", "inst-etf", "public_equity", "etf",
		"USD", "10", "500", nil, nil, nil, "{}")
	conn := openConn(t, path)
	got := map[string]canonical.PositionChange{}
	for _, b := range collectSnapshots(t, conn, window(t, conn, -1)) {
		for _, p := range b.Positions {
			got[p.PositionKey] = p
		}
	}
	fund := got["inst-fund"]
	if dec(fund.BookValue) != "1000" || fund.Basis != basisFor(canonical.AssetClassPrivateEquity, canonical.VehicleFund, false) {
		t.Errorf("fund book %s stamped %+v, want 1000 paid in", dec(fund.BookValue), fund.Basis)
	}
	if etf := got["inst-etf"]; etf.BookValue != nil || !etf.Basis.IsZero() {
		t.Errorf("etf without a book value: %v stamped %+v, want neither", etf.BookValue, etf.Basis)
	}
}
