package silver

import (
	"regexp"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// A table reads top down: the first rule whose verb, text and condition
// a row meets holds, an empty verb or text matches every row, and a row
// that meets none is no match.
func TestClassifyTakesTheFirstRuleARowMeets(t *testing.T) {
	rules := []KindRule{
		{Verb: "Deposit", When: Cash, Kind: canonical.TxKindDeposit, Action: canonical.TxActionIn},
		{Verb: "Deposit", Kind: canonical.TxKindTransferIn, Action: canonical.TxActionIn},
		{Verb: "Expense", Text: regexp.MustCompile(`^dust$`), Action: canonical.TxActionConverted},
		{Verb: "Expense", Action: canonical.TxActionSpend},
		{Text: regexp.MustCompile(`LOST`), Kind: canonical.TxKindOther, Action: canonical.TxActionLost},
	}
	cases := []struct {
		row    KindRow
		ok     bool
		kind   canonical.TxKind
		action canonical.TxAction
	}{
		{KindRow{Verb: "Deposit", Cash: true}, true, canonical.TxKindDeposit, canonical.TxActionIn},
		{KindRow{Verb: "Deposit"}, true, canonical.TxKindTransferIn, canonical.TxActionIn},
		{KindRow{Verb: "Expense", Text: "dust"}, true, "", canonical.TxActionConverted},
		{KindRow{Verb: "Expense", Text: "dusty"}, true, "", canonical.TxActionSpend},
		{KindRow{Verb: "Anything", Text: "LOST KEY"}, true, canonical.TxKindOther, canonical.TxActionLost},
		{KindRow{Verb: "Anything"}, false, "", ""},
	}
	for _, c := range cases {
		r, ok := Classify(rules, c.row)
		if ok != c.ok || r.Kind != c.kind || r.Action != c.action {
			t.Errorf("%+v: %v %q %q, want %v %q %q", c.row, ok, r.Kind, r.Action, c.ok, c.kind, c.action)
		}
	}
}
