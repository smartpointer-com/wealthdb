package returns

import (
	"math"
	"testing"
	"time"
)

func day(y int, m time.Month, d int) int64 {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400
}

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("%s = %.15f, want %.15f", name, got, want)
	}
}

// threeMonths is January to March 2024 at 10%, -5% and 2%.
func threeMonths() *Index {
	return NewIndex([]BenchmarkMonth{
		{End: day(2024, 1, 31), R: 0.10, OK: true},
		{End: day(2024, 2, 29), R: -0.05, OK: true},
		{End: day(2024, 3, 31), R: 0.02, OK: true},
	})
}

func TestIndexBounds(t *testing.T) {
	ix := threeMonths()
	if ix.Start() != day(2023, 12, 31) || ix.End() != day(2024, 3, 31) {
		t.Errorf("bounds %d %d", ix.Start(), ix.End())
	}
	if NewIndex(nil) != nil {
		t.Error("no months should be no index")
	}
}

func TestGrowthCompoundsWholeMonths(t *testing.T) {
	ix := threeMonths()
	for _, c := range []struct {
		name     string
		from, to int64
		want     float64
	}{
		{"all from the day before", day(2023, 12, 31), day(2024, 3, 31), 1.10 * 0.95 * 1.02},
		{"all from the first", day(2024, 1, 1), day(2024, 3, 31), 1.10 * 0.95 * 1.02},
		{"one month between month ends", day(2024, 1, 31), day(2024, 2, 29), 0.95},
		{"a first of the month takes the month whole", day(2024, 2, 1), day(2024, 3, 31), 0.95 * 1.02},
		{"a window ending on a first has none of its month", day(2024, 1, 1), day(2024, 2, 1), 1.10},
		{"no time", day(2024, 2, 10), day(2024, 2, 10), 1},
	} {
		g, ok := ix.Growth(c.from, c.to)
		if !ok {
			t.Errorf("%s: no growth", c.name)
			continue
		}
		near(t, c.name, g, c.want)
	}
}

func TestGrowthInsideAMonthCompoundsPerDay(t *testing.T) {
	ix := threeMonths()
	// February 2024 has 29 days; the end of the 10th has 10 of them done.
	g, _ := ix.Growth(day(2024, 2, 10), day(2024, 2, 29))
	near(t, "rest of February", g, math.Pow(0.95, 19.0/29))
	g, _ = ix.Growth(day(2024, 1, 31), day(2024, 2, 10))
	near(t, "start of February", g, math.Pow(0.95, 10.0/29))
	g, _ = ix.Growth(day(2024, 1, 16), day(2024, 3, 16))
	near(t, "across months", g, math.Pow(1.10, 15.0/31)*0.95*math.Pow(1.02, 16.0/31))
}

func TestGrowthStopsAtTheIndexEdges(t *testing.T) {
	ix := threeMonths()
	if _, ok := ix.Growth(day(2023, 12, 30), day(2024, 1, 31)); ok {
		t.Error("a start before the index should have no growth")
	}
	if _, ok := ix.Growth(day(2024, 1, 31), day(2024, 4, 1)); ok {
		t.Error("an end after the index should have no growth")
	}
	if _, ok := ix.Growth(day(2024, 2, 29), day(2024, 1, 31)); ok {
		t.Error("an end before the start should have no growth")
	}
	var none *Index
	if _, ok := none.Growth(0, 1); ok {
		t.Error("no index should have no growth")
	}
}

func TestAGapBlocksOnlyTheSpansThroughIt(t *testing.T) {
	ix := NewIndex([]BenchmarkMonth{
		{End: day(2024, 1, 31), R: 0.10, OK: true},
		{End: day(2024, 2, 29)},
		{End: day(2024, 3, 31), R: 0.02, OK: true},
	})
	if _, ok := ix.Growth(day(2024, 1, 1), day(2024, 3, 31)); ok {
		t.Error("a span through the gap should have no growth")
	}
	if _, ok := ix.Growth(day(2024, 2, 10), day(2024, 2, 20)); ok {
		t.Error("a span inside the gap should have no growth")
	}
	if g, ok := ix.Growth(day(2023, 12, 31), day(2024, 1, 31)); !ok || math.Abs(g-1.10) > 1e-12 {
		t.Errorf("before the gap: %v %v", g, ok)
	}
	if g, ok := ix.Growth(day(2024, 2, 29), day(2024, 3, 31)); !ok || math.Abs(g-1.02) > 1e-12 {
		t.Errorf("after the gap: %v %v", g, ok)
	}
}

func TestAMonthOfTotalLossHasNoGrowthAfterIt(t *testing.T) {
	ix := NewIndex([]BenchmarkMonth{
		{End: day(2024, 1, 31), R: -1, OK: true},
		{End: day(2024, 2, 29), R: 0.10, OK: true},
	})
	if g, ok := ix.Growth(day(2023, 12, 31), day(2024, 2, 29)); !ok || g != 0 {
		t.Errorf("through the loss: %v %v", g, ok)
	}
	if _, ok := ix.Growth(day(2024, 1, 31), day(2024, 2, 29)); ok {
		t.Error("a start at a level of zero should have no growth")
	}
}

