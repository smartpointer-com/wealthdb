package yield

import (
	"strings"
	"testing"
	"time"
)

// The books here are invented. The day is 2026-06-15: its twelve months
// are the 365 days from 2025-06-16, day 0, to the day, day 364.

const year = 365

// held is a holding's daily amounts: each span an amount over its days.
func held(spans ...Span) Daily { return Daily{Days: year, Spans: spans} }

// dayOf is a day's index in the window.
func dayOf(d *time.Time) int { return int(d.Sub(*date(2025, 6, 16)).Hours() / 24) }

// A position's realized yield is over its mean amount on the days it was
// held: a payer of 4% a year held half the year reads the 2% that half
// paid, and carries partial. Its account's row, which counts a day not
// held as zero, reads 4%. Held a quarter of the year, it is thin: it has
// no realized yield, and carries thin.
func TestAHoldingHeldHalfTheYear(t *testing.T) {
	t.Parallel()
	in := inputs()
	half := Holding{HoldingKey: HoldingKey{"broker", "A", "QHALF"}, Instrument: "QHALF", AssetClass: "public_equity",
		Vehicle: "stock", Currency: "USD", Value: 10000, Amount: 10000, Quantity: num(100)}
	late := half
	late.HoldingKey, late.Instrument = HoldingKey{"broker", "B", "QLATE"}, "QLATE"
	in.Holdings = []Holding{half, late}
	in.Daily[half.HoldingKey] = held(Span{From: year - 183, To: year, Amount: 10000})
	in.Daily[late.HoldingKey] = held(Span{From: year - 91, To: year, Amount: 10000})
	in.Income[half.HoldingKey] = Received{TypeDividends: 10000 * 0.04 * 183 / year}
	in.Income[late.HoldingKey] = Received{TypeDividends: 10000 * 0.04 * 91 / year}
	b := Compute(in)
	p := position(b, half.HoldingKey)
	near(t, "half a year", p.Realized, 4*183.0/year, 1e-9)
	if p.Tags[TagThin] || !p.Tags[TagPartial] {
		t.Errorf("half a year: tags %v", p.Tags)
	}
	if q := position(b, late.HoldingKey); q.Realized != nil || !q.Tags[TagThin] || q.Tags[TagUnknown] ||
		q.Tags[TagPartial] {
		t.Errorf("a quarter: realized %v tags %v", q.Realized, q.Tags)
	}
	for _, r := range b.Aggregate(GrainAccounts, false) {
		if r.AccountID == "A" {
			near(t, "its account", r.Realized, 4, 1e-9)
		}
	}
}

// A position yields its instrument's realized yield: the income of every
// account that held it, one that sold it included, over the instrument's
// mean amount on the days any account held it. Every holding of it shows
// that figure, a part-year one too, and none is partial: some account held
// it every day. An account's row keeps its holdings' own income over
// their mean daily values: the part-year holding's account is thin.
func TestRealizedYieldPoolsTheInstrument(t *testing.T) {
	t.Parallel()
	in := inputs()
	long := Holding{HoldingKey: HoldingKey{"broker", "A", "QSTK"}, Instrument: "QSTK", AssetClass: "public_equity",
		Vehicle: "stock", Currency: "USD", Value: 10000, Amount: 10000, Quantity: num(100)}
	late := long
	late.HoldingKey, late.Value, late.Amount, late.Quantity = HoldingKey{"broker", "B", "QSTK"}, 5000, 5000, num(50)
	in.Holdings = []Holding{long, late}
	in.Daily[long.HoldingKey] = held(Span{From: 0, To: year, Amount: 10000})
	in.Daily[late.HoldingKey] = held(Span{From: year - 120, To: year, Amount: 5000})
	// An account that held 20,000 dollars of it to 200 days ago, whose
	// income its sold position keeps.
	in.FormerDaily[InstrumentKey{"broker", "QSTK"}] = []Daily{held(Span{From: 0, To: year - 200, Amount: 20000})}
	in.Held[InstrumentKey{"broker", "QSTK"}] = []string{"A", "B", "C"}
	in.Income[long.HoldingKey] = Received{TypeDividends: 200}
	in.Income[HoldingKey{"broker", "C", "QSTK"}] = Received{TypeDividends: 300}
	in.Income[late.HoldingKey] = Received{TypeDividends: 25}
	b := Compute(in)
	mean := (10000*year + 5000*120 + 20000*(year-200.0)) / year
	for _, h := range []Holding{long, late} {
		p := position(b, h.HoldingKey)
		near(t, h.Account, p.Realized, 525/mean*100, 1e-9)
		if p.Tags[TagThin] || p.Tags[TagPartial] {
			t.Errorf("%s: tags %v", h.Account, p.Tags)
		}
	}
	for _, r := range b.Aggregate(GrainAccounts, false) {
		switch r.AccountID {
		case "A":
			near(t, "the long account", r.Realized, 2, 1e-9)
		case "B":
			if r.Realized != nil || !strings.Contains(" "+r.Quality+" ", " thin ") {
				t.Errorf("the part-year account: realized %v quality %q", r.Realized, r.Quality)
			}
		}
	}
	// Held by no account on 100 of its days, its mean is that of the 265
	// it was held, and it is partial.
	in.Holdings, in.Held = []Holding{late}, map[InstrumentKey][]string{}
	in.FormerDaily[InstrumentKey{"broker", "QSTK"}] = []Daily{held(Span{From: 0, To: year - 220, Amount: 5000})}
	gap := position(Compute(in), late.HoldingKey)
	near(t, "a gap", gap.Realized, 25.0/5000*100, 1e-9)
	if !gap.Tags[TagPartial] {
		t.Errorf("a gap: tags %v", gap.Tags)
	}
}

