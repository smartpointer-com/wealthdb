package synthetic

import "github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"

// init registers this kind's co-located ReturnsPolicy: flow_complete on the
// shared bank external and transfer-like sets, every other knob at its
// default.
//
// A generic kind cannot know which institution a source models — one
// synthetic silver may stand for a bank, the next for a broker, a crypto
// wallet or a book of private holdings — so it registers the bank-style
// default, and a deployment states each source's own regime with config
// `returns_policy_overrides`: `flow_regime` for a source whose ledger is not
// flow-complete (nav_only, crypto_partial) and `accounts_grain` for one whose
// accounts should not surface as rows of their own (docs/DESIGN.md §5.6).
func init() {
	returns.RegisterPolicy(kindName, returns.DefaultReturnsPolicy(returns.BankFlowPolicy()))
}
