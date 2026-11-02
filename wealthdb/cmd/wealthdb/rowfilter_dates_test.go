package main

import (
	"encoding/csv"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestDateMarksMatchTheCells holds the registries' date marks to what
// the views print, over every view of every fixture: a marked column
// prints dates or periods only, and a left-aligned column whose every
// cell is a date or a period is marked, or a condition on it would
// compare text. A view that takes a period runs monthly too, so its
// period columns print.
func TestDateMarksMatchTheCells(t *testing.T) {
	t.Parallel()
	fixtures := []string{setupSpendingGold(t), setupIncomeGold(t), setupCashflowGold(t), setupReturnsGold(t),
		setupGainsGold(t), setupExposureGold(t), setupYieldGold(t)}
	s := newTestMCP("", false)
	for _, cfg := range fixtures {
		srv := onFixtureToday(newTestMCP(cfg, false))
		for _, tool := range s.tools() {
			if tool.family == nil || tool.name == "status" {
				continue
			}
			periods := []string{""}
			if slices.ContainsFunc(tool.params, func(p param) bool { return p.name == "period" }) {
				periods = append(periods, "monthly")
			}
			for _, v := range tool.family.viewsOrOne() {
				for _, period := range periods {
					checkDateMarks(t, srv, tool, v, period)
				}
			}
		}
	}
}

// checkDateMarks is TestDateMarksMatchTheCells for one call: view v of
// tool, with period when it is not "".
func checkDateMarks(t *testing.T, srv *mcpServer, tool toolSpec, v, period string) {
	t.Helper()
	args := map[string]any{"format": "csv", "columns": "all", "max_chars": maxIntegerParam}
	if v != "" {
		args["view"] = v
	}
	req := defaultRequest(v, "USD")
	if period != "" {
		args["period"] = period
		req.period = period
		if tool.family.window == asOfDate {
			// A view as of one day takes period as a series.
			req.series = []seriesPoint{{}}
		}
	}
	out, err := callTool(t, srv, tool.name, args)
	if err != nil {
		return // a view the fixture cannot serve, or one that needs options
	}
	_, body, _ := strings.Cut(out, "\n")
	recs, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil || len(recs) < 2 {
		return
	}
	rep := tool.family.build(nil, req, &toolArgs{values: map[string]any{}})
	for j, h := range recs[0] {
		var col *reportColumn
		for i := range rep.columns {
			if rep.columns[i].header == h {
				col = &rep.columns[i]
			}
		}
		if col == nil || col.align != 0 {
			continue
		}
		cells, dates := 0, 0
		for _, r := range recs[1:] {
			// A period column's total and average rows are no period.
			if c := strings.TrimSpace(r[j]); c != "" && c != "total" && c != "monthly_average" {
				cells++
				if _, ok := cellDay(c); ok {
					dates++
				}
			}
		}
		switch {
		case col.date && dates < cells:
			t.Errorf("%s %s %s: %s is marked a date, but prints %d cells that are none", tool.name, v, period, h, cells-dates)
		case !col.date && cells > 0 && dates == cells:
			t.Errorf("%s %s %s: %s prints dates only, but is not marked a date", tool.name, v, period, h)
		}
	}
}

// TestDateNamedColumnsAreMarked holds every registry to its names: a
// column named as a date (date, datetime, period, period_start, *_date,
// *_at, *as_of) is marked one, whether or not a fixture prints it.
func TestDateNamedColumnsAreMarked(t *testing.T) {
	t.Parallel()
	dateNamed := func(name string) bool {
		switch name {
		case "date", "datetime", "period", "period_start":
			return true
		}
		return strings.HasSuffix(name, "_date") || strings.HasSuffix(name, "_at") || strings.HasSuffix(name, "as_of")
	}
	for _, tool := range newTestMCP("", false).tools() {
		if tool.family == nil {
			continue
		}
		for _, v := range tool.family.viewsOrOne() {
			for _, series := range []bool{false, true} {
				req := defaultRequest(v, "USD")
				if series {
					req.series = []seriesPoint{{}}
				}
				for _, c := range tool.family.build(nil, req, &toolArgs{values: map[string]any{}}).columns {
					if dateNamed(c.name) && !c.date {
						t.Errorf("%s %s: %s is named as a date, but is not marked one", tool.name, v, c.name)
					}
				}
			}
		}
	}
}

// TestCellDay reads every form a date or period cell prints, by its
// first day, and nothing else: a relative word, a slash date and a
// day or month that does not exist are no cell date.
func TestCellDay(t *testing.T) {
	t.Parallel()
	day := func(s string) int64 {
		d, err := time.Parse(time.DateOnly, s)
		if err != nil {
			t.Fatal(err)
		}
		return d.Unix() / 86400
	}
	for _, c := range []struct {
		cell string
		want string // "" for no date
	}{
		{"2026", "2026-01-01"}, {" 2026-03 ", "2026-03-01"}, {"2026-03-15", "2026-03-15"},
		{"2026-Q2", "2026-04-01"}, {"2026-q4", "2026-10-01"}, {"2024-02-29", "2024-02-29"},
		{"2026-03-15 10:00", "2026-03-15"}, {"2026-03-15T10:00:00Z", "2026-03-15"},
		{"", ""}, {"total", ""}, {"n/a", ""}, {"today", ""}, {"2026-Q5", ""}, {"2026-Q0", ""},
		{"2026-13", ""}, {"2026-00", ""}, {"2026-02-30", ""}, {"2026-3", ""}, {"20260", ""},
		{"2026/03/15", ""}, {"03/15/2026", ""}, {"202a", ""}, {"2026-03-1x", ""}, {"2026-03-15 later", ""},
	} {
		got, ok := cellDay(c.cell)
		switch {
		case c.want == "" && ok:
			t.Errorf("cellDay(%q) = %d, want no date", c.cell, got)
		case c.want != "" && (!ok || got != day(c.want)):
			t.Errorf("cellDay(%q) = %d %v, want %s", c.cell, got, ok, c.want)
		}
	}
}
