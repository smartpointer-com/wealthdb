package gold

import (
	"context"
	"database/sql"
	"fmt"
	"math"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// lotAux is what an event's value still needs once every key's
// currency is known.
type lotAux struct {
	ccy    string  // the currency Value is in
	fmv    bool    // Value is to be the market value of Qty
	fee    float64 // a third-currency fee to value
	feeCcy string
}

func (f *lotFeed) readRates(ctx context.Context, db *sql.DB) error {
	r, err := loadLotRates(ctx, db)
	f.rates = r
	return err
}

// resolveValues settles every value that needed the keys' currencies:
// a market value, a third-currency fee, an amount in another currency.
// A value that has no rate is unknown, and a fee without one is a
// finding.
func (f *lotFeed) resolveValues() {
	for i := range f.events {
		ev, aux := &f.events[i], f.aux[i]
		info := f.info[ev.Key]
		ccy := info.currency
		day := int64(lots.DayOf(ev.At))
		if aux.fmv {
			if r, ok := f.rates.rate(info.inst, ccy, day); ok && ccy != "" {
				ev.Value, ev.HasValue = ev.Qty*r, true
			} else {
				ev.HasValue = false
				if ev.Kind == lots.Acquire {
					ev.CostOrigin = lots.CostNone
				}
			}
		} else if ev.HasValue && aux.ccy != "" && ccy != "" && aux.ccy != ccy {
			if r, ok := f.rates.rate(aux.ccy, ccy, day); ok {
				ev.Value *= r
			} else {
				ev.HasValue = false
			}
		}
		if aux.fee > 0 {
			r, ok := f.rates.rate(aux.feeCcy, ccy, day)
			switch {
			case !ok || ccy == "":
				f.findings = append(f.findings, lots.Finding{Kind: lots.FindFeeUnvalued, Key: ev.Key, Account: ev.Account, At: ev.At, Qty: aux.fee, Txn: ev.Txn})
			case ev.Kind == lots.Acquire && ev.HasValue:
				ev.Value += aux.fee * r
			case ev.Kind == lots.Dispose && ev.HasValue:
				ev.Value = math.Max(ev.Value-aux.fee*r, 0)
			}
		}
	}
	f.aux = nil
}

// lotRateMaxAge is how many days a rate may be older than the day it
// prices: a coin whose last quote is months old has no market value
// that day, it has an unknown one.
const lotRateMaxAge = 7

// lotRateBridges are the currencies a cross rate goes through, in the
// order fx_rates_into tries them.
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
