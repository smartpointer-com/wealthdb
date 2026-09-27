package carta

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"

// init registers this source's co-located ReturnsPolicy. See
// returns.PrivateMarketLedgerPolicy for the double-entry custody-ledger
// rationale (transactions.go emits the pairs on the custody account).
func init() {
	returns.RegisterPolicy(kindName, returns.PrivateMarketLedgerPolicy())
}
