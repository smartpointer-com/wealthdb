package returns

import (
	"errors"
	"math"
	"testing"
)

// npvAt re-derives the NPV at a rate for root verification in tests.
func npvAt(cfs []datedCF, r float64) float64 {
	day0 := cfs[0].day
	for _, c := range cfs {
		if c.day < day0 {
			day0 = c.day
		}
	}
	s := 0.0
	for _, c := range cfs {
		t := float64(c.day-day0) / xirrDayBasis
		s += c.amount / math.Pow(1+r, t)
	}
	return s
}

func TestXIRRGoldenVectors(t *testing.T) {
	tests := []struct {
		name       string
		v0, v1     float64
		start, end int64
		flows      []Flow
		wantRate   float64
		tol        float64
	}{
		{"ten-percent-one-year", 1000, 1100, 0, 365, nil, 0.10, 2e-3},
		{"flat", 1000, 1000, 0, 365, nil, 0.0, 2e-3},
		// Invest 1000 at year 1, receive 2200 at year 2 ⇒ -1000(1+r)+2200=0 ⇒ r=1.2.
		{"contribution-then-exit", 0, 2200, 0, 730, []Flow{{Day: 365, Amount: 1000}}, 1.20, 5e-3},
		// Near-total loss: pay 1000, get back 1 a year later ⇒ r≈-0.999.
		{"near-total-loss", 1000, 1, 0, 365, nil, -0.999, 2e-3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := XIRR(tc.v0, tc.v1, tc.start, tc.end, tc.flows)
			if err != nil {
				t.Fatalf("XIRR err: %v", err)
			}
			almost(t, r, tc.wantRate, tc.tol, "rate")
			// Independent root check: NPV at the returned rate must be ~0.
			cfs := []datedCF{{tc.start, -tc.v0}}
			for _, f := range tc.flows {
				cfs = append(cfs, datedCF{f.Day, -f.Amount})
			}
			cfs = append(cfs, datedCF{tc.end, tc.v1})
			if npv := npvAt(cfs, r); math.Abs(npv) > 1e-4 {
				t.Errorf("NPV at root = %.6f, want ~0", npv)
			}
		})
	}
}

func TestXIRREdgeCases(t *testing.T) {
	// No sign change: only capital in, nothing returned.
	if _, err := XIRR(100, 0, 0, 365, []Flow{{Day: 180, Amount: 50}}); !errors.Is(err, ErrNoSignChange) {
		t.Errorf("all-outflow: err = %v, want ErrNoSignChange", err)
	}
	// Fewer than two cash flows (internal guard).
	if _, err := xirr([]datedCF{{0, -100}}); !errors.Is(err, ErrNoFlows) {
		t.Errorf("single cf: err = %v, want ErrNoFlows", err)
	}
	// Rate never reported below the -100% floor.
	r, err := XIRR(1000, 1, 0, 365, nil)
	if err != nil {
		t.Fatalf("near-total-loss err: %v", err)
	}
	if r <= xirrRateFloor {
		t.Errorf("rate %.5f breached floor %.5f", r, xirrRateFloor)
	}
}

func TestXIRRBisectionFallback(t *testing.T) {
	// Direct bisection on a clean linear NPV with a root at 0.5.
	if r, err := bisectXIRR(func(r float64) float64 { return 0.5 - r }); err != nil {
		t.Fatalf("bisect err: %v", err)
	} else {
		almost(t, r, 0.5, 1e-6, "bisected root")
	}

	// No sign change anywhere in the scan domain ⇒ ErrNoConverge.
	if _, err := bisectXIRR(func(float64) float64 { return 1.0 }); !errors.Is(err, ErrNoConverge) {
		t.Errorf("constant npv: err=%v, want ErrNoConverge", err)
	}

	// NaN samples (here for r < -0.5) must not bracket a spurious root against a
	// poisoned previous sample; the root at 0.2 must still be found.
	nanThenLinear := func(r float64) float64 {
		if r < -0.5 {
			return math.NaN()
		}
		return 0.2 - r
	}
	if r, err := bisectXIRR(nanThenLinear); err != nil {
		t.Fatalf("nan-then-linear err: %v", err)
	} else {
		almost(t, r, 0.2, 1e-6, "root after NaN region")
	}

	// End-to-end near-total drawdown whose true root sits BELOW the -100% floor:
	// the step-tol stall against the floor is not a root (NPV far from 0), so
	// XIRR reports ErrNoConverge rather than presenting a bogus clamped ≈ -100%.
	if _, err := XIRR(1000, 0.0001, 0, 365, nil); !errors.Is(err, ErrNoConverge) {
		t.Errorf("sub-floor loss: err=%v, want ErrNoConverge (no bogus clamped rate)", err)
	}
}

func TestMWRSignChanges(t *testing.T) {
	// Contribution, then a withdrawal, then another contribution: the investor
	// vector flips sign more than once ⇒ possibly non-unique IRR.
	flows := []Flow{{Day: 30, Amount: 50}, {Day: 60, Amount: -30}, {Day: 90, Amount: 40}}
	if n := MWRSignChanges(0, 100, 0, 120, flows); n <= 1 {
		t.Errorf("sign changes = %d, want > 1 (non-unique)", n)
	}
	// Plain: contributions only then a terminal value ⇒ exactly one change.
	if n := MWRSignChanges(0, 200, 0, 120, []Flow{{Day: 30, Amount: 50}, {Day: 60, Amount: 50}}); n != 1 {
		t.Errorf("sign changes = %d, want 1 (unique)", n)
	}
}
