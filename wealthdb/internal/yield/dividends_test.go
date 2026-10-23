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
// the year pays the interim in December and the final of a year earlier
// in June.
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
		b := Compute(in)
		dividendOf(t, b, h, 0.1+0.25, (0.1+0.25)*100)
		for _, m := range []struct {
			d    *time.Time
			want float64
		}{{date(2026, 12, 1), 10}, {date(2027, 6, 1), 25}} {
			near(t, m.d.Format("January"), num(incomeOf(b, h.HoldingKey, monthIndex(b, *m.d), RuleCash, PathConstant,
				0)), m.want, 1e-9)
		}
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

// A security moved from one source to another pools what the first paid
// and held, under its own instrument there (HeldSecurity): its pay months
// count in the frequency, and its income and its amounts, in their own
// currency, in the realized yield. A broker's account that sold it a
// month ago was paid three quarters, the bank's holding the last; some
// source held it every day.
func TestASecurityMovedBetweenSources(t *testing.T) {
	t.Parallel()
	in := inputs()
	in.FXRates["CHF"] = 1.25
	h := stock("B", "QBNK", 100)
	h.Source, h.SecurityID = "bank", "isin:XX0000000001"
	in.Holdings = []Holding{h}
	old := InstrumentKey{"broker", "QOLD"}
	in.Held[old] = []string{"A"}
	in.HeldSecurity[old], in.HeldSecurity[InstrumentKey{"bank", "QBNK"}] = h.SecurityID, h.SecurityID
	sold := HoldingKey{"broker", "A", "QOLD"}
	for _, d := range quarterly[:3] {
		pay(in, sold, "A", d, 50, num(100))
	}
	pay(in, h.HoldingKey, "B", quarterly[3], 50, num(100))
	in.Income[sold], in.Income[h.HoldingKey] = Received{TypeDividends: 150}, Received{TypeDividends: 50}
	in.Daily[h.HoldingKey] = Daily{Days: 365, Spans: []Span{{From: 335, To: 365, Amount: 10000}}}
	in.FormerDaily[old] = []Daily{{Days: 365, Currency: "CHF", Spans: []Span{{From: 0, To: 335, Amount: 8000}}}}
	p := dividendOf(t, Compute(in), h, 2, 200)
	near(t, "realized", p.Realized, 200.0/10000*100, 1e-9)
	if p.Tags[TagPartial] || p.Tags[TagThin] {
		t.Errorf("tags %v", p.Tags)
	}
}

// The record day's lag is known for a receipt of a Japanese share, of a
// share of the United Kingdom, of a share of a market with a business-day
// lag, and of any other share that is known. A share reads the market of
// the currency it is quoted in, whatever its issuer's country: the United
// States, the United Kingdom, Hong Kong, Switzerland, Denmark or Sweden.
// One in euros reads its issuer's market, where that is of the euro area.
// A business-day lag counts its market's business days. Any other knows
// none. Each lag's day lies in its record window, and no window reaches
// further than RecordWindowDays.
func TestRecordLag(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		situs, ccy     string
		receipt, share bool
		want           Lag
	}{
		{"JP", "USD", true, false, RecordLagJapaneseReceipt}, {"DE", "USD", true, false, RecordLagEuropeanReceipt},
		{"SE", "USD", true, false, RecordLagEuropeanReceipt}, {"FI", "USD", true, false, RecordLagReceipt},
		{"GB", "USD", true, false, RecordLagUKReceipt}, {"", "USD", true, false, Lag{}},
		{"US", "USD", false, true, RecordLagUSShare}, {"CH", "USD", false, true, RecordLagUSShare},
		{"", "USD", false, true, RecordLagUSShare},
		{"CH", "CHF", false, true, RecordLagEuropeanShare.of("CH")},
		{"IT", "EUR", false, true, RecordLagEuropeanShare.of("IT")},
		{"DK", "DKK", false, true, RecordLagEuropeanShare.of("DK")},
		{"IT", "CHF", false, true, RecordLagEuropeanShare.of("CH")},
		{"AT", "EUR", false, true, RecordLagEuropeanShare.of("AT")},
		{"DE", "EUR", false, true, RecordLagEuropeanShare.of("DE")},
		{"SE", "SEK", false, true, RecordLagSwedishShare}, {"FI", "EUR", false, true, Lag{}},
		{"NL", "EUR", false, true, Lag{}}, {"NO", "NOK", false, true, Lag{}}, {"PT", "EUR", false, true, Lag{}},
		{"GB", "GBP", false, true, RecordLagUKHongKongShare}, {"GB", "GBX", false, true, RecordLagUKHongKongShare},
		{"IE", "GBP", false, true, RecordLagUKHongKongShare},
		{"HK", "HKD", false, true, RecordLagUKHongKongShare}, {"CN", "HKD", false, true, RecordLagUKHongKongShare},
		{"JP", "JPY", false, true, Lag{}}, {"US", "USD", false, false, Lag{}},
	} {
		if got := RecordLag(c.situs, c.ccy, c.receipt, c.share); got != c.want {
			t.Errorf("%q in %s, receipt %v, share %v: %+v, want %+v", c.situs, c.ccy, c.receipt, c.share, got, c.want)
		}
	}
	for _, l := range recordLags {
		if l.Days < l.Near || l.Days > l.Far || l.Far > RecordWindowDays {
			t.Errorf("%+v: its day lies outside its window, or reaches past %d days", l, RecordWindowDays)
		}
	}
}

