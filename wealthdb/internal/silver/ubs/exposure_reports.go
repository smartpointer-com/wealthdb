package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// The split of a private-markets fund, from its quarterly reports
// (docs/adapters/ubs.md, private markets; docs/EXPOSURE.md §9).
//
// ubs-web reads each quarterly report into private_market_reports and
// its per-investment table into private_market_report_lines (collector
// migration 0017). A report whose lines reconcile to the table's total
// becomes one exposure record for the feeder fund's ISIN, dated the
// report's capital-account date: its region split, and its sector split
// where the table prints a sector, each computed from the lines at
// residual value and stated in percent. A line at or below zero adds
// nothing. The table's strategy column (buyout, growth) states no
// sector and no asset class, so it is not read; the holding keeps its
// own class.
//
// The records go to gold's exposure_reports, which the declarations
// merge beside the hand ledger: on a day the latest record wins, and
// the ledger's on an equal date.

const (
	reportsTable     = "private_market_reports"
	reportLinesTable = "private_market_report_lines"
)

// reportRegions maps the geographies the tables print onto the regions.
// Global states no region; a label this map does not know reads as
// unknown too.
var reportRegions = map[string]canonical.Region{
	"north america": canonical.RegionNorthAmerica, "n. america": canonical.RegionNorthAmerica,
	"united states": canonical.RegionNorthAmerica, "usa": canonical.RegionNorthAmerica,
	"canada":        canonical.RegionNorthAmerica,
	"latin america": canonical.RegionLatinAmerica, "brazil": canonical.RegionLatinAmerica,
	"europe": canonical.RegionEurope, "western europe": canonical.RegionEurope,
	"united kingdom": canonical.RegionEurope, "uk": canonical.RegionEurope, "germany": canonical.RegionEurope,
	"france": canonical.RegionEurope, "nordics": canonical.RegionEurope,
	"middle east": canonical.RegionMiddleEastAfrica, "africa": canonical.RegionMiddleEastAfrica,
	"israel":       canonical.RegionMiddleEastAfrica,
	"asia-pacific": canonical.RegionAsiaPacific, "asia pacific": canonical.RegionAsiaPacific,
	"apac": canonical.RegionAsiaPacific, "asia": canonical.RegionAsiaPacific,
	"australia": canonical.RegionAsiaPacific, "india": canonical.RegionAsiaPacific,
	"china": canonical.RegionAsiaPacific, "japan": canonical.RegionAsiaPacific,
	"korea":         canonical.RegionAsiaPacific,
	"rest of world": canonical.RegionOther, "row": canonical.RegionOther,
}

// reportSectors maps a printed sector onto a sector by the first word
// stem it contains, in this order: "Healthcare Technology" is health
// care, "Industrial and office products distribution & retail"
// industrials, "Consumer / Retail" consumer cyclical. A label no stem
// matches reads as unknown.
var reportSectors = []struct {
	stem   string
	sector canonical.Sector
}{
	{"health", canonical.SectorHealthCare},
	{"software", canonical.SectorTechnology}, {"technolog", canonical.SectorTechnology},
	{"financ", canonical.SectorFinancials}, {"insurance", canonical.SectorFinancials},
	{"consumer staples", canonical.SectorConsumerStaples},
	{"industrial", canonical.SectorIndustrials}, {"business services", canonical.SectorIndustrials},
	{"consumer", canonical.SectorConsumerCyclical}, {"retail", canonical.SectorConsumerCyclical},
	{"energy", canonical.SectorEnergy}, {"material", canonical.SectorMaterials},
	{"real estate", canonical.SectorRealEstate}, {"utilit", canonical.SectorUtilities},
	{"communication", canonical.SectorCommunication}, {"media", canonical.SectorCommunication},
	{"telecom", canonical.SectorCommunication},
}

func reportRegion(label string) string {
	if r, ok := reportRegions[strings.ToLower(strings.TrimSpace(label))]; ok {
		return string(r)
	}
	return canonical.Unknown
}

func reportSector(label string) string {
	l := strings.ToLower(label)
	for _, s := range reportSectors {
		if strings.Contains(l, s.stem) {
			return string(s.sector)
		}
	}
	return canonical.Unknown
}

