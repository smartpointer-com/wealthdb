package plaid

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// holdingBasis stamps a holding's book value: the cost basis Plaid
// passes on from the institution, which says neither how it was
// computed nor whether fees are in it (docs/DESIGN.md §7.4).
var holdingBasis = canonical.Basis{
	Origin: canonical.BasisStated, Method: canonical.BasisMethodUnknown, Fees: canonical.BasisFeesUnknown,
}

// taxLot is one element of a holding's `tax_lots`, as Plaid states it:
// the figures gold reads, and the element itself for the lot's payload.
// A short lot states a negative quantity.
type taxLot struct {
	Quantity     *canonical.Decimal `json:"quantity"`
	CostBasis    *canonical.Decimal `json:"cost_basis"`
	CurrentValue *canonical.Decimal `json:"current_value"`
	Purchased    string             `json:"original_purchase_datetime"`
	raw          json.RawMessage
}

// parseTaxLots reads silver's `tax_lots`, a JSON array as Plaid sends it.
// An empty array states no lots.
func parseTaxLots(s string) ([]taxLot, error) {
	if s == "" {
		return nil, nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal([]byte(s), &elems); err != nil {
		return nil, fmt.Errorf("tax_lots: %w", err)
	}
	lots := make([]taxLot, len(elems))
	for i, e := range elems {
		if err := json.Unmarshal(e, &lots[i]); err != nil {
			return nil, fmt.Errorf("tax_lots[%d]: %w", i, err)
		}
		lots[i].raw = e
	}
	return lots, nil
}

// purchaseDate is the lot's purchase day as Plaid writes it, at UTC
// midnight, or nil where it states none it can be read as.
func (l taxLot) purchaseDate() *time.Time {
	for _, layout := range []string{time.RFC3339, time.DateOnly} {
		if t, err := time.Parse(layout, l.Purchased); err == nil {
			d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
			return &d
		}
	}
	return nil
}

// positionLots are the open lots of pos, one per tax lot of its
// holdings, keyed by their order (1, 2, ...). Each lot's cost is
// Plaid's, stated; Plaid states no term or coverage.
func positionLots(pos canonical.PositionChange, lots []taxLot) []canonical.PositionLotChange {
	out := make([]canonical.PositionLotChange, len(lots))
	for i, l := range lots {
		lot := canonical.PositionLotChange{
			SnapshotAt:           pos.SnapshotAt,
			AccountExternalID:    pos.AccountExternalID,
			PositionKey:          pos.PositionKey,
			LotKey:               strconv.Itoa(i + 1),
			InstrumentExternalID: pos.InstrumentExternalID,
			Currency:             pos.Currency,
			Quantity:             l.Quantity,
			BookValue:            l.CostBasis,
			MarketValue:          l.CurrentValue,
			AcquisitionDate:      l.purchaseDate(),
			Payload:              l.raw,
		}
		if l.CostBasis != nil {
			lot.BasisOrigin = canonical.BasisStated
		}
		out[i] = lot
	}
	return out
}

// earliestAcquisition is the earliest acquisition date among lots, or
// nil where none states one.
func earliestAcquisition(lots []canonical.PositionLotChange) *time.Time {
	var first *time.Time
	for _, l := range lots {
		if l.AcquisitionDate != nil && (first == nil || l.AcquisitionDate.Before(*first)) {
			first = l.AcquisitionDate
		}
	}
	return first
}
