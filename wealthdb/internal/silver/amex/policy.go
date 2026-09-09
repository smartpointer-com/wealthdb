package amex

import "github.com/ptu-gh/wealthdb/wealthdb/internal/returns"

// init registers this source's co-located ReturnsPolicy, beside the
// silver.Register(&Adapter{}) call. Every account this source emits is a
// card. See returns.CardIssuerPolicy for why a card contributes nothing to
// returns at any grain, and gold.returnsInvisibleKind for where they are
// dropped.
func init() {
	returns.RegisterPolicy(kindName, returns.CardIssuerPolicy())
}
