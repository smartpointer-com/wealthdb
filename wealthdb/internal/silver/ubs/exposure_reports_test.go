package ubs

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// newReportFixture builds a ubs-web silver holding the quarterly-report
// tables (collector migration 0017). Every name, figure and identifier is
// invented.
func newReportFixture(t *testing.T) *webReader {
	t.Helper()
	const schema = `
CREATE TABLE private_market_reports (
    source_doc_token TEXT PRIMARY KEY, instrument_isin TEXT, fund_name TEXT,
    as_of INTEGER, lines_as_of INTEGER, currency_iso TEXT, commitment REAL,
    capital_called REAL, cumulative_expenses REAL, valuation REAL,
    shares_in_issue REAL, nav_per_share REAL, line_count INTEGER,
    lines_total REAL, lines_reconciled INTEGER, payload TEXT);
CREATE TABLE private_market_report_lines (
    source_doc_token TEXT, line_no INTEGER, name TEXT, region TEXT,
    sector TEXT, strategy TEXT, cost REAL, residual_value REAL, payload TEXT,
    PRIMARY KEY (source_doc_token, line_no));`
	_, db := silvertest.NewDB(t, schema)
	return &webReader{db: db}
}

// seedReport seeds one report and its lines: each line a region, a sector
// ("" for none) and a residual value.
func seedReport(t *testing.T, r *webReader, token, isin string, asOf int64, reconciled int, lines ...[3]any) {
	t.Helper()
	silvertest.Exec(t, r.db, `INSERT INTO private_market_reports (source_doc_token, instrument_isin, fund_name,
	    as_of, line_count, lines_reconciled, payload) VALUES (?, ?, 'Example Feeder Fund II', ?, ?, ?, '{}')`,
		token, isin, asOf, len(lines), reconciled)
	for i, l := range lines {
		var sector any
		if s := l[1].(string); s != "" {
			sector = s
		}
		silvertest.Exec(t, r.db, `INSERT INTO private_market_report_lines (source_doc_token, line_no, name, region,
		    sector, residual_value, payload) VALUES (?, ?, ?, ?, ?, ?, '{}')`,
			token, i+1, fmt.Sprintf("Investment %c", 'A'+i), l[0], sector, l[2])
	}
}

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// The split is computed from the lines at residual value: by region
// always, by sector where the table prints one. A line at or below zero
// adds nothing, and a label no map knows is unknown.
func TestQuarterlyReportSplitsAtResidualValue(t *testing.T) {
	t.Parallel()
	r := newReportFixture(t)
	const q2 = 1909008000
	seedReport(t, r, "doc-a", "XX0000000011", q2, 1,
		[3]any{"North America", "Software", 617.3},
		[3]any{"Europe", "Financial Services", 205.8},
		[3]any{"Asia-Pacific", "Consumer / Retail", 308.6},
		[3]any{"Atlantis", "Space Tourism", 102.9},
		[3]any{"Europe", "Industrials", -4.1})
	reports, err := r.quarterlyReports(context.Background(), fullWindow())
	if err != nil {
		t.Fatal(err)
	}
	got := reports.records
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	rec := got[0]
	if rec.InstrumentExternalID != "XX0000000011" || rec.AsOf != q2 || rec.Note != "Example Feeder Fund II" {
		t.Errorf("record = %+v", rec)
	}
	region, sector := rec.Dimensions[canonical.DimensionRegion], rec.Dimensions[canonical.DimensionSector]
	const total = 617.3 + 205.8 + 308.6 + 102.9
	near(t, "north_america", region["north_america"], 100*617.3/total)
	near(t, "europe", region["europe"], 100*205.8/total)
	near(t, "asia_pacific", region["asia_pacific"], 100*308.6/total)
	near(t, "unknown region", region["unknown"], 100*102.9/total)
	near(t, "technology", sector["technology"], 100*617.3/total)
	near(t, "financials", sector["financials"], 100*205.8/total)
	near(t, "consumer_cyclical", sector["consumer_cyclical"], 100*308.6/total)
	near(t, "unknown sector", sector["unknown"], 100*102.9/total)
	if _, ok := sector["industrials"]; ok {
		t.Error("a line below zero counted")
	}
	if len(rec.Dimensions) != 2 {
		t.Errorf("dimensions = %v, want region and sector", rec.Dimensions)
	}
}

