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

func TestAssetClassValidV2(t *testing.T) {
	for _, a := range []AssetClass{
		AssetClassPublicEquity, AssetClassPrivateEquity, AssetClassFixedIncome,
		AssetClassPrivateDebt, AssetClassRealEstate, AssetClassInfrastructure,
		AssetClassMetal, AssetClassCrypto, AssetClassCash,
		AssetClassForeignExchange, AssetClassHedgeFund, AssetClassMultiAsset,
		AssetClassOther,
	} {
		if !a.ValidV2() {
			t.Errorf("AssetClass(%q).ValidV2() = false, want true", a)
		}
	}
	// Legacy-only values are NOT valid V2 exposures — they are
	// wrappers or blends that become (asset_class, vehicle) pairs.
	for _, a := range []AssetClass{
		AssetClassEquity, AssetClassETF, AssetClassBondETF, AssetClassFund,
		AssetClassBond, AssetClassOption, AssetClassFuture, AssetClassFxForward,
		AssetClassFxOption, AssetClassMoneyMarket, AssetClassOTCDerivative,
		AssetClassSPV, AssetClassPrivateFund, AssetClassConvertibleNote,
		AssetClassMortgage, "",
	} {
		if a.ValidV2() {
			t.Errorf("AssetClass(%q).ValidV2() = true, want false (legacy-only)", a)
		}
	}
	// The legacy Valid() set must be unchanged by the migration — the
	// legacy column is a control. A V2-only value is not legacy-valid.
	if AssetClassCash.Valid() {
		t.Error("AssetClassCash.Valid() = true; cash is a V2-only value, must not be legacy-valid")
	}
	if !AssetClassEquity.Valid() {
		t.Error("AssetClassEquity.Valid() = false; legacy set must still accept it")
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