// A business day's lag steps over the weekend and the closing days of its
// market's settlement system. Two business days before a Monday or a
// Tuesday pay day fall on the Thursday or the Friday, before a Wednesday
// on the Monday; four before a Monday on the Tuesday. A calendar lag
// counts every day. TARGET2 settles on 2 January and on 31 December: two
// business days before Monday 5 January 2026, a French or German share's
// ex-day is Wednesday 31 December. Switzerland closes on 2 January and on
// 31 December too: a Swiss share's steps over them to Monday 29 December.
// Easter, Ascension, Whit Monday, 1 May and each market's own days step
// over the same way, where its market closes: Denmark settles on 1 May.
// The windows count the lag's own days: two to four business days before
// a Monday reach from the Tuesday to the Thursday; four to six from the
// Thursday before, 1 May closed, to the Tuesday; two to four before 5
// January 2026 from 29 to 31 December in the euro area, from 22 to 29
// December in Switzerland; 3 to 35 calendar days from 6 April to 8 May.
func TestARecordDay(t *testing.T) {
	t.Parallel()
	share := func(situs, ccy string) Lag { return RecordLag(situs, ccy, false, true) }
	for _, c := range []struct {
		lag       Lag
		pay, want *time.Time
	}{
		{RecordLagEuropeanShare, date(2026, 5, 11), date(2026, 5, 7)},
		{RecordLagEuropeanShare, date(2026, 5, 12), date(2026, 5, 8)},
		{RecordLagEuropeanShare, date(2026, 5, 13), date(2026, 5, 11)},
		{RecordLagSwedishShare, date(2026, 5, 11), date(2026, 5, 5)},
		{RecordLagUSShare, date(2026, 5, 11), date(2026, 4, 11)},
		{share("CH", "CHF"), date(2026, 1, 5), date(2025, 12, 29)},
		{share("DE", "EUR"), date(2026, 1, 5), date(2025, 12, 31)},
		{share("FR", "EUR"), date(2026, 1, 5), date(2025, 12, 31)},
		// 24 December in Switzerland, not in TARGET2.
		{share("CH", "CHF"), date(2025, 12, 29), date(2025, 12, 22)},
		{share("DE", "EUR"), date(2025, 12, 29), date(2025, 12, 23)},
		// TARGET2 closes on 1 January: a French share paid on Friday 2
		// January reads Tuesday 30 December.
		{share("FR", "EUR"), date(2026, 1, 2), date(2025, 12, 30)},
		// Maundy Thursday, Good Friday and Easter Monday in Denmark; Good
		// Friday and Easter Monday in Germany.
		{share("DK", "DKK"), date(2026, 4, 8), date(2026, 4, 1)},
		{share("DE", "EUR"), date(2026, 4, 8), date(2026, 4, 2)},
		// Ascension and Whit Monday close Switzerland, not TARGET2: Austria,
		// Germany and France settle on them.
		{share("CH", "CHF"), date(2026, 5, 15), date(2026, 5, 12)},
		{share("DE", "EUR"), date(2026, 5, 15), date(2026, 5, 13)},
		{share("CH", "CHF"), date(2026, 5, 26), date(2026, 5, 21)},
		{share("AT", "EUR"), date(2026, 5, 26), date(2026, 5, 22)},
		{share("FR", "EUR"), date(2026, 5, 26), date(2026, 5, 22)},
		// 1 August in Switzerland; TARGET2 settles on 15 August, Italy's too.
		{share("CH", "CHF"), date(2025, 8, 5), date(2025, 7, 31)},
		{share("IT", "EUR"), date(2025, 8, 18), date(2025, 8, 14)},
		// Denmark settles on 1 May, and closes on Ascension and the day
		// after.
		{share("DK", "DKK"), date(2026, 5, 4), date(2026, 4, 30)},
		{share("DE", "EUR"), date(2026, 5, 4), date(2026, 4, 29)},
		{share("DK", "DKK"), date(2026, 5, 18), date(2026, 5, 12)},
		// 6 January, 6 June and Midsummer Eve in Sweden, 5 June in Denmark.
		{share("SE", "SEK"), date(2026, 1, 8), date(2025, 12, 30)},
		{share("SE", "SEK"), date(2025, 6, 11), date(2025, 6, 4)},
		{share("SE", "SEK"), date(2026, 6, 24), date(2026, 6, 17)},
		{share("DK", "DKK"), date(2026, 6, 9), date(2026, 6, 4)},
	} {
		if got := c.lag.RecordDay(*c.pay); !got.Equal(*c.want) {
			t.Errorf("%+v before %s: %s, want %s", c.lag, c.pay.Format(time.DateOnly), got.Format(time.DateOnly),
				c.want.Format(time.DateOnly))
		}
	}
	for _, c := range []struct {
		lag           Lag
		pay, from, to *time.Time
	}{
		{RecordLagEuropeanShare, date(2026, 5, 11), date(2026, 5, 5), date(2026, 5, 7)},
		{RecordLagSwedishShare, date(2026, 5, 11), date(2026, 4, 30), date(2026, 5, 5)},
		{share("FR", "EUR"), date(2026, 1, 5), date(2025, 12, 29), date(2025, 12, 31)},
		{share("CH", "CHF"), date(2026, 1, 5), date(2025, 12, 22), date(2025, 12, 29)},
		{RecordLagUSShare, date(2026, 5, 11), date(2026, 4, 6), date(2026, 5, 8)},
	} {
		if from, to := c.lag.Window(*c.pay); !from.Equal(*c.from) || !to.Equal(*c.to) {
			t.Errorf("%+v before %s: window %s to %s, want %s to %s", c.lag, c.pay.Format(time.DateOnly),
				from.Format(time.DateOnly), to.Format(time.DateOnly), c.from.Format(time.DateOnly),
				c.to.Format(time.DateOnly))
		}
	}
}

