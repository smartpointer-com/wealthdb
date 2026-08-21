package returns

import "testing"

func TestOnboardingFlow(t *testing.T) {
	// The synthetic books the full debut value (real pre-debut flows are
	// subsumed by the caller).
	f, ok := OnboardingFlow(10, 1000)
	if !ok || f.Day != 10 || !approxEqual(f.Amount, 1000, 1e-9) {
		t.Errorf("debut: got (%+v, %v), want {10,1000},true", f, ok)
	}
	// A ~0 or negative amount (a step-up fully netted by sibling funding
	// drops) is suppressed.
	if _, ok := OnboardingFlow(10, 0); ok {
		t.Error("zero debut value must suppress the synthetic onboarding flow")
	}
	if _, ok := OnboardingFlow(10, -5); ok {
		t.Error("negative debut value must suppress the synthetic onboarding flow")
	}
}

func TestClosureFlowAndZeroing(t *testing.T) {
	if _, ok := ClosureFlow(0, 500); ok {
		t.Error("no explicit closure (day 0) must not produce a closure flow")
	}
	// The synthetic books the full boundary value.
	f, ok := ClosureFlow(50, 500)
	if !ok || !approxEqual(f.Amount, -500, 1e-9) || f.Day != 50 {
		t.Errorf("closure: got (%+v, %v), want {50,-500},true", f, ok)
	}
	// A real zero at the boundary (an adapter's exit-day zero marker) leaves
	// nothing to book; the spine still zeroes via ZeroedValue.
	if _, ok := ClosureFlow(50, 0); ok {
		t.Error("zero-boundary closure must book no flow")
	}

	if v := ZeroedValue(500, 40, 0); v != 500 {
		t.Errorf("not-closed: %v, want 500", v)
	}
	if v := ZeroedValue(500, 40, 50); v != 500 {
		t.Errorf("before closure: %v, want 500", v)
	}
	if v := ZeroedValue(500, 60, 50); v != 0 {
		t.Errorf("after closure: %v, want 0", v)
	}
}

// TestClosurePhantomLossRegression locks the §2.7/§3 finding: booking the
// closure outflow WITHOUT zeroing the carried-forward spine double-counts the
// position and prints a spurious return; zeroing makes it clean.
func TestClosurePhantomLossRegression(t *testing.T) {
	const (
		bBegin, bEnd = 1000.0, 1100.0 // rest-of-aggregate over the bucket
		vLast        = 500.0          // closing constituent's last value
		bucketStart  = int64(0)
		bucketEnd    = int64(30)
		closureDay   = int64(5)
	)
	flow, ok := ClosureFlow(closureDay, vLast)
	if !ok {
		t.Fatal("expected a closure flow")
	}
	flows := []Flow{flow}

	// Begin value: the constituent is still present at bucket start (0 < 5).
	vBegin := bBegin + ZeroedValue(vLast, bucketStart, closureDay)

	// WITH zeroing (correct): the constituent is gone from V_end.
	vEndZeroed := bEnd + ZeroedValue(vLast, bucketEnd, closureDay)
	rWith, okW := ModifiedDietz(vBegin, vEndZeroed, bucketStart, bucketEnd, flows)

	// WITHOUT zeroing (the bug): carry-forward keeps vLast in V_end.
	vEndCarried := bEnd + vLast
	rBug, okB := ModifiedDietz(vBegin, vEndCarried, bucketStart, bucketEnd, flows)

	if !okW || !okB {
		t.Fatal("both Dietz computations should be well-defined")
	}
	// The bug inflates the return by ~vLast/denominator — a large, spurious gap.
	if rBug-rWith < 0.4 {
		t.Errorf("phantom not reproduced: rBug=%.4f rWith=%.4f (gap %.4f, want > 0.4)", rBug, rWith, rBug-rWith)
	}
	// The zeroed result is close to the true rest-of-aggregate return (~10%);
	// the bug is wildly off.
	almost(t, rWith, 0.0923, 5e-3, "zeroed return ≈ clean B return")
	if rBug < 0.4 {
		t.Errorf("buggy return %.4f should be grossly inflated", rBug)
	}
}