func TestShadowValues(t *testing.T) {
	ix := threeMonths()
	from := day(2023, 12, 31)
	flows := []Flow{
		{Day: from, Amount: 999}, // on the start day: already in v0
		{Day: day(2024, 1, 31), Amount: 50},
		{Day: day(2024, 2, 10), Amount: -20},
	}
	ends := []int64{day(2024, 1, 31), day(2024, 2, 29), day(2024, 3, 31)}
	got, ok, _ := ix.ShadowValues(100, from, flows, ends)
	jan := 100*1.10 + 50
	feb := jan*math.Pow(0.95, 10.0/29)*1 - 20
	feb *= math.Pow(0.95, 19.0/29)
	for i, want := range []float64{jan, feb, feb * 1.02} {
		if !ok[i] {
			t.Fatalf("end %d has no value", i)
		}
		near(t, "shadow", got[i], want)
	}
}

func TestShadowValuesStopAtAGap(t *testing.T) {
	ix := NewIndex([]BenchmarkMonth{
		{End: day(2024, 1, 31), R: 0.10, OK: true},
		{End: day(2024, 2, 29)},
		{End: day(2024, 3, 31), R: 0.02, OK: true},
	})
	ends := []int64{day(2024, 1, 31), day(2024, 2, 29), day(2024, 3, 31)}
	got, ok, _ := ix.ShadowValues(100, day(2023, 12, 31), nil, ends)
	if !ok[0] || ok[1] || ok[2] {
		t.Fatalf("ok = %v", ok)
	}
	near(t, "before the gap", got[0], 110)
}

// The public market equivalent: a shadow with the benchmark's own return
// solves to that return, whatever the flows.
func TestAShadowTheFlowsEmptyStaysShort(t *testing.T) {
	ix := threeMonths()
	flows := []Flow{{Day: day(2024, 1, 10), Amount: -150}, {Day: day(2024, 1, 20), Amount: 200}}
	ends := []int64{day(2024, 1, 5), day(2024, 1, 31), day(2024, 2, 29)}
	got, ok, short := ix.ShadowValues(100, day(2023, 12, 31), flows, ends)
	if !ok[1] || got[1] <= 0 {
		t.Fatalf("the shadow recovers: %v %v", got, ok)
	}
	if short[0] || !short[1] || !short[2] {
		t.Errorf("short = %v, want from the day the flows emptied it", short)
	}
}

func TestShadowXIRRIsTheBenchmarkRate(t *testing.T) {
	var months []BenchmarkMonth
	for m := time.January; m <= time.December; m++ {
		end := time.Date(2023, m+1, 0, 0, 0, 0, 0, time.UTC).Unix() / 86400
		months = append(months, BenchmarkMonth{End: end, R: 0.01, OK: true})
	}
	ix := NewIndex(months)
	from, to := day(2022, 12, 31), day(2023, 12, 31)
	flows := []Flow{{Day: day(2023, 3, 31), Amount: 300}, {Day: day(2023, 9, 30), Amount: -150}}
	v, ok, _ := ix.ShadowValues(1000, from, flows, []int64{to})
	if !ok[0] {
		t.Fatal("no shadow")
	}
	rate, err := XIRR(1000, v[0], from, to, flows)
	if err != nil {
		t.Fatal(err)
	}
	// 1% a month over 365 days, on XIRR's 365.25-day year.
	if want := math.Pow(1.01, 12*365.25/365); math.Abs(1+rate-want) > 2e-3 {
		t.Errorf("rate %v, want about %v", 1+rate, want)
	}
}

func TestXIRRFromFindsTheSameRoot(t *testing.T) {
	flows := []Flow{{Day: 100, Amount: 500}, {Day: 200, Amount: -100}}
	cold, err := XIRR(1000, 1600, 0, 365, flows)
	if err != nil {
		t.Fatal(err)
	}
	warm, err := XIRRFrom(cold+0.01, 1000, 1600, 0, 365, flows)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(cold-warm) > 1e-8 {
		t.Errorf("cold %v warm %v", cold, warm)
	}
}

func TestFlowSeriesNetsADayAndSolvesAsXIRR(t *testing.T) {
	flows := []Flow{{Day: 0, Amount: 7}, {Day: 30, Amount: 200}, {Day: 30, Amount: -50}, {Day: 90, Amount: 300}, {Day: 200, Amount: -100}}
	s := NewFlowSeries(0, flows)
	if got := s.Upto(30); len(got) != 1 || got[0].Amount != 150 {
		t.Fatalf("through day 30: %+v", got)
	}
	if got := s.Upto(29); len(got) != 0 {
		t.Fatalf("through day 29: %+v", got)
	}
	if got := s.Upto(1000); len(got) != 3 {
		t.Fatalf("all: %+v", got)
	}
	want, err := XIRR(1000, 1500, 0, 365, flows[1:])
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Solve(0.1, 1000, 1500, 365, 3)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got-want) > 1e-8 {
		t.Errorf("solve %v, XIRR %v", got, want)
	}
	// A flow past the end day is left out.
	want, _ = XIRR(1000, 1200, 0, 100, flows[1:4])
	got, _ = s.Solve(0.2, 1000, 1200, 100, 2)
	if math.Abs(got-want) > 1e-8 {
		t.Errorf("solve %v, XIRR %v", got, want)
	}
}

func TestFlowSeriesFallsBackWhereNewtonCannot(t *testing.T) {
	s := NewFlowSeries(0, []Flow{{Day: 100, Amount: 10}})
	// A near-total loss from a guess far above it.
	got, err := s.Solve(50, 1000, 1, 365, 1)
	want, wantErr := XIRR(1000, 1, 0, 365, s.Upto(365))
	if (err == nil) != (wantErr == nil) || math.Abs(got-want) > 1e-6 {
		t.Errorf("solve %v %v, XIRR %v %v", got, err, want, wantErr)
	}
}
