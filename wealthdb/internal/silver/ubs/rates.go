package ubs

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// The netbanking's interest conditions (ubs-web `account_rates`, filled by
// the collector's rates pass) reach gold on the cash balances the snapshot
// stream already emits, under the stated-rate keys every source writes
// (canonical.StatedRateKeys): one schedule under the payload's own keys,
// or under interest_conditions when a capture states schedules of
// several valid_from days. Only the credit side goes: it is what a
// positive balance earns. The debit side stays in silver. A balance whose
// PSN record states its own tiers keeps those: the yield pass reads the
// PSN shape there.
//
// A capture is dated by its download instant, and no balance is added for
// it. Its conditions go on the account's balances at the newest instant
// at or before the capture: the balance stated at the capture itself when
// there is one, else the one before it. A later capture that reaches the
// same instant replaces an earlier one's. An account no capture states a
// condition for gets no keys, which reads as no rate, not as zero.

// rateCapture is one capture's credit conditions for one account: one
// schedule per valid_from day, the days ascending. The netbanking states
// a tiered schedule as one row per tier, all of one day; rows of other
// days are other schedules (a change already announced, say), and the
// engine holds the one in force on each day.
type rateCapture struct {
	at        int64
	schedules []rateSchedule
}

// rateSchedule is one day's rate, its tiers in the rows' order; validFrom
// is the rows' day as silver states it, empty when they state none.
type rateSchedule struct {
	validFrom string
	rate      canonical.StatedRate
}

// creditRates reads every capture's credit conditions, per account, the
// captures in time order. A silver migrated before the table existed
// reads none.
func (r *webReader) creditRates(ctx context.Context) (map[string][]rateCapture, error) {
	if r == nil {
		return nil, nil
	}
	ok, err := r.hasTable(ctx, "account_rates")
	if err != nil || !ok {
		return nil, err
	}
	const q = `
SELECT snapshot_at, account_external_id, rate_pct, upper_limit, COALESCE(valid_from, '')
  FROM account_rates
 WHERE side = 'credit'
 ORDER BY account_external_id, snapshot_at, COALESCE(valid_from, ''), tier`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("ubs-web account_rates: %w", err)
	}
	defer rows.Close()
	out := map[string][]rateCapture{}
	for rows.Next() {
		var (
			at        int64
			account   string
			pct       float64
			upTo      sql.NullFloat64
			validFrom string
		)
		if err := rows.Scan(&at, &account, &pct, &upTo, &validFrom); err != nil {
			return nil, err
		}
		caps := out[account]
		if len(caps) == 0 || caps[len(caps)-1].at != at {
			caps = append(caps, rateCapture{at: at})
		}
		c := &caps[len(caps)-1]
		if len(c.schedules) == 0 || c.schedules[len(c.schedules)-1].validFrom != validFrom {
			sc := rateSchedule{validFrom: validFrom, rate: canonical.StatedRate{Side: canonical.RateSideCredit}}
			if validFrom != "" {
				day, err := time.Parse(time.DateOnly, validFrom)
				if err != nil {
					return nil, fmt.Errorf("ubs-web account_rates %s: valid_from: %w", account, err)
				}
				sc.rate.ValidFrom = &day
			}
			c.schedules = append(c.schedules, sc)
		}
		sc := &c.schedules[len(c.schedules)-1]
		tier := canonical.StatedTier{Pct: pct}
		if upTo.Valid {
			v := upTo.Float64
			tier.UpTo = &v
		}
		sc.rate.Tiers = append(sc.rate.Tiers, tier)
		out[account] = caps
	}
	return out, rows.Err()
}

// keys is the stated-rate keys for one capture: each schedule's tiers
// ascending by limit with the open tier last. A schedule of one row is a
// flat rate, whatever limit the row states.
func (c rateCapture) keys() map[string]any {
	rs := make([]canonical.StatedRate, len(c.schedules))
	for i, sc := range c.schedules {
		r := sc.rate
		if len(r.Tiers) == 1 {
			r.Tiers = []canonical.StatedTier{{Pct: r.Tiers[0].Pct}}
		} else {
			r.Tiers = slices.Clone(r.Tiers)
			slices.SortStableFunc(r.Tiers, func(a, b canonical.StatedTier) int {
				switch {
				case a.UpTo == nil && b.UpTo == nil:
					return 0
				case a.UpTo == nil:
					return 1
				case b.UpTo == nil:
					return -1
				}
				return cmp.Compare(*a.UpTo, *b.UpTo)
			})
		}
		rs[i] = r
	}
	return canonical.StatedRateKeys(rs...)
}

// stampRates puts each capture's keys on the balances it lands on (see
// the header). Balances are matched by account across every batch, so
// the stream is read whole first.
func stampRates(batches []canonical.SnapshotBatch, rates map[string][]rateCapture) error {
	type ref struct{ batch, row int }
	at := map[string]map[int64][]ref{}
	for bi := range batches {
		for ci, cb := range batches[bi].CashBalances {
			if _, ok := rates[cb.AccountExternalID]; !ok {
				continue
			}
			if at[cb.AccountExternalID] == nil {
				at[cb.AccountExternalID] = map[int64][]ref{}
			}
			at[cb.AccountExternalID][cb.SnapshotAt] = append(at[cb.AccountExternalID][cb.SnapshotAt], ref{bi, ci})
		}
	}
	for account, instants := range at {
		times := make([]int64, 0, len(instants))
		for t := range instants {
			times = append(times, t)
		}
		slices.Sort(times)
		landed := map[int64]rateCapture{}
		for _, c := range rates[account] {
			i, found := slices.BinarySearch(times, c.at)
			if !found {
				i--
			}
			if i >= 0 {
				landed[times[i]] = c
			}
		}
		for t, c := range landed {
			keys := c.keys()
			for _, rf := range instants[t] {
				cb := &batches[rf.batch].CashBalances[rf.row]
				payload, err := silver.WithStatedRates(cb.Payload, keys)
				if err != nil {
					return fmt.Errorf("ubs-web rates %s: %w", account, err)
				}
				cb.Payload = payload
			}
		}
	}
	return nil
}

// rateStampStream reads its inner stream whole on the first Next, stamps
// the rates and replays the batches. Every snapshot stream this adapter
// merges is already built in memory, so reading it whole holds nothing
// the streams did not.
type rateStampStream struct {
	inner   silver.SnapshotStream
	rates   map[string][]rateCapture
	batches []canonical.SnapshotBatch
	read    bool
	idx     int
}

func (s *rateStampStream) Next(ctx context.Context) (canonical.SnapshotBatch, bool, error) {
	if !s.read {
		for {
			b, more, err := s.inner.Next(ctx)
			if err != nil {
				return canonical.SnapshotBatch{}, false, err
			}
			s.batches = append(s.batches, b)
			if !more {
				break
			}
		}
		s.read = true
		if err := stampRates(s.batches, s.rates); err != nil {
			return canonical.SnapshotBatch{}, false, err
		}
	}
	if s.idx >= len(s.batches) {
		return canonical.SnapshotBatch{}, false, nil
	}
	b := s.batches[s.idx]
	s.idx++
	return b, s.idx < len(s.batches), nil
}

func (s *rateStampStream) Close() error { return s.inner.Close() }
