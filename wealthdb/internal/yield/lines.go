package yield

import (
	"math"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/fixedincome"
)

// FundLine is one line of a fund's composition, as far as its kind, its
// yield and its duration go. Class is the asset class its asset type
// maps to, "" where none does.
type FundLine struct {
	Key                                        string
	Weight, Value, Quantity                    float64
	Currency                                   string
	Maturity                                   *time.Time
	Coupon, Rate, YTM, YTW, Duration, Modified *float64
	Kind, Class                                string
	Defaulted, InKind, Floats                  bool
}

// FundLines are a fund's lines at an instant and what each says of its
// security (ReadLine), in the same order.
type FundLines struct {
	Lines []FundLine
	Infos []LineInfo
}

// PickLines fills in.Lines from the lines of the funds of keys, sorted: a
// security's line is the one of the most value held, its weight times
// the value of the fund that holds it in in's currency; of equal ones,
// the first fund's, by its key, and the first line of it.
func PickLines(in *Inputs, funds map[string]*FundLines, keys []string) {
	held := map[string]float64{}
	for _, h := range in.Holdings {
		held[h.SecurityID] += math.Abs(h.Value)
	}
	in.Lines = map[string]LineInfo{}
	most := map[string]float64{}
	for _, key := range keys {
		f := funds[key]
		for i, l := range f.Lines {
			if l.Key == "" {
				continue
			}
			v := math.Abs(l.Weight) * held[key]
			if _, seen := in.Lines[l.Key]; !seen || v > most[l.Key] {
				in.Lines[l.Key], most[l.Key] = f.Infos[i], v
			}
		}
	}
}

// ReadLine is what one fund line says of its security on day: its price
// in its own currency (per 100 of face for a line with a coupon and a
// maturity, per unit otherwise, its value read at the exchange rate it
// was valued at), and the yield (to worst where it states one) and
// modified duration it states, else those its price implies. A line in
// default or arrears, or paid in kind, implies no yield.
func ReadLine(in *Inputs, l FundLine, day time.Time) LineInfo {
	info := LineInfo{Coupon: l.Coupon, Maturity: l.Maturity, CouponKind: l.Kind, Currency: l.Currency,
		Duration: first(l.Modified, l.Duration), Default: l.Defaulted}
	if !l.Defaulted && !l.InKind {
		info.Yield = first(l.YTW, l.YTM)
	}
	if l.Quantity <= 0 || l.Value <= 0 {
		return info
	}
	rate := 1.0
	if l.Rate != nil && *l.Rate > 0 {
		rate = *l.Rate
	}
	debt := l.Coupon != nil && l.Maturity != nil
	price := l.Value * rate / l.Quantity
	if debt {
		price *= 100
	}
	info.Price = &price
	if !debt || !l.Maturity.After(day) || info.Yield != nil || !(price > 1 && price < 1000) ||
		l.Floats || l.Defaulted || l.InKind {
		return info
	}
	b := fixedincome.Bond{Coupon: *l.Coupon, Frequency: in.ConventionFrequency(l.Currency), Maturity: *l.Maturity,
		DayCount: canonical.DayCountActAct}
	if v, err := b.Yield(day, price+b.Accrued(day)); err == nil {
		info.Yield = &v
		if info.Duration == nil {
			r := b.Risk(day, v).Modified
			info.Duration = &r
		}
	}
	return info
}

// SummarizeLines is the LineSummary of a fund's lines on day, given
// what each says of its security (ReadLine). A line is debt where its
// asset type maps to fixed income, or maps to nothing and the line is a
// bond: it states a coupon or a yield, floats, or is in default or paid
// in kind. It is equity where its type maps to public equity or real
// estate, and a derivative where it maps to other or foreign exchange.
// The debt and equity shares are of the whole fund, as exposure reads a
// partial composition: what the lines leave out of 100% is neither.
func SummarizeLines(lines []FundLine, infos []LineInfo, day time.Time, aged bool) *LineSummary {
	s := &LineSummary{AsOf: day, Aged: aged}
	var total, wy, sy, wd, sd, wm, sm float64
	yearOut := day.AddDate(1, 0, 0)
	for i, l := range lines {
		w := math.Abs(l.Weight)
		total += w
		debt := false
		switch l.Class {
		case string(canonical.AssetClassFixedIncome):
			debt = true
		case string(canonical.AssetClassPublicEquity), string(canonical.AssetClassRealEstate):
			s.EquityPct += w
		case string(canonical.AssetClassOther), string(canonical.AssetClassForeignExchange):
			s.Derivatives = true
		case "":
			debt = l.Coupon != nil || l.YTM != nil || l.YTW != nil || l.Floats || l.Defaulted || l.InKind
		}
		if debt {
			s.DebtPct += w
			if l.Maturity != nil && l.Maturity.Before(yearOut) {
				s.ShortDebtPct += w
			}
		}
		switch {
		case l.Floats:
			s.FloatingPct += w
			continue
		case l.Defaulted:
			s.DefaultPct += w
			continue
		case l.InKind:
			s.PaidInKindPct += w
			continue
		}
		if l.Maturity == nil || !l.Maturity.After(day) {
			continue
		}
		wm += w * (l.Maturity.Sub(day).Hours() / 24 / 365.25)
		sm += w
		y, dur := infos[i].Yield, infos[i].Duration
		if y != nil {
			wy += w * *y
			sy += w
		}
		if dur != nil {
			wd += w * *dur
			sd += w
		}
	}
	if total == 0 {
		return s
	}
	if sy > 0 {
		s.Yield = ptr(wy / sy)
	}
	if sd > 0 {
		s.Duration = ptr(wd / sd)
	}
	if sm > 0 {
		s.MaturityYears = ptr(wm / sm)
	}
	scale := 100 / total
	s.CoveredPct, s.DurationPct = sy*scale, sd*scale
	whole := 100 / max(total, 100)
	s.DebtPct, s.ShortDebtPct, s.EquityPct = s.DebtPct*whole, s.ShortDebtPct*whole, s.EquityPct*whole
	s.FloatingPct *= scale
	s.DefaultPct *= scale
	s.PaidInKindPct *= scale
	return s
}
