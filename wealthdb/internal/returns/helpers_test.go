package returns

import (
	"math"
	"testing"
)

func almost(t *testing.T, got, want, tol float64, msg string) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s: got %.6f, want %.6f (±%.0e)", msg, got, want, tol)
	}
}

func approxEqual(a, b, tol float64) bool { return math.Abs(a-b) <= tol }
