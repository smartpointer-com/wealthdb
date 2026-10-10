package schwab

import (
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// init registers schwab's lot policy (docs/LOTS.md §6): the statement
// lots are the anchors and the documents' realized lots the stated
// sales; the engine replays the history around them.
func init() {
	p := lots.TradingPolicy()
	p.Classify = classify
	lots.RegisterPolicy(kindName, p)
}

var classifyCorporate = lots.ClassifyCorporate(corporateRule)

// classify is the default reading with corporateRule, except that a
// TRADE moving no cash (kindFor's other) is no event: it restates a
// holding the account keeps.
func classify(t lots.Txn) lots.Classified {
	if t.Kind == string(canonical.TxKindOther) && t.Type == "TRADE" {
		return lots.Classified{Action: lots.Ignore}
	}
	return classifyCorporate(t)
}

// corporateRule maps a corporate action by its action, else its
// description. Cash-only actions (rights redemptions, litigation
// proceeds, option exchanges) move no lot.
func corporateRule(t lots.Txn) lots.CorporateRule {
	a, d := strings.ToLower(t.Action), strings.ToLower(t.Description)
	switch {
	case a == "return of capital":
		return lots.CorpReturnOfCapital
	case a == "unissued rights redemption", a == "litigation", strings.HasPrefix(d, "exchange "):
		return lots.CorpIgnore
	case strings.HasPrefix(d, "removed due to expiration"):
		return lots.CorpExpiry
	}
	return lots.CorpReorg
}
