package canonical

import "testing"

func TestAssetClassValid(t *testing.T) {
	cases := []struct {
		v    AssetClass
		want bool
	}{
		{AssetClassEquity, true},
		{AssetClassETF, true},
		{AssetClassOther, true},
		{AssetClassFxForward, true},
		{"", false},
		{"unknown", false},
		{AssetClass("EQUITY"), false}, // case-sensitive
	}
	for _, c := range cases {
		if got := c.v.Valid(); got != c.want {
			t.Errorf("AssetClass(%q).Valid() = %v, want %v", c.v, got, c.want)
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
