package ubs

import (
	"context"
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
		    sector, residual_value, payload) VALUES (?, ?, 'Investment Alpha', ?, ?, ?, '{}')`,
			token, i+1, l[0], sector, l[2])
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
	got, err := r.exposureReports(context.Background(), fullWindow())
	if err != nil {
		t.Fatal(err)
	}
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
	got, err := r.exposureReports(context.Background(), canonical.Window{HasChanges: true, Start: q2, End: q2})
	if err != nil {
		t.Fatal(err)
	}
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
	got, err := r.exposureReports(context.Background(), fullWindow())
	if err != nil || got != nil {
		t.Errorf("exposureReports = %v, %v; want none", got, err)
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
		"Healthcare Technology":                                string(canonical.SectorHealthCare),
		"Health Care Software":                                 string(canonical.SectorHealthCare),
		"Enterprise Software":                                  string(canonical.SectorTechnology),
		"Information Technology":                               string(canonical.SectorTechnology),
		"Industrial and office products distribution & retail": string(canonical.SectorIndustrials),
		"Consumer / Retail":                                    string(canonical.SectorConsumerCyclical),
		"Diversified":                                          canonical.Unknown,
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
	stream, err := conn.Snapshots(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var got []canonical.ExposureReportChange
	for {
		batch, more, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, batch.ExposureReports...)
		if !more {
			break
		}
	}
	if len(got) != 1 || got[0].InstrumentExternalID != "XX0000000066" || got[0].AsOf != 100*day {
		t.Fatalf("reports %+v, want XX0000000066 on day 100", got)
	}
	near(t, "europe", got[0].Dimensions[canonical.DimensionRegion]["europe"], 75)
}
