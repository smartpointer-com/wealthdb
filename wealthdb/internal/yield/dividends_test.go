package yield

import (
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The books here are invented. The day is 2026-06-15.

// stock is a holding of a stock at broker, priced at 100 a share.
func stock(acct, iid string, qty float64) Holding {
	return Holding{HoldingKey: HoldingKey{"broker", acct, iid}, Instrument: iid, AssetClass: "public_equity",
		Vehicle: "stock", Currency: "USD", Value: qty * 100, Amount: qty * 100, Quantity: num(qty)}
}

// quarterly are a quarterly payer's pay days of the year to the day.
var quarterly = []*time.Time{date(2025, 9, 10), date(2025, 12, 10), date(2026, 3, 10), date(2026, 6, 10)}

// pay books to k a dividend of amount that account acct received on day
// d, on qty shares (nil for unknown), which it held RecordLagDays before
// too.
func pay(in *Inputs, k HoldingKey, acct string, d *time.Time, amount float64, qty *float64) {
	in.Payments[k] = append(in.Payments[k], Payment{Day: *d, Amount: amount, Account: acct, Quantity: qty,
		Before: qty, Type: TypeDividends})
}

// trimmed says the last payment booked to k was paid on before shares,
// which its account held RecordLagDays before its day.
func trimmed(in *Inputs, k HoldingKey, before float64) {
	in.Payments[k][len(in.Payments[k])-1].Before = num(before)
}

// dividendOf is a holding's dividend yield and its next twelve months'
// income.
func dividendOf(t *testing.T, b *Book, h Holding, yield, income float64) Position {
	t.Helper()
	p := position(b, h.HoldingKey)
	near(t, h.Account+" dividend yield", p.DividendYield, yield, 1e-9)
	near(t, h.Account+" income 12m", p.Income12, income, 1e-9)
	return p
}

// Two accounts paid on one day pay one rate, not the sum of theirs: the
// lines of two accounts booked to one holding (HoldingOf) still read per
// account, and the day's rate is the median of the accounts'. The rate is
// the instrument's: a holding paid nothing of its own shows it, and so
// does one whose payments state no quantity. Each is paid it on its own
// quantity.
func TestTwoAccountsPaidOnOneDay(t *testing.T) {
	t.Parallel()
	in := inputs()
	a, b, c := stock("A", "QSTK", 100), stock("B", "QSTK", 300), stock("C", "QSTK", 50)
	in.Holdings = []Holding{a, b, c}
	for _, d := range quarterly {
		pay(in, a.HoldingKey, "A", d, 50, num(100))
		pay(in, a.HoldingKey, "B", d, 150, num(300))
		pay(in, c.HoldingKey, "C", d, 25, nil)
	}
	book := Compute(in)
	for _, h := range in.Holdings {
		if p := dividendOf(t, book, h, 2, *h.Quantity*2); !p.Tags[TagIndicated] {
			t.Errorf("%s: tags %v", h.Account, p.Tags)
		}
	}
}

// An account that sold out keeps its dividends under its sold position
// (Sold). They are its instrument's still: its lines read on its own
// quantity, the day both accounts were paid reads one rate, and a
// dividend it received after selling, on nothing it held, rates nothing.
func TestAnAccountThatSoldOut(t *testing.T) {
	t.Parallel()
	in := inputs()
	b := stock("B", "QSTK", 100)
	in.Holdings = []Holding{b}
	in.Held[InstrumentKey{"broker", "QSTK"}] = []string{"A", "B"}
	sold, ok := in.Sold("broker", "A", "QSTK")
	if !ok {
		t.Fatal("A sold no position")
	}
	for i, d := range quarterly {
		pay(in, b.HoldingKey, "B", d, 50, num(100))
		qty := num(400)
		if i == len(quarterly)-1 {
			qty = nil
		}
		pay(in, sold, "A", d, 200, qty)
	}
	dividendOf(t, Compute(in), b, 2, 200)
}

// A holding trimmed between the record day and the pay day is paid on
// what it held before, but held less on the day: that day's rate reads
// four times the others', or one and three fifths. Paid on under two
// thirds what it held RecordLagDays before, it rates nothing: the rate
// before it holds, and the quarter counts in the frequency.
func TestATrimBeforeThePayDay(t *testing.T) {
	t.Parallel()
	for _, after := range []float64{100, 250} {
		in := inputs()
		h := stock("A", "QSTK", after)
		in.Holdings = []Holding{h}
		for i, d := range quarterly {
			qty := num(400)
			if i == len(quarterly)-1 {
				qty = num(after)
			}
			pay(in, h.HoldingKey, "A", d, 200, qty)
		}
		trimmed(in, h.HoldingKey, 400)
		b := Compute(in)
		dividendOf(t, b, h, 2, 2*after)
		for _, m := range []int{3, 6, 9, 12} {
			near(t, "a quarter", num(incomeOf(b, h.HoldingKey, m, RuleCash, PathConstant, 0)), 0.5*after, 1e-9)
		}
	}
}

// A payment whose quantity is unknown, such as one before the first
// snapshot, rates nothing: today's quantity never stands in. The rate of
// the last payment that has one holds; with none, the holding paid
// within the year indicates nothing, and reads unknown.
func TestAPaymentWithNoQuantity(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := stock("A", "QSTK", 200)
	in.Holdings = []Holding{h}
	for i, d := range quarterly {
		qty := num(100)
		if i == len(quarterly)-1 {
			qty = nil
		}
		pay(in, h.HoldingKey, "A", d, 100, qty)
	}
	dividendOf(t, Compute(in), h, 4, 800)
	for i := range in.Payments[h.HoldingKey] {
		in.Payments[h.HoldingKey][i].Quantity = nil
	}
	if p := position(Compute(in), h.HoldingKey); p.DividendYield != nil || !p.Tags[TagUnknown] {
		t.Errorf("no quantity: yield %v tags %v", p.DividendYield, p.Tags)
	}
}

// The lines of one month are one payment, though days apart: a
// semiannual payer whose every payment comes with a second, smaller line
// days before it pays both, twice a year.
func TestASemiannualPayerWithTwoLinesInAMonth(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := stock("A", "QSTK", 100)
	in.Holdings = []Holding{h}
	for _, m := range []int{7, 13} {
		pay(in, h.HoldingKey, "A", date(2025, m, 6), 5, num(100))
		pay(in, h.HoldingKey, "A", date(2025, m, 20), 50, num(100))
	}
	b := Compute(in)
	dividendOf(t, b, h, 0.55*2, 110)
	near(t, "January", num(incomeOf(b, h.HoldingKey, monthIndex(b, *date(2027, 1, 1)), RuleCash, PathConstant, 0)), 55,
		1e-9)
}

// A special dividend stays out of the indicated rate, but it is income
// received: the realized yield counts it.
func TestASpecialDividend(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := stock("A", "QSTK", 100)
	in.Holdings = []Holding{h}
	for _, d := range quarterly {
		pay(in, h.HoldingKey, "A", d, 50, num(100))
	}
	pay(in, h.HoldingKey, "A", date(2026, 4, 20), 300, num(100))
	in.Income[h.HoldingKey] = Received{TypeDividends: 4*50 + 300}
	in.Daily[h.HoldingKey] = Daily{Days: 365, Spans: []Span{{From: 0, To: 365, Amount: 10000}}}
	p := dividendOf(t, Compute(in), h, 2, 200)
	near(t, "realized", p.Realized, 5, 1e-9)
}

// A dividend's rate in units that changed since by a factor no whole
// number explains stays as it was, and the holding carries units.
func TestADividendInOldUnits(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := stock("A", "QSTK", 150)
	in.Holdings = []Holding{h}
	for _, d := range quarterly {
		pay(in, h.HoldingKey, "A", d, 50, num(100))
		in.Payments[h.HoldingKey][len(in.Payments[h.HoldingKey])-1].Units = true
	}
	if p := dividendOf(t, Compute(in), h, 2, 300); !p.Tags[TagUnits] || !p.Tags[TagIndicated] {
		t.Errorf("tags %v", p.Tags)
	}
}

// A payment above twice the median rate of the payments is a special,
// a one-off return of capital beside a yearly dividend among them: its
// rate counts nothing, and its month, off the payer's schedule, counts in
// no frequency. With fewer than three rates in the year, the median is
// that of the 24 months; with fewer than three there too, a special is a
// rate above twice the smallest.
func TestASpecialAmongFewPayments(t *testing.T) {
	t.Parallel()
	for _, days := range [][]*time.Time{{date(2024, 9, 10), date(2025, 9, 10)}, {date(2025, 9, 10)}} {
		in := inputs()
		h := stock("A", "QADR", 100)
		in.Holdings = []Holding{h}
		for _, d := range days {
			pay(in, h.HoldingKey, "A", d, 10, num(100))
		}
		pay(in, h.HoldingKey, "A", date(2026, 3, 10), 100, num(100))
		b := Compute(in)
		dividendOf(t, b, h, 0.1, 10)
		near(t, "September", num(incomeOf(b, h.HoldingKey, 3, RuleCash, PathConstant, 0)), 10, 1e-9)
	}
}

// A semiannual payer of a small interim and a larger final dividend,
// trimmed to a quarter between the final's record day and its pay day:
// the final's rate reads ten times the interim's before it. Paid on under
// two thirds what it held RecordLagDays before, it rates nothing. Where no
// quantity shows the trim, the median of the 24 months reads it as a
// special, which the median of the year's two rates would not; its
// month still counts, as the same month a year earlier paid. Either way
// the year pays the interim and the final of a year earlier.
func TestASemiannualPayerTrimmedBeforeItsPayDay(t *testing.T) {
	t.Parallel()
	for _, before := range []float64{400, 100} {
		in := inputs()
		h := stock("A", "QADR", 100)
		in.Holdings = []Holding{h}
		for _, p := range []struct {
			d    *time.Time
			rate float64
		}{{date(2024, 12, 10), 0.1}, {date(2025, 6, 10), 0.25}, {date(2025, 12, 10), 0.1}} {
			pay(in, h.HoldingKey, "A", p.d, p.rate*before, num(before))
		}
		pay(in, h.HoldingKey, "A", date(2026, 6, 10), 0.25*400, num(100))
		trimmed(in, h.HoldingKey, before)
		dividendOf(t, Compute(in), h, 0.1+0.25, (0.1+0.25)*100)
	}
}

// A yearly payer trimmed over the year, not between its record day and
// its pay day, pays the rate of its last payment: under half what it held
// on its previous pay day is no trim the payment shows.
func TestAYearlyPayerTrimmedOverTheYear(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := stock("A", "QADR", 300)
	in.Holdings = []Holding{h}
	pay(in, h.HoldingKey, "A", date(2025, 5, 20), 0.6*700, num(700))
	pay(in, h.HoldingKey, "A", date(2026, 5, 20), 0.7*300, num(300))
	dividendOf(t, Compute(in), h, 0.7, 0.7*300)
}

// A payer twice a year of a small interim and a larger final pays both a
// year, whichever came last: not the final twice, nor the interim. Each
// half pays its own: the next payment is the one a year after the
// payment before last. A final three times the interim is no special
// among two or three payments.
func TestAnUnevenSemiannualPayer(t *testing.T) {
	t.Parallel()
	days := []*time.Time{date(2024, 9, 10), date(2025, 3, 10), date(2025, 9, 10), date(2026, 3, 10)}
	for _, c := range []struct {
		rates []float64
		from  int
	}{{[]float64{0.1, 0.3, 0.1, 0.3}, 0}, {[]float64{0.3, 0.1, 0.3, 0.1}, 0}, {[]float64{0.1, 0.3, 0.1, 0.3}, 1},
		{[]float64{0.1, 0.3, 0.1, 0.3}, 2}} {
		in := inputs()
		h := stock("A", "QADR", 100)
		in.Holdings = []Holding{h}
		for i := c.from; i < len(days); i++ {
			pay(in, h.HoldingKey, "A", days[i], c.rates[i]*100, num(100))
		}
		b := Compute(in)
		p := dividendOf(t, b, h, 0.4, 40)
		if !p.Tags[TagIndicated] {
			t.Errorf("%v from %d: tags %v", c.rates, c.from, p.Tags)
		}
		for i, d := range []*time.Time{date(2026, 9, 1), date(2027, 3, 1)} {
			near(t, "a half", num(incomeOf(b, h.HoldingKey, monthIndex(b, *d), RuleCash, PathConstant, 0)),
				c.rates[2+i]*100, 1e-9)
		}
	}
}

// The median of the year's rates reads a raise as no special: a
// quarterly payer that raised its dividend two and a half times pays the
// new rate.
func TestADividendRaise(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := stock("A", "QSTK", 100)
	in.Holdings = []Holding{h}
	for i, d := range []*time.Time{date(2024, 9, 10), date(2024, 12, 10), date(2025, 3, 10), date(2025, 6, 10),
		date(2025, 9, 10), date(2025, 12, 10), date(2026, 3, 10), date(2026, 6, 10)} {
		amount := 20.0
		if i >= 6 {
			amount = 50
		}
		pay(in, h.HoldingKey, "A", d, amount, num(100))
	}
	dividendOf(t, Compute(in), h, 0.5*4, 200)
}

// The accounts paid on one day pay its median rate: an account that
// reads far above the two others moves it nothing.
func TestThreeAccountsAndAnOutlier(t *testing.T) {
	t.Parallel()
	in := inputs()
	a, b, c := stock("A", "QSTK", 100), stock("B", "QSTK", 100), stock("C", "QSTK", 100)
	in.Holdings = []Holding{a, b, c}
	for _, d := range quarterly {
		pay(in, a.HoldingKey, "A", d, 50, num(100))
		pay(in, b.HoldingKey, "B", d, 50, num(100))
		pay(in, c.HoldingKey, "C", d, 150, num(100))
	}
	dividendOf(t, Compute(in), a, 2, 200)
}

// A holding topped up between the record day and the pay day is paid on
// what it held before, but holds more on the day: paid on over one and a
// half times what it held RecordLagDays before, that day rates nothing,
// and the rate before it holds.
func TestATopUpBeforeThePayDay(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := stock("A", "QSTK", 1000)
	in.Holdings = []Holding{h}
	for i, d := range quarterly {
		qty := num(100)
		if i == len(quarterly)-1 {
			qty = num(1000)
		}
		pay(in, h.HoldingKey, "A", d, 50, qty)
	}
	trimmed(in, h.HoldingKey, 100)
	dividendOf(t, Compute(in), h, 2, 2000)
}

// A security held at two sources pools there as at one: the source whose
// account was never paid shows the rate the other's payments give.
func TestASecurityAtTwoSources(t *testing.T) {
	t.Parallel()
	in := inputs()
	a, b := stock("A", "QSTK", 100), stock("A", "QSTK", 300)
	a.SecurityID, b.Source, b.SecurityID = "symbol:QSTK", "bank", "symbol:QSTK"
	in.Holdings = []Holding{a, b}
	for _, d := range quarterly {
		pay(in, a.HoldingKey, "A", d, 50, num(100))
	}
	book := Compute(in)
	dividendOf(t, book, a, 2, 200)
	dividendOf(t, book, b, 2, 600)
}

// A sold position's received income is withheld as its instrument's
// holding's is, at its own account's wrapper.
func TestASoldPositionsWithholding(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := stock("A", "QSTK", 100)
	in.Holdings = []Holding{h}
	in.Held[InstrumentKey{"broker", "QSTK"}] = []string{"A", "S"}
	in.Situs[h.HoldingKey] = "XX"
	in.Accounts[AccountKey{"broker", "S"}] = Account{TaxWrapper: "retirement"}
	in.Withholding["XX"] = map[string]float64{canonical.WithholdingDefault: 30, "retirement": 15}
	sold, _ := in.Sold("broker", "S", "QSTK")
	w := Compute(in).Projection.Withholding
	if w[h.HoldingKey] != 30 || w[sold] != 15 {
		t.Errorf("withholding %v", w)
	}
}
