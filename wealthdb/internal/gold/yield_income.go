package gold

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// incomeTypeSQL maps an income line to the projection's types, by its
// detailed type and the instrument it names (i, and tm, its terms in
// force at the window's end): interest a bond pays is a coupon, but a
// bill's or a zero's discount is interest; a dividend a fund pays is a
// distribution.
var incomeTypeSQL = fmt.Sprintf(`CASE
    WHEN l.income_detailed = 'INCOME_INTEREST_EARNED' AND i.vehicle = 'bond'
         AND NOT COALESCE(tm.coupon_kind = '%[7]s' OR tm.coupon_pct = 0, FALSE) THEN '%[1]s'
    WHEN l.income_detailed = 'INCOME_INTEREST_EARNED' THEN '%[2]s'
    WHEN l.income_detailed = 'INCOME_DIVIDENDS' AND i.vehicle IN ('etf', 'fund') THEN '%[3]s'
    WHEN l.income_detailed = 'INCOME_DIVIDENDS' THEN '%[4]s'
    WHEN l.income_detailed = 'INCOME_DISTRIBUTIONS' THEN '%[3]s'
    WHEN l.income_detailed = 'INCOME_RENTAL' THEN '%[5]s'
    WHEN l.income_detailed = 'INCOME_RETIREMENT_PENSION' THEN '%[6]s' END`, yield.TypeCoupons, yield.TypeInterest,
	yield.TypeDistributions, yield.TypeDividends, yield.TypeRent, yield.TypePension, canonical.CouponZero)

// readIncome reads the income of the twelve months to the instant, in
// the output currency, per holding or sold position (lineHolding), and
// per account where a line names no instrument; and the payments of the
// 24 months to it the same way, in the line's currency. A dividend or a
// distribution comes with the quantities yield.Payment documents, read
// under its account's keys (yieldUnits.keysOf, quantity, span,
// firstHeld), its record day the one the dividend calendar states or
// derives, else the one its issuer's learned gap gives, else the one its
// market's lag gives (yield.RecordOf, calendarOf, gapOf, lagOf). Where the
// calendar gives the days, the quantity reads as the ex day began, and
// that day is its record window alone; where a learned gap gives them, it
// reads the same way across the gap's window (yield.Lag.ExWindow).
func (b *yieldBase) readIncome(ctx context.Context, db querier, in *yield.Inputs) error {
	from := in.Day.AddDate(-2, 0, 0).Unix()
	var lines []incomeLine
	if err := incomeLines(ctx, db, from, in.AsOf, in.Currency, func(l incomeLine) {
		lines = append(lines, l)
	}); err != nil {
		return err
	}
	if b.units == nil {
		// A payment of the window's first days reads what was held up to
		// RecordLagDays before it, or across its record window.
		u, err := readYieldUnits(ctx, db, in, unitIDs(in, lines),
			from-max(yield.RecordLagDays, yield.RecordWindowDays)*SecondsPerDay, in.AsOf)
		if err != nil {
			return err
		}
		b.units = u
	}
	in.Index()
	yearAgo := in.Day.AddDate(-1, 0, 0)
	for _, l := range lines {
		key, held := lineHolding(in, &l)
		day := time.Unix(l.At, 0).UTC()
		recent := day.After(yearAgo)
		switch {
		case held:
			if recent && l.Value != nil {
				if in.Income[key] == nil {
					in.Income[key] = yield.Received{}
				}
				in.Income[key][l.Type] += *l.Value
			}
			p := yield.Payment{Day: day, Amount: l.Amount, Currency: l.Currency, Account: l.Account, Type: l.Type}
			if paysPerShare(l.Type) {
				start := day.Truncate(24 * time.Hour).Unix()
				pay := time.Unix(start, 0).UTC()
				keys := b.units.keysOf(in, l.Source, l.Account, l.Instrument)
				p.Quantity, p.Units = b.units.quantity(keys, start)
				lag := b.lagOf(in, l)
				events, share, market, security := b.calendarOf(in, l)
				learned := b.gapOf(security, share, market)
				readDay, source := yield.RecordOf(events, pay, lag, learned, share, market, b.cycles)
				p.RecordSource = source
				if source == yield.RecordNone {
					p.Before, _ = b.units.quantity(keys, start-yield.RecordLagDays*SecondsPerDay)
				} else {
					read := readDay.Unix()
					p.LagDays = int((start - read) / SecondsPerDay)
					far, near := readDay, readDay
					switch source {
					case yield.RecordLagged:
						far, near = lag.Window(pay)
					case yield.RecordLearned:
						far, near = learned.ExWindow(pay, market, b.cycles)
						p.ReachDays = int((start - far.Unix()) / SecondsPerDay)
					}
					q, units := b.units.quantity(keys, read)
					if q != nil && b.units.seenBy(keys, far.Unix()) {
						p.Record, p.Units = q, units
						p.Least, p.Most = b.units.span(keys, far.Unix(), near.Unix())
					} else {
						// None held on the day read, none known, or the
						// account first seen inside the window: the pay day's
						// quantity rates, against the first the account shows
						// held after the day read (yield.Payment).
						p.Before = b.units.firstHeld(keys, read, start)
					}
				}
			}
			in.Payments[key] = append(in.Payments[key], p)
		case l.Instrument == "" && recent:
			ac := yield.AccountCurrency{AccountKey: yield.AccountKey{Source: l.Source, Account: l.Account},
				Currency: l.Currency}
			if l.Value != nil {
				if in.AccountIncome[ac] == nil {
					in.AccountIncome[ac] = yield.Received{}
				}
				in.AccountIncome[ac][l.Type] += *l.Value
			}
			if l.Type == yield.TypeRent || l.Type == yield.TypePension || l.Type == yield.TypeInterest {
				in.AccountPayments = append(in.AccountPayments, yield.AccountPayment{AccountCurrency: ac, Day: day,
					Amount: l.Amount, Type: l.Type})
			}
		}
	}
	return nil
}

