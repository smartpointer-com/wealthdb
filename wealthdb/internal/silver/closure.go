package silver

import (
	"encoding/json"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// ClosureMarkerPayload stamps each zero-valued closure row so the rows are
// self-explanatory in gold queries.
var ClosureMarkerPayload = json.RawMessage(`{"closure_marker": true}`)

// ClosureMarkerBatch builds the exit-day zero snapshot for a portfolio that
// fully empties at t — the closure mirror of a debut snapshot. An empty batch
// cannot express "everything exited": gold's history and as-of queries read
// only emitted rows, so without a marker the value spine and the holdings
// views carry the last pre-exit marks forward as phantom value. The marker
// replays the previous held snapshot's positions at zero value (plus their
// instrument rows, re-emitted so a windowed load satisfies the positions→
// instruments FK, and the custody account, so its seen-range extends to t).
//
// Callers emit it only on the held→empty transition — never before the first
// holding (a pre-inception zero would fabricate an earlier, zero-base
// inception) and never repeated across consecutive empty dates (the
// carry-forward spine holds the zero).
func ClosureMarkerBatch(prev canonical.SnapshotBatch, t int64, account canonical.AccountChange) canonical.SnapshotBatch {
	batch := canonical.SnapshotBatch{Accounts: []canonical.AccountChange{account}}
	for _, p := range prev.Positions {
		mv := canonical.NewDecimalFromInt(0)
		np := canonical.PositionChange{
			SnapshotAt:           t,
			AccountExternalID:    p.AccountExternalID,
			PositionKey:          p.PositionKey,
			InstrumentExternalID: p.InstrumentExternalID,
			AssetClass:           p.AssetClass,
			Vehicle:              p.Vehicle,
			Currency:             p.Currency,
			MarketValue:          &mv,
			Payload:              ClosureMarkerPayload,
		}
		if p.Quantity != nil {
			q := canonical.NewDecimalFromInt(0)
			np.Quantity = &q
		}
		batch.Positions = append(batch.Positions, np)
	}
	for _, i := range prev.Instruments {
		ni := i
		ni.FirstSeenAt = t
		ni.LastSeenAt = t
		batch.Instruments = append(batch.Instruments, ni)
	}
	return batch
}
