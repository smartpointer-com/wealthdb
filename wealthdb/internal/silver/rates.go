package silver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The rates table: the published interest-rate series of the fred and
// curves collectors' silver (collectors/fred/migrations/0002_rates.sql,
// collectors/curves/migrations/0001_initial.sql), which the synthetic
// kind repeats column for column. One row per observation day and
// series: as_of is the day's UTC midnight, value a decimal string in
// percent. NewRateStream reads it into RateObservationChange records.

// RatesTable is the rates table's name. A reader checks that the file
// holds it before it streams the table.
const RatesTable = "rates"

// RateBatchSize bounds the rate observations one batch carries. It is a
// variable so a test can lower it.
var RateBatchSize = 2000

// rateStream emits the rate observations of a window in (as_of,
// series_id) order, a page at a time.
type rateStream struct {
	db *sql.DB
	w  canonical.Window
	// page's cursor is the (as_of, series_id) the last page ended on. It
	// starts below every as_of, since a series can reach back before 1970.
	page keyset
}

// NewRateStream streams the rates rows whose as_of lies in w. Nothing is
// read for a window without changes. The caller checks that the file
// holds the table. A value that is no decimal fails the load.
func NewRateStream(db *sql.DB, w canonical.Window) SnapshotStream {
	if !w.HasChanges {
		return NewSnapshotStream(nil)
	}
	return &rateStream{db: db, w: w, page: keyset{after: []any{int64(math.MinInt64), ""}}}
}

func (s *rateStream) Close() error { return nil }

func (s *rateStream) Next(ctx context.Context) (canonical.SnapshotBatch, bool, error) {
	var b canonical.SnapshotBatch
	if s.page.done {
		return b, false, nil
	}
	var err error
	b.RateObservations, err = readPage(ctx, s.db, canonical.Span{Start: s.w.Start, End: s.w.End}, RateBatchSize, "rates", `
SELECT as_of, series_id, value, payload
  FROM `+RatesTable+`
 WHERE as_of BETWEEN ? AND ? AND (as_of, series_id) > (?, ?)
 ORDER BY as_of, series_id LIMIT ?`, &s.page, scanRate)
	return b, !s.page.done, err
}

// scanRate reads one rates row into its record and its key.
func scanRate(rows *sql.Rows) (canonical.RateObservationChange, []any, error) {
	var (
		asOf          int64
		series, value string
		payload       sql.NullString
	)
	if err := rows.Scan(&asOf, &series, &value, &payload); err != nil {
		return canonical.RateObservationChange{}, nil, fmt.Errorf("rates scan: %w", err)
	}
	v, err := canonical.NewDecimalFromString(value)
	if err != nil {
		return canonical.RateObservationChange{}, nil, fmt.Errorf("rates (%d, %s): value %q: %w", asOf, series, value, err)
	}
	return canonical.RateObservationChange{
		SeriesID: series, AsOf: asOf, Value: v,
		Payload: JSONOrNil(payload),
	}, []any{asOf, series}, nil
}

// The fx_rates table: the fred collector's daily FX reference rates
// (collectors/fred/migrations/0001_initial.sql). One row per
// observation day and currency pair, already in the canonical
// convention: (base, quote, mid) means "1 quote = mid base".
// NewFxRateStream reads it into FxRateChange records.

// FxRatesTable is the fx_rates table's name.
const FxRatesTable = "fx_rates"

// FxBatchSize bounds the FX rates one batch carries, so a full-history
// re-emit (decades of a few pairs) builds no giant batch. A batch may
// span several days. It is a variable so a test can lower it.
var FxBatchSize = 2000

// fxStream emits the FX rates of a window in (snapshot_at, base, quote)
// order, a page at a time.
type fxStream struct {
	db   *sql.DB
	w    canonical.Window
	what string
	page keyset
}

// NewFxRateStream streams the fx_rates rows whose snapshot_at lies in w.
// Nothing is read for a window without changes. what prefixes the
// stream's errors. A mid that is no decimal fails the load.
func NewFxRateStream(db *sql.DB, w canonical.Window, what string) SnapshotStream {
	if !w.HasChanges {
		return NewSnapshotStream(nil)
	}
	return &fxStream{db: db, w: w, what: what, page: keyset{after: []any{int64(math.MinInt64), "", ""}}}
}

func (s *fxStream) Close() error { return nil }

func (s *fxStream) Next(ctx context.Context) (canonical.SnapshotBatch, bool, error) {
	var b canonical.SnapshotBatch
	if s.page.done {
		return b, false, nil
	}
	var err error
	b.FxRates, err = readPage(ctx, s.db, canonical.Span{Start: s.w.Start, End: s.w.End}, FxBatchSize, s.what+" Snapshots", `
SELECT snapshot_at, base_currency_iso, quote_currency_iso, mid, payload
  FROM `+FxRatesTable+`
 WHERE snapshot_at BETWEEN ? AND ?
   AND (snapshot_at, base_currency_iso, quote_currency_iso) > (?, ?, ?)
 ORDER BY snapshot_at, base_currency_iso, quote_currency_iso LIMIT ?`, &s.page, s.scan)
	return b, !s.page.done, err
}

// scan reads one fx_rates row into its record and its key.
func (s *fxStream) scan(rows *sql.Rows) (canonical.FxRateChange, []any, error) {
	var (
		snap                      int64
		base, quote, mid, payload string
	)
	if err := rows.Scan(&snap, &base, &quote, &mid, &payload); err != nil {
		return canonical.FxRateChange{}, nil, fmt.Errorf("%s Snapshots scan: %w", s.what, err)
	}
	rate, err := canonical.NewDecimalFromString(mid)
	if err != nil {
		return canonical.FxRateChange{}, nil, fmt.Errorf("%s fx_rates mid %q (snap=%d): %w", s.what, mid, snap, err)
	}
	return canonical.FxRateChange{
		SnapshotAt:    snap,
		BaseCurrency:  base,
		QuoteCurrency: quote,
		MidRate:       rate,
		Payload:       json.RawMessage(payload),
	}, []any{snap, base, quote}, nil
}
