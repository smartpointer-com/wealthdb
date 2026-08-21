package equityzen

import (
	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/returns"
)

// init registers this source's co-located ReturnsPolicy. The cash-flow ledger
// is complete double-entry on the custody account (transactions.go): the
// deposit/withdrawal legs are real dated cash crossings of the EquityZen
// boundary, so they count as external capital, while
// buy/sell/contribution/distribution are the internal halves of those pairs
// and must NOT count — marking both legs external would cancel every event to
// a net-0 flow. No transfer-like set: deposit/withdrawal never participate in
// netting. CapitalCallRisk keeps the nav_only_capital_call_risk tag on any
// window that observes no flows (a positions-only silver), so value-growth
// returns stay flagged when the ledger is absent. ClosureLedgerExact: an
// exit's withdrawal legs are the realized proceeds, dated on the exit day
// itself (the adapter emits a zero snapshot there), so a full-portfolio
// closure books the real proceeds and the realized-vs-last-mark delta shows
// as return.
func init() {
	p := returns.DefaultReturnsPolicy(returns.NewFlowPolicy(
		returns.RegimeFlowComplete,
		[]canonical.TxKind{canonical.TxKindDeposit, canonical.TxKindWithdrawal},
		nil,
	))
	p.CapitalCallRisk = true
	p.ClosureScope = returns.ClosureLedgerExact
	returns.RegisterPolicy(kindName, p)
}