// unitIDs are the instruments the units of the dividends and
// distributions of lines read: each line's id folded under its account
// and under each account of its portfolio (portfolioKeys).
func unitIDs(in *yield.Inputs, lines []incomeLine) map[yield.InstrumentKey]bool {
	paid := map[yield.InstrumentKey]bool{}
	for _, l := range lines {
		if l.Instrument != "" && paysPerShare(l.Type) {
			for _, k := range portfolioKeys(in, l.Source, l.Account, l.Instrument) {
				paid[yield.InstrumentKey{Source: l.Source, Instrument: k.iid}] = true
			}
		}
	}
	return paid
}

// lagOf is the lag of the record day of the dividend a line pays
// (yieldBase.lag), under the first id its instrument reads under that has
// one (portfolioKeys).
func (b *yieldBase) lagOf(in *yield.Inputs, l incomeLine) yield.Lag {
	for _, k := range portfolioKeys(in, l.Source, l.Account, l.Instrument) {
		if lag := b.lag[yield.InstrumentKey{Source: l.Source, Instrument: k.iid}]; lag.Days > 0 {
			return lag
		}
	}
	return yield.Lag{}
}

// calendarOf is what the dividend calendar states of the security a line
// pays (yieldBase.calendar), whether it is a share (yieldBase.share), the
// market whose settlement cycle derives its record day
// (yieldBase.market), and the security: each under the first id its
// instrument reads under that has one (portfolioKeys), as lagOf reads its
// lag. The security is the instrument's as a position names it
// (yield.Inputs.HeldSecurity).
func (b *yieldBase) calendarOf(in *yield.Inputs, l incomeLine) (events []yield.CalendarEvent, share bool,
	market, security string) {
	for _, k := range portfolioKeys(in, l.Source, l.Account, l.Instrument) {
		ik := yield.InstrumentKey{Source: l.Source, Instrument: k.iid}
		if security == "" {
			security = in.HeldSecurity[ik]
		}
		if market == "" {
			market = b.market[ik]
		}
		share = share || b.share[ik]
	}
	return b.calendar[security], share, market, security
}

// gapOf is the gap learned of the issuer of a security (yieldBase.gaps):
// its own, else, for a fund, its pool's (yield.FundPool) in the business
// days of the fund's market; the zero Lag where none is learned. A share
// takes no pool's: an exchange-traded fund pays days after its record
// day, a company weeks.
func (b *yieldBase) gapOf(security string, share bool, market string) yield.Lag {
	if g, ok := b.gaps.security[security]; ok && security != "" {
		return g
	}
	if pool := yield.FundPool(market); !share && pool != "" {
		if g, ok := b.gaps.pool[pool]; ok {
			g.Market = market
			return g
		}
	}
	return yield.Lag{}
}

// paysPerShare says a payment of type t is paid per share of what its
// holding holds: a dividend or a distribution.
func paysPerShare(t yield.IncomeType) bool {
	return t == yield.TypeDividends || t == yield.TypeDistributions
}

