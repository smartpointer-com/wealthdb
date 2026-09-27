package firstcitizens

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"

// init registers this source's co-located ReturnsPolicy, beside the
// silver.Register(&Adapter{}) call. See returns.DepositBankPolicy for the
// flow-complete + hidden-plumbing rationale.
func init() {
	returns.RegisterPolicy(kindName, returns.DepositBankPolicy())
}