// Easter Sunday follows from the year by the Gregorian computus, from 22
// March to 25 April.
func TestEaster(t *testing.T) {
	t.Parallel()
	for _, want := range []*time.Time{date(2000, 4, 23), date(2008, 3, 23), date(2011, 4, 24), date(2019, 4, 21),
		date(2024, 3, 31), date(2025, 4, 20), date(2026, 4, 5), date(2038, 4, 25), date(2285, 3, 22)} {
		if got := easter(want.Year()); !got.Equal(*want) {
			t.Errorf("%d: %s, want %s", want.Year(), got.Format(time.DateOnly), want.Format(time.DateOnly))
		}
	}
}

// A payment whose market fixes its record day, where its account held
// some then, is paid on that quantity (Record), and no guard applies: a
// holding halved between its record day and its pay day reads the raise
// it was paid. Where the account held none on that day, or none is
// known, it rates on its pay day's quantity under the guard, as where no
// record day is known: paid on under two thirds of the quantity that
// guards it, it rates nothing, and the rate before it holds; with no such
// quantity, it reads its own.
func TestARecordDaysQuantity(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		lag               int
		record, before    *float64
		amount, qty, rate float64
	}{
		{RecordLagUSShare.Days, num(100), num(300), 60, 50, 0.6},
		{RecordLagUSShare.Days, nil, num(300), 40, 50, 0.5},
		{RecordLagUSShare.Days, nil, nil, 60, 100, 0.6}, {0, nil, num(300), 40, 50, 0.5},
	} {
		in := inputs()
		h := stock("A", "QSTK", 50)
		in.Holdings = []Holding{h}
		for _, d := range quarterly[:3] {
			pay(in, h.HoldingKey, "A", d, 50, num(100))
		}
		p := Payment{Day: *quarterly[3], Amount: c.amount, Account: "A", Quantity: num(c.qty), Record: c.record,
			Before: c.before, LagDays: c.lag, Type: TypeDividends}
		if c.lag > 0 {
			p.RecordSource = RecordLagged
		}
		in.Payments[h.HoldingKey] = append(in.Payments[h.HoldingKey], p)
		dividendOf(t, Compute(in), h, c.rate*4, c.rate*4*50)
	}
}

