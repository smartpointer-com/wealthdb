package fidelity

import (
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// init registers the lot policy of the fidelity kind, which also reads
// the svb statements (docs/LOTS.md §6). Fees unknown: the statements do
// not say whether commission is in an amount. Dated by settlement: a
// row's date is its settlement or effective date. A corporate action
// reads by the action its description opens with.
func init() {
	p := lots.TradingPolicy()
	p.DatedBySettlement = true
	p.Classify = lots.ClassifyCorporate(corporateRule)
	lots.RegisterPolicy(kindName, p)
}

// corporateRule maps the action a corporate action's description opens
// with. Mergers, conversions, name changes, reverse splits, tenders for
// new shares and distributions of shares all pair their legs
// (CorpReorg).
func corporateRule(t lots.Txn) lots.CorporateRule {
	d := strings.ToUpper(t.Description)
	switch {
	case strings.HasPrefix(d, "RETURN OF CAPITAL"):
		return lots.CorpReturnOfCapital
	case strings.HasPrefix(d, "IN LIEU OF"):
		return lots.CorpCashInLieu
	case strings.HasPrefix(d, "EXPIRED"):
		return lots.CorpExpiry
	}
	return lots.CorpReorg
}
