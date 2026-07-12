package canonical

import "testing"

// TestAssetClassValidCoarse locks in the intermediate 1-D vocabulary
// (used by adapters to canonicalise a raw silver class) and its
// distinction from Valid, which checks the exposure set written to
// gold. The two overlap only on the shared strings.
func TestAssetClassValidCoarse(t *testing.T) {
	// Coarse-only labels: valid coarse, NOT valid exposures.
	for _, a := range []AssetClass{
		AssetClassEquity, AssetClassETF, AssetClassBondETF, AssetClassFund,
		AssetClassBond, AssetClassOption, AssetClassFuture, AssetClassFxForward,
		AssetClassFxOption, AssetClassMoneyMarket, AssetClassOTCDerivative,
		AssetClassSPV, AssetClassPrivateFund, AssetClassConvertibleNote,
		AssetClassMortgage,
	} {
		if !a.ValidCoarse() {
			t.Errorf("AssetClass(%q).ValidCoarse() = false, want true", a)
		}
		if a.Valid() {
			t.Errorf("AssetClass(%q).Valid() = true, want false (coarse-only)", a)
		}
	}
	// Exposure-only values: valid exposures, NOT coarse labels.
	for _, a := range []AssetClass{
		AssetClassPublicEquity, AssetClassFixedIncome, AssetClassPrivateDebt,
		AssetClassInfrastructure, AssetClassCash, AssetClassForeignExchange,
		AssetClassHedgeFund, AssetClassMultiAsset,
	} {
		if a.ValidCoarse() {
			t.Errorf("AssetClass(%q).ValidCoarse() = true, want false (exposure-only)", a)
		}
	}
	// Shared strings: valid in both vocabularies.
	for _, a := range []AssetClass{
		AssetClassPrivateEquity, AssetClassRealEstate, AssetClassMetal,
		AssetClassCrypto, AssetClassOther,
	} {
		if !a.ValidCoarse() || !a.Valid() {
			t.Errorf("AssetClass(%q): want valid in both vocabularies", a)
		}
	}
	for _, a := range []AssetClass{"", "unknown", "EQUITY"} {
		if a.ValidCoarse() {
			t.Errorf("AssetClass(%q).ValidCoarse() = true, want false", a)
		}
	}
}

func TestAccountKindValid(t *testing.T) {
	cases := []struct {
		v    AccountKind
		want bool
	}{
		{AccountKindBrokerage, true},
		{AccountKindSafekeeping, true},
		{AccountKindOverlay, true},
		{"", false},
		{"BROKERAGE", false},
		{"portfolio", false}, // removed in migration 0004
	}
	for _, c := range cases {
		if got := c.v.Valid(); got != c.want {
			t.Errorf("AccountKind(%q).Valid() = %v, want %v", c.v, got, c.want)
		}
	}
}

func TestTxKindValid(t *testing.T) {
	cases := []struct {
		v    TxKind
		want bool
	}{
		{TxKindBuy, true},
		{TxKindCorporateAction, true},
		{TxKindContribution, true},
		{TxKindDistribution, true},
		{TxKindOther, true},
		{"", false},
		{"buy_or_sell", false},
	}
	for _, c := range cases {
		if got := c.v.Valid(); got != c.want {
			t.Errorf("TxKind(%q).Valid() = %v, want %v", c.v, got, c.want)
		}
	}
}

func TestBalanceKindValid(t *testing.T) {
	cases := []struct {
		v    BalanceKind
		want bool
	}{
		{BalanceKindOpening, true},
		{BalanceKindAggregated, true},
		{"", false},
		{"running", false},
	}
	for _, c := range cases {
		if got := c.v.Valid(); got != c.want {
			t.Errorf("BalanceKind(%q).Valid() = %v, want %v", c.v, got, c.want)
		}
	}
}

func TestFxModeValid(t *testing.T) {
	cases := []struct {
		v    FxMode
		want bool
	}{
		{FxModeHistoric, true},
		{FxModeCurrent, true},
		{"", false},
		{"spot", false},
	}
	for _, c := range cases {
		if got := c.v.Valid(); got != c.want {
			t.Errorf("FxMode(%q).Valid() = %v, want %v", c.v, got, c.want)
		}
	}
}
