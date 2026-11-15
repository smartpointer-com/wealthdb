package silver

import (
	"context"
	"errors"
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

// Feeds asks each feed in order, merges what they answer, and names the
// feed whose clock fails.
func TestFeedsMergeAndNameTheFailingFeed(t *testing.T) {
	ctx := context.Background()
	feed := func(name string, s canonical.Status, w canonical.Window, err error) Feed {
		return Feed{Name: name,
			Status:       func(context.Context) (canonical.Status, error) { return s, err },
			ChangeWindow: func(context.Context, int64) (canonical.Window, error) { return w, err }}
	}
	a := canonical.UnknownStatus()
	a.LatestChangeNumber, a.ObservedSnapshotsThrough = 100, 100
	b := canonical.UnknownStatus()
	b.LatestChangeNumber, b.ObservedSnapshotsThrough = 200, 200
	wa := canonical.Window{HasChanges: true, Start: 10, End: 20, NewChangeNumber: 100}
	wb := canonical.Window{NewChangeNumber: 200}
	f := Feeds{Kind: "k", List: []Feed{feed("a", a, wa, nil), feed("b", b, wb, nil)}}
	s, err := f.Status(ctx)
	if err != nil || s != MergeStatuses([]canonical.Status{a, b}, false) {
		t.Errorf("Status = %+v (%v)", s, err)
	}
	f.Whole = true
	if s, _ := f.Status(ctx); s.ObservedSnapshotsThrough != 200 {
		t.Errorf("whole Status observed through %d, want the later 200", s.ObservedSnapshotsThrough)
	}
	w, err := f.ChangeWindow(ctx, 50)
	if err != nil || !reflect.DeepEqual(w, MergeWindows(50, []canonical.Window{wa, wb})) {
		t.Errorf("ChangeWindow = %+v (%v)", w, err)
	}
	f.List = append(f.List, feed("c", a, wa, errors.New("boom")))
	if _, err := f.Status(ctx); err == nil || err.Error() != "k c status: boom" {
		t.Errorf("a failing Status = %v", err)
	}
	if _, err := f.ChangeWindow(ctx, 0); err == nil || err.Error() != "k c ChangeWindow: boom" {
		t.Errorf("a failing ChangeWindow = %v", err)
	}
}
