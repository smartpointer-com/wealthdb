package swissquote

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// syntheticTxID builds a stable transaction_external_id from the
// transaction's identifying tuple. Swissquote silver doesn't
// expose a stable per-event ID (Order # is shared across partial
// fills and is "00000000" for non-trade rows), so gold derives
// one. The hash is deterministic for the same inputs across
// reloads, which preserves identity under amendment-window
// replays.
//
// See docs/adapters/swissquote.md §2.
func syntheticTxID(
	account string,
	occurredAt int64,
	txType, symbol, currency string,
	netAmount canonical.Decimal,
) string {
	// Include all the columns that, taken together, uniquely
	// identify a row in the source CSV. If Swissquote ever ships
	// two indistinguishable rows that are nonetheless legitimate
	// distinct events, we'd collapse them — but in practice their
	// per-row content is unique enough.
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%s|%s|%s|%s",
		account, occurredAt, txType, symbol, currency, netAmount.String())
	return hex.EncodeToString(h.Sum(nil))[:32] // 128 bits is plenty at portfolio scale
}
