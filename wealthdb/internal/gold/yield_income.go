package gold

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
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

// readIncome reads the income lines of the 24 months to the instant, each
// with its value in each currency of the base (incomeLinesIn), and the
// units of the instruments they pay (readYieldUnits).
func (b *yieldBase) readIncome(ctx context.Context, db querier) error {
	from := b.in.Day.AddDate(-2, 0, 0).Unix()
	if err := incomeLinesIn(ctx, db, from, b.in.AsOf, b.currencies, func(l incomeLine, values []*float64) {
		b.lines, b.values = append(b.lines, l), append(b.values, values)
	}); err != nil {
		return err
	}
	// A payment of the window's first days reads what was held up to
	// RecordLagDays before it, or across its record window.
	u, err := readYieldUnits(ctx, db, b.in, unitIDs(b.in, b.lines),
		from-max(yield.RecordLagDays, yield.RecordWindowDays)*SecondsPerDay, b.in.AsOf)
	if err != nil {
		return err
	}
	b.units = u
	return nil
}

// incomeAttribution is what the base's income lines pay under one set of
// holdings, whatever the currency: each line's fate, and the payments of
// each holding or sold position and of each account. The holdings are
// what a line's holding depends on (holdingIDs).
type incomeAttribution struct {
	holdings        []holdingID
	fates           []lineFate
	payments        map[yield.HoldingKey][]yield.Payment
	accountPayments []yield.AccountPayment
}

// lineFate is where a line's value counts: the holding or sold position
// key (held), else the account and currency ac (account), as its type
// typ, where it is of the twelve months to the instant (recent).
type lineFate struct {
	key           yield.HoldingKey
	ac            yield.AccountCurrency
	typ           yield.IncomeType
	held, account bool
	recent        bool
}

// holdingID is what of a holding the attribution of a line reads
// (lineHolding): its key, its instrument and its vehicle.
type holdingID struct {
	yield.HoldingKey
	instrument, vehicle string
}

// holdingIDs are the ids of holdings, in their order.
func holdingIDs(holdings []yield.Holding) []holdingID {
	out := make([]holdingID, len(holdings))
	for i, h := range holdings {
		out[i] = holdingID{h.HoldingKey, h.Instrument, h.Vehicle}
	}
	return out
}

// attribution is the base's income lines attributed under in's holdings
// (attribute), read once for every currency whose holdings are the same.
func (b *yieldBase) attribution(in *yield.Inputs) *incomeAttribution {
	ids := holdingIDs(in.Holdings)
	for _, a := range b.attributions {
		if slices.Equal(a.holdings, ids) {
			return a
		}
	}
	a := b.attribute(in, ids)
	b.attributions = append(b.attributions, a)
	return a
}

