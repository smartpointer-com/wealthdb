package yield

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// ReceivedLine is one payment received: income of a type the projection
// counts, named by the holding it belongs to, else by its account (the
// account's cash for interest, `income:<type>:<CCY>` for anything else,
// AccountIncomeKey).
type ReceivedLine struct {
	Holding HoldingKey
	// Day is when it was booked, to the second.
	Day  time.Time
	Type IncomeType
	// Value is in the output currency, at the day's rate.
	Value float64
}

// Assumptions are what the expected income rests on: the rate path, what
// a maturing principal does, and a parallel shift of the floating rates
// in basis points.
type Assumptions struct {
	Path    Path
	Rule    Rule
	ShiftBP float64
}

// DefaultAssumptions are constant rates and maturing principal to cash.
var DefaultAssumptions = Assumptions{Path: PathConstant, Rule: RuleCash}

// Window is the span an income view covers, by day: From's first second
// to To's last. Today splits it: what came before was received, what
// comes after is expected.
type Window struct {
	From, To time.Time
}

// NextMonths is the window of the n months after the instant's month.
func (b *Book) NextMonths(n int) Window {
	first := b.Projection.MonthStart(1)
	return Window{From: first, To: first.AddDate(0, n, 0).Add(-time.Second)}
}

// Last is the last second the projection covers.
func (b *Book) Last() time.Time { return b.Projection.End().Add(-time.Second) }

// Amount is one line of income in a month: received or expected, of one
// holding (or account) and type, in the output currency.
type Amount struct {
	Holding HoldingKey
	Month   time.Time // its first day
	// Day is a payment's day; nil for income that accrues, and for a
	// payment of which only the month is known.
	Day      *time.Time
	Type     IncomeType
	Received bool
	Value    float64
	// Withheld is what the withholding table withholds from it; nil
	// where the table names no rate for the holding's situs.
	Withheld *float64
	Basis    Basis
	// Reinvested is how a maturity's principal earns it; "" for a
	// holding's own income.
	Reinvested Rung
	// ShiftBP is the shift it moves with, where it rests on a floating
	// rate.
	ShiftBP float64
	// Principal is a maturity's principal, outside the income totals.
	Principal bool
}

// Amounts are the income of a window: what was received on or before
// today, what is expected after it under the assumptions, month by month
// (an expected amount counts in its month, an accrual its share of the
// window's days; the current month holds both halves), and the principal
// a maturity returns.
func (b *Book) Amounts(received []ReceivedLine, w Window, a Assumptions) []Amount {
	p := b.Projection
	withheld := func(k HoldingKey, v float64) *float64 {
		if pct, ok := p.Withholding[k]; ok {
			x := v * pct / 100
			return &x
		}
		return nil
	}
	out := []Amount{}
	for _, r := range received {
		if r.Day.Before(w.From) || r.Day.After(w.To) || r.Day.Unix() > b.In.AsOf {
			continue
		}
		d := r.Day
		out = append(out, Amount{Holding: r.Holding, Month: monthStart(r.Day), Day: &d, Type: r.Type, Received: true,
			Value: r.Value, Withheld: withheld(r.Holding, r.Value)})
	}
	type key struct {
		h          HoldingKey
		m          int
		day        time.Time
		t          IncomeType
		basis      Basis
		reinvested Rung
	}
	sums := map[key]float64{}
	floating := map[key]bool{}
	var order []key
	for _, part := range p.Parts {
		if part.Rule != a.Rule {
			continue
		}
		k := part.share(w.From, w.To)
		if k == 0 {
			continue
		}
		basis := part.Basis
		if c := b.In.Curves[part.Currency]; a.Path == PathForward && c != nil && c.Zero != nil &&
			part.Principal != 0 && (basis == BasisStated || basis == BasisDeclared) {
			basis = BasisForward
		}
		var day time.Time
		if part.Day != nil && !part.MonthOnly {
			day = *part.Day
		}
		pk := key{part.Holding, part.Month, day, part.Type, basis, part.Reinvested}
		if _, ok := sums[pk]; !ok {
			order = append(order, pk)
		}
		sums[pk] += k * part.Income(a.Path, p.Rate(part.Currency, part.Month, a.Path), a.ShiftBP) * p.FX[part.Holding]
		floating[pk] = floating[pk] || part.Floating()
	}
	for _, k := range order {
		v := sums[k]
		if v == 0 && k.basis != BasisUnknown {
			continue
		}
		am := Amount{Holding: k.h, Month: p.MonthStart(k.m), Type: k.t, Value: v, Withheld: withheld(k.h, v),
			Basis: k.basis, Reinvested: k.reinvested}
		if !k.day.IsZero() {
			d := k.day
			am.Day = &d
		}
		if floating[k] {
			am.ShiftBP = a.ShiftBP
		}
		out = append(out, am)
	}
	for _, e := range p.Events {
		if e.Rule == a.Rule && !e.Day.Before(w.From) && !e.Day.After(w.To) {
			d := e.Day
			out = append(out, Amount{Holding: e.Holding, Month: p.MonthStart(e.Month), Day: &d, Type: e.Type,
				Value: e.Amount * p.FX[e.Holding], Basis: e.Basis, Principal: true})
		}
	}
	return out
}

