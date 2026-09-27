package main

import (
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

func TestFormatWatermark(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   int64
		want string
	}{
		{-1, "-1"},
		{0, "0"},
		{1750000000, "1750000000 (2025-06-15 15:06:40Z)"},
	}
	for _, tc := range cases {
		if got := formatWatermark(tc.in); got != tc.want {
			t.Errorf("formatWatermark(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestStatusVerbosePrintsBothFamilies pins `status -v`'s per-family
// block.
//
// Both enrichment families report a backlog here, and the income
// section is two lines; deleting them leaves the rest of the suite
// green, which is exactly the shape the load summary's missing test
// had.
func TestStatusVerbosePrintsBothFamilies(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	printStatusVerbose(&out, &gold.SourceStatus{
		OtherAssetClassCount:     1,
		OtherTxKindCount:         2,
		GuessedTxKindCount:       3,
		MissingVehicleCount:      4,
		UncategorizedSpendCount:  5,
		ExcludedUnmappedCount:    6,
		UncategorizedIncomeCount: 7,
	})
	got := out.String()

	for _, want := range []string{
		"  spending:",
		"    uncategorised:           5 spending lines",
		"  income:",
		"    uncategorised:           7 income lines",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status -v is missing %q:\n%s", want, got)
		}
	}
	// Spending first, then income — the order the pass runs them in and
	// the order `load` reports them in.
	if strings.Index(got, "  spending:") > strings.Index(got, "  income:") {
		t.Errorf("income is reported before spending:\n%s", got)
	}
	// The catch-all-kind counter is spending's alone, deliberately: it
	// joins spend_scoped_accounts(), so a second copy under income
	// would double-count the same rows wherever the two scopes agree.
	if strings.Count(got, "excluded_unmapped") != 1 {
		t.Errorf("excluded_unmapped appears %d times, want 1:\n%s",
			strings.Count(got, "excluded_unmapped"), got)
	}
}
