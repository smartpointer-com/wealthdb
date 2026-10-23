package silver

import (
	"reflect"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The extrema span the union, a feed's -1 counts for nothing, and the
// observed watermarks follow whole.
func TestMergeStatusesSpansTheFeeds(t *testing.T) {
	a := canonical.Status{
		OldestSnapshotAt: 100, LatestSnapshotAt: 500,
		OldestTransactionAt: -1, LatestTransactionAt: -1,
		LatestChangeNumber:       700,
		ObservedSnapshotsThrough: 700, ObservedTransactionsThrough: 650,
	}
	b := canonical.Status{
		OldestSnapshotAt: 200, LatestSnapshotAt: 600,
		OldestTransactionAt: 50, LatestTransactionAt: 400,
		LatestChangeNumber:       800,
		ObservedSnapshotsThrough: 800, ObservedTransactionsThrough: -1,
	}
	want := canonical.Status{
		OldestSnapshotAt: 100, LatestSnapshotAt: 600,
		OldestTransactionAt: 50, LatestTransactionAt: 400,
		LatestChangeNumber:       800,
		ObservedSnapshotsThrough: 800, ObservedTransactionsThrough: 650,
	}
	if got := MergeStatuses([]canonical.Status{a, b}, true); got != want {
		t.Errorf("whole = %+v, want %+v", got, want)
	}
	want.ObservedSnapshotsThrough = 700
	if got := MergeStatuses([]canonical.Status{a, b}, false); got != want {
		t.Errorf("parts = %+v, want %+v", got, want)
	}
	if got := MergeStatuses(nil, false); got != canonical.UnknownStatus() {
		t.Errorf("no feeds = %+v, want the unknown status", got)
	}
}

// The window spans the changed feeds only, and the change number moves
// by any feed's progress, changed or not.
func TestMergeWindowsSpansTheChangedFeeds(t *testing.T) {
	idle := canonical.Window{Start: 0, End: 9000, NewChangeNumber: 950}
	early := canonical.Window{HasChanges: true, Start: 100, End: 300, NewChangeNumber: 900}
	late := canonical.Window{HasChanges: true, Start: 200, End: 500, NewChangeNumber: 800}
	for name, c := range map[string]struct {
		windows []canonical.Window
		want    canonical.Window
	}{
		"none":       {nil, canonical.Window{NewChangeNumber: 10}},
		"idle":       {[]canonical.Window{idle}, canonical.Window{NewChangeNumber: 950}},
		"one":        {[]canonical.Window{idle, late}, canonical.Window{HasChanges: true, Start: 200, End: 500, NewChangeNumber: 950}},
		"both":       {[]canonical.Window{early, late}, canonical.Window{HasChanges: true, Start: 100, End: 500, NewChangeNumber: 900}},
		"behind":     {[]canonical.Window{{NewChangeNumber: 5}}, canonical.Window{NewChangeNumber: 10}},
		"late first": {[]canonical.Window{late, early}, canonical.Window{HasChanges: true, Start: 100, End: 500, NewChangeNumber: 900}},
	} {
		if got := MergeWindows(10, c.windows); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", name, got, c.want)
		}
	}
}