// A table that prints no sector gives a region split alone. An
// unreconciled report gives nothing, a report outside the window
// nothing, and of two documents for one quarter the first one counts.
func TestQuarterlyReportRecords(t *testing.T) {
	t.Parallel()
	r := newReportFixture(t)
	const q1, q2 = 1901145600, 1909008000
	seedReport(t, r, "doc-b", "XX0000000022", q2, 1, [3]any{"N. America", "", 21.7}, [3]any{"ROW", "", 65.1})
	seedReport(t, r, "doc-c", "XX0000000022", q2, 1, [3]any{"Europe", "", 17.3})
	seedReport(t, r, "doc-d", "XX0000000033", q2, 0, [3]any{"Europe", "", 17.3})
	seedReport(t, r, "doc-e", "XX0000000044", q1, 1, [3]any{"Europe", "", 17.3})
	seedReport(t, r, "doc-f", "XX0000000055", q2, 1, [3]any{"Europe", "", 0.0})
	reports, err := r.quarterlyReports(context.Background(), canonical.Window{HasChanges: true, Start: q2, End: q2})
	if err != nil {
		t.Fatal(err)
	}
	got := reports.records
	if len(got) != 1 || got[0].InstrumentExternalID != "XX0000000022" {
		t.Fatalf("got %+v, want XX0000000022 alone", got)
	}
	dims := got[0].Dimensions
	if _, ok := dims[canonical.DimensionSector]; ok || len(dims) != 1 {
		t.Errorf("dimensions = %v, want region alone", dims)
	}
	near(t, "north_america", dims[canonical.DimensionRegion]["north_america"], 25)
	near(t, "other", dims[canonical.DimensionRegion]["other"], 75)

	lo, hi, err := r.reportRange(context.Background())
	if err != nil || lo != q1 || hi != q2 {
		t.Errorf("reportRange = %d, %d (%v), want %d, %d", lo, hi, err, q1, q2)
	}
}

// A silver from before migration 0017 holds no reports.
func TestQuarterlyReportsNeedTheirTables(t *testing.T) {
	t.Parallel()
	r := newCardFixture(t)
	got, err := r.quarterlyReports(context.Background(), fullWindow())
	if err != nil || got.batches() != nil {
		t.Errorf("quarterlyReports = %+v, %v; want none", got, err)
	}
	if lo, hi, err := r.reportRange(context.Background()); err != nil || lo != -1 || hi != -1 {
		t.Errorf("reportRange = %d, %d, %v", lo, hi, err)
	}
}

// A printed sector maps by the first stem it contains, in the table's
// order, so a label naming two sectors reads as the earlier one.
func TestReportSectorStemOrder(t *testing.T) {
	t.Parallel()
	for label, want := range map[string]string{
		"Healthcare Technology":                  string(canonical.SectorHealthCare),
		"Health Care Software":                   string(canonical.SectorHealthCare),
		"Enterprise Software":                    string(canonical.SectorTechnology),
		"Information Technology":                 string(canonical.SectorTechnology),
		"Industrial supplies wholesale & retail": string(canonical.SectorIndustrials),
		"Consumer / Retail":                      string(canonical.SectorConsumerCyclical),
		"Diversified":                            canonical.Unknown,
	} {
		if got := reportSector(label); got != want {
			t.Errorf("reportSector(%q) = %q, want %q", label, got, want)
		}
	}
}

