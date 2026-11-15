package swissquote

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
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
	// Include all the columns that, taken together, identify a row
	// in the source CSV. Two rows can still agree on all of them: an
	// export may list one movement twice. distinctTxIDs keeps those
	// apart.
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%s|%s|%s|%s",
		account, occurredAt, txType, symbol, currency, netAmount.String())
	return hex.EncodeToString(h.Sum(nil))[:32] // 128 bits is plenty at portfolio scale
}

// distinctTxIDs gives each transaction of txs that shares its id with
// an earlier one a suffix, `-2`, `-3`, …, so no two carry one id. A row
// whose id is its own keeps it unchanged, so every id already in gold
// stays. The copies of one id are numbered in the order of their
// payload, then their instrument, not in the order the silver returns
// them, so a reload numbers them alike. The order of txs stays.
func distinctTxIDs(txs []canonical.TransactionChange) {
	groups := map[string][]int{}
	for i := range txs {
		id := txs[i].TransactionExternalID
		groups[id] = append(groups[id], i)
	}
	for id, idx := range groups {
		if len(idx) < 2 {
			continue
		}
		content := func(i int) string {
			instrument := ""
			if p := txs[i].InstrumentExternalID; p != nil {
				instrument = *p
			}
			return string(txs[i].Payload) + "\x00" + instrument
		}
		order := slices.Clone(idx)
		slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(content(a), content(b)) })
		for n, i := range order[1:] {
			txs[i].TransactionExternalID = id + "-" + strconv.Itoa(n+2)
		}
	}
}