// A final well above the interim, among few rates, reads as a special.
// Where its month counts in the frequency, the same month a year earlier
// having paid, and its half has no regular rate within 400 days (the
// earlier final lies 410 days back), it stands as that half's rate, up to
// six times the interim: the year pays the interim and the final, each
// in its own month, and nothing is partial. A final paid with a special,
// twelve times the interim, where the final a year earlier was twice it,
// is no half's payment: the final a year earlier stands for it, and the
// year pays 0.3. Where that one has no rate either, the year pays the
// interim once, and is partial. On 25 November, after an interim of no
// rate a year after the last rated one, the final still comes next, in
// May, and the interim in November.
func TestAnUnevenFinalIsNoSpecial(t *testing.T) {
	t.Parallel()
	// read books the payments; a prior final below 0 has no rate.
	read := func(day *time.Time, prior, final float64, more ...*time.Time) (*Book, Holding) {
		in := inputs()
		in.Day, in.AsOf = *day, day.Unix()+86399
		h := stock("A", "QADR", 100)
		in.Holdings = []Holding{h}
		for _, p := range []struct {
			d    *time.Time
			rate float64
		}{{date(2024, 11, 10), 0.1}, {date(2025, 5, 1), prior}, {date(2025, 11, 10), 0.1}, {date(2026, 5, 10), final}} {
			if p.rate < 0 {
				pay(in, h.HoldingKey, "A", p.d, 20, nil)
				continue
			}
			pay(in, h.HoldingKey, "A", p.d, p.rate*100, num(100))
		}
		for _, d := range more {
			pay(in, h.HoldingKey, "A", d, 10, nil)
		}
		return Compute(in), h
	}
	month := func(b *Book, h Holding, d *time.Time) *float64 {
		return num(incomeOf(b, h.HoldingKey, monthIndex(b, *d), RuleCash, PathConstant, 0))
	}
	b, h := read(date(2026, 6, 15), 0.12, 0.3)
	if p := dividendOf(t, b, h, 0.1+0.3, 40); p.Tags[TagPartial] {
		t.Errorf("tags %v", p.Tags)
	}
	near(t, "November", month(b, h, date(2026, 11, 1)), 10, 1e-9)
	b, h = read(date(2026, 6, 15), 0.2, 1.2)
	if p := dividendOf(t, b, h, 0.1+0.2, 30); p.Tags[TagPartial] {
		t.Errorf("a final and a special of twelve times: tags %v", p.Tags)
	}
	b, h = read(date(2026, 6, 15), -1, 1.2)
	if p := dividendOf(t, b, h, 0.1, 10); !p.Tags[TagPartial] {
		t.Errorf("a final and a special of twelve times, no final a year earlier: tags %v", p.Tags)
	}
	b, h = read(date(2026, 11, 25), 0.12, 0.3, date(2026, 11, 10))
	near(t, "May", month(b, h, date(2027, 5, 1)), 30, 1e-9)
	near(t, "November", month(b, h, date(2027, 11, 1)), 10, 1e-9)
}

