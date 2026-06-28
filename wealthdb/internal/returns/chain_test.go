package returns

import (
	"math"
	"testing"
	"time"
)

func TestChainFlowFreeTelescopes(t *testing.T) {
	// Flow-free buckets are pure value ratios: chaining must telescope to the
	// overall value ratio, independent of bucketing.
	cum, ok := Chain([]Bucket{{R: 0.2, OK: true}, {R: 0.1, OK: true}})
	if !ok {
		t.Fatal("ok = false")
	}
	almost(t, cum, 1.2*1.1-1, 1e-12, "telescoped cum")
}

func TestChainBreaksOnDegenerate(t *testing.T) {
	if _, ok := Chain([]Bucket{{R: 0.2, OK: true}, {R: 0, OK: false}}); ok {
		t.Error("a degenerate bucket must break the chain (ok=false)")
	}
	if _, ok := Chain(nil); ok {
		t.Error("empty chain must be ok=false")
	}
}

// TestChainBucketSizeMattersWithFlows guards §3.A: with an external flow the
// cumulative chained Modified-Dietz is NOT bucket-size invariant. This is why
// the published headline must be pinned to a canonical bucket.
func TestChainBucketSizeMattersWithFlows(t *testing.T) {
	// One quarterly bucket over [0,90] with a +50 flow at the midpoint.
	rQ, ok := ModifiedDietz(100, 200, 0, 90, []Flow{{Day: 45, Amount: 50}})
	if !ok {
		t.Fatal("quarterly bucket degenerate")
	}
	cumQ, _ := Chain([]Bucket{{R: rQ, OK: true}})

	// Two monthly buckets split at the flow: 100→120, then 120→200 with the
	// +50 flow at the second bucket's start.
	r1, _ := ModifiedDietz(100, 120, 0, 45, nil)
	r2, _ := ModifiedDietz(120, 200, 45, 90, []Flow{{Day: 45, Amount: 50}})
	cumM, _ := Chain([]Bucket{{R: r1, OK: true}, {R: r2, OK: true}})

	if math.Abs(cumM-cumQ) < 1e-3 {
		t.Errorf("expected bucket-dependent headline: cumQ=%.6f cumM=%.6f (diff %.6f)", cumQ, cumM, cumM-cumQ)
	}
	// Sanity against the §3.A worked numbers.
	almost(t, cumQ, 0.40, 1e-9, "cumQ")
	almost(t, cumM, 1.2*(30.0/170.0+1)-1, 1e-9, "cumM")
}

func TestAnnualize(t *testing.T) {
	almost(t, Annualize(0.10, 365.25), 0.10, 1e-9, "one-year")
	// Half a year at 21% cumulative annualizes to (1.21)^2 - 1.
	almost(t, Annualize(0.21, 365.25/2), 1.21*1.21-1, 1e-9, "half-year")
}

func TestDeAnnualize(t *testing.T) {
	almost(t, DeAnnualize(0.10, 365.25), 0.10, 1e-9, "one-year identity")
	almost(t, DeAnnualize(0.21, 365.25/2), math.Pow(1.21, 0.5)-1, 1e-9, "half-year")
	// DeAnnualize inverts Annualize.
	almost(t, DeAnnualize(Annualize(0.3, 180), 180), 0.3, 1e-9, "inverse")
}

func TestShouldAnnualize(t *testing.T) {
	cases := []struct {
		mode string
		days float64
		want bool
	}{
		{"auto", 200, false},
		{"auto", 400, true},
		{"always", 10, true},
		{"never", 400, false},
	}
	for _, c := range cases {
		if got := ShouldAnnualize(c.mode, c.days); got != c.want {
			t.Errorf("ShouldAnnualize(%q,%v) = %v, want %v", c.mode, c.days, got, c.want)
		}
	}
}