// TestTheMergedStreamCarriesTheReports drives Connection.ChangeWindow and
// Connection.Snapshots on a web-only silver: the window reaches back to a
// report dated before the live dumps, and the stream yields its split.
func TestTheMergedStreamCarriesTheReports(t *testing.T) {
	ctx := context.Background()
	const day = int64(86400)
	web := newWebFixture(t)
	silvertest.Exec(t, web.db, `
CREATE TABLE dump_runs (snapshot_at INTEGER PRIMARY KEY, run_dir TEXT);
CREATE TABLE transactions (value_date INTEGER);
CREATE TABLE portfolios (
    snapshot_at INTEGER, portfolio_external_id TEXT, banking_relationship_id TEXT,
    description TEXT, payload TEXT);
CREATE TABLE accounts (
    snapshot_at INTEGER, account_external_id TEXT, kind TEXT, currency_iso TEXT,
    banking_relationship_id TEXT, portfolio_external_id TEXT, description TEXT, payload TEXT);
CREATE TABLE positions (
    snapshot_at INTEGER, instrument_isin TEXT, currency_iso TEXT, description TEXT);
CREATE TABLE private_market_reports (
    source_doc_token TEXT PRIMARY KEY, instrument_isin TEXT, fund_name TEXT,
    as_of INTEGER, lines_as_of INTEGER, currency_iso TEXT, commitment REAL,
    capital_called REAL, cumulative_expenses REAL, valuation REAL,
    shares_in_issue REAL, nav_per_share REAL, line_count INTEGER,
    lines_total REAL, lines_reconciled INTEGER, payload TEXT);
CREATE TABLE private_market_report_lines (
    source_doc_token TEXT, line_no INTEGER, name TEXT, region TEXT,
    sector TEXT, strategy TEXT, cost REAL, residual_value REAL, payload TEXT,
    PRIMARY KEY (source_doc_token, line_no));`)
	silvertest.Exec(t, web.db, `INSERT INTO dump_runs VALUES (?, 'run')`, 400*day)
	seedReport(t, web, "doc-m", "XX0000000066", 100*day, 1, [3]any{"Europe", "", 30.0}, [3]any{"Asia", "", 10.0})

	conn := &Connection{web: web}
	w, err := conn.ChangeWindow(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !w.HasChanges || w.Start > 100*day {
		t.Fatalf("window %+v does not reach the report's day %d", w, 100*day)
	}
	var got []canonical.ExposureReportChange
	var comps []canonical.FundCompositionChange
	var lines []canonical.FundLineChange
	for _, batch := range silvertest.Snapshots(t, conn, w) {
		got = append(got, batch.ExposureReports...)
		comps, lines = append(comps, batch.FundCompositions...), append(lines, batch.FundLines...)
	}
	if len(got) != 1 || got[0].InstrumentExternalID != "XX0000000066" || got[0].AsOf != 100*day {
		t.Fatalf("reports %+v, want XX0000000066 on day 100", got)
	}
	near(t, "europe", got[0].Dimensions[canonical.DimensionRegion]["europe"], 75)
	if len(comps) != 1 || comps[0].FundRef != "isin:XX0000000066" || len(lines) != 2 {
		t.Errorf("compositions %+v with %d lines, want isin:XX0000000066 with 2", comps, len(lines))
	}
}

// A reconciled report is also the fund's composition: each line above
// zero at its residual value over every line's, so a negative line
// lifts the others above 100. A line states its region and sector where
// a map knows them, and is keyed by its name. The composition holds
// until the fund's next report, reconciled or not; the last one has no
// bound.
func TestQuarterlyReportComposition(t *testing.T) {
	t.Parallel()
	r := newReportFixture(t)
	const q1, q2, q3 = 1901145600, 1909008000, 1916956800
	seedReport(t, r, "doc-p", "XX0000000077", q1, 1,
		[3]any{"North America", "Software", 60.0},
		[3]any{"Atlantis", "Space Tourism", 30.0},
		[3]any{"Europe", "", 0.0},
		[3]any{"Europe", "Industrials", -10.0})
	seedReport(t, r, "doc-q", "XX0000000077", q2, 0, [3]any{"Europe", "", 5.0})
	seedReport(t, r, "doc-r", "XX0000000077", q3, 1, [3]any{"Rest of World", "", 5.0})
	got, err := r.quarterlyReports(context.Background(), fullWindow())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.comps) != 2 || len(got.records) != 2 {
		t.Fatalf("got %d compositions and %d records, want 2 and 2", len(got.comps), len(got.records))
	}
	c := got.comps[0]
	if c.FundRef != "isin:XX0000000077" || c.AsOf != q1 || c.Basis != canonical.CompositionHoldings ||
		c.Origin != canonical.CompositionParsed || c.Provider != reportProvider || c.LineCount != 2 {
		t.Errorf("composition = %+v", c)
	}
	if c.ValidUntil == nil || *c.ValidUntil != q2 {
		t.Errorf("valid until %v, want the unreconciled report's day %d", c.ValidUntil, q2)
	}
	if got.comps[1].ValidUntil != nil {
		t.Errorf("the last composition is bound to %d", *got.comps[1].ValidUntil)
	}
	ws, _ := c.WeightSum.Float64()
	near(t, "weight sum", ws, 112.5)
	var first []canonical.FundLineChange
	for _, l := range got.lines {
		if l.AsOf == q1 {
			first = append(first, l)
		}
	}
	if len(first) != 2 {
		t.Fatalf("lines %+v, want the two above zero", first)
	}
	a, b := first[0], first[1]
	wa, _ := a.Weight.Float64()
	wb, _ := b.Weight.Float64()
	near(t, "first weight", wa, 75)
	near(t, "second weight", wb, 37.5)
	if a.LineNo != 1 || *a.Name != "Investment A" || a.SecurityKey != canonical.SecurityKey("", "", "", "", "Investment A") {
		t.Errorf("first line = %+v", a)
	}
	if a.Region == nil || *a.Region != "north_america" || a.Sector == nil || *a.Sector != "technology" {
		t.Errorf("first line's labels = %v, %v", a.Region, a.Sector)
	}
	if b.Region != nil || b.Sector != nil {
		t.Errorf("an unknown label is stated: %v, %v", b.Region, b.Sector)
	}
	if r := got.lines[len(got.lines)-1].Region; r == nil || *r != "other" {
		t.Errorf("rest of world reads %v, want other", r)
	}
	// A window that holds the first report alone still bounds it by the
	// next one.
	narrow, err := r.quarterlyReports(context.Background(), canonical.Window{HasChanges: true, Start: q1, End: q1})
	if err != nil {
		t.Fatal(err)
	}
	if len(narrow.comps) != 1 || narrow.comps[0].ValidUntil == nil || *narrow.comps[0].ValidUntil != q2 {
		t.Errorf("narrow window: %+v, want the first composition bound to %d", narrow.comps, q2)
	}
}