func monthStart(d time.Time) time.Time {
	return time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// periodStart is the first day of the calendar month, quarter or year
// (months 1, 3 or 12) a day falls in.
func periodStart(d time.Time, months int) time.Time {
	m := (int(d.Month())-1)/months*months + 1
	return time.Date(d.Year(), time.Month(m), 1, 0, 0, 0, 0, time.UTC)
}

// windowMonths is the months a window covers, a month it cuts counting
// its share of days.
func windowMonths(w Window) float64 {
	n := 0.0
	for m := monthStart(w.From); !m.After(w.To); m = m.AddDate(0, 1, 0) {
		end := m.AddDate(0, 1, 0)
		n += earlier(end, w.To.Add(time.Second)).Sub(later(m, w.From)).Hours() / end.Sub(m).Hours()
	}
	return n
}

// tally is what the quality column of a set of expected amounts reads:
// the share of the reinvested income each rung earns, and the shift the
// floating income moves with.
type tally struct {
	rungs      map[Rung]float64
	reinvested float64
	shiftBP    float64
}

func (t *tally) add(a Amount) {
	if a.Received || a.Principal {
		return
	}
	if a.Reinvested != "" {
		if t.rungs == nil {
			t.rungs = map[Rung]float64{}
		}
		t.rungs[a.Reinvested] += a.Value
		t.reinvested += a.Value
	}
	if a.ShiftBP != 0 {
		t.shiftBP = a.ShiftBP
	}
}

// quality is the rungs' shares of the reinvested income (shares), then
// `shift=±Nbp`.
func (t tally) quality() string {
	var parts []string
	if s := shares(Rungs, t.rungs, t.reinvested); s != "" {
		parts = append(parts, s)
	}
	if t.shiftBP != 0 {
		parts = append(parts, fmt.Sprintf("shift=%+.0fbp", t.shiftBP))
	}
	return strings.Join(parts, " ")
}

// shares is the share of total each key of by holds, in the order of
// keys: pctLabel's `key=NN%` or `key<1%`, `key=0%` for none; a key alone
// prints alone, and keys of no total print their names.
func shares[K ~string](keys []K, by map[K]float64, total float64) string {
	var held []K
	for _, k := range keys {
		if _, ok := by[k]; ok {
			held = append(held, k)
		}
	}
	if len(held) == 1 || total == 0 {
		var names []string
		for _, k := range held {
			names = append(names, string(k))
		}
		return strings.Join(names, " ")
	}
	var parts []string
	for _, k := range held {
		if by[k] == 0 {
			parts = append(parts, string(k)+"=0%")
		} else {
			parts = append(parts, pctLabel(string(k), by[k], total))
		}
	}
	return strings.Join(parts, " ")
}

// ProjectionRow is one period of the income projection: what was
// received and is expected, in total and per type, what the
// withholding table withholds, and what the expected part rests on.
type ProjectionRow struct {
	// Period is the first day of the calendar month, quarter or year;
	// the window cuts the first and the last. Total and Average are the
	// summary rows.
	Period         time.Time
	Total, Average bool
	Received       float64
	Expected       float64
	ByType         map[IncomeType][2]float64 // received, expected
	Withheld       *float64
	Principal      float64
	Status, Basis  string
	Quality        string
}

// ProjectionRows is the income of the window per calendar period of
// months months (1 monthly, 3 quarterly, 12 annual), then a total and
// the average of the window's months.
func (b *Book) ProjectionRows(amounts []Amount, w Window, months int) []ProjectionRow {
	var rows []ProjectionRow
	index := map[time.Time]int{}
	for m := periodStart(w.From, months); !m.After(w.To); m = m.AddDate(0, months, 0) {
		index[m] = len(rows)
		rows = append(rows, ProjectionRow{Period: m, ByType: map[IncomeType][2]float64{}})
	}
	bases := make([]map[Basis]float64, len(rows))
	tallies := make([]tally, len(rows))
	for i := range bases {
		bases[i] = map[Basis]float64{}
	}
	total := ProjectionRow{Total: true, ByType: map[IncomeType][2]float64{}}
	totalBases := map[Basis]float64{}
	var totalTally tally
	for _, a := range amounts {
		i, ok := index[periodStart(a.Month, months)]
		if !ok {
			continue
		}
		r := &rows[i]
		if a.Principal {
			r.Principal += a.Value
			total.Principal += a.Value
			continue
		}
		side := 0
		if a.Received {
			r.Received += a.Value
			total.Received += a.Value
		} else {
			side = 1
			r.Expected += a.Value
			total.Expected += a.Value
			bases[i][a.Basis] += a.Value
			totalBases[a.Basis] += a.Value
			tallies[i].add(a)
			totalTally.add(a)
		}
		bt, tt := r.ByType[a.Type], total.ByType[a.Type]
		bt[side] += a.Value
		tt[side] += a.Value
		r.ByType[a.Type], total.ByType[a.Type] = bt, tt
		if a.Withheld != nil {
			r.Withheld = addTo(r.Withheld, *a.Withheld)
			total.Withheld = addTo(total.Withheld, *a.Withheld)
		}
	}
	for i := range rows {
		rows[i].Status = status(rows[i].Received, rows[i].Expected)
		rows[i].Basis = basisShares(bases[i], rows[i].Expected)
		rows[i].Quality = tallies[i].quality()
	}
	total.Status = status(total.Received, total.Expected)
	total.Basis = basisShares(totalBases, total.Expected)
	total.Quality = totalTally.quality()
	n := windowMonths(w)
	avg := ProjectionRow{Average: true, Received: total.Received / n, Expected: total.Expected / n,
		ByType: map[IncomeType][2]float64{}}
	for t, v := range total.ByType {
		avg.ByType[t] = [2]float64{v[0] / n, v[1] / n}
	}
	if total.Withheld != nil {
		avg.Withheld = ptr(*total.Withheld / n)
	}
	return append(rows, total, avg)
}

func addTo(p *float64, v float64) *float64 {
	if p == nil {
		return &v
	}
	x := *p + v
	return &x
}

func status(received, expected float64) string {
	switch {
	case received != 0 && expected != 0:
		return "mixed"
	case expected != 0:
		return "expected"
	case received != 0:
		return "received"
	}
	return ""
}

// basisShares is each basis's share of the expected amount, in the order
// of Bases (shares).
func basisShares(by map[Basis]float64, expected float64) string {
	if expected == 0 {
		return ""
	}
	nonzero := map[Basis]float64{}
	for b, v := range by {
		if v != 0 {
			nonzero[b] = v
		}
	}
	return shares(Bases, nonzero, expected)
}

// IncomeRow is one entity's income over the window: received, expected
// and their total, what the same window a year earlier received, the
// next twelve months' expected income, and what the expected part
// rests on. A position's row carries the holding.
type IncomeRow struct {
	Source, Portfolio, PortfolioName, AccountID string
	Account                                     Account
	Position                                    *Position
	Holding                                     HoldingKey
	Received, Expected, Next12                  float64
	// LastYear is what the same window a year earlier received; nil
	// where that window is not over.
	LastYear       *float64
	Withheld       *float64
	Basis, Quality string
}

// Total is the window's received and expected income.
func (r IncomeRow) Total() float64 { return r.Received + r.Expected }

// IncomeRows is the income of a window per entity of grain, positions
// for GrainPositions: every holding of the book, those that pay nothing
// among them, then any no longer held that received income in the
// window, or in the same window a year earlier. lastYear are the amounts
// of the same window a year earlier, nil where that window is not over;
// next12 the expected amounts of the twelve months after the current
// one.
func (b *Book) IncomeRows(g Grain, amounts, lastYear, next12 []Amount) []IncomeRow {
	type agg struct {
		row   IncomeRow
		bases map[Basis]float64
		tally tally
	}
	rows := map[entity]*agg{}
	var order []entity
	positions := b.positionIndex()
	get := func(h HoldingKey) *agg {
		a := b.In.Accounts[AccountKey{h.Source, h.Account}]
		k := entityOfKey(g, h, a)
		r := rows[k]
		if r == nil {
			r = &agg{row: IncomeRow{Source: k.source, Portfolio: k.portfolio, AccountID: k.account},
				bases: map[Basis]float64{}}
			if g != GrainSources {
				r.row.Account, r.row.PortfolioName = a, a.PortfolioName
			}
			if g == GrainPositions {
				r.row.Holding, r.row.Position = h, positions[h]
			}
			rows[k] = r
			order = append(order, k)
		}
		return r
	}
	if g == GrainPositions {
		for _, p := range b.Positions {
			get(p.HoldingKey)
		}
	}
	for _, a := range amounts {
		if a.Principal {
			continue
		}
		r := get(a.Holding)
		if a.Received {
			r.row.Received += a.Value
		} else {
			r.row.Expected += a.Value
			r.bases[a.Basis] += a.Value
			r.tally.add(a)
		}
		if a.Withheld != nil {
			r.row.Withheld = addTo(r.row.Withheld, *a.Withheld)
		}
	}
	for _, a := range lastYear {
		if !a.Principal && a.Received {
			r := get(a.Holding)
			r.row.LastYear = addTo(r.row.LastYear, a.Value)
		}
	}
	if lastYear != nil {
		for _, r := range rows {
			if r.row.LastYear == nil {
				r.row.LastYear = ptr(0)
			}
		}
	}
	for _, a := range next12 {
		if !a.Principal && !a.Received {
			get(a.Holding).row.Next12 += a.Value
		}
	}
	out := make([]IncomeRow, 0, len(order))
	for _, k := range order {
		r := rows[k]
		r.row.Basis = dominantBasis(r.bases, r.row.Expected)
		r.row.Quality = r.tally.quality()
		out = append(out, r.row)
	}
	slices.SortStableFunc(out, func(x, y IncomeRow) int {
		if g == GrainPositions {
			if c := cmp.Compare(x.Source+"\x00"+x.AccountID, y.Source+"\x00"+y.AccountID); c != 0 {
				return c
			}
		}
		return cmp.Compare(y.Total()+y.Next12, x.Total()+x.Next12)
	})
	return out
}

// dominantBasis is the basis of the largest share of the expected
// income, "(none)" where nothing is expected.
func dominantBasis(by map[Basis]float64, expected float64) string {
	if expected == 0 {
		if v, ok := by[BasisUnknown]; ok && v == 0 {
			return string(BasisUnknown)
		}
		return "(none)"
	}
	best, bestV := Basis(""), 0.0
	for _, b := range Bases {
		if v := by[b]; math.Abs(v) > math.Abs(bestV) {
			best, bestV = b, v
		}
	}
	return string(best)
}

// CalendarRow is one expected payment: income on its day, or in its
// month where only the month is known, or a principal returned at
// maturity.
type CalendarRow struct {
	Amount
	Position *Position
	Account  Account
	Currency string
	// Own is the amount in the holding's currency.
	Own     float64
	Quality string
}

// Calendar is the window's expected amounts in date order, a holding's
// month-only amounts after its dated ones of that month. An amount of
// zero, what an unknown holding expects, is no payment.
func (b *Book) Calendar(amounts []Amount) []CalendarRow {
	positions := b.positionIndex()
	var out []CalendarRow
	for _, a := range amounts {
		if a.Received || (a.Value == 0 && !a.Principal) {
			continue
		}
		var t tally
		t.add(a)
		r := CalendarRow{Amount: a, Position: positions[a.Holding],
			Account:  b.In.Accounts[AccountKey{a.Holding.Source, a.Holding.Account}],
			Currency: b.Projection.Currency[a.Holding], Quality: t.quality()}
		if fx := b.Projection.FX[a.Holding]; fx != 0 {
			r.Own = a.Value / fx
		}
		out = append(out, r)
	}
	when := func(r CalendarRow) time.Time {
		if r.Day != nil {
			return *r.Day
		}
		return r.Month.AddDate(0, 1, -1)
	}
	slices.SortStableFunc(out, func(x, y CalendarRow) int {
		if c := when(x).Compare(when(y)); c != 0 {
			return c
		}
		return cmp.Compare(x.Holding.Source+x.Holding.Account+x.Holding.PositionKey,
			y.Holding.Source+y.Holding.Account+y.Holding.PositionKey)
	})
	return out
}
