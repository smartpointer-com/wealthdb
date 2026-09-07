package amex

import "github.com/ptu-gh/wealthdb/wealthdb/internal/returns"

// init registers this source's co-located ReturnsPolicy, beside the
// silver.Register(&Adapter{}) call.
//
// Every account this source emits is a card, and the returns engine drops
// `card` accounts at the loader (gold.returnsInvisibleKind), so no policy knob
// here can change any return figure. The registration exists because gold
// guards that every whitelisted silver kind declares a policy — an unregistered
// kind falls back to a conservative default and is reported as
// `unknown_adapter_policy` — and CardIssuerPolicy is the honest declaration:
// this source contributes nothing to returns at any grain.
func init() {
	returns.RegisterPolicy(kindName, returns.CardIssuerPolicy())
}
