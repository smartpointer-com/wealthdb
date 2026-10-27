package gold

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

// DailyRates is fx_daily read whole: per currency pair, one rate per epoch
// day, day-ascending, each the price of one unit of `from` in `to`. The
// source priority is already applied (migration 0020), and each pair has
// a day-0 floor row (0023). It holds the data only; a caller sets its own
// policy on top (the lot engine's age limit and bridges, the matcher's
// direct pairs).
type DailyRates struct {
	pairs map[[2]string][]dayRate
}

type dayRate struct {
	day  int64
	rate float64
}

// LoadDailyRates reads fx_daily into memory.
func LoadDailyRates(ctx context.Context, db interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}) (*DailyRates, error) {
	rows, err := db.QueryContext(ctx, `SELECT from_ccy, to_ccy, day, rate FROM fx_daily ORDER BY 1, 2, 3`)
	if err != nil {
		return nil, fmt.Errorf("gold: read fx_daily: %w", err)
	}
	defer rows.Close()
	r := &DailyRates{pairs: map[[2]string][]dayRate{}}
	for rows.Next() {
		var from, to string
		var d dayRate
		if err := rows.Scan(&from, &to, &d.day, &d.rate); err != nil {
			return nil, fmt.Errorf("gold: read fx_daily: %w", err)
		}
		k := [2]string{from, to}
		r.pairs[k] = append(r.pairs[k], d)
	}
	return r, rows.Err()
}

// AtOrBefore is the direct pair's rate on the latest day at or before
// `day`, and that day, as the reports' ASOF join reads it. ok is false
// for a day before the pair's first or a pair fx_daily lacks; with the
// day-0 floor, only the second is reachable from a real day.
func (r *DailyRates) AtOrBefore(from, to string, day int64) (rate float64, rateDay int64, ok bool) {
	s := r.pairs[[2]string{from, to}]
	i := sort.Search(len(s), func(k int) bool { return s[k].day > day }) - 1
	if i < 0 {
		return 0, 0, false
	}
	return s[i].rate, s[i].day, true
}

// Direct is AtOrBefore without the rate's day: the shape of
// TransferMatchOpts.Rate.
func (r *DailyRates) Direct(from, to string, day int64) (float64, bool) {
	x, _, ok := r.AtOrBefore(from, to, day)
	return x, ok
}
