package returns

import "testing"

func TestOnboardingFlow(t *testing.T) {
	// No real funding in the debut bucket ⇒ inject the full opening value.
	f, ok := OnboardingFlow(10, 1000, 0)
	if !ok || f.Day != 10 || !approxEqual(f.Amount, 1000, 1e-9) {
		t.Errorf("no-funding: got (%+v, %v), want {10,1000},true", f, ok)
	}
	// A real funding flow already explains the opening value ⇒ suppress (dedup).
	if _, ok := OnboardingFlow(10, 1000, 1000); ok {
		t.Error("fully-funded debut must suppress the synthetic onboarding flow")
	}
	// Partial real funding ⇒ inject only the unexplained remainder.
	f, ok = OnboardingFlow(10, 1000, 400)
	if !ok || !approxEqual(f.Amount, 600, 1e-9) {
		t.Errorf("partial-funding: got (%+v,%v), want amount 600", f, ok)
	}
}

func TestClosureFlowAndZeroing(t *testing.T) {
	if _, _, ok := ClosureFlow(0, 500); ok {
		t.Error("no explicit closure (day 0) must not produce a closure flow")
	}
	f, zeroFrom, ok := ClosureFlow(50, 500)
	if !ok || zeroFrom != 50 || !approxEqual(f.Amount, -500, 1e-9) {
		t.Errorf("closure: got (%+v, zeroFrom=%d, %v), want {50,-500},50,true", f, zeroFrom, ok)
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
	flow, zeroFrom, ok := ClosureFlow(closureDay, vLast)
	if !ok {
		t.Fatal("expected a closure flow")
	}
	flows := []Flow{flow}

	// Begin value: the constituent is still present at bucket start (0 < 5).
	vBegin := bBegin + ZeroedValue(vLast, bucketStart, zeroFrom)

	// WITH zeroing (correct): the constituent is gone from V_end.
	vEndZeroed := bEnd + ZeroedValue(vLast, bucketEnd, zeroFrom)
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
