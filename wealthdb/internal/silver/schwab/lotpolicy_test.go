package schwab

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

func TestCorporateRuleReadsTheActionThenTheDescription(t *testing.T) {
	cases := []struct {
		action, desc string
		want         lots.CorporateRule
	}{
		{"Return Of Capital", "EXAMPLE CORP", lots.CorpReturnOfCapital},
		{"Unissued Rights Redemption", "EXAMPLE CORP", lots.CorpIgnore},
		{"Litigation", "EXAMPLE CORP", lots.CorpIgnore},
		{"", "Exchange CALL EXAMPLE CORP $10", lots.CorpIgnore},
		{"", "Removed due to Expiration CALL EXAMPLE", lots.CorpExpiry},
		{"Reverse Split", "EXAMPLE CORP", lots.CorpReorg},
		{"", "EXAMPLE FUND FORWARD SPLIT WITH STOCK SPLIT SHARES", lots.CorpReorg},
	}
	for _, c := range cases {
		if got := corporateRule(lots.Txn{Action: c.action, Description: c.desc}); got != c.want {
			t.Errorf("%q %q: %v, want %v", c.action, c.desc, got, c.want)
		}
	}
}

// Cash in lieu is cash in lieu in both feeds: an api JOURNAL that pays it
// (kindFor's corporate_action) and the web feed's "Cash In Lieu" action.
// The web feed's other corporate actions keep their reading.
func TestCashInLieuReadsAlikeInBothFeeds(t *testing.T) {
	cases := []struct {
		typ, action, desc string
		want              lots.CorporateRule
	}{
		{"JOURNAL", "", "Cash In Lieu EXAMPLE HOLDINGS INC", lots.CorpCashInLieu},
		{"JOURNAL", "", "CASH IN LIEU OF FRACTIONAL SHARES", lots.CorpCashInLieu},
		{"", "Cash In Lieu", "EXAMPLE HOLDINGS INC", lots.CorpCashInLieu},
		{"", "Reverse Split", "EXAMPLE HOLDINGS INC", lots.CorpReorg},
	}
	for _, c := range cases {
		got := corporateRule(lots.Txn{Type: c.typ, Action: c.action, Description: c.desc})
		if got != c.want {
			t.Errorf("%q %q %q: %v, want %v", c.typ, c.action, c.desc, got, c.want)
		}
	}
}

func TestClassifyLeavesATradeWithoutCashAlone(t *testing.T) {
	cases := []struct {
		kind, typ string
		qty       float64
		want      lots.Action
	}{
		{"other", "TRADE", 10, lots.Ignore},
		{"sell", "TRADE", -10, lots.Sell},
		{"other", "MEMORANDUM", 10, lots.In},
		{"corporate_action", "RECEIVE_AND_DELIVER", 10, lots.Corporate},
	}
	for _, c := range cases {
		if got := classify(lots.Txn{Kind: c.kind, Type: c.typ, Quantity: c.qty}).Action; got != c.want {
			t.Errorf("%s %s: %v, want %v", c.kind, c.typ, got, c.want)
		}
	}
}
