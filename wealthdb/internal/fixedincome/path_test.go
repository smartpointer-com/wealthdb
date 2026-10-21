package fixedincome

import "testing"

func TestConstantPath(t *testing.T) {
	t.Parallel()
	p := ConstantPath(3.5, 12)
	if len(p) != 12 {
		t.Fatalf("path of %d months, want 12", len(p))
	}
	for _, r := range p {
		near(t, "rate", r, 3.5, 0)
	}
	for _, months := range []int{0, -3} {
		if p := ConstantPath(3.5, months); len(p) != 0 {
			t.Errorf("a path of %d months has %d rates", months, len(p))
		}
	}
}

// A forward path is the curve's path plus the spread.
func TestForwardPath(t *testing.T) {
	t.Parallel()
	c := mustCurve(t, Point{3, 4}, Point{12, 3}, Point{24, 3})
	base := c.Path(24, 3)
	p := ForwardPath(c, 24, 3, 0.5)
	if len(p) != len(base) {
		t.Fatalf("path of %d months, want %d", len(p), len(base))
	}
	for m := range p {
		near(t, "rate", p[m], base[m]+0.5, 1e-12)
	}
	near(t, "month 0", p[0], 4.5, 1e-9)
	// A negative spread lowers the path.
	near(t, "below the curve", ForwardPath(c, 1, 3, -0.25)[0], 3.75, 1e-9)
}

// A shift moves every month by the same basis points, on a copy.
func TestShift(t *testing.T) {
	t.Parallel()
	p := []float64{1, 2.5, -0.25}
	got := Shift(p, 25)
	for m, want := range []float64{1.25, 2.75, 0} {
		near(t, "shifted", got[m], want, 1e-12)
	}
	near(t, "the original", p[0], 1, 0)
	near(t, "down", Shift(p, -100)[1], 1.5, 1e-12)
	if got := Shift(nil, 10); len(got) != 0 {
		t.Errorf("a shift of no months has %d rates", len(got))
	}
}
