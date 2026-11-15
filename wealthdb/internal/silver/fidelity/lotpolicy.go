package fidelity

import (
	"regexp"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// lotPolicy is the lot policy of the fidelity kind, which also reads
// the svb statements (docs/LOTS.md §6). Fees unknown: the statements do
// not say whether commission is in an amount. Dated by settlement: a
// row's date is its settlement or effective date. What a row does to the
// lots, where its kind does not say, is its action (lotAction).
func lotPolicy() *lots.Policy {
	p := lots.TradingPolicy()
	p.DatedBySettlement = true
	return &p
}

// optionLeg matches the statements' option legs, "PUT (XYZ)" and "CALL
// (XYZ)", which name the underlying and nothing of the contract, so the
// adapter keys them on the stock.
var optionLeg = regexp.MustCompile(`\b(PUT|CALL) \(`)

// lotAction is what a row of kind with narrative description does to
// the lots of its instrument where its kind does not say it. An option
// leg is no event of the stock it is keyed on, whatever its kind: its
// contracts are not shares, and its cost is not theirs. A corporate
// action reads by the action its narrative opens with. Mergers,
// conversions, name changes, reverse splits, tenders for new shares and
// distributions of shares all pair their legs.
func lotAction(kind canonical.TxKind, description string) canonical.TxAction {
	d := strings.ToUpper(description)
	switch {
	case optionLeg.MatchString(d):
		return canonical.TxActionIgnore
	case kind != canonical.TxKindCorporateAction:
		return ""
	case strings.HasPrefix(d, "RETURN OF CAPITAL"):
		return canonical.TxActionReturnOfCapital
	case strings.HasPrefix(d, "IN LIEU OF"):
		return canonical.TxActionCashInLieu
	case strings.HasPrefix(d, "EXPIRED"):
		return canonical.TxActionExpiry
	}
	return canonical.TxActionReorganisation
}
