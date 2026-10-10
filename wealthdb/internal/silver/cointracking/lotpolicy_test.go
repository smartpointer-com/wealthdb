package cointracking

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

func TestClassifyLotsReadsTheCTType(t *testing.T) {
	cases := []struct {
		name string
		t    lots.Txn
		want lots.Classified
	}{
		{"deposit", lots.Txn{Type: "Deposit", Kind: "transfer_in"}, lots.Classified{Action: lots.In}},
		{"airdrop", lots.Txn{Type: "Airdrop", Kind: "transfer_in"}, lots.Classified{Action: lots.Income}},
		{"gift received", lots.Txn{Type: "Gift / Tip", Kind: "transfer_in"}, lots.Classified{Action: lots.Income}},
		{"staking", lots.Txn{Type: "Staking", Kind: "staking", Quantity: 1}, lots.Classified{Action: lots.Income}},
		{"dust in", lots.Txn{Type: "Income (non taxable)", Comment: "Dust Sweeping"}, lots.Classified{Action: lots.Acquired}},
		{"move in", lots.Txn{Type: "Income (non taxable)"}, lots.Classified{Action: lots.In}},
		{"dust out", lots.Txn{Type: "Other Expense", Comment: "Dust Sweeping"},
			lots.Classified{Action: lots.Exchange, Disposal: lots.DisposeSell}},
		{"spend", lots.Txn{Type: "Spend"}, lots.Classified{Action: lots.Exchange, Disposal: lots.DisposeSpend}},
		{"fee", lots.Txn{Type: "Other Fee"}, lots.Classified{Action: lots.Exchange, Disposal: lots.DisposeFee}},
		{"lost", lots.Txn{Type: "Stolen"}, lots.Classified{Action: lots.Gone, Disposal: lots.DisposeLost}},
		{"donation", lots.Txn{Type: "Donation"}, lots.Classified{Action: lots.Gone, Disposal: lots.DisposeGift}},
		{"withdrawal", lots.Txn{Type: "Withdrawal"}, lots.Classified{Action: lots.Out}},
		{"trade, own-currency fee", lots.Txn{Type: "Trade", Kind: "buy", BuyCurrency: "BTC", SellCurrency: "USD",
			FeeAmount: 1, FeeCurrency: "USD"}, lots.Classified{Action: lots.Buy}},
		{"trade, third-currency fee", lots.Txn{Type: "Trade", Kind: "buy", BuyCurrency: "BTC", SellCurrency: "USD",
			FeeAmount: 1, FeeCurrency: "BNB"}, lots.Classified{Action: lots.Buy, Fee: true}},
		{"crypto trade, sell leg", lots.Txn{ID: "t:s", Type: "Trade", Kind: "sell", BuyCurrency: "ETH", SellCurrency: "BTC",
			FeeAmount: 1, FeeCurrency: "BNB"}, lots.Classified{Action: lots.Sell, Disposal: lots.DisposeSell}},
		{"crypto trade, buy leg", lots.Txn{ID: "t:b", Type: "Trade", Kind: "buy", BuyCurrency: "ETH", SellCurrency: "BTC",
			FeeAmount: 1, FeeCurrency: "BNB"}, lots.Classified{Action: lots.Buy, Fee: true}},
		{"no type: the kind", lots.Txn{Kind: "transfer_in"}, lots.Classified{Action: lots.In}},
	}
	for _, c := range cases {
		if got := classifyLots(c.t); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestPolicyIsRegisteredPooledWithFeesIncluded(t *testing.T) {
	p, ok := lots.PolicyFor(kindName)
	if !ok || p.Mode != lots.Fill || p.Grain != lots.GrainPortfolio || p.Fees != lots.FeesIncluded {
		t.Fatalf("policy %+v", p)
	}
	if !p.Skip("CHF") || p.Skip("BTC") {
		t.Error("a fiat ticker is never a key; a coin is")
	}
}
