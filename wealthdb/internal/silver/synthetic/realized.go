package synthetic

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Realized lots. A row of `realized_lots` is one realized lot as one
// tax document states it, and becomes one canonical.RealizedLotChange
// column for column. The same sale in two documents is two rows; per
// account and tax year the best-ranked document kind present is
// primary (realizedOrder).

var _ silver.RealizedLotReader = (*Connection)(nil)

// realizedOrder ranks the document kinds for silver.MarkPrimary: the
// form that was reported first, then the summaries a broker prints of
// it, then the statements and trade confirmations.
var realizedOrder = []canonical.RealizedDocKind{
	canonical.RealizedForm1099B, canonical.RealizedYearEndSummary, canonical.RealizedGainLossReport,
	canonical.RealizedClosedPositions, canonical.RealizedStatement, canonical.RealizedTrade,
}

func realizedRank(k canonical.RealizedDocKind) int { return slices.Index(realizedOrder, k) }

// RealizedLots returns every realized lot the silver states, primaries
// marked. A lot's book value is its cost, stamped by its instrument's
// pair as a lot (basisFor). A document kind outside the vocabulary
// fails the load: gold has no catch-all kind to put the row under. A
// silver without the table states none.
func (c *Connection) RealizedLots(ctx context.Context) ([]canonical.RealizedLotChange, error) {
	ok, err := silver.HasTables(ctx, c.db, "realized_lots")
	if err != nil || !ok {
		return nil, err
	}
	pairs, err := c.instrumentPairs(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT realized_lot_id, account_id, instrument_id, description, document_kind, tax_year,
       acquisition_date, disposal_date, currency, quantity, proceeds, book_value,
       realized_gain_loss, term, payload
  FROM realized_lots
 ORDER BY realized_lot_id`)
	if err != nil {
		return nil, fmt.Errorf("synthetic realized lots: %w", err)
	}
	defer rows.Close()
	var out []canonical.RealizedLotChange
	for rows.Next() {
		var (
			id, account, kind, ccy, payload string
			taxYear                         int
			instrument, description         sql.NullString
			acquired, disposed, term        sql.NullString
			qty, proceeds, cost, gain       sql.NullString
		)
		if err := rows.Scan(&id, &account, &instrument, &description, &kind, &taxYear,
			&acquired, &disposed, &ccy, &qty, &proceeds, &cost, &gain, &term, &payload); err != nil {
			return nil, err
		}
		dk := canonical.RealizedDocKind(kind)
		if !dk.Valid() {
			return nil, fmt.Errorf("synthetic realized lot %s: document_kind %q", id, kind)
		}
		var extra annotations
		r := canonical.RealizedLotChange{
			RealizedLotExternalID: id,
			AccountExternalID:     account,
			InstrumentExternalID:  silver.StrPtrIfNonEmpty(instrument.String),
			Description:           silver.StrPtrIfNonEmpty(description.String),
			DocumentKind:          dk,
			TaxYear:               taxYear,
			AcquisitionDate:       calendarDate(acquired),
			DisposalDate:          calendarDate(disposed),
			Currency:              ccy,
			Quantity:              silver.AbsPtr(silver.DecimalPtrOrNil(qty)),
			Proceeds:              silver.AbsPtr(silver.DecimalPtrOrNil(proceeds)),
			RealizedGainLoss:      silver.DecimalPtrOrNil(gain),
			Term:                  lotTerm(term, &extra),
		}
		pair := pairs[instrument.String]
		r.SetBookValue(silver.AbsPtr(silver.DecimalPtrOrNil(cost)), basisFor(pair.ac, pair.veh, true))
		r.Payload = payloadWith(payload, extra)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	silver.MarkPrimary(out, realizedRank, nil)
	return out, nil
}

// pair is an instrument's (exposure, vehicle) pair.
type pair struct {
	ac  canonical.AssetClass
	veh canonical.Vehicle
}

// instrumentPairs reads each instrument's pair in its latest version,
// through the same guard as a position's.
func (c *Connection) instrumentPairs(ctx context.Context) (map[string]pair, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT instrument_id, asset_class, vehicle FROM instruments ORDER BY instrument_id, valid_from`)
	if err != nil {
		return nil, fmt.Errorf("synthetic instrument pairs: %w", err)
	}
	defer rows.Close()
	out := map[string]pair{}
	for rows.Next() {
		var id, assetClass, vehicle string
		if err := rows.Scan(&id, &assetClass, &vehicle); err != nil {
			return nil, err
		}
		var ignored annotations
		ac, veh := taxonomyPair(assetClass, vehicle, &ignored)
		out[id] = pair{ac, veh}
	}
	return out, rows.Err()
}
