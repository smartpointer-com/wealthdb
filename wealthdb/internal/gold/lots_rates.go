package gold

import (
	"context"
	"database/sql"
	"fmt"
)

// lotRateMaxAge is how many days a rate may be older than the day it
// prices: a coin whose last quote is months old has no market value
// that day, it has an unknown one.
const lotRateMaxAge = 7

// lotRateBridges are the currencies a cross rate goes through, in the
// order fx_rates_into (0119) tries them.
var lotRateBridges = []string{"CHF", "USD"}

// lotRates prices a currency or a coin in another on a day, from
// fx_daily: directly, else through lotRateBridges in order, the order
// fx_rates_into applies. Unlike the reports' rates, a rate older than
// lotRateMaxAge prices nothing.
type lotRates struct {
	daily *DailyRates
}

func loadLotRates(ctx context.Context, db *sql.DB) (*lotRates, error) {
	d, err := LoadDailyRates(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("lots: %w", err)
	}
	return &lotRates{daily: d}, nil
}

func (r *lotRates) direct(from, to string, day int64) (float64, bool) {
	x, at, ok := r.daily.AtOrBefore(from, to, day)
	if !ok || day-at > lotRateMaxAge {
		return 0, false
	}
	return x, true
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
