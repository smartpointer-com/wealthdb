package schwab

import (
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// init registers schwab's lot policy (docs/LOTS.md §6): the statement
// lots are the anchors and the documents' realized lots the stated
// sales; the engine replays the history around them.
func init() {
	p := lots.TradingPolicy()
	p.Classify = lots.ClassifyCorporate(corporateRule)
	lots.RegisterPolicy(kindName, p)
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
