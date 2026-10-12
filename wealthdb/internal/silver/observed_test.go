package silver

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

func TestCoveredThroughStopsAtTheFirstGap(t *testing.T) {
	d := func(n int64) int64 { return n * 86400 }
	for name, c := range map[string]struct {
		spans []Span
		want  int64
	}{
		"none":                {nil, -1},
		"one":                 {[]Span{{d(10), d(20)}}, d(20)},
		"overlapping":         {[]Span{{d(15), d(30)}, {d(10), d(20)}}, d(30)},
		"next day joins":      {[]Span{{d(10), d(20)}, {d(21), d(25)}}, d(25)},
		"a gap stops it":      {[]Span{{d(10), d(20)}, {d(23), d(40)}}, d(20)},
		"a full run heals it": {[]Span{{d(10), d(20)}, {d(23), d(40)}, {d(0), d(30)}}, d(40)},
		"a later run inside":  {[]Span{{d(0), d(50)}, {d(5), d(10)}}, d(50)},
	} {
		if got := CoveredThrough(c.spans, SpanSlack); got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
	}
}

func TestMergeObservedTakesTheWholeOrEveryPart(t *testing.T) {
	a, b, none := canonical.UnknownStatus(), canonical.UnknownStatus(), canonical.UnknownStatus()
	a.ObservedSnapshotsThrough, a.ObservedTransactionsThrough = 100, 90
	b.ObservedSnapshotsThrough, b.ObservedTransactionsThrough = 80, 95
	if s, tx := mergeObserved([]canonical.Status{a, b, none}, true); s != 100 || tx != 95 {
		t.Errorf("whole: %d %d", s, tx)
	}
	if s, tx := mergeObserved([]canonical.Status{a, b, none}, false); s != 80 || tx != 90 {
		t.Errorf("parts: %d %d", s, tx)
	}
	if s, tx := mergeObserved([]canonical.Status{none}, false); s != -1 || tx != -1 {
		t.Errorf("none: %d %d", s, tx)
	}
}
