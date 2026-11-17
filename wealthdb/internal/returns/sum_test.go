package returns

import (
	"math"
	"math/rand/v2"
	"testing"
)

// A sum reads the same whatever the order of its terms, and it is the
// exact total rounded once.
func TestSumIsExactInAnyOrder(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(3, 4))
	for trial := 0; trial < 2000; trial++ {
		terms := make([]float64, 2+rng.IntN(30))
		for i := range terms {
			mag := []float64{1e-5, 1e-3, 1, 1e3}[rng.IntN(4)]
			terms[i] = float64(rng.IntN(2000000)-1000000) / 100 * mag
		}
		var a Sum
		for _, x := range terms {
			a.Add(x)
		}
		rng.Shuffle(len(terms), func(i, j int) { terms[i], terms[j] = terms[j], terms[i] })
		var b Sum
		for _, x := range terms {
			b.Add(x)
		}
		if a.Total() != b.Total() {
			t.Fatalf("trial %d: %v in one order, %v in another", trial, a.Total(), b.Total())
		}
	}
	var s Sum
	for _, x := range []float64{1e16, 1, -1e16, 0.1, 0.2} {
		s.Add(x)
	}
	if got, want := s.Total(), 1.3; math.Abs(got-want) > 1e-15 {
		t.Errorf("Total = %v, want %v", got, want)
	}
	var empty Sum
	if empty.Total() != 0 {
		t.Errorf("an empty sum = %v, want 0", empty.Total())
	}
}

// A day's flows are one cash flow: a deposit and a withdrawal of one size
// on one day add no sign change, in either order, and the rate and the
// Dietz return do not follow the order of a day's flows.
func TestADaysFlowsReadAsOne(t *testing.T) {
	t.Parallel()
	in, out := Flow{Day: 10, Amount: 100}, Flow{Day: 10, Amount: -100}
	other := []Flow{{Day: 10, Amount: 0.1}, {Day: 10, Amount: 1e6}, {Day: 10, Amount: -1e6 + 0.2}, {Day: 20, Amount: 50}}
	orders := [][]Flow{
		append([]Flow{in, out}, other...),
		append([]Flow{out, in}, other[2], other[0], other[1], other[3]),
	}
	var changes []int
	var rates, dietz []float64
	for _, fl := range orders {
		changes = append(changes, MWRSignChanges(1000, 1200, 0, 30, fl))
		r, err := XIRR(1000, 1200, 0, 30, fl)
		if err != nil {
			t.Fatal(err)
		}
		rates = append(rates, r)
		d, _ := ModifiedDietz(1000, 1200, 0, 30, fl)
		dietz = append(dietz, d)
	}
	if changes[0] != 1 || changes[1] != 1 {
		t.Errorf("sign changes = %v, want 1 in either order", changes)
	}
	if rates[0] != rates[1] || dietz[0] != dietz[1] {
		t.Errorf("rates %v, Dietz %v: want each the same in either order", rates, dietz)
	}
}