// reportLine is one investment line: its labels and its residual value.
type reportLine struct {
	region   string
	sector   sql.NullString
	residual float64
}

// reportSplit is a report's split by region, and by sector where every
// line it counts prints one, in percent of the lines' residual value.
// ok is false when no line has a value above zero.
func reportSplit(lines []reportLine) (dims map[string]map[string]float64, ok bool) {
	var total float64
	for _, l := range lines {
		if l.residual > 0 {
			total += l.residual
		}
	}
	if total <= 0 {
		return nil, false
	}
	region, sector := map[string]float64{}, map[string]float64{}
	sectored := true
	for _, l := range lines {
		if l.residual <= 0 {
			continue
		}
		pct := 100 * l.residual / total
		region[reportRegion(l.region)] += pct
		if !l.sector.Valid {
			sectored = false
			continue
		}
		sector[reportSector(l.sector.String)] += pct
	}
	dims = map[string]map[string]float64{canonical.DimensionRegion: region}
	if sectored {
		dims[canonical.DimensionSector] = sector
	}
	return dims, true
}

// exposureReports reads the reconciled quarterly reports dated in w, one
// record per fund and date: where two documents report the same quarter,
// the first by document token.
func (r *webReader) exposureReports(ctx context.Context, w canonical.Window) ([]canonical.ExposureReportChange, error) {
	if !w.HasChanges {
		return nil, nil
	}
	ok, err := silver.HasTables(ctx, r.db, reportsTable, reportLinesTable)
	if err != nil || !ok {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `
WITH chosen AS (
    SELECT source_doc_token, instrument_isin, as_of, fund_name, line_count,
           ROW_NUMBER() OVER (PARTITION BY instrument_isin, as_of ORDER BY source_doc_token) AS n
      FROM `+reportsTable+`
     WHERE lines_reconciled = 1 AND as_of BETWEEN ? AND ?)
SELECT c.instrument_isin, c.as_of, c.fund_name, c.line_count, l.region, l.sector, l.residual_value
  FROM chosen c
  JOIN `+reportLinesTable+` l USING (source_doc_token)
 WHERE c.n = 1 AND l.residual_value IS NOT NULL
 ORDER BY c.instrument_isin, c.as_of, l.line_no`, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("ubs-web quarterly reports: %w", err)
	}
	defer rows.Close()
	type report struct {
		isin  string
		asOf  int64
		name  sql.NullString
		count int
		lines []reportLine
	}
	var reports []*report
	for rows.Next() {
		var (
			rep  report
			line reportLine
		)
		if err := rows.Scan(&rep.isin, &rep.asOf, &rep.name, &rep.count, &line.region, &line.sector,
			&line.residual); err != nil {
			return nil, fmt.Errorf("ubs-web quarterly reports scan: %w", err)
		}
		if n := len(reports); n == 0 || reports[n-1].isin != rep.isin || reports[n-1].asOf != rep.asOf {
			reports = append(reports, &rep)
		}
		last := reports[len(reports)-1]
		last.lines = append(last.lines, line)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []canonical.ExposureReportChange
	for _, rep := range reports {
		dims, ok := reportSplit(rep.lines)
		if !ok {
			continue
		}
		src, err := json.Marshal(map[string]any{"document": "quarterly report", "lines": rep.count})
		if err != nil {
			return nil, err
		}
		out = append(out, canonical.ExposureReportChange{
			InstrumentExternalID: rep.isin, AsOf: rep.asOf, Dimensions: dims,
			Source: src, Note: rep.name.String,
		})
	}
	return out, nil
}

// reportRange is the span of the quarterly reports' dates, so the change
// window reaches every report: (-1, -1) for a silver without them.
func (r *webReader) reportRange(ctx context.Context) (int64, int64, error) {
	ok, err := silver.HasTables(ctx, r.db, reportsTable)
	if err != nil || !ok {
		return -1, -1, err
	}
	return silver.Extrema(ctx, r.db, "reportRange", []string{
		`SELECT MIN(as_of), MAX(as_of) FROM ` + reportsTable,
	})
}
