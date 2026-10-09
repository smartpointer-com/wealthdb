package schwab

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Open lots: the tax lots a statement prints under each holding
// (schwab-web `open_lots`, silver migration 0006). Statements of the
// 2020-2024 layout print them; the others print none. A lot is keyed
// like its holding in `historical_position_snapshots` plus its place in
// print order, and lands in gold's `position_lots` beside the holding's
// position row (docs/DESIGN.md §7.4).

// holdingKey names a statement holding the way silver keys it: period
// end, account suffix, instrument key.
type holdingKey struct {
	asOf          int64
	suffix        string
	instrumentKey string
}

// openLot is one printed lot line.
type openLot struct {
	index      int64
	quantity   sql.NullFloat64
	unitCost   sql.NullFloat64
	costBasis  sql.NullFloat64
	acquired   *time.Time
	unrealized sql.NullFloat64
	term       canonical.LotTerm
	footnotes  sql.NullString
	sha256     string
	payload    string
}

// openLotsInWindow reads the lots of every holding whose period end
// falls in the window, in print order. Empty on a silver without the
// table.
func (r *webReader) openLotsInWindow(ctx context.Context, w canonical.Window) (map[holdingKey][]openLot, error) {
	out := map[holdingKey][]openLot{}
	ok, err := silver.HasTables(ctx, r.db, "open_lots")
	if err != nil || !ok {
		return out, err
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT as_of_date, account_external_id, instrument_key, lot_index,
       quantity, unit_cost, cost_basis, acquired_date,
       unrealized_gain_loss, term, footnotes, source_sha256, payload
  FROM open_lots
 WHERE as_of_date BETWEEN ? AND ?
 ORDER BY as_of_date, account_external_id, instrument_key, lot_index`, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("schwab-web open lots: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			k              holdingKey
			l              openLot
			acquired, term sql.NullString
		)
		if err := rows.Scan(&k.asOf, &k.suffix, &k.instrumentKey, &l.index,
			&l.quantity, &l.unitCost, &l.costBasis, &acquired,
			&l.unrealized, &term, &l.footnotes, &l.sha256, &l.payload); err != nil {
			return nil, fmt.Errorf("schwab-web open lots scan: %w", err)
		}
		l.acquired = isoDate(acquired.String)
		l.term = lotTerm(term.String)
		out[k] = append(out[k], l)
	}
	return out, rows.Err()
}

// change projects the lot onto the holding's position row: same
// snapshot, account and position key. The book value is the printed
// cost basis, signed like the quantity (a short lot prints both
// negative); a lot whose basis Schwab does not know ("N/A") has none.
// Statements do not say whether a lot is covered.
func (l openLot) change(asOf int64, account, positionKey string) canonical.PositionLotChange {
	instrument := positionKey
	c := canonical.PositionLotChange{
		SnapshotAt:           asOf,
		AccountExternalID:    account,
		PositionKey:          positionKey,
		LotKey:               strconv.FormatInt(l.index, 10),
		InstrumentExternalID: &instrument,
		Currency:             "USD",
		Quantity:             silver.DecimalPtrFromNullFloat(l.quantity),
		BookValue:            silver.DecimalPtrFromNullFloat(l.costBasis),
		AcquisitionDate:      l.acquired,
		Term:                 l.term,
		SourceDocument:       silver.StrPtrIfNonEmpty(l.sha256),
		Payload:              l.payloadJSON(),
	}
	if c.BookValue != nil {
		c.BasisOrigin = canonical.BasisStated
	}
	return c
}

// payloadJSON is the silver lot payload (holding days, the raw line)
// with the printed figures gold has no column for: the endnote
// markers, the cost per share and the unrealized gain.
func (l openLot) payloadJSON() json.RawMessage {
	p := map[string]any{}
	_ = json.Unmarshal([]byte(l.payload), &p) // best-effort: the extras still land
	p["footnotes"] = nullable(l.footnotes.String, l.footnotes.Valid)
	p["unit_cost"] = nullable(l.unitCost.Float64, l.unitCost.Valid)
	p["unrealized_gain_loss"] = nullable(l.unrealized.Float64, l.unrealized.Valid)
	b, err := json.Marshal(p)
	if err != nil {
		return json.RawMessage(l.payload)
	}
	return b
}

func nullable[T any](v T, valid bool) any {
	if !valid {
		return nil
	}
	return v
}

// earliestAcquired is the earliest acquired date among a holding's
// lots, nil when none prints one.
func earliestAcquired(lots []openLot) *time.Time {
	var first *time.Time
	for _, l := range lots {
		if l.acquired != nil && (first == nil || l.acquired.Before(*first)) {
			first = l.acquired
		}
	}
	return first
}

// isoDate reads an ISO calendar date as UTC midnight; nil for anything
// else ("Various", an empty cell).
func isoDate(s string) *time.Time {
	t, err := time.Parse(time.DateOnly, strings.TrimSpace(s))
	if err != nil {
		return nil
	}
	return &t
}

// lotTerm maps the printed holding period onto gold's term. Any other
// label (a 1099-B can print one) states no term; it stays in the
// payload.
func lotTerm(s string) canonical.LotTerm {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "SHORT":
		return canonical.LotTermShort
	case "LONG":
		return canonical.LotTermLong
	}
	return ""
}