// A payer that raised its dividend steeply pays its new rate on every pay
// day since: a rate that holds the level of the newest pay days, two or
// more within a third of one another, in more than one month, is no
// special. A quarterly payer whose dividend rose tenfold two payments ago,
// its newest a little higher, pays the new rate four times a year, both
// new days regular.
func TestASteepRaiseIsNoSpecial(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := stock("A", "QSTK", 100)
	in.Holdings = []Holding{h}
	for i, rate := range []float64{0.1, 0.1, 1, 1.2} {
		pay(in, h.HoldingKey, "A", quarterly[i], rate*100, num(100))
	}
	for _, d := range payDays(in, h) {
		if d.special {
			t.Errorf("%s reads as a special", d.day.Format(time.DateOnly))
		}
	}
	dividendOf(t, Compute(in), h, 1.2*4, 1.2*4*100)
}

// A payer twice a year with a small interim and a large final that
// recurs: the final, above six times the interim and a special by the bar,
// stands as its half's rate where the final a year earlier paid within a
// third of it. The year pays both halves, and nothing is partial.
func TestARecurringLargeFinalStands(t *testing.T) {
	t.Parallel()
	in := inputs()
	in.Day, in.AsOf = *date(2026, 10, 20), date(2026, 10, 20).Unix()+86399
	h := stock("A", "QADR", 100)
	in.Holdings = []Holding{h}
	for _, p := range []struct {
		d    *time.Time
		rate float64
	}{{date(2024, 12, 10), 0.1}, {date(2025, 7, 10), 0.5}, {date(2025, 12, 10), 0.1}, {date(2026, 7, 10), 0.65}} {
		pay(in, h.HoldingKey, "A", p.d, p.rate*100, num(100))
	}
	if p := dividendOf(t, Compute(in), h, 0.1+0.65, 75); p.Tags[TagPartial] {
		t.Errorf("tags %v", p.Tags)
	}
}

// A payer twice a year whose newest final has no rate (its quantity is
// unknown) takes the final a year earlier, though that lies more than 400
// days back: the year pays both halves, and nothing is partial. Only the
// same month of the year stands in: where the final a year earlier fell a
// month off, in June, the interim pays once a year, partial.
func TestALostHalfTakesItsMonthAYearEarlier(t *testing.T) {
	t.Parallel()
	for _, prior := range []*time.Time{date(2025, 7, 10), date(2025, 6, 10)} {
		in := inputs()
		in.Day, in.AsOf = *date(2026, 9, 20), date(2026, 9, 20).Unix()+86399
		h := stock("A", "QSTK", 100)
		in.Holdings = []Holding{h}
		pay(in, h.HoldingKey, "A", date(2024, 10, 10), 20, num(100))
		pay(in, h.HoldingKey, "A", prior, 25, num(100))
		pay(in, h.HoldingKey, "A", date(2025, 10, 10), 20, num(100))
		pay(in, h.HoldingKey, "A", date(2026, 7, 10), 25, nil)
		yield, partial := 0.2+0.25, false
		if prior.Month() != time.July {
			yield, partial = 0.2, true
		}
		if p := dividendOf(t, Compute(in), h, yield, yield*100); p.Tags[TagPartial] != partial {
			t.Errorf("a final a year earlier in %s: tags %v", prior.Month(), p.Tags)
		}
	}
}

