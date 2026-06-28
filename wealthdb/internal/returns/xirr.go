package returns

import (
	"errors"
	"math"
)

// Sentinel errors returned by XIRR so callers can map them to quality flags
// (mwr_no_flows, mwr_no_sign_change, mwr_no_converge) instead of presenting a
// bogus rate.
var (
	// ErrNoFlows is returned when fewer than two cash flows are supplied.
	ErrNoFlows = errors.New("returns: fewer than two cash flows")
	// ErrNoSignChange is returned when the assembled investor cash-flow vector
	// never changes sign — there is no IRR root.
	ErrNoSignChange = errors.New("returns: cash flows have no sign change")
	// ErrNoConverge is returned when neither Newton nor the bisection fallback
	// bracketed a root in the searched rate domain.
	ErrNoConverge = errors.New("returns: XIRR did not converge")
)

const (
	xirrDayBasis  = 365.25  // annualization day-count
	xirrRateFloor = -0.9999 // hard lower bound: never report worse than ~-100%
	xirrRateCeil  = 100.0   // upper search bound (10,000%/yr)
	xirrScanStep  = 0.01    // bracket-scan resolution for the bisection fallback
	newtonMaxIter = 100
	bisectMaxIter = 200
	npvTol        = 1e-7
	stepTol       = 1e-10
)

type datedCF struct {
	day    int64
	amount float64
}

// XIRR computes the money-weighted, annualized internal rate of return for an
// entity over [startDay, endDay], given its opening value v0, terminal value v1,
// and the external flows in between.
//
// It assembles the investor cash-flow vector itself — pay in the opening value
// (-v0), each capital-in flow is money out of pocket (-Amount), and receive the
// terminal value (+v1) — and solves NPV(r)=0 with Newton-Raphson (analytic
// derivative) plus a bisection fallback on [xirrRateFloor, xirrRateCeil].
//
// Callers must NOT invoke XIRR for a window with no external flows: an entity
// with only [-v0, +v1] has a single sign change and would yield the holding
// period return dressed up as an IRR. Treat len(flows)==0 as mwr_no_flows.
func XIRR(v0, v1 float64, startDay, endDay int64, flows []Flow) (float64, error) {
	return xirr(investorCFs(v0, v1, startDay, endDay, flows))
}

// investorCFs assembles the investor cash-flow vector: pay in the opening value
// (-v0), each capital-in flow is money out of pocket (-Amount), and receive the
// terminal value (+v1). Shared by XIRR and MWRSignChanges so the two can't drift.
func investorCFs(v0, v1 float64, startDay, endDay int64, flows []Flow) []datedCF {
	cfs := make([]datedCF, 0, len(flows)+2)
	cfs = append(cfs, datedCF{startDay, -v0})
	for _, f := range flows {
		cfs = append(cfs, datedCF{f.Day, -f.Amount})
	}
	cfs = append(cfs, datedCF{endDay, v1})
	return cfs
}

func xirr(cfs []datedCF) (float64, error) {
	if len(cfs) < 2 {
		return 0, ErrNoFlows
	}
	var pos, neg bool
	day0 := cfs[0].day
	for _, c := range cfs {
		if c.day < day0 {
			day0 = c.day
		}
		switch {
		case c.amount > 0:
			pos = true
		case c.amount < 0:
			neg = true
		}
	}
	if !pos || !neg {
		return 0, ErrNoSignChange
	}

	npv := func(r float64) float64 {
		base := 1.0 + r
		s := 0.0
		for _, c := range cfs {
			t := float64(c.day-day0) / xirrDayBasis
			s += c.amount / math.Pow(base, t)
		}
		return s
	}
	dnpv := func(r float64) float64 {
		base := 1.0 + r
		s := 0.0
		for _, c := range cfs {
			t := float64(c.day-day0) / xirrDayBasis
			s += -t * c.amount / math.Pow(base, t+1)
		}
		return s
	}

	// Newton-Raphson from a 10% guess.
	r := 0.1
	for i := 0; i < newtonMaxIter; i++ {
		if 1.0+r <= 0 {
			break // out of domain → bisection
		}
		f := npv(r)
		if math.Abs(f) < npvTol {
			return r, nil
		}
		d := dnpv(r)
		if d == 0 || math.IsNaN(d) || math.IsInf(d, 0) {
			break
		}
		next := r - f/d
		if math.IsNaN(next) || math.IsInf(next, 0) {
			break
		}
		if next <= xirrRateFloor {
			next = (r + xirrRateFloor) / 2 // damp toward the floor, don't overshoot below -100%
		}
		if math.Abs(next-r) < stepTol {
			return next, nil
		}
		r = next
	}
	return bisectXIRR(npv)
}

// bisectXIRR scans [xirrRateFloor, xirrRateCeil] for the first sign-change
// bracket and bisects it. Robust where Newton diverges (e.g. near-total loss).
func bisectXIRR(npv func(float64) float64) (float64, error) {
	prev := xirrRateFloor
	fPrev := npv(prev)
	havePrev := !math.IsNaN(fPrev) && !math.IsInf(fPrev, 0)
	if havePrev && math.Abs(fPrev) < npvTol {
		return prev, nil
	}
	for cur := prev + xirrScanStep; cur <= xirrRateCeil; cur += xirrScanStep {
		fCur := npv(cur)
		if math.IsNaN(fCur) || math.IsInf(fCur, 0) {
			havePrev = false // don't bracket against a poisoned sample
			continue
		}
		if math.Abs(fCur) < npvTol {
			return cur, nil
		}
		if havePrev && (fPrev < 0) != (fCur < 0) {
			return bisect(npv, prev, cur), nil
		}
		prev, fPrev, havePrev = cur, fCur, true
	}
	return 0, ErrNoConverge
}

func bisect(npv func(float64) float64, lo, hi float64) float64 {
	fLo := npv(lo)
	for i := 0; i < bisectMaxIter; i++ {
		mid := (lo + hi) / 2
		fMid := npv(mid)
		if math.Abs(fMid) < npvTol || (hi-lo)/2 < stepTol {
			return mid
		}
		if (fLo < 0) == (fMid < 0) {
			lo, fLo = mid, fMid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// MWRSignChanges counts sign changes in the investor cash-flow vector
// (-v0, -flows, +v1) ordered by day. More than one change means the IRR may be
// non-unique (Descartes' rule of signs) — callers flag mwr_nonunique.
func MWRSignChanges(v0, v1 float64, startDay, endDay int64, flows []Flow) int {
	cfs := investorCFs(v0, v1, startDay, endDay, flows)
	stableSortByDay(cfs)

	changes := 0
	prevSign := 0
	for _, c := range cfs {
		s := 0
		switch {
		case c.amount > 0:
			s = 1
		case c.amount < 0:
			s = -1
		}
		if s == 0 {
			continue
		}
		if prevSign != 0 && s != prevSign {
			changes++
		}
		prevSign = s
	}
	return changes
}

// stableSortByDay is a tiny insertion sort (cash-flow vectors are short).
func stableSortByDay(cfs []datedCF) {
	for i := 1; i < len(cfs); i++ {
		for j := i; j > 0 && cfs[j-1].day > cfs[j].day; j-- {
			cfs[j-1], cfs[j] = cfs[j], cfs[j-1]
		}
	}
}
