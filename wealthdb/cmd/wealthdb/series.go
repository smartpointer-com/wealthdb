package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"slices"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// A series is a view repeated at the end of every period of a window:
// `exposure` and `yield` take one with --period, the MCP yield tool with
// period. The window is FROM [TO], the last twelve months by default.

// seriesView is a view at req.asOf, or its series over req.series when
// that holds a point.
func seriesView[T any](req request, registry []columnSpec[T], defaults []string,
	fetch func(context.Context, *sql.DB, int64) ([]T, error)) *report {
	if len(req.series) == 0 {
		return newReport(registry, defaults, func(ctx context.Context, db *sql.DB) ([]T, error) {
			return fetch(ctx, db, req.asOf)
		})
	}
	return seriesReport(registry, defaults, req.series, fetch)
}

// seriesPoint is one period of a series: its label and the instant its
// figures are taken at, the period's last second or the window's end.
type seriesPoint struct {
	label string
	at    int64
}

// seriesRow is a row of a view, tagged with its period.
type seriesRow[T any] struct {
	period string
	row    T
}

// seriesReport repeats a view at every point of a series, a period
// column ahead of the view's own. A point before the first holding gold
// holds is skipped: nothing was held then.
func seriesReport[T any](registry []columnSpec[T], defaults []string, points []seriesPoint,
	fetch func(context.Context, *sql.DB, int64) ([]T, error)) *report {
	reg := []columnSpec[seriesRow[T]]{{Name: "period", Align: output.AlignLeft,
		Extract: func(r seriesRow[T]) string { return r.period }}}
	for _, c := range registry {
		col := columnSpec[seriesRow[T]]{Name: c.Name, Header: c.Header, Align: c.Align, Privacy: c.Privacy,
			Extract: func(r seriesRow[T]) string { return c.Extract(r.row) }}
		if c.PrivacyFunc != nil {
			col.PrivacyFunc = func(r seriesRow[T]) PrivacyClass { return c.PrivacyFunc(r.row) }
		}
		if c.PrivacyExtract != nil {
			col.PrivacyExtract = func(r seriesRow[T]) string { return c.PrivacyExtract(r.row) }
		}
		if c.Pseudonym != nil {
			col.Pseudonym = func(r seriesRow[T]) (string, string) { return c.Pseudonym(r.row) }
		}
		reg = append(reg, col)
	}
	return newReport(reg, append([]string{"period"}, defaults...), func(ctx context.Context, db *sql.DB) ([]seriesRow[T], error) {
		first, ok, err := gold.FirstHoldingsAt(ctx, db)
		if err != nil || !ok {
			return nil, err
		}
		var out []seriesRow[T]
		for _, p := range points {
			if p.at < first {
				continue
			}
			rows, err := fetch(ctx, db, p.at)
			if err != nil {
				return nil, err
			}
			for _, r := range rows {
				out = append(out, seriesRow[T]{period: p.label, row: r})
			}
		}
		return out, nil
	})
}

// seriesPeriodLengths are the periods a series takes, in display order,
// each with its length in months; a finer one would repeat the
// look-through more often than compositions change. The income
// projection buckets by the same periods.
var seriesPeriodLengths = []struct {
	name   string
	months int
}{{"monthly", 1}, {"quarterly", 3}, {"annual", 12}}

// seriesPeriods are the periods' names, in display order.
var seriesPeriods = func() []string {
	names := make([]string, len(seriesPeriodLengths))
	for i, p := range seriesPeriodLengths {
		names[i] = p.name
	}
	return names
}()

// seriesPeriodMonths is the length in months of the period named name,
// one of seriesPeriods.
func seriesPeriodMonths(name string) int {
	for _, p := range seriesPeriodLengths {
		if p.name == name {
			return p.months
		}
	}
	return 0
}

// seriesPoints are the periods of `period` that meet the window [from,
// to], each taken at its end (gold.PeriodEnds) and labelled by it. None
// when from is after to.
func seriesPoints(from, to int64, period string) []seriesPoint {
	var out []seriesPoint
	for _, at := range gold.PeriodEnds(from, to, seriesPeriodMonths(period)) {
		out = append(out, seriesPoint{label: periodLabel(&at, period), at: at})
	}
	return out
}

// parseSeries reads a command's --period. Without one it takes no
// positional and returns nil. With one it returns the series over its
// positional window (seriesWindow). It refuses a period that is none of
// seriesPeriods, what checkSeriesOptions refuses, and a window it cannot
// read.
func parseSeries(cmd string, fs *flag.FlagSet, view, period string, views []string, now time.Time) ([]seriesPoint, error) {
	if period == "" {
		return nil, noPositionals(fs, cmd)
	}
	if err := checkChoice(cmd, "period", period, seriesPeriods...); err != nil {
		return nil, err
	}
	has := func(name string) bool {
		if name == "as_of" {
			return isSet(fs, "d", "as-of")
		}
		return isSet(fs, name)
	}
	flag := func(name string) string {
		if name == "as_of" {
			return "-d"
		}
		return "--" + name
	}
	if err := checkSeriesOptions(cmd, view, views, has, flag); err != nil {
		return nil, err
	}
	points, err := seriesWindow(fs.Args(), period, now)
	switch {
	case errors.Is(err, errSeriesAfterToday):
		return nil, errs.Newf(errs.ExitUsage, "%s: %s", cmd, err.Error())
	case err != nil:
		fs.Usage()
		return nil, errs.Newf(errs.ExitUsage, "%s: %s", cmd, err.Error())
	}
	return points, nil
}

// checkSeriesOptions refuses what a series cannot honour, the CLI and
// the MCP server alike: a view not of views, and one as-of day. has says
// an option is given ("as_of" for the day); flag spells an option as the
// front-end does.
func checkSeriesOptions(cmd, view string, views []string, has func(string) bool, flag func(string) string) error {
	if !slices.Contains(views, view) {
		return errs.Newf(errs.ExitUsage, "%s: %s belongs to the %s views", cmd, flag("period"), andList(views))
	}
	if has("as_of") {
		return errs.Newf(errs.ExitUsage, "%s: %s names one day; a series takes a window instead", cmd, flag("as_of"))
	}
	return nil
}

// errSeriesAfterToday refuses a series whose window starts after today.
var errSeriesAfterToday = errors.New("the window starts after today")

// seriesWindow is the series of period over the window args states
// (FROM [TO], as parseTrailingYearWindow reads it). Nothing is held after
// today, so the window stops there; one that starts after it is
// errSeriesAfterToday.
func seriesWindow(args []string, period string, now time.Time) ([]seriesPoint, error) {
	from, to, err := parseTrailingYearWindow(args, now)
	if err != nil {
		return nil, err
	}
	points := seriesPoints(from, min(to, anchorToDay(now.UTC(), true).Unix()), period)
	if len(points) == 0 {
		return nil, errSeriesAfterToday
	}
	return points, nil
}
