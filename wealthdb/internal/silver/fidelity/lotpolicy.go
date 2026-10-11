package fidelity

import (
	"regexp"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// init registers the lot policy of the fidelity kind, which also reads
// the svb statements (docs/LOTS.md §6). Fees unknown: the statements do
// not say whether commission is in an amount. Dated by settlement: a
// row's date is its settlement or effective date. A corporate action
// reads by the action its description opens with. An option leg the
// statement books on the underlying's key moves no lot of the stock.
func init() {
	p := lots.TradingPolicy()
	p.DatedBySettlement = true
	p.Classify = classify
	lots.RegisterPolicy(kindName, p)
}

var classifyCorporate = lots.ClassifyCorporate(corporateRule)

// optionLeg matches the statements' option legs, "PUT (XYZ)" and "CALL
// (XYZ)", which name the underlying and nothing of the contract, so the
// adapter keys them on the stock.
var optionLeg = regexp.MustCompile(`\b(PUT|CALL) \(`)

// classify is the default reading with corporateRule, except that an
// option leg is no event of the stock it is keyed on: its contracts
// are not shares, and its cost is not theirs.
func classify(t lots.Txn) lots.Classified {
	if optionLeg.MatchString(strings.ToUpper(t.Description)) {
		return lots.Classified{Action: lots.Ignore}
	}
	return classifyCorporate(t)
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
