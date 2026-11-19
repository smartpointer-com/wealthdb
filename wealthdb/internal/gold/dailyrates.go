package gold

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

// DailyRates is fx_daily read whole, or the pairs a caller reads: per
// currency pair, one rate per epoch day, day-ascending, each the price of
// one unit of `from` in `to`. The source priority is already applied
// (fx_norm), and each pair has a day-0 floor row (both in
// derived/10_fx.sql). It holds the data only; a caller sets its own policy
// on top (an age limit, Market; the lot engine's bridges; the matcher's
// direct pairs).
type DailyRates struct {
	pairs map[[2]string][]dayRate
}

type dayRate struct {
	day  int64
	rate float64
}

// marketRateMaxAge is how many days a rate may be older than the day it
// prices at market (DailyRates.Market): a coin whose last quote is months
// old has no market value that day, it has an unknown one. The lot
// engine's values and the coin rule's price read it alike.
const marketRateMaxAge = 7

// LoadDailyRates reads fx_daily into memory.
func LoadDailyRates(ctx context.Context, db rowQuerier) (*DailyRates, error) {
	return loadDailyRates(ctx, db, "")
}

// loadCoinUSD reads the USD rates of coins from fx_daily, for CoinUSD. Nil
// for no coins.
func loadCoinUSD(ctx context.Context, db rowQuerier, coins []string) (*DailyRates, error) {
	if len(coins) == 0 {
		return nil, nil
	}
	return loadDailyRates(ctx, db, "WHERE to_ccy = 'USD' AND list_contains(?, from_ccy)", coins)
}

// rowQuerier is what reads fx_daily: a database or a transaction.
type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// loadDailyRates reads the rows of fx_daily that where, a WHERE clause or
// "" for all, admits.
func loadDailyRates(ctx context.Context, db rowQuerier, where string, args ...any) (*DailyRates, error) {
	rows, err := db.QueryContext(ctx, `SELECT from_ccy, to_ccy, day, rate FROM fx_daily `+where+` ORDER BY 1, 2, 3`, args...)
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

// Market is the direct pair's rate at or before day, where it is at most
// marketRateMaxAge days older than day: a rate older than that, the day-0
// floor's included, prices nothing.
func (r *DailyRates) Market(from, to string, day int64) (float64, bool) {
	x, at, ok := r.AtOrBefore(from, to, day)
	if !ok || day-at > marketRateMaxAge {
		return 0, false
	}
	return x, true
}