// The other half of a payer twice a year lies 1 to 10 months from its
// last rated month, before it or after: a special 11 months after it is
// that month's own payment a year on. A payer of 0.1 whose last rated
// month is May 2025, April 2025 paying too, has paid two specials since,
// each in a month that paid a year earlier: 0.4 in November and 0.8 in
// April 2026. November's stands as the other half's rate, under six
// times May's, and the year pays 0.4 and 0.1, each in its own month.
func TestTheOtherHalfIsNoPaymentAYearOn(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := stock("A", "QADR", 100)
	in.Holdings = []Holding{h}
	for _, p := range []struct {
		d    *time.Time
		rate float64
	}{{date(2024, 11, 20), 0.1}, {date(2025, 4, 20), 0.1}, {date(2025, 5, 20), 0.1}, {date(2025, 11, 20), 0.4},
		{date(2026, 4, 20), 0.8}} {
		pay(in, h.HoldingKey, "A", p.d, p.rate*100, num(100))
	}
	b := Compute(in)
	if p := dividendOf(t, b, h, 0.4+0.1, 50); p.Tags[TagPartial] {
		t.Errorf("tags %v", p.Tags)
	}
	for _, m := range []struct {
		what   string
		d      *time.Time
		income float64
	}{{"November", date(2026, 11, 1), 40}, {"May", date(2027, 5, 1), 10}} {
		near(t, m.what, num(incomeOf(b, h.HoldingKey, monthIndex(b, *m.d), RuleCash, PathConstant, 0)), m.income,
			1e-9)
	}
}

// A month 11 months from the last rated one is that month's own payment
// a year on, never the other half. A payer twice a year rated its interim
// of 0.4 in December 2024 and its final of 0.6 on 1 July 2025; its interim
// of December 2025 and its final of 12 June 2026 have no rate. The other
// half's newest month is December 2025, not June: its month a year earlier
// stands for it, and the year pays both halves. The last pay day, in June,
// was the final's half, so the interim pays next, in December.
func TestTheLostHalfIsNoPaymentAYearOn(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := stock("A", "QSTK", 100)
	in.Holdings = []Holding{h}
	pay(in, h.HoldingKey, "A", date(2024, 12, 10), 40, num(100))
	pay(in, h.HoldingKey, "A", date(2025, 7, 1), 60, num(100))
	pay(in, h.HoldingKey, "A", date(2025, 12, 10), 40, nil)
	pay(in, h.HoldingKey, "A", date(2026, 6, 12), 60, nil)
	b := Compute(in)
	if p := dividendOf(t, b, h, 0.4+0.6, 100); p.Tags[TagPartial] {
		t.Errorf("tags %v", p.Tags)
	}
	for _, m := range []struct {
		what   string
		d      *time.Time
		income float64
	}{{"December", date(2026, 12, 1), 40}, {"June", date(2027, 6, 1), 60}} {
		near(t, m.what, num(incomeOf(b, h.HoldingKey, monthIndex(b, *m.d), RuleCash, PathConstant, 0)), m.income,
			1e-9)
	}
}

// A month 13 months after the last rated one is that month's own payment a
// year on too, drifted. A payer twice a year rated its interim of 0.4 in
// November 2024 and its final of 0.6 on 20 May 2025; its interim of
// November 2025 and its final of 10 June 2026 have no rate. The other
// half's newest month is November 2025, not June: its month a year
// earlier stands for it, and each half pays a year after its newest pay
// day, the interim in November, the final in June.
func TestAPaymentThirteenMonthsOnIsItsOwnHalf(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := stock("A", "QSTK", 100)
	in.Holdings = []Holding{h}
	pay(in, h.HoldingKey, "A", date(2024, 11, 20), 40, num(100))
	pay(in, h.HoldingKey, "A", date(2025, 5, 20), 60, num(100))
	pay(in, h.HoldingKey, "A", date(2025, 11, 20), 40, nil)
	pay(in, h.HoldingKey, "A", date(2026, 6, 10), 60, nil)
	b := Compute(in)
	if p := dividendOf(t, b, h, 0.4+0.6, 100); p.Tags[TagPartial] {
		t.Errorf("tags %v", p.Tags)
	}
	for _, m := range []struct {
		what   string
		d      *time.Time
		income float64
	}{{"November", date(2026, 11, 1), 40}, {"June", date(2027, 6, 1), 60}} {
		near(t, m.what, num(incomeOf(b, h.HoldingKey, monthIndex(b, *m.d), RuleCash, PathConstant, 0)), m.income,
			1e-9)
	}
}

