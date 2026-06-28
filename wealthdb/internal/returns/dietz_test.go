package returns

import "testing"

func TestModifiedDietz(t *testing.T) {
	tests := []struct {
		name       string
		v0, v1     float64
		start, end int64
		flows      []Flow
		wantR      float64
		wantOK     bool
	}{
		{"no-flow", 100, 110, 0, 30, nil, 0.10, true},
		// GIPS-style: 500 added at the period midpoint (w=0.5).
		{"mid-period-contribution", 1000, 1700, 0, 30, []Flow{{Day: 15, Amount: 500}}, 0.16, true},
		// Withdrawal at the midpoint.
		{"mid-period-withdrawal", 1000, 600, 0, 30, []Flow{{Day: 15, Amount: -300}}, -100.0 / 850.0, true},
		// Degenerate: V0=0 with a single end-of-period inflow (w=0) ⇒ denom 0.
		{"degenerate-v0-zero-end-inflow", 0, 50, 0, 30, []Flow{{Day: 30, Amount: 50}}, 0, false},
		// Net-negative (mortgage/liability) base ⇒ denom ≤ 0 ⇒ undefined.
		{"nonpositive-base", -100, -90, 0, 30, nil, 0, false},
		// Zero-length window (start==end), no flows, positive base ⇒ plain ratio.
		{"zero-len-noflow-positive-v0", 100, 110, 30, 30, nil, 0.10, true},
		// Zero-length window with a negative base ⇒ not OK.
		{"zero-len-negative-v0", -100, -90, 30, 30, nil, 0, false},
		// Zero-length window with a flow ⇒ can't time-weight ⇒ not OK.
		{"zero-len-with-flow", 100, 110, 30, 30, []Flow{{Day: 30, Amount: 50}}, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := ModifiedDietz(tc.v0, tc.v1, tc.start, tc.end, tc.flows)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if tc.wantOK {
				almost(t, r, tc.wantR, 1e-9, "R")
			}
		})
	}
}
