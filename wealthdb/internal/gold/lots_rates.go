package gold

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

// lotRateMaxAge is how many days a rate may be older than the day it
// prices: a coin whose last quote is months old has no market value
// that day, it has an unknown one.
const lotRateMaxAge = 7

// lotRateBridges are the currencies a cross rate goes through, in the
// order fx_rates_to (0118) tries them.
var lotRateBridges = []string{"CHF", "USD"}

type dayRate struct {
	day  int64
	rate float64
}

// lotRates prices a currency or a coin in another on a day, from
// fx_daily: directly, else through lotRateBridges in order, the order
// fx_rates_to applies. Unlike the reports' rates, a rate older than
// lotRateMaxAge prices nothing.
type lotRates struct {
	pairs map[[2]string][]dayRate
}

func loadLotRates(ctx context.Context, db *sql.DB) (*lotRates, error) {
	rows, err := db.QueryContext(ctx, `SELECT from_ccy, to_ccy, day, rate FROM fx_daily ORDER BY from_ccy, to_ccy, day`)
	if err != nil {
		return nil, fmt.Errorf("lots: read fx_daily: %w", err)
	}
	defer rows.Close()
	r := &lotRates{pairs: map[[2]string][]dayRate{}}
	for rows.Next() {
		var from, to string
		var d dayRate
		if err := rows.Scan(&from, &to, &d.day, &d.rate); err != nil {
			return nil, fmt.Errorf("lots: read fx_daily: %w", err)
		}
		k := [2]string{from, to}
		r.pairs[k] = append(r.pairs[k], d)
	}
	return r, rows.Err()
}

func (r *lotRates) direct(from, to string, day int64) (float64, bool) {
	s := r.pairs[[2]string{from, to}]
	i := sort.Search(len(s), func(i int) bool { return s[i].day > day }) - 1
	if i < 0 || day-s[i].day > lotRateMaxAge {
		return 0, false
	}
	return s[i].rate, true
}

// rate is the price of one unit of from in to on day.
func (r *lotRates) rate(from, to string, day int64) (float64, bool) {
	if from == to {
		return 1, true
	}
	if x, ok := r.direct(from, to, day); ok {
		return x, true
	}
	for _, via := range lotRateBridges {
		if from == via || to == via {
			continue
		}
		a, ok1 := r.direct(from, via, day)
		b, ok2 := r.direct(via, to, day)
		if ok1 && ok2 {
			return a * b, true
		}
	}
	return 0, false
}