// Two specials of one size between regular pay days stay specials: each
// holds no level of the newest pay days, as a regular one follows it. A
// quarterly payer of 0.25 pays 1.00 more on 20 December and on 20 March:
// the year pays 0.25 four times. So it does where the regular pay day
// between them, 10 March, has no rate: a pay day of no rate ends the run.
func TestTwoSpecialsOfOneSizeStaySpecials(t *testing.T) {
	t.Parallel()
	for _, lost := range []bool{false, true} {
		in := inputs()
		in.Day, in.AsOf = *date(2026, 4, 15), date(2026, 4, 15).Unix()+86399
		h := stock("A", "QSTK", 100)
		in.Holdings = []Holding{h}
		for _, d := range []*time.Time{date(2025, 6, 10), date(2025, 9, 10), date(2025, 12, 10), date(2026, 3, 10)} {
			qty := num(100)
			if lost && d.Month() == time.March {
				qty = nil
			}
			pay(in, h.HoldingKey, "A", d, 25, qty)
		}
		pay(in, h.HoldingKey, "A", date(2025, 12, 20), 100, num(100))
		pay(in, h.HoldingKey, "A", date(2026, 3, 20), 100, num(100))
		dividendOf(t, Compute(in), h, 0.25*4, 100)
	}
}

// A level held only in one month of the year makes no regular rate: the
// newest pay day, repeated within a third by its own month a year
// earlier, stays a special. A payer of two small dividends pays 1.0 in
// June 2025 and 1.15 in June 2026: June 2026 is a special, and the year
// pays the June 2025 rate once.
func TestALevelInOneMonthIsNoRaise(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := stock("A", "QSTK", 100)
	in.Holdings = []Holding{h}
	for _, p := range []struct {
		d    *time.Time
		rate float64
	}{{date(2025, 1, 10), 0.1}, {date(2025, 2, 10), 0.1}, {date(2025, 6, 10), 1}, {date(2026, 6, 10), 1.15}} {
		pay(in, h.HoldingKey, "A", p.d, p.rate*100, num(100))
	}
	days := payDays(in, h)
	if n := len(days); n != 4 || !days[3].special || days[2].special {
		t.Errorf("specials %+v", days)
	}
	dividendOf(t, Compute(in), h, 1, 100)
}

// A rate on the record day's quantity stands where the account's most
// over its least across the record window is at most 1.1: B, which held
// 100 to 105 across it, or 64 to 70.4, exactly 1.1 times, rates its
// raise. Above that, a trade near the true record day leaves the quantity
// paid on unknown: A, which held 50 to 100, rates nothing, nor does B at
// 100 to 112, and the day's rate is the other account's. Alone, A's day
// still counts in the frequency, and the rate before it holds;
// unguarded, A's 0.8 would pay.
func TestARecordWindowGuardsTheRecordDay(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		least, most, rate float64
	}{{0, 0, 0.5}, {100, 105, 0.6}, {64, 70.4, 0.6}, {100, 112, 0.5}} {
		in := inputs()
		h := stock("A", "QSTK", 100)
		in.Holdings = []Holding{h}
		for _, d := range quarterly[:3] {
			pay(in, h.HoldingKey, "A", d, 50, num(100))
		}
		record := func(acct string, amount, least, most float64) Payment {
			return Payment{Day: *quarterly[3], Amount: amount, Account: acct, Quantity: num(least),
				Record: num(least), Least: num(least), Most: num(most), LagDays: RecordLagUSShare.Days,
				RecordSource: RecordLagged, Type: TypeDividends}
		}
		ps := append(in.Payments[h.HoldingKey], record("A", 40, 50, 100))
		if c.most > 0 {
			ps = append(ps, record("B", 0.6*c.least, c.least, c.most))
		}
		in.Payments[h.HoldingKey] = ps
		dividendOf(t, Compute(in), h, c.rate*4, c.rate*4*100)
	}
}