// incomeLine is one income transaction of a type the projection counts:
// its amount in its currency, its value in the output currency (nil
// where no rate converts it), and its description.
type incomeLine struct {
	Source, Account, Instrument, Currency, Description string
	At                                                 int64
	Type                                               yield.IncomeType
	Amount                                             float64
	Value                                              *float64
}

// couponOf reads an account's interest line that names no instrument but
// whose description names a bond (canonical.ReadCouponLine), as a broker
// books a Treasury's coupon: it is the coupon of the one bond of its
// account in the book that the line pays. Where no held bond, or more
// than one, pays it, a line that names a bond issue is the account's
// coupons (lineHolding); any other, such as a deposit's that states its
// rate and its end, stays interest.
func couponOf(in *yield.Inputs, l *incomeLine) {
	if l.Instrument != "" || l.Type != yield.TypeInterest {
		return
	}
	line, ok := canonical.ReadCouponLine(l.Description)
	if !ok {
		return
	}
	var bonds []yield.Holding
	for _, h := range in.Holdings {
		if h.Source == l.Source && h.Account == l.Account && h.Vehicle == string(canonical.VehicleBond) {
			bonds = append(bonds, h)
		}
	}
	paid, ok := line.OnePaying(len(bonds), func(i int) (string, *float64, *time.Time) {
		t := in.Terms[yield.InstrumentKey{Source: bonds[i].Source, Instrument: bonds[i].Instrument}]
		return bonds[i].Instrument, t.Coupon, t.Maturity
	})
	if ok || line.Issue {
		l.Type, l.Instrument = yield.TypeCoupons, paid
	}
}

// lineHolding is the holding a line belongs to (yield.Inputs.HoldingOf),
// else the sold position that holds it (yield.Inputs.Sold); false where
// neither does. Interest that names a bond is its coupon (couponOf). A
// sold position's coupons pool with the bond's amounts in its realized
// yield. A coupon whose line names no bond, or one that neither its
// account, its portfolio, nor one account of its source alone held,
// names no instrument: it reads as the account's coupons
// (`income:coupons:<CCY>`), received, but neither the cash's realized
// yield nor the projection reads it. So a coupon reads the same whether
// its line names its bond or only its description does.
func lineHolding(in *yield.Inputs, l *incomeLine) (yield.HoldingKey, bool) {
	couponOf(in, l)
	key, held := in.HoldingOf(l.Source, l.Account, l.Instrument)
	if !held {
		key, held = in.Sold(l.Source, l.Account, l.Instrument)
	}
	if !held && l.Type == yield.TypeCoupons {
		l.Instrument = ""
		return yield.HoldingKey{}, false
	}
	return key, held
}

// incomeLines calls fn with each income line of [from, to], oldest
// first: an income transaction of a type the projection counts, in ccy
// at its day's rate.
func incomeLines(ctx context.Context, db querier, from, to int64, ccy string, fn func(incomeLine)) error {
	return scanAll(ctx, db, `
WITH l AS (
    SELECT l.silver_source_id, l.account_external_id, t.instrument_external_id, l.transaction_external_id,
           l.occurred_at, l.currency, l.net_amount::DOUBLE AS amount, `+incomeTypeSQL+` AS type,
           COALESCE(t.description, '') AS description
      FROM income_lines_base(?, ?) l
      JOIN transactions t ON t.silver_source_id = l.silver_source_id
           AND t.transaction_external_id = l.transaction_external_id
      LEFT JOIN instruments i ON i.silver_source_id = t.silver_source_id
           AND i.instrument_external_id = t.instrument_external_id
      LEFT JOIN instrument_terms_at(?) tm ON tm.silver_source_id = t.silver_source_id
           AND tm.instrument_external_id = t.instrument_external_id),
fx AS (SELECT * FROM fx_rates_to(?))
SELECT l.silver_source_id, l.account_external_id, COALESCE(l.instrument_external_id, ''), l.occurred_at,
       l.currency, l.amount, l.type, l.description,
       CASE WHEN l.currency = ? THEN l.amount ELSE l.amount * fx.rate END AS value
  FROM l
  ASOF LEFT JOIN fx ON fx.from_ccy = l.currency AND fx.day <= l.occurred_at // 86400
 WHERE l.type IS NOT NULL
 ORDER BY l.occurred_at, l.silver_source_id, l.account_external_id, l.transaction_external_id`, func(rows *sql.Rows) error {
		var l incomeLine
		var typ string
		var value sql.NullFloat64
		if err := rows.Scan(&l.Source, &l.Account, &l.Instrument, &l.At, &l.Currency, &l.Amount, &typ, &l.Description,
			&value); err != nil {
			return err
		}
		l.Type, l.Value = yield.IncomeType(typ), nullFloatToPtr(value)
		fn(l)
		return nil
	}, from, to, to, ccy, ccy)
}