// The weights are rounded to eight places and sum to exactly 100 where
// no line is below zero: the largest line takes what the rounding
// leaves.
func TestQuarterlyReportWeightsSumExactly(t *testing.T) {
	t.Parallel()
	lines := []reportLine{{no: 1, name: "Investment A", residual: 1}, {no: 2, name: "Investment B", residual: 1},
		{no: 3, name: "Investment C", residual: 1.000001}}
	comp, out, ok := reportComposition("XX0000000088", 0, lines)
	if !ok || len(out) != 3 {
		t.Fatalf("composition %+v, %d lines, %v", comp, len(out), ok)
	}
	var sum canonical.Decimal
	for _, l := range out {
		if l.Weight.Exponent() < -8 {
			t.Errorf("line %d weight %s has more than eight places", l.LineNo, l.Weight)
		}
		sum = sum.Add(*l.Weight)
	}
	if !sum.Equal(canonical.NewDecimalFromInt(100)) || !comp.WeightSum.Equal(sum) {
		t.Errorf("weights sum to %s, composition says %s; want 100", sum, comp.WeightSum)
	}
	if _, _, ok := reportComposition("XX0000000088", 0, []reportLine{{residual: -1}, {residual: 0}}); ok {
		t.Error("lines at or below zero gave a composition")
	}
}
