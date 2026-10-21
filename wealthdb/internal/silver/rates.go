package silver

import (
	"context"
	"database/sql"
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
	b.RateObservations, err = readPage(ctx, s.db, s.w, RateBatchSize, "rates", `
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
