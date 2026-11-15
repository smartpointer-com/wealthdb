package cointracking

import (
	"database/sql"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// The kind table reads a row by its CT type, its comment and whether the
// asset is cash: a deposit and an airdrop are both transfer_in, and only
// the action tells them apart.
func TestKindTableReadsTheCTType(t *testing.T) {
	cases := []struct {
		name, typ, comment string
		cash               bool
		kind               canonical.TxKind
		action             canonical.TxAction
	}{
		{"coin deposit", "Deposit", "", false, canonical.TxKindTransferIn, canonical.TxActionIn},
		{"cash deposit", "Deposit", "", true, canonical.TxKindDeposit, canonical.TxActionIn},
		{"coin withdrawal", "Withdrawal", "", false, canonical.TxKindTransferOut, canonical.TxActionOut},
		{"cash withdrawal", "Withdrawal", "", true, canonical.TxKindWithdrawal, canonical.TxActionOut},
		{"airdrop", "Airdrop", "", false, canonical.TxKindTransferIn, canonical.TxActionIncome},
		{"gift received", "Gift / Tip", "", false, canonical.TxKindTransferIn, canonical.TxActionIncome},
		{"staking", "Staking", "", false, canonical.TxKindStaking, canonical.TxActionIncome},
		{"reward", "Reward / Bonus", "", false, canonical.TxKindInterest, canonical.TxActionIncome},
		{"dust in", "Income (non taxable)", "Dust Sweeping", false, canonical.TxKindTransferIn, canonical.TxActionAcquired},
		{"dust in, any case", "Income (non taxable)", "DUST SWEEPING", false, canonical.TxKindTransferIn,
			canonical.TxActionAcquired},
		{"move in", "Income (non taxable)", "", false, canonical.TxKindTransferIn, canonical.TxActionIn},
		{"move out", "Expense (non taxable)", "", false, canonical.TxKindTransferOut, canonical.TxActionOut},
		{"dust out", "Other Expense", "Dust Sweeping", false, "", canonical.TxActionConverted},
		{"expense", "Other Expense", "Dust", false, "", canonical.TxActionSpend},
		{"spend", "Spend", "", false, canonical.TxKindOther, canonical.TxActionSpend},
		{"fee", "Other Fee", "", false, canonical.TxKindFee, canonical.TxActionFee},
		{"stolen", "Stolen", "", false, canonical.TxKindOther, canonical.TxActionLost},
		{"donation", "Donation", "", false, canonical.TxKindTransferOut, canonical.TxActionGift},
	}
	for _, c := range cases {
		r, ok := silver.Classify(kindTable, silver.KindRow{Verb: c.typ, Text: c.comment, Cash: c.cash})
		if !ok || r.Kind != c.kind || r.Action != c.action {
			t.Errorf("%s: %v %q %q, want %q %q", c.name, ok, r.Kind, r.Action, c.kind, c.action)
		}
	}
	if r, ok := silver.Classify(kindTable, silver.KindRow{Verb: "Margin Profit"}); ok {
		t.Errorf("an unknown type matched %+v", r)
	}
}

// A fee is a third-currency fee when it is in neither of the trade's
// currencies, in any case, and not zero.
func TestThirdCurrencyFee(t *testing.T) {
	amt := func(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
	cases := []struct {
		amount, fee, buy, sell string
		want                   bool
	}{
		{"1", "BNB", "BTC", "USD", true},
		{"1", "bnb", "BTC", "USD", true},
		{"1", "USD", "BTC", "USD", false},
		{"1", "btc", "BTC", "USD", false},
		{"0", "BNB", "BTC", "USD", false},
		{"", "BNB", "BTC", "USD", false},
		{"1", "", "BTC", "USD", false},
	}
	for _, c := range cases {
		if got := thirdCurrencyFee(amt(c.amount), c.fee, c.buy, c.sell); got != c.want {
			t.Errorf("%+v: %v, want %v", c, got, c.want)
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