// A cash balance held from the 5th to the 24th of every month is zero at
// every month end, but its mean daily value is what it held: two thirds
// of the year at its amount. Its account's interest over that mean is
// its realized yield.
func TestACashBalanceBetweenTheMonthEnds(t *testing.T) {
	t.Parallel()
	in := inputs()
	h := cash("bank", "C", "USD", 30000)
	in.Holdings = []Holding{h}
	var spans []Span
	days := 0
	for m := 0; m < 13; m++ {
		from, to := dayOf(date(2025, 6+m, 5)), dayOf(date(2025, 6+m, 25))
		from, to = max(from, 0), min(to, year)
		if from < to {
			spans, days = append(spans, Span{From: from, To: to, Amount: 30000}), days+to-from
		}
	}
	for _, s := range spans {
		for _, end := range []*time.Time{date(2025, 6, 30), date(2025, 12, 31), date(2026, 5, 31)} {
			if d := dayOf(end); d >= s.From && d < s.To {
				t.Fatalf("held at %v", end)
			}
		}
	}
	in.Daily[h.HoldingKey] = held(spans...)
	in.AccountIncome[AccountCurrency{AccountKey{"bank", "C"}, "USD"}] = Received{TypeInterest: 240}
	p := position(Compute(in), h.HoldingKey)
	near(t, "realized", p.Realized, 240/(30000*float64(days)/year)*100, 1e-9)
	if p.Tags[TagThin] || days < year/2 {
		t.Errorf("held %d days: tags %v", days, p.Tags)
	}
}

// An account thin on more than half its days has no realized yield, and
// its row carries thin whole: a savings balance that sat near zero until
// a deposit a month ago. A row the thin balance is part of, but which is
// not thin itself, yields its holdings' income over their summed mean
// daily values, and carries thin for the balance's share.
func TestAThinAccount(t *testing.T) {
	t.Parallel()
	in := inputs()
	savings := cash("bank", "S", "USD", 50000)
	stock := Holding{HoldingKey: HoldingKey{"broker", "B", "QSTK"}, Instrument: "QSTK", AssetClass: "public_equity",
		Vehicle: "stock", Currency: "USD", Value: 10000, Amount: 10000, Quantity: num(100)}
	in.Holdings = []Holding{savings, stock}
	in.Rates[AccountKey{"bank", "S"}] = []Rate{{Credit: true, Tier: 1, Pct: 2}}
	in.Daily[savings.HoldingKey] = held(Span{From: 0, To: year - 30, Amount: 100},
		Span{From: year - 30, To: year, Amount: 50000})
	in.Daily[stock.HoldingKey] = held(Span{From: 0, To: year, Amount: 10000})
	in.AccountIncome[AccountCurrency{AccountKey{"bank", "S"}, "USD"}] = Received{TypeInterest: 20}
	in.Income[stock.HoldingKey] = Received{TypeDividends: 200}
	b := Compute(in)
	if p := position(b, savings.HoldingKey); p.Realized != nil || !p.Tags[TagThin] || p.Tags[TagUnknown] {
		t.Errorf("savings: realized %v tags %v", p.Realized, p.Tags)
	}
	for _, r := range b.Aggregate(GrainAccounts, false) {
		switch {
		case r.Total:
			mean := 10000 + (100*float64(year-30)+50000*30)/year
			near(t, "total", r.Realized, 220/mean*100, 1e-9)
			if !strings.Contains(r.Quality, "thin=83%") {
				t.Errorf("total quality %q", r.Quality)
			}
		case r.AccountID == "S":
			if r.Realized != nil || !strings.Contains(" "+r.Quality+" ", " thin ") {
				t.Errorf("thin account: realized %v quality %q", r.Realized, r.Quality)
			}
		case r.AccountID == "B":
			near(t, "stock account", r.Realized, 2, 1e-9)
		}
	}
}

// An entity's realized yield is its holdings' income over the sum of
// their mean daily values, not a mean of their yields weighted by today's
// values: two holdings that traded places in size ten days ago.
func TestAnEntitysRealizedYield(t *testing.T) {
	t.Parallel()
	in := inputs()
	in.FXRates["CHF"] = 1.25
	grew := Holding{HoldingKey: HoldingKey{"broker", "A", "QGREW"}, Instrument: "QGREW", AssetClass: "public_equity",
		Vehicle: "stock", Currency: "USD", Value: 9000, Amount: 9000, Quantity: num(90)}
	shrank := grew
	shrank.HoldingKey, shrank.Instrument, shrank.Currency = HoldingKey{"broker", "A", "QSHRANK"}, "QSHRANK", "CHF"
	shrank.Amount, shrank.Value = 800, 1000
	in.Holdings = []Holding{grew, shrank}
	in.Daily[grew.HoldingKey] = held(Span{From: 0, To: year - 10, Amount: 1000}, Span{From: year - 10, To: year,
		Amount: 9000})
	in.Daily[shrank.HoldingKey] = held(Span{From: 0, To: year - 10, Amount: 7200}, Span{From: year - 10, To: year,
		Amount: 800})
	in.Income[grew.HoldingKey] = Received{TypeDividends: 50}
	in.Income[shrank.HoldingKey] = Received{TypeDividends: 270}
	b := Compute(in)
	// The mean daily values sum to 10,000 dollars: 1,219.18 and 8,780.82.
	// The last row is the total, or the account's one row converted.
	for _, converted := range []bool{false, true} {
		rows := b.Aggregate(GrainAccounts, converted)
		near(t, "pooled", rows[len(rows)-1].Realized, 320.0/10000*100, 1e-9)
	}
	near(t, "grew", position(b, grew.HoldingKey).Realized, 50/((1000*355+9000*10)/365.0)*100, 1e-9)
}
