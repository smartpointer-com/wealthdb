package carta

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// A fund reports its first NAV on its first capital-account statement, which
// can follow its first capital call by years. Until then the interest has no
// valuation of its own, and leaving it out of the portfolio would book every
// call as a loss in the period it was paid and the first NAV as a gain. So
// until a fund's first NAV the interest is carried at the capital paid in,
// less any capital paid back, and the first NAV then marks it. Its book value
// is the capital paid in, gross of any paid back, as on a NAV row.

// fundFlow is one fund cash event: positive for capital called, negative for
// capital distributed.
type fundFlow struct {
	at  int64
	net float64
}

// fundBook is one fund's cash events, oldest first, and the day of its first
// NAV.
type fundBook struct {
	ccy      string
	flows    []fundFlow
	firstNAV int64
	hasNAV   bool
}

// fundLedger maps a fund entity to its book.
type fundLedger map[int64]*fundBook

func (c *Connection) loadFundLedger(ctx context.Context) (fundLedger, error) {
	ledger := fundLedger{}
	rows, err := c.db.QueryContext(ctx, `
SELECT entity_external_id, flow_date, kind, amount, COALESCE(currency, 'USD')
  FROM cash_flows
 WHERE kind IN ('capital_call', 'distribution')
   AND flow_date IS NOT NULL AND amount IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("loadFundLedger: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			eid             int64
			date, kind, ccy string
			amount          float64
		)
		if err := rows.Scan(&eid, &date, &kind, &amount, &ccy); err != nil {
			return nil, err
		}
		at, ok := flowDateUnix(date)
		if !ok {
			continue
		}
		if kind == "distribution" {
			amount = -amount
		}
		b, ok := ledger[eid]
		if !ok {
			b = &fundBook{ccy: normCcy(ccy)}
			ledger[eid] = b
		}
		b.flows = append(b.flows, fundFlow{at: at, net: amount})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	navs, err := c.db.QueryContext(ctx,
		`SELECT entity_external_id, MIN(snapshot_at) FROM fund_metrics GROUP BY entity_external_id`)
	if err != nil {
		return nil, fmt.Errorf("loadFundLedger: %w", err)
	}
	defer navs.Close()
	for navs.Next() {
		var eid, at int64
		if err := navs.Scan(&eid, &at); err != nil {
			return nil, err
		}
		if b, ok := ledger[eid]; ok {
			b.firstNAV, b.hasNAV = at, true
		}
	}
	if err := navs.Err(); err != nil {
		return nil, err
	}
	for _, b := range ledger {
		sort.SliceStable(b.flows, func(i, j int) bool { return b.flows[i].at < b.flows[j].at })
	}
	return ledger, nil
}

// carryDates are the days a carried value changes: every fund cash event
// before that fund's first NAV.
func (l fundLedger) carryDates() []int64 {
	var out []int64
	for _, b := range l {
		for _, f := range b.flows {
			if !b.hasNAV || f.at < b.firstNAV {
				out = append(out, f.at)
			}
		}
	}
	return out
}

// carryAt is the fund's carried value at t, capital paid in less capital paid
// back on or before t, and its book value, the capital paid in alone. False
// once the fund has a NAV on or before t, and when nothing is carried.
func (b *fundBook) carryAt(t int64) (value, called float64, ok bool) {
	if b.hasNAV && b.firstNAV <= t {
		return 0, 0, false
	}
	for _, f := range b.flows {
		if f.at > t {
			break
		}
		value += f.net
		if f.net > 0 {
			called += f.net
		}
	}
	value = math.Round(value*100) / 100
	called = math.Round(called*100) / 100
	return value, called, value > 0
}

// acquisitionDate is the day of the fund's first capital call, the day the
// interest was acquired, or nil when the ledger holds no call for it.
func (l fundLedger) acquisitionDate(eid int64) *time.Time {
	b, ok := l[eid]
	if !ok {
		return nil
	}
	for _, f := range b.flows {
		if f.net > 0 {
			return silver.DatePtrFromNullUnix(sql.NullInt64{Int64: f.at, Valid: true})
		}
	}
	return nil
}

// appendFundCarryAt adds the carried position of every fund that has no NAV
// on or before t. It runs after appendFundAt, whose NAV positions it never
// duplicates.
func appendFundCarryAt(t int64, acct string, ledger fundLedger, batch *canonical.SnapshotBatch, active map[int64]string, classesNew map[int64]canonical.AssetClass, vehicles map[int64]canonical.Vehicle) error {
	ids := make([]int64, 0, len(ledger))
	for eid := range ledger {
		ids = append(ids, eid)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, eid := range ids {
		if _, placed := active[eid]; placed {
			continue
		}
		b := ledger[eid]
		v, called, ok := b.carryAt(t)
		if !ok {
			continue
		}
		carried, err := canonical.NewDecimalFromString(strconv.FormatFloat(v, 'f', 2, 64))
		if err != nil {
			return fmt.Errorf("appendFundCarryAt: %w", err)
		}
		book, err := canonical.NewDecimalFromString(strconv.FormatFloat(called, 'f', 2, 64))
		if err != nil {
			return fmt.Errorf("appendFundCarryAt: %w", err)
		}
		payload, err := json.Marshal(map[string]string{
			"valuation_basis": "called_capital",
			"called_capital":  carried.String(),
		})
		if err != nil {
			return err
		}
		instKey := instrumentID(eid)
		batch.Positions = append(batch.Positions, canonical.PositionChange{
			SnapshotAt:           t,
			AccountExternalID:    acct,
			PositionKey:          positionKey(eid),
			InstrumentExternalID: &instKey,
			AssetClass:           canonical.AssetClassPrivateEquity,
			Vehicle:              canonical.VehicleFund,
			Currency:             b.ccy,
			MarketValue:          &carried,
			BookValue:            &book,
			AcquisitionDate:      ledger.acquisitionDate(eid),
			Payload:              payload,
		})
		active[eid] = b.ccy
		classesNew[eid] = canonical.AssetClassPrivateEquity
		vehicles[eid] = canonical.VehicleFund
	}
	return nil
}
