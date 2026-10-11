package silver

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

// Removals are the holdings a portal stopped listing, keyed by position
// key, each with the time of the first complete dump that omitted it.
//
// A silver keyed by event date records nothing when a portal drops a
// holding whose marks are unchanged, so the holding would persist in
// gold until the next data date. The roster a dump lists is negative
// data about every holding it leaves out: the adapter adds the dump
// time to its snapshot times and emits the roster without the removed
// holdings there, each remaining position at its own mark
// (canonical.PositionChange.ValuedAt). Gold then ends the holding at
// the dump, with no mark invented for it.
//
// Absence is evidence only inside what a dump read. The adapters that
// use this read whole-portfolio dumps and skip dry runs; a run that
// covers one account or one asset class must never feed it (the
// ragged edge: DESIGN.md §6.4).
type Removals map[string]int64

// QueryRemovals runs q, which yields (position key, removal time) rows,
// into a Removals.
func QueryRemovals(ctx context.Context, db *sql.DB, label, q string, args ...any) (Removals, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("%s removals: %w", label, err)
	}
	defer rows.Close()
	out := Removals{}
	for rows.Next() {
		var (
			key string
			at  int64
		)
		if err := rows.Scan(&key, &at); err != nil {
			return nil, err
		}
		out[key] = at
	}
	return out, rows.Err()
}

// Removed reports whether the holding is out of the roster at t.
func (r Removals) Removed(key string, t int64) bool {
	at, ok := r[key]
	return ok && at <= t
}

// Times are the distinct removal times in ascending order: each is a
// snapshot time, since the roster changes there.
func (r Removals) Times() []int64 {
	seen := make(map[int64]bool, len(r))
	out := make([]int64, 0, len(r))
	for _, at := range r {
		if !seen[at] {
			seen[at] = true
			out = append(out, at)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Reach returns the later of t and the latest removal time, or t when
// nothing was removed. A window's end and a status's latest snapshot
// reach to the latest removal, since the dump time can lie after every
// mark.
func (r Removals) Reach(t int64) int64 {
	for _, at := range r {
		if at > t {
			t = at
		}
	}
	return t
}
