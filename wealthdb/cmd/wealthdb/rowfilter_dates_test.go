package main

import (
	"encoding/csv"
	"strings"
	"testing"
)

// TestDateMarksMatchTheCells holds the registries' date marks to what
// the views print, over every view of every fixture: a marked column
// prints dates or periods only, and a left-aligned column whose every
// cell is a date or a period is marked, or a condition on it would
// compare text.
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
			for _, v := range tool.family.viewsOrOne() {
				args := map[string]any{"format": "csv", "columns": "all", "max_chars": maxIntegerParam}
				if v != "" {
					args["view"] = v
				}
				out, err := callTool(t, srv, tool.name, args)
				if err != nil {
					continue // a view the fixture cannot serve, or one that needs options
				}
				_, body, _ := strings.Cut(out, "\n")
				recs, err := csv.NewReader(strings.NewReader(body)).ReadAll()
				if err != nil || len(recs) < 2 {
					continue
				}
				rep := tool.family.build(nil, defaultRequest(v, "USD"), &toolArgs{values: map[string]any{}})
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
						t.Errorf("%s %s: %s is marked a date, but prints %d cells that are none", tool.name, v, h, cells-dates)
					case !col.date && cells > 0 && dates == cells:
						t.Errorf("%s %s: %s prints dates only, but is not marked a date", tool.name, v, h)
					}
				}
			}
		}
	}
}
