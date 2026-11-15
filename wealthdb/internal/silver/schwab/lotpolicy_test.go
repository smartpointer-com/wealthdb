package schwab

import (
	"encoding/json"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

func TestCorporateActionReadsTheActionThenTheDescription(t *testing.T) {
	cases := []struct {
		action, desc string
		want         canonical.TxAction
	}{
		{"Return Of Capital", "EXAMPLE CORP", canonical.TxActionReturnOfCapital},
		{"Unissued Rights Redemption", "EXAMPLE CORP", canonical.TxActionCashOnly},
		{"Litigation", "EXAMPLE CORP", canonical.TxActionCashOnly},
		{"", "Exchange CALL EXAMPLE CORP $10", canonical.TxActionCashOnly},
		{"", "Removed due to Expiration CALL EXAMPLE", canonical.TxActionExpiry},
		{"Reverse Split", "EXAMPLE CORP", canonical.TxActionReorganisation},
		{"", "EXAMPLE FUND FORWARD SPLIT WITH STOCK SPLIT SHARES", canonical.TxActionReorganisation},
	}
	for _, c := range cases {
		if got := lotAction(canonical.TxKindCorporateAction, "", c.action, c.desc); got != c.want {
			t.Errorf("%q %q: %q, want %q", c.action, c.desc, got, c.want)
		}
	}
}

// Cash in lieu is cash in lieu in both feeds: an api JOURNAL that pays it
// (kindFor's corporate_action) and the web feed's "Cash In Lieu" action.
// The web feed's other corporate actions keep their reading.
func TestCashInLieuReadsAlikeInBothFeeds(t *testing.T) {
	cases := []struct {
		typ, action, desc string
		want              canonical.TxAction
	}{
		{"JOURNAL", "", "Cash In Lieu EXAMPLE HOLDINGS INC", canonical.TxActionCashInLieu},
		{"JOURNAL", "", "CASH IN LIEU OF FRACTIONAL SHARES", canonical.TxActionCashInLieu},
		{"", "Cash In Lieu", "EXAMPLE HOLDINGS INC", canonical.TxActionCashInLieu},
		{"", "Reverse Split", "EXAMPLE HOLDINGS INC", canonical.TxActionReorganisation},
	}
	for _, c := range cases {
		if got := lotAction(canonical.TxKindCorporateAction, c.typ, c.action, c.desc); got != c.want {
			t.Errorf("%q %q %q: %q, want %q", c.typ, c.action, c.desc, got, c.want)
		}
	}
}

// A TRADE moving no cash is no event; every other row of a kind that
// says what it does is left to its kind.
func TestLotActionLeavesATradeWithoutCashAlone(t *testing.T) {
	cases := []struct {
		kind canonical.TxKind
		typ  string
		want canonical.TxAction
	}{
		{canonical.TxKindOther, "TRADE", canonical.TxActionIgnore},
		{canonical.TxKindSell, "TRADE", ""},
		{canonical.TxKindOther, "MEMORANDUM", ""},
		{canonical.TxKindCorporateAction, "RECEIVE_AND_DELIVER", canonical.TxActionReorganisation},
	}
	for _, c := range cases {
		if got := lotAction(c.kind, c.typ, "", ""); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.kind, c.typ, got, c.want)
		}
	}
}

// The payload's type and action read as the strings it states: the
// "Action" key over the "action" one, a null as no value.
func TestPayloadVerbs(t *testing.T) {
	cases := []struct{ payload, typ, action string }{
		{`{"type":"TRADE","netAmount":0}`, "TRADE", ""},
		{`{"Action":"Cash In Lieu","action":"Split"}`, "", "Cash In Lieu"},
		{`{"Action":null,"action":"Split"}`, "", "Split"},
		{`{"Action":"","action":"Split"}`, "", ""},
		{`{"action":"Split","type":7}`, "7", "Split"},
		{`not json`, "", ""},
		{``, "", ""},
	}
	for _, c := range cases {
		if typ, action := payloadVerbs(json.RawMessage(c.payload)); typ != c.typ || action != c.action {
			t.Errorf("%s: %q %q, want %q %q", c.payload, typ, action, c.typ, c.action)
		}
	}
}

// The readers state the action of the rows they emit: a TRADE that
// moves no cash, and a corporate action by its words.
func TestSetLotActions(t *testing.T) {
	desc := "Cash In Lieu EXAMPLE HOLDINGS INC"
	txs := []canonical.TransactionChange{
		{Kind: canonical.TxKindOther, Payload: json.RawMessage(`{"type":"TRADE"}`)},
		{Kind: canonical.TxKindCorporateAction, Description: &desc, Payload: json.RawMessage(`{"type":"JOURNAL"}`)},
		{Kind: canonical.TxKindBuy, Payload: json.RawMessage(`{"type":"TRADE"}`)},
	}
	setLotActions(txs)
	for i, want := range []canonical.TxAction{canonical.TxActionIgnore, canonical.TxActionCashInLieu, ""} {
		if txs[i].Action != want {
			t.Errorf("row %d: %q, want %q", i, txs[i].Action, want)
		}
	}
}