func TestCanonicalHeadlineBucket(t *testing.T) {
	if got := CanonicalHeadlineBucket([]int64{0, 1, 2, 3, 4}); got != BucketDaily {
		t.Errorf("dense snapshots: got %v, want BucketDaily", got)
	}
	if got := CanonicalHeadlineBucket([]int64{0, 30, 60, 90}); got != BucketMonthly {
		t.Errorf("sparse snapshots: got %v, want BucketMonthly", got)
	}
	if got := CanonicalHeadlineBucket([]int64{5}); got != BucketMonthly {
		t.Errorf("single snapshot: got %v, want BucketMonthly", got)
	}
	// All snapshots on the same day ⇒ no positive gaps ⇒ monthly.
	if got := CanonicalHeadlineBucket([]int64{5, 5, 5}); got != BucketMonthly {
		t.Errorf("duplicate snapshot days: got %v, want BucketMonthly", got)
	}
}

func TestDayToTimeUTC(t *testing.T) {
	if got := DayToTimeUTC(0); !got.Equal(time.Unix(0, 0).UTC()) {
		t.Errorf("DayToTimeUTC(0) = %v, want epoch", got)
	}
	if got := DayToTimeUTC(1); got.Day() != 2 || got.Month() != time.January || got.Year() != 1970 {
		t.Errorf("DayToTimeUTC(1) = %v, want 1970-01-02", got)
	}
}

func TestBucketBoundaries(t *testing.T) {
	day := func(y int, m time.Month, d int) int64 {
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400
	}
	assertContiguous := func(t *testing.T, bs [][2]int64, from, to int64) {
		t.Helper()
		if bs[0][0] != from {
			t.Errorf("first start %d, want %d", bs[0][0], from)
		}
		if bs[len(bs)-1][1] != to {
			t.Errorf("last end %d, want %d", bs[len(bs)-1][1], to)
		}
		for i := 0; i+1 < len(bs); i++ {
			if bs[i][1] != bs[i+1][0] {
				t.Errorf("gap between bucket %d end %d and %d start %d", i, bs[i][1], i+1, bs[i+1][0])
			}
		}
	}

	// Monthly: mid-Jan → mid-Mar 2021 ⇒ [Jan15,Jan31],[Jan31,Feb28],[Feb28,Mar20].
	from, to := day(2021, time.January, 15), day(2021, time.March, 20)
	m := BucketBoundaries(from, to, BucketMonthly)
	if len(m) != 3 {
		t.Errorf("monthly buckets = %d, want 3", len(m))
	}
	assertContiguous(t, m, from, to)

	// Total: one bucket spanning the window.
	if tot := BucketBoundaries(from, to, BucketTotal); len(tot) != 1 || tot[0] != [2]int64{from, to} {
		t.Errorf("total = %v, want one [%d,%d]", tot, from, to)
	}

	// Daily: a 4-day span ⇒ 4 one-day buckets.
	d := BucketBoundaries(100, 104, BucketDaily)
	if len(d) != 4 {
		t.Errorf("daily buckets = %d, want 4", len(d))
	}
	assertContiguous(t, d, 100, 104)

	// Quarterly across a year boundary: Nov 2021 → Feb 2022 ⇒ Q4-end split.
	qf, qt := day(2021, time.November, 1), day(2022, time.February, 1)
	q := BucketBoundaries(qf, qt, BucketQuarterly)
	assertContiguous(t, q, qf, qt)
	if len(q) != 2 {
		t.Errorf("quarterly buckets = %d, want 2 (split at 2021-12-31)", len(q))
	}

	// Annual across two year boundaries: Nov 2021 → Feb 2023 ⇒ splits at
	// 2021-12-31 and 2022-12-31 ⇒ 3 buckets.
	af, at := day(2021, time.November, 1), day(2023, time.February, 1)
	a := BucketBoundaries(af, at, BucketAnnual)
	assertContiguous(t, a, af, at)
	if len(a) != 3 {
		t.Errorf("annual buckets = %d, want 3", len(a))
	}
}
