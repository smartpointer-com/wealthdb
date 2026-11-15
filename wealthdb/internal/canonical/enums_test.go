package canonical

import "testing"

func TestAccountKindValid(t *testing.T) {
	cases := []struct {
		v    AccountKind
		want bool
	}{
		{AccountKindBrokerage, true},
		{AccountKindSafekeeping, true},
		{AccountKindOverlay, true},
		{AccountKindMortgage, true},
		{AccountKindCard, true},
		{AccountKindDonorAdvisedFund, true},
		{"", false},
		{"BROKERAGE", false},
		{"credit_card", false}, // the value is 'card'
		{"portfolio", false},   // removed in migration 0004
	}
	for _, c := range cases {
		if got := c.v.Valid(); got != c.want {
			t.Errorf("AccountKind(%q).Valid() = %v, want %v", c.v, got, c.want)
		}
	}
}

// A card, a mortgage and an overlay keep their source's kind; every
// other kind an override may restate.
func TestAccountKindFollowsSource(t *testing.T) {
	for k := range accountKindValues {
		want := k == AccountKindCard || k == AccountKindMortgage || k == AccountKindOverlay
		if got := k.FollowsSource(); got != want {
			t.Errorf("AccountKind(%q).FollowsSource() = %v, want %v", k, got, want)
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
		{TxKindPurchase, true},
		{TxKindRefund, true},
		{TxKindCardPayment, true},
		{TxKindReward, true},
		{TxKindOther, true},
		{"", false},
		{"buy_or_sell", false},
		{"payment", false}, // the card-payment value is 'card_payment'
		{"rewards", false}, // singular
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
