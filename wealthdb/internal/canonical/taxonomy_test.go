package canonical

import "testing"

func TestVehicleValid(t *testing.T) {
	for _, v := range []Vehicle{
		VehicleStock, VehicleETF, VehicleFund, VehicleSPV, VehicleBond,
		VehicleConvertibleNote, VehicleLoan, VehicleOption, VehicleFuture,
		VehicleForward, VehicleTimeDeposit, VehicleDemandDeposit,
		VehiclePhysical, VehicleStructuredProduct, VehicleRight,
		VehicleMortgage, VehicleEscrow, VehicleOther,
	} {
		if !v.Valid() {
			t.Errorf("Vehicle(%q).Valid() = false, want true", v)
		}
	}
	for _, v := range []Vehicle{"", "shares", "ETF", "cash", "money_market"} {
		if Vehicle(v).Valid() {
			t.Errorf("Vehicle(%q).Valid() = true, want false", v)
		}
	}
}

func TestAssetClassValid(t *testing.T) {
	for _, a := range []AssetClass{
		AssetClassPublicEquity, AssetClassPrivateEquity, AssetClassFixedIncome,
		AssetClassPrivateDebt, AssetClassRealEstate, AssetClassInfrastructure,
		AssetClassMetal, AssetClassCrypto, AssetClassCash,
		AssetClassForeignExchange, AssetClassHedgeFund, AssetClassMultiAsset,
		AssetClassOther,
	} {
		if !a.Valid() {
			t.Errorf("AssetClass(%q).Valid() = false, want true", a)
		}
	}
	// The adapters' intermediate 1-D labels are NOT exposure values —
	// they are wrappers or blends that reach gold as (asset_class,
	// vehicle) pairs, so Valid must reject them.
	for _, a := range []AssetClass{
		AssetClassEquity, AssetClassETF, AssetClassBondETF, AssetClassFund,
		AssetClassBond, AssetClassOption, AssetClassFuture, AssetClassFxForward,
		AssetClassFxOption, AssetClassMoneyMarket, AssetClassOTCDerivative,
		AssetClassSPV, AssetClassPrivateFund, AssetClassConvertibleNote,
		AssetClassMortgage, "",
	} {
		if a.Valid() {
			t.Errorf("AssetClass(%q).Valid() = true, want false (intermediate-only)", a)
		}
	}
}

func TestValidTaxonomyPair(t *testing.T) {
	ok := []struct {
		a AssetClass
		v Vehicle
	}{
		{AssetClassPublicEquity, VehicleStock},
		{AssetClassPublicEquity, VehicleETF},
		{AssetClassFixedIncome, VehicleETF},  // bond ETF
		{AssetClassFixedIncome, VehicleBond}, // direct bond
		{AssetClassCash, VehicleDemandDeposit},
		{AssetClassCash, VehicleFund}, // money-market fund
		{AssetClassMetal, VehiclePhysical},
		{AssetClassCrypto, VehiclePhysical},
		{AssetClassPrivateEquity, VehicleSPV},
		{AssetClassInfrastructure, VehicleFund},
		{AssetClassHedgeFund, VehicleFund},
		{AssetClassRealEstate, VehicleMortgage},
		{AssetClassForeignExchange, VehicleForward},
		{AssetClassPrivateDebt, VehicleConvertibleNote},
		{AssetClassPrivateEquity, VehicleConvertibleNote}, // pre-seed note, equity-like
		{AssetClassPrivateDebt, VehicleEscrow},
		{AssetClassOther, VehicleOther},
	}
	for _, c := range ok {
		if !ValidTaxonomyPair(c.a, c.v) {
			t.Errorf("ValidTaxonomyPair(%q, %q) = false, want true", c.a, c.v)
		}
	}
	bad := []struct {
		a AssetClass
		v Vehicle
	}{
		{AssetClassCrypto, VehicleMortgage},   // nonsensical
		{AssetClassHedgeFund, VehicleStock},   // hedge funds are funds
		{AssetClassCash, VehicleStock},        // cash isn't a share
		{AssetClassMetal, VehicleBond},        // metals aren't bonds
		{AssetClassPublicEquity, VehicleLoan}, // equity isn't a loan
		{"nonsense", VehicleStock},
		{AssetClassPublicEquity, "nonsense"},
	}
	for _, c := range bad {
		if ValidTaxonomyPair(c.a, c.v) {
			t.Errorf("ValidTaxonomyPair(%q, %q) = true, want false", c.a, c.v)
		}
	}
}
