package angellist

import (
	"context"
	"database/sql"
	"sort"

	"github.com/shopspring/decimal"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Money moved between the funding wallet and a position counts at cost
// until the next mark. The portal marks a position on its K-1 or fund
// statements and on the dumps that state a value, and a mark can follow
// a capital call by months. The wallet's cash moves on the ledger's own
// day (fundingCash), so a position whose value waited for its mark would
// read every call as a loss and every payout as a gain until then. So a
// position's value is its last mark plus what the ledger moved into it
// after the mark's day, or, before any mark, what the ledger moved in.
// What moved in is its investments less its refunds and its payouts,
// and the value never goes below zero. A mark as of a day covers the
// money moved on that UTC day, whatever its time, and the next mark
// replaces the sum. A position is held from its first payment where that
// comes before its first event, and a deal the ledger alone names is held
// while what it moved in is above zero. Each movement shows at the end of
// its ledger day, the instant the wallet's cash for that day is stamped
// at, so a read inside a day sees both sides of a payment or neither. The
// rule is an adapter calculation: silver keeps the events as the portal
// states them, and needs its funding ledger linked to positions (silver
// migration 0007).

// The valuation_basis values of a position carried at cost, in its payload.
const (
	// basisCost: the value is cost. Before any mark it is what the ledger
	// moved in or the cost the event states, and a mark worth exactly its
	// nonzero book value says cost too (emit). Silver states the same word
	// on an event valued at cost.
	basisCost = "cost"
	// basisMarkPlusCost: the value is the last mark plus what moved in
	// since, which rides beside it as net_paid_in_since_mark.
	basisMarkPlusCost = "mark_plus_cost"
)

// ledgerPaidInBasis stamps a book value the funding ledger derives where
// the portal states none: the capital paid in, less what was refunded.
var ledgerPaidInBasis = canonical.Basis{
	Origin: canonical.BasisDerived, Method: canonical.BasisMethodPaidIn, Fees: canonical.BasisFeesIncluded,
}

// payment is money the ledger moved into a position: positive for capital
// paid in, negative for a refund or a payout. capital says it is an
// investment or a refund, which the paid-in book value counts; a payout
// returns money without changing what was paid in.
type payment struct {
	at, day int64
	amount  decimal.Decimal
	capital bool
}

// mark is a state that marks its position: its date, value and the
// capital it states contributed.
type mark struct {
	at          int64
	value       decimal.Decimal
	contributed sql.NullInt64
}

// costCarry values the positions from their marks and the ledger.
type costCarry struct {
	// marks are each position's marks, oldest first.
	marks map[string][]mark
	// paid are each position's payments, oldest first. A position with
	// none is valued by its events alone.
	paid map[string][]payment
	// dayEnd is the last ledger row's instant on each UTC day the ledger
	// moves money.
	dayEnd map[int64]int64
}

// isMark says whether a state marks its position: it states a value on a
// basis other than cost.
func isMark(s state) bool {
	return s.market.Valid && s.basis != basisCost
}

func utcDay(t int64) int64 { return t / 86400 }

// newCostCarry pairs the ledger's payments and day ends with the marks
// read out of states.
func newCostCarry(paid map[string][]payment, dayEnd map[int64]int64) *costCarry {
	return &costCarry{marks: map[string][]mark{}, paid: paid, dayEnd: dayEnd}
}

// readMarks reads the marks out of states.
func (cc *costCarry) readMarks(states []silver.Holding) {
	for _, h := range states {
		if s := h.Row.(state); isMark(s) {
			cc.marks[h.Key] = append(cc.marks[h.Key], mark{h.At, decimal.New(s.market.Int64, -2), s.contributed})
		}
	}
	for _, m := range cc.marks {
		sort.SliceStable(m, func(i, j int) bool { return m[i].at < m[j].at })
	}
}

// shown is the instant a payment shows at: the end of its ledger day.
func (cc *costCarry) shown(p payment) int64 {
	if end, ok := cc.dayEnd[p.day]; ok && end > p.at {
		return end
	}
	return p.at
}

// value is a held position's value at t, its latest event being h: the
// value, the day it is valued at, and what its payload states about it.
// A value from a mark is valued at the mark's day: money moved in since
// is cost, not a fresh mark. A value at cost alone is valued at the first
// payment, the day the cost is from, or before any payment at the event's.
// A position the ledger moves nothing into is valued by its events: its
// last mark, plus the change in the capital its later event states
// contributed where both state one, else the cost its event states.
func (cc *costCarry) value(h silver.Holding, t int64) (*canonical.Decimal, int64, map[string]any) {
	s := h.Row.(state)
	m, marked := cc.lastMark(h.Key, t)
	paid, ok := cc.paid[h.Key]
	if !ok {
		switch {
		case isMark(s):
			return minorPtr(s.market), h.At, nil
		case marked:
			if !s.contributed.Valid || !m.contributed.Valid || s.contributed.Int64 == m.contributed.Int64 {
				return &m.value, m.at, nil
			}
			since := decimal.New(s.contributed.Int64-m.contributed.Int64, -2)
			return floorZero(m.value.Add(since)), m.at, markPlusCost(since)
		}
		v := minorPtr(s.market)
		if v == nil {
			v = minorPtr(s.contributed)
		}
		if v == nil {
			return nil, h.At, nil
		}
		return v, h.At, map[string]any{"valuation_basis": basisCost}
	}
	var since decimal.Decimal
	for _, p := range paid {
		if p.at <= t && (!marked || p.day > utcDay(m.at)) {
			since = since.Add(p.amount)
		}
	}
	switch {
	case !marked:
		valuedAt := h.At
		if paid[0].at <= t {
			valuedAt = paid[0].at
		}
		return floorZero(since), valuedAt, map[string]any{"valuation_basis": basisCost}
	case since.IsZero():
		return &m.value, m.at, nil
	}
	return floorZero(m.value.Add(since)), m.at, markPlusCost(since)
}

// paidIn is the capital the ledger paid into the position by t, less
// refunds: its book value where the portal states none. Nil without a
// payment by then.
func (cc *costCarry) paidIn(key string, t int64) *canonical.Decimal {
	var sum decimal.Decimal
	seen := false
	for _, p := range cc.paid[key] {
		if p.capital && p.at <= t {
			sum, seen = sum.Add(p.amount), true
		}
	}
	if !seen {
		return nil
	}
	return floorZero(sum)
}

func markPlusCost(since decimal.Decimal) map[string]any {
	return map[string]any{
		"valuation_basis":        basisMarkPlusCost,
		"net_paid_in_since_mark": since.String(),
	}
}

func floorZero(v decimal.Decimal) *canonical.Decimal {
	if v.IsNegative() {
		v = decimal.Decimal{}
	}
	return &v
}

// lastMark is the position's latest mark on or before t.
func (cc *costCarry) lastMark(key string, t int64) (mark, bool) {
	m := cc.marks[key]
	i := sort.Search(len(m), func(i int) bool { return m[i].at > t })
	if i == 0 {
		return mark{}, false
	}
	return m[i-1], true
}

// dates are the instants a carried value changes: each ledger movement into
// or out of a position held then, at the end of its ledger day. They are
// snapshot times, so the money shows in the value on the day it leaves or
// reaches the wallet.
func (cc *costCarry) dates(states []silver.Holding, removals silver.Removals) []int64 {
	byKey := map[string][]silver.Holding{}
	for _, h := range states {
		byKey[h.Key] = append(byKey[h.Key], h)
	}
	var out []int64
	for key, paid := range cc.paid {
		hs := byKey[key]
		for _, p := range paid {
			at := cc.shown(p)
			// The latest state on or before the instant: states are in
			// replacement order, which for one position is date order.
			var latest *silver.Holding
			for i := range hs {
				if hs[i].At <= at {
					latest = &hs[i]
				}
			}
			if latest != nil && latest.Open && !removals.Removed(key, at) {
				out = append(out, at)
			}
		}
	}
	return out
}

// withLedger adds the states the ledger implies to states, which are in
// position and date order.
//   - A position whose first payment comes before its first event opens at
//     that payment's day end, at cost.
//   - A deal the ledger names but no event does is held at cost while what
//     it moved in is above zero, from the end of the day that takes it
//     there, and leaves at the end of the day that takes it to zero or
//     below: a commitment refunded in full holds nothing after its refund.
//
// deals holds what such a deal's states read from its offering.
func (cc *costCarry) withLedger(states []silver.Holding, deals map[string]state) []silver.Holding {
	out := make([]silver.Holding, 0, len(states)+2*len(deals))
	for i := 0; i < len(states); {
		key, first := states[i].Key, states[i]
		if p := cc.paid[key]; len(p) > 0 && cc.shown(p[0]) < first.At && first.Open {
			s := first.Row.(state)
			s.market, s.basis, s.contributed, s.taxBasis = sql.NullInt64{}, basisCost, sql.NullInt64{}, sql.NullInt64{}
			out = append(out, silver.Holding{Key: key, At: cc.shown(p[0]), Open: true, Row: s})
		}
		for ; i < len(states) && states[i].Key == key; i++ {
			out = append(out, states[i])
		}
	}
	keys := make([]string, 0, len(deals))
	for key := range deals {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		var net decimal.Decimal
		open := false
		paid := cc.paid[key]
		for i := 0; i < len(paid); {
			day := paid[i].day
			at := cc.shown(paid[i])
			for ; i < len(paid) && paid[i].day == day; i++ {
				net = net.Add(paid[i].amount)
			}
			if held := net.IsPositive(); held != open {
				out = append(out, silver.Holding{Key: key, At: at, Open: held, Row: deals[key]})
				open = held
			}
		}
	}
	return out
}

// ledgerTypes are the funding-ledger types that move money between the
// wallet and a position: an investment pays capital in, a refund and a
// disbursement pay money back.
const ledgerTypes = `('investment', 'refund', 'disbursement')`

// readLedger reads what the funding ledger moved into each position, and
// the last row's instant on each day the ledger moves money. The ledger
// signs a movement from the wallet's side, so money into the position is
// the negated amount. A silver whose ledger names no position has no
// payments.
func (c *Connection) readLedger(ctx context.Context) (map[string][]payment, map[int64]int64, error) {
	paid := map[string][]payment{}
	dayEnd := map[int64]int64{}
	if !c.schema.fundingTransactions {
		return paid, dayEnd, nil
	}
	err := silver.ScanRows(ctx, c.db, "ledgerDays", `
SELECT occurred_at / 86400, MAX(occurred_at) FROM funding_transactions GROUP BY 1`, func(rows *sql.Rows) error {
		var day, end int64
		if err := rows.Scan(&day, &end); err != nil {
			return err
		}
		dayEnd[day] = end
		return nil
	})
	if err != nil || !c.schema.fundingLinks {
		return paid, dayEnd, err
	}
	q := `
SELECT position_external_id, occurred_at, amount_minor, type <> 'disbursement'
  FROM funding_transactions
 WHERE type IN ` + ledgerTypes + `
   AND position_external_id IS NOT NULL AND position_external_id <> ''
   AND amount_minor IS NOT NULL
 ORDER BY position_external_id, occurred_at, transaction_external_id`
	err = silver.ScanRows(ctx, c.db, "paidIn", q, func(rows *sql.Rows) error {
		var (
			key        string
			at, amount int64
			capital    bool
		)
		if err := rows.Scan(&key, &at, &amount, &capital); err != nil {
			return err
		}
		paid[key] = append(paid[key], payment{at: at, day: utcDay(at), amount: decimal.New(-amount, -2), capital: capital})
		return nil
	})
	return paid, dayEnd, err
}

// ledgerDeals are the offerings the funding ledger moves money into that no
// position event names, each with what its states read from the offering.
func (c *Connection) ledgerDeals(ctx context.Context) (map[string]state, error) {
	out := map[string]state{}
	if !c.schema.fundingLinks {
		return out, nil
	}
	q := `
SELECT o.position_external_id, COALESCE(o.currency, 'USD'), COALESCE(o.kind, ''),
       COALESCE(o.company_name, ''), o.investment_date, COALESCE(o.payload, '')
  FROM offerings o
 WHERE o.position_external_id NOT IN (SELECT position_external_id FROM position_snapshots)
   AND EXISTS (SELECT 1 FROM funding_transactions ft
                WHERE ft.position_external_id = o.position_external_id
                  AND ft.type IN ` + ledgerTypes + ` AND ft.amount_minor IS NOT NULL)`
	err := silver.ScanRows(ctx, c.db, "ledgerDeals", q, func(rows *sql.Rows) error {
		var (
			key string
			s   = state{basis: basisCost}
		)
		if err := rows.Scan(&key, &s.currency, &s.kind, &s.company, &s.invested, &s.payload); err != nil {
			return err
		}
		out[key] = s
		return nil
	})
	return out, err
}
