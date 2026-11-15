package silver

import (
	"context"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// A source read through more than one feed (a subsource each) answers
// Status and ChangeWindow by merging what its feeds answer. The merges
// below are the one rule for every such adapter.

// MergeStatuses combines the Status of a source's feeds. The snapshot and
// transaction extrema span the union of the feeds' and the change number
// is the latest; a feed's -1 ("no observable state") does not count. The
// observed watermarks follow whole: feeds that each read the whole
// source take the later, feeds that each read a part of it the earlier
// (mergeObserved).
func MergeStatuses(statuses []canonical.Status, whole bool) canonical.Status {
	out := canonical.UnknownStatus()
	for _, s := range statuses {
		out.OldestSnapshotAt = earliest(out.OldestSnapshotAt, s.OldestSnapshotAt)
		out.LatestSnapshotAt = latest(out.LatestSnapshotAt, s.LatestSnapshotAt)
		out.OldestTransactionAt = earliest(out.OldestTransactionAt, s.OldestTransactionAt)
		out.LatestTransactionAt = latest(out.LatestTransactionAt, s.LatestTransactionAt)
		out.LatestChangeNumber = latest(out.LatestChangeNumber, s.LatestChangeNumber)
	}
	out.ObservedSnapshotsThrough, out.ObservedTransactionsThrough = mergeObserved(statuses, whole)
	return out
}

// earliest and latest fold two extrema, either of which may be the -1
// sentinel: the known one wins over it.
func earliest(a, b int64) int64 {
	switch {
	case a == -1:
		return b
	case b == -1:
		return a
	}
	return min(a, b)
}

func latest(a, b int64) int64 {
	switch {
	case a == -1:
		return b
	case b == -1:
		return a
	}
	return max(a, b)
}

// MergeWindows combines the change windows a source's feeds yield past
// sinceN. The source has changes when any feed has, over the union of
// the changed feeds' spans, and its new change number is the latest of
// sinceN and every feed's, so the watermark moves by whatever progress
// any feed made.
func MergeWindows(sinceN int64, windows []canonical.Window) canonical.Window {
	out := canonical.Window{NewChangeNumber: sinceN}
	for _, w := range windows {
		if w.HasChanges {
			if !out.HasChanges {
				out.Start, out.End = w.Start, w.End
			} else {
				out.Start = min(out.Start, w.Start)
				out.End = max(out.End, w.End)
			}
			out.HasChanges = true
		}
		out.NewChangeNumber = max(out.NewChangeNumber, w.NewChangeNumber)
	}
	return out
}

// Feed is one configured feed of a source, named for error messages,
// with its own Status and ChangeWindow.
type Feed struct {
	Name         string
	Status       func(context.Context) (canonical.Status, error)
	ChangeWindow func(context.Context, int64) (canonical.Window, error)
}

// Feeds are the configured feeds of a source read through more than one
// (a subsource each). Whole says each feed reads the whole source
// (MergeStatuses). Kind names the source in errors.
type Feeds struct {
	Kind  string
	Whole bool
	List  []Feed
}

// Status merges the feeds' Status (MergeStatuses).
func (f Feeds) Status(ctx context.Context) (canonical.Status, error) {
	var statuses []canonical.Status
	for _, fd := range f.List {
		s, err := fd.Status(ctx)
		if err != nil {
			return canonical.Status{}, fmt.Errorf("%s %s status: %w", f.Kind, fd.Name, err)
		}
		statuses = append(statuses, s)
	}
	return MergeStatuses(statuses, f.Whole), nil
}

// ChangeWindow merges the feeds' windows past sinceN (MergeWindows).
func (f Feeds) ChangeWindow(ctx context.Context, sinceN int64) (canonical.Window, error) {
	var windows []canonical.Window
	for _, fd := range f.List {
		w, err := fd.ChangeWindow(ctx, sinceN)
		if err != nil {
			return canonical.Window{}, fmt.Errorf("%s %s ChangeWindow: %w", f.Kind, fd.Name, err)
		}
		windows = append(windows, w)
	}
	return MergeWindows(sinceN, windows), nil
}