// The one-half rule counts calendar months: a payer twice a year whose
// pay day drifted earlier shows its June twice in the year, and with its
// December's rate lost, it pays the June rate once a year, partial.
func TestADriftedPayDayIsOneHalf(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := stock("A", "QSTK", 100)
	in.Holdings = []Holding{h}
	pay(in, h.HoldingKey, "A", date(2025, 6, 20), 20, num(100))
	pay(in, h.HoldingKey, "A", date(2025, 12, 10), 20, nil)
	pay(in, h.HoldingKey, "A", date(2026, 6, 10), 20, num(100))
	if p := dividendOf(t, Compute(in), h, 0.2, 20); !p.Tags[TagPartial] {
		t.Errorf("tags %v", p.Tags)
	}
}

// A year of three calendar months of payments, four months apart and
// one of them rated, reads twice a year (payFrequency). The one-half
// rule needs exactly two calendar months, so the rate stays at twice a
// year.
func TestThreePayMonthsAreNoHalf(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := stock("A", "QSTK", 100)
	in.Holdings = []Holding{h}
	pay(in, h.HoldingKey, "A", date(2025, 10, 10), 50, nil)
	pay(in, h.HoldingKey, "A", date(2026, 2, 10), 50, nil)
	pay(in, h.HoldingKey, "A", date(2026, 6, 10), 50, num(100))
	if p := dividendOf(t, Compute(in), h, 0.5*2, 100); p.Tags[TagPartial] {
		t.Errorf("tags %v", p.Tags)
	}
}

// A payer twice a year whose one half's rate is lost, its quantity
// unknown, pays the other half's once a year, in its own month, and its
// dividend covers part of the year: partial. With both known, it pays
// both, each in its own half: the interim in December, the final in June.
func TestASemiannualPayerWithAHalfLost(t *testing.T) {
	t.Parallel()
	for _, lost := range []bool{true, false} {
		in := inputs()
		h := stock("A", "QADR", 100)
		in.Holdings = []Holding{h}
		final := num(100)
		if lost {
			final = nil
		}
		pay(in, h.HoldingKey, "A", date(2025, 12, 10), 10, num(100))
		pay(in, h.HoldingKey, "A", date(2026, 6, 10), 25, final)
		b := Compute(in)
		month := func(d *time.Time) *float64 {
			return num(incomeOf(b, h.HoldingKey, monthIndex(b, *d), RuleCash, PathConstant, 0))
		}
		if !lost {
			if p := dividendOf(t, b, h, 0.1+0.25, 35); p.Tags[TagPartial] {
				t.Errorf("both halves: tags %v", p.Tags)
			}
			near(t, "December", month(date(2026, 12, 1)), 10, 1e-9)
			near(t, "June", month(date(2027, 6, 1)), 25, 1e-9)
			continue
		}
		if p := dividendOf(t, b, h, 0.1, 10); !p.Tags[TagPartial] || !p.Tags[TagIndicated] {
			t.Errorf("a half lost: tags %v", p.Tags)
		}
		near(t, "December", month(date(2026, 12, 1)), 10, 1e-9)
	}
}

// An account is one of a source: two sources' accounts of one id, paid
// on one day, pay the median of their rates, not one rate of their sum.
func TestTwoSourcesAccountsOfOneID(t *testing.T) {
	t.Parallel()
	in := inputs()
	a, b := stock("A", "QSTK", 100), stock("A", "QSTK", 300)
	a.SecurityID, b.Source, b.SecurityID = "symbol:QSTK", "bank", "symbol:QSTK"
	in.Holdings = []Holding{a, b}
	for _, d := range quarterly {
		pay(in, a.HoldingKey, "A", d, 50, num(100))
		pay(in, b.HoldingKey, "A", d, 150, num(300))
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