// attribute attributes the base's income lines under in's holdings, in
// the lines' order: each to its holding or sold position (lineHolding),
// else, where it names no instrument, to its account; and the payments
// of the 24 months to the instant the same way, in the line's currency.
// A dividend or a distribution comes with the quantities yield.Payment
// documents, read under its account's keys (yield.Holders.KeysOf, quantity,
// span, firstHeld), its record day the one the dividend calendar states
// or derives, else the one its issuer's learned gap gives, else the one
// its market's lag gives (yield.RecordOf, calendarOf, gapOf, lagOf).
// Where the calendar gives the days, the quantity reads as the ex day
// began, and that day is its record window alone; where a learned gap
// gives them, it reads the same way across the gap's window
// (yield.Lag.ExWindow).
func (b *yieldBase) attribute(in *yield.Inputs, ids []holdingID) *incomeAttribution {
	a := &incomeAttribution{holdings: ids, fates: make([]lineFate, len(b.lines)),
		payments: map[yield.HoldingKey][]yield.Payment{}}
	yearAgo := in.Day.AddDate(-1, 0, 0)
	for i := range b.lines {
		l := b.lines[i]
		key, held := lineHolding(in, &l)
		day := time.Unix(l.At, 0).UTC()
		recent := day.After(yearAgo)
		switch {
		case held:
			a.fates[i] = lineFate{key: key, typ: l.Type, held: true, recent: recent}
			p := yield.Payment{Day: day, Amount: l.Amount, Currency: l.Currency, Account: l.Account, Type: l.Type}
			if paysPerShare(l.Type) {
				start := day.Truncate(24 * time.Hour).Unix()
				pay := time.Unix(start, 0).UTC()
				keys := b.units.KeysOf(l.Source, l.Account, l.Instrument)
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
			a.payments[key] = append(a.payments[key], p)
		case l.Instrument == "" && recent:
			ac := yield.AccountCurrency{AccountKey: yield.AccountKey{Source: l.Source, Account: l.Account},
				Currency: l.Currency}
			a.fates[i] = lineFate{ac: ac, typ: l.Type, account: true, recent: recent}
			if l.Type == yield.TypeRent || l.Type == yield.TypePension || l.Type == yield.TypeInterest {
				a.accountPayments = append(a.accountPayments, yield.AccountPayment{AccountCurrency: ac, Day: day,
					Amount: l.Amount, Type: l.Type})
			}
		}
	}
	return a
}

// income gives in the payments of its attribution (attribution) and the
// income of the twelve months to the instant in its currency, the base's
// ci-th: per holding or sold position, and per account where a line names
// no instrument, each line's value added in the lines' order.
func (b *yieldBase) income(in *yield.Inputs, ci int) {
	a := b.attribution(in)
	in.Payments, in.AccountPayments = a.payments, a.accountPayments
	for i, f := range a.fates {
		v := b.values[i][ci]
		switch {
		case f.held:
			if f.recent && v != nil {
				if in.Income[f.key] == nil {
					in.Income[f.key] = yield.Received{}
				}
				in.Income[f.key][f.typ] += *v
			}
		case f.account:
			if v != nil {
				if in.AccountIncome[f.ac] == nil {
					in.AccountIncome[f.ac] = yield.Received{}
				}
				in.AccountIncome[f.ac][f.typ] += *v
			}
		}
	}
}

// unitIDs are the instruments the units of the dividends and
// distributions of lines read: each line's id folded under its account
// and under each account of its portfolio (yield.Inputs.PortfolioKeys).
func unitIDs(in *yield.Inputs, lines []incomeLine) map[yield.InstrumentKey]bool {
	paid := map[yield.InstrumentKey]bool{}
	for _, l := range lines {
		if l.Instrument != "" && paysPerShare(l.Type) {
			for _, k := range in.PortfolioKeys(l.Source, l.Account, l.Instrument) {
				paid[yield.InstrumentKey{Source: l.Source, Instrument: k.Instrument}] = true
			}
		}
	}
	return paid
}

// lagOf is the lag of the record day of the dividend a line pays
// (yieldBase.lag), under the first id its instrument reads under that has
// one (yield.Inputs.PortfolioKeys).
func (b *yieldBase) lagOf(in *yield.Inputs, l incomeLine) yield.Lag {
	for _, k := range in.PortfolioKeys(l.Source, l.Account, l.Instrument) {
		if lag := b.lag[yield.InstrumentKey{Source: l.Source, Instrument: k.Instrument}]; lag.Days > 0 {
			return lag
		}
	}
	return yield.Lag{}
}

// calendarOf is what the dividend calendar states of the security a
// line pays (yieldBase.calendar), whether it is a share
// (yieldBase.share), the market whose settlement cycle derives its
// record day (yieldBase.market), and the security: each under the first
// id its instrument reads under that has one
// (yield.Inputs.PortfolioKeys), as lagOf reads its lag. The security is
// the instrument's as a position names it (yield.Inputs.HeldSecurity).
func (b *yieldBase) calendarOf(in *yield.Inputs, l incomeLine) (events []yield.CalendarEvent, share bool,
	market, security string) {
	for _, k := range in.PortfolioKeys(l.Source, l.Account, l.Instrument) {
		ik := yield.InstrumentKey{Source: l.Source, Instrument: k.Instrument}
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
	return incomeLinesIn(ctx, db, from, to, []string{ccy}, func(l incomeLine, values []*float64) {
		l.Value = values[0]
		fn(l)
	})
}

// incomeLinesIn calls fn with each income line of [from, to], oldest
// first, and its value in each currency of currencies, in their order, at
// its day's rate (nil where no rate converts it).
func incomeLinesIn(ctx context.Context, db querier, from, to int64, currencies []string,
	fn func(incomeLine, []*float64)) error {
	var ctes, values, joins strings.Builder
	args := []any{from, to}
	for i, ccy := range currencies {
		fx, p := fmt.Sprintf("fx%d", i), fmt.Sprintf("$%d", len(args)+1)
		args = append(args, ccy)
		fmt.Fprintf(&ctes, ",\n%s AS (SELECT * FROM fx_rates_to(%s))", fx, p)
		fmt.Fprintf(&values, ",\n       CASE WHEN l.currency = %s THEN l.amount ELSE l.amount * %s.rate END", p, fx)
		fmt.Fprintf(&joins, "\n  ASOF LEFT JOIN %[1]s ON %[1]s.from_ccy = l.currency"+
			" AND %[1]s.day <= l.occurred_at // 86400", fx)
	}
	return scanAll(ctx, db, `
WITH l AS (
    SELECT l.silver_source_id, l.account_external_id, t.instrument_external_id, l.transaction_external_id,
           l.occurred_at, l.currency, l.net_amount::DOUBLE AS amount, `+incomeTypeSQL+` AS type,
           COALESCE(t.description, '') AS description
      FROM income_lines_base($1, $2) l
      JOIN transactions t ON t.silver_source_id = l.silver_source_id
           AND t.transaction_external_id = l.transaction_external_id
      LEFT JOIN instruments i ON i.silver_source_id = t.silver_source_id
           AND i.instrument_external_id = t.instrument_external_id
      LEFT JOIN instrument_terms_at($2) tm ON tm.silver_source_id = t.silver_source_id
           AND tm.instrument_external_id = t.instrument_external_id)`+ctes.String()+`
SELECT l.silver_source_id, l.account_external_id, COALESCE(l.instrument_external_id, ''), l.occurred_at,
       l.currency, l.amount, l.type, l.description`+values.String()+`
  FROM l`+joins.String()+`
 WHERE l.type IS NOT NULL
 ORDER BY l.occurred_at, l.silver_source_id, l.account_external_id, l.transaction_external_id`, func(rows *sql.Rows) error {
		var l incomeLine
		var typ string
		nulls := make([]sql.NullFloat64, len(currencies))
		dest := []any{&l.Source, &l.Account, &l.Instrument, &l.At, &l.Currency, &l.Amount, &typ, &l.Description}
		for i := range nulls {
			dest = append(dest, &nulls[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		l.Type = yield.IncomeType(typ)
		values := make([]*float64, len(currencies))
		for i, v := range nulls {
			values[i] = nullFloatToPtr(v)
		}
		fn(l, values)
		return nil
	}, args...)
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
