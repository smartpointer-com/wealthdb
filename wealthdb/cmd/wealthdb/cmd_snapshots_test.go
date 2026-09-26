package main

import (
	"strings"
	"testing"
)

// daySecs is 24h of Unix seconds; the test times are mid-day UTC
// offsets so date grouping is unambiguous.
const daySecs = 24 * 60 * 60

func TestPrintSnapshotDates(t *testing.T) {
	t.Parallel()
	d0 := int64(1750000000) // 2025-06-15
	d1 := d0 + daySecs      // 2025-06-16
	d2 := d1 + daySecs      // 2025-06-17
	times := []int64{d0, d1, d1 + 60, d1 + 120, d2}

	cases := []struct {
		name       string
		times      []int64
		latestOnly bool
		want       []string
	}{
		{"empty", nil, false, []string{"src  (no snapshots)"}},
		{"empty latest", nil, true, []string{"src  (no snapshots)"}},
		{"grouped", times, false, []string{
			"src  " + formatDate(d0),
			"src  " + formatDate(d1) + " (×3)",
			"src  " + formatDate(d2),
		}},
		{"latest only", times, true, []string{
			"src  " + formatDate(d2),
		}},
		{"latest only keeps intra-day count", times[:4], true, []string{
			"src  " + formatDate(d1) + " (×3)",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sb strings.Builder
			printSnapshotDates(&sb, len("src"), "src", tc.times, tc.latestOnly)
			got := strings.Split(strings.TrimRight(sb.String(), "\n"), "\n")
			if len(got) != len(tc.want) {
				t.Fatalf("lines = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("line %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// The id column pads to the fleet-wide widest id so dates align
// across sources in -a mode.
func TestPrintSnapshotDatesPadsID(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	printSnapshotDates(&sb, 10, "short", []int64{1750000000}, false)
	want := "short       " + formatDate(1750000000) + "\n"
	if sb.String() != want {
		t.Errorf("out = %q, want %q", sb.String(), want)
	}
}
