package silver

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// The observed watermarks of canonical.Status: when a source was last
// looked at, as against when its latest fact is dated. A portal that
// marks its holdings quarterly and was read this morning is observed
// through this morning; its latest snapshot is the quarter's end.

// LatestRun is the instant of the latest run a query yields one
// (MAX(...)) row for, -1 when there is none: the observed watermark of a
// silver whose every run reads the source's holdings, and its ledger from
// its last run or the start of its history.
func LatestRun(ctx context.Context, db *sql.DB, kind, q string) (int64, error) {
	var t sql.NullInt64
	if err := db.QueryRowContext(ctx, q).Scan(&t); err != nil {
		return -1, fmt.Errorf("%s Status observed: %w", kind, err)
	}
	if !t.Valid {
		return -1, nil
	}
	return t.Int64, nil
}

// ObserveByRuns sets both observed watermarks of s to the latest run
// (LatestRun).
func ObserveByRuns(ctx context.Context, db *sql.DB, kind, q string, s *canonical.Status) error {
	t, err := LatestRun(ctx, db, kind, q)
	if err != nil {
		return err
	}
	s.ObservedSnapshotsThrough, s.ObservedTransactionsThrough = t, t
	return nil
}

// Span is one run's fetch window, [Since, Until] in Unix seconds UTC.
type Span struct{ Since, Until int64 }

// SpanSlack joins two windows that meet within a day: windows are
// fetched by date, so one that ends on a date and one that starts on the
// next leave no gap.
const SpanSlack = 86400

// CoveredThrough is the end of the first gapless stretch the spans
// cover, from the earliest start: the instant through which every moment
// has been fetched by some run. Spans that meet within slack join. -1
// when there is no span.
func CoveredThrough(spans []Span, slack int64) int64 {
	if len(spans) == 0 {
		return -1
	}
	s := slices.Clone(spans)
	slices.SortFunc(s, func(a, b Span) int {
		switch {
		case a.Since < b.Since:
			return -1
		case a.Since > b.Since:
			return 1
		}
		return 0
	})
	end := s[0].Until
	for _, sp := range s[1:] {
		if sp.Since > end+slack {
			break
		}
		end = max(end, sp.Until)
	}
	return end
}

// RunSpans reads the (since, until) rows a query yields, one per run.
func RunSpans(ctx context.Context, db *sql.DB, kind, q string) ([]Span, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%s Status windows: %w", kind, err)
	}
	defer rows.Close()
	var spans []Span
	for rows.Next() {
		var sp Span
		if err := rows.Scan(&sp.Since, &sp.Until); err != nil {
			return nil, fmt.Errorf("%s Status windows: %w", kind, err)
		}
		spans = append(spans, sp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s Status windows: %w", kind, err)
	}
	return spans, nil
}

// CoveredByRuns is CoveredThrough over RunSpans.
func CoveredByRuns(ctx context.Context, db *sql.DB, kind, q string) (int64, error) {
	spans, err := RunSpans(ctx, db, kind, q)
	if err != nil {
		return -1, err
	}
	return CoveredThrough(spans, SpanSlack), nil
}

// mergeObserved combines the observed watermarks of a source's feeds.
// Feeds that each read the whole source take the later (whole); feeds
// that each read a part of it take the earlier, since the source is
// observed only as far as every part is. A feed with no watermark (-1)
// does not count.
func mergeObserved(statuses []canonical.Status, whole bool) (snapshots, transactions int64) {
	pick := func(get func(canonical.Status) int64) int64 {
		out := int64(-1)
		for _, st := range statuses {
			v := get(st)
			switch {
			case v < 0:
			case out < 0:
				out = v
			case whole:
				out = max(out, v)
			default:
				out = min(out, v)
			}
		}
		return out
	}
	return pick(func(s canonical.Status) int64 { return s.ObservedSnapshotsThrough }),
		pick(func(s canonical.Status) int64 { return s.ObservedTransactionsThrough })
}