// IncomeReceived is every income line of [from, to] that the income
// projection counts as received, each named by the holding of the book
// it belongs to, else by the sold position that holds it (lineHolding),
// else by its account: a payment of interest by the account's cash in its
// currency, a line that names an instrument by its account and that
// instrument, by the id the matching folds it into, as Sold names a sold
// position, and any other by the account and its type.
func IncomeReceived(ctx context.Context, db querier, in *yield.Inputs, from, to int64) ([]yield.ReceivedLine, error) {
	var out []yield.ReceivedLine
	err := incomeLines(ctx, db, from, to, in.Currency, func(l incomeLine) {
		key, held := lineHolding(in, &l)
		r := yield.ReceivedLine{Day: time.Unix(l.At, 0).UTC(), Type: l.Type}
		if l.Value != nil {
			r.Value = *l.Value
		}
		switch {
		case held:
			r.Holding = key
		case l.Type == yield.TypeInterest && l.Instrument == "":
			r.Holding = yield.HoldingKey{Source: l.Source, Account: l.Account, PositionKey: "cash:" + l.Currency}
		default:
			r.Holding = yield.AccountIncomeKey(yield.AccountCurrency{
				AccountKey: yield.AccountKey{Source: l.Source, Account: l.Account}, Currency: l.Currency}, l.Type)
			if l.Instrument != "" {
				r.Holding.PositionKey = in.Fold(l.Source, l.Account, l.Instrument)
			}
		}
		out = append(out, r)
	})
	if err != nil {
		return nil, fmt.Errorf("IncomeReceived: %w", err)
	}
	return out, nil
}

// ForwardParams are what a forward view projects: the book of At valued
// in Ccy, over the window [From, To], under Assumptions.
type ForwardParams struct {
	At, From, To int64
	Ccy          string
	Assumptions  yield.Assumptions
}

// PastProjection refuses a forward window that ends after Last, the
// projection's last day.
type PastProjection struct{ Last time.Time }

func (e PastProjection) Error() string {
	return "the projection ends on " + e.Last.Format(time.DateOnly) + "; the window ends after it"
}

// forwardBook is the book of p.At and p's window, refused with
// PastProjection where the window ends after the projection does: what
// every forward view (income, cash flow) projects over.
func forwardBook(ctx context.Context, db querier, p ForwardParams) (*yield.Book, yield.Window, error) {
	in, err := YieldInputs(ctx, db, p.At, p.Ccy)
	if err != nil {
		return nil, yield.Window{}, err
	}
	b := yield.Compute(in)
	w := yield.Window{From: time.Unix(p.From, 0).UTC(), To: time.Unix(p.To, 0).UTC()}
	if w.To.After(b.Last()) {
		return nil, yield.Window{}, PastProjection{Last: b.Last()}
	}
	return b, w, nil
}

// Forward is what every forward income view reads: the book of today,
// the window and its amounts, the amounts of the same window a year
// before, and those of the next twelve months.
type Forward struct {
	Book                      *yield.Book
	Window                    yield.Window
	Amounts, LastYear, Next12 []yield.Amount
}

// ForwardIncome is the Forward of p. What the window holds is received
// up to p.At and expected after it. Last year's window counts only once
// it is over: before, LastYear is nil.
func ForwardIncome(ctx context.Context, db querier, p ForwardParams) (*Forward, error) {
	b, w, err := forwardBook(ctx, db, p)
	if err != nil {
		return nil, err
	}
	received, err := IncomeReceived(ctx, db, b.In, w.From.Unix(), min(w.To.Unix(), p.At))
	if err != nil {
		return nil, err
	}
	f := &Forward{Book: b, Window: w, Amounts: b.Amounts(received, w, p.Assumptions)}
	lw := yield.Window{From: w.From.AddDate(-1, 0, 0), To: w.To.AddDate(-1, 0, 0)}
	if lw.To.Unix() <= p.At {
		prior, err := IncomeReceived(ctx, db, b.In, lw.From.Unix(), lw.To.Unix())
		if err != nil {
			return nil, err
		}
		f.LastYear = b.Amounts(prior, lw, p.Assumptions)
	}
	f.Next12 = b.Amounts(nil, b.NextMonths(12), p.Assumptions)
	return f, nil
}
