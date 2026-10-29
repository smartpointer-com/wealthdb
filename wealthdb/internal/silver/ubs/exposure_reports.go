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

// What a private-markets fund holds, from its quarterly reports
// (docs/adapters/ubs.md §13; docs/EXPOSURE.md §4, §9).
//
// ubs-web reads each quarterly report into private_market_reports and
// its per-investment table into private_market_report_lines (collector
// migration 0017). A report whose lines reconcile to the table's total
// gives the feeder fund's ISIN two things, both dated the report's
// capital-account date:
//
//   - an exposure record: its region split, and its sector split where
//     the table prints a sector, each from the lines at residual value
//     and in percent. A line at or below zero adds nothing. The records
//     go to gold's exposure_reports, which the declarations merge beside
//     the hand ledger: on a day the latest record wins, and the
//     ledger's on an equal date.
//   - a composition (origin parsed): one line per investment above
//     zero, at its residual value over the lines' total, so the lines
//     sum above 100 where any line is negative and the fund's remainder
//     reads as cash. Each line is keyed by its name and states its
//     region and sector. The composition holds until the fund's next
//     report, reconciled or not.
//
// The table's strategy column (buyout, growth) states no sector and no
// asset class, so it is not read; the holding and its lines keep the
// holding's own class.

const (
	reportsTable     = "private_market_reports"
	reportLinesTable = "private_market_report_lines"
	// reportProvider names the reports as a composition's provider.
	reportProvider = "ubs-report"
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
	no       int64
	name     string
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

// quarterlyReports are what the reconciled quarterly reports dated in w
// say: one exposure record per fund and date, and the composition of
// the same report with its lines. Where two documents report the same
// quarter, the first by document token counts.
type quarterlyReports struct {
	records []canonical.ExposureReportChange
	comps   []canonical.FundCompositionChange
	lines   []canonical.FundLineChange
}

// batches are the reports as a snapshot stream's batches: none where
// they hold nothing. A composition comes with its report's record.
func (q quarterlyReports) batches() []canonical.SnapshotBatch {
	if len(q.records) == 0 {
		return nil
	}
	return []canonical.SnapshotBatch{{ExposureReports: q.records, FundCompositions: q.comps, FundLines: q.lines}}
}

// quarterlyReports reads what the reconciled quarterly reports dated in
// w say, each composition bound to the fund's next report.
func (r *webReader) quarterlyReports(ctx context.Context, w canonical.Window) (quarterlyReports, error) {
	var out quarterlyReports
	if !w.HasChanges {
		return out, nil
	}
	ok, err := silver.HasTables(ctx, r.db, reportsTable, reportLinesTable)
	if err != nil || !ok {
		return out, err
	}
	type report struct {
		isin           string
		asOf           int64
		name, currency sql.NullString
		count          int
		// next is the date of the fund's next report, reconciled or not,
		// outside w too.
		next  sql.NullInt64
		lines []reportLine
	}
	var reports []*report
	err = silver.ScanRows(ctx, r.db, `
WITH nxt AS (
    SELECT instrument_isin, as_of, LEAD(as_of) OVER (PARTITION BY instrument_isin ORDER BY as_of) AS next_as_of
      FROM (SELECT DISTINCT instrument_isin, as_of FROM `+reportsTable+`)),
chosen AS (
    SELECT source_doc_token, instrument_isin, as_of, fund_name, currency_iso, line_count,
           ROW_NUMBER() OVER (PARTITION BY instrument_isin, as_of ORDER BY source_doc_token) AS n
      FROM `+reportsTable+`
     WHERE lines_reconciled = 1 AND as_of BETWEEN ? AND ?)
SELECT c.instrument_isin, c.as_of, c.fund_name, c.currency_iso, c.line_count, x.next_as_of,
       l.line_no, l.name, l.region, l.sector, l.residual_value
  FROM chosen c
  JOIN nxt x USING (instrument_isin, as_of)
  JOIN `+reportLinesTable+` l USING (source_doc_token)
 WHERE c.n = 1 AND l.residual_value IS NOT NULL
 ORDER BY c.instrument_isin, c.as_of, l.line_no`, func(rows *sql.Rows) error {
		var (
			rep  report
			line reportLine
		)
		if err := rows.Scan(&rep.isin, &rep.asOf, &rep.name, &rep.currency, &rep.count, &rep.next, &line.no,
			&line.name, &line.region, &line.sector, &line.residual); err != nil {
			return err
		}
		if n := len(reports); n == 0 || reports[n-1].isin != rep.isin || reports[n-1].asOf != rep.asOf {
			reports = append(reports, &rep)
		}
		last := reports[len(reports)-1]
		last.lines = append(last.lines, line)
		return nil
	}, w.Start, w.End)
	if err != nil {
		return out, fmt.Errorf("ubs-web quarterly reports: %w", err)
	}
	for _, rep := range reports {
		dims, ok := reportSplit(rep.lines)
		if !ok {
			continue
		}
		src, err := json.Marshal(map[string]any{"document": "quarterly report", "lines": rep.count})
		if err != nil {
			return out, err
		}
		out.records = append(out.records, canonical.ExposureReportChange{
			InstrumentExternalID: rep.isin, AsOf: rep.asOf, Dimensions: dims,
			Source: src, Note: rep.name.String,
		})
		comp, lines, ok := reportComposition(rep.isin, rep.asOf, rep.lines)
		if !ok {
			continue
		}
		comp.FundName, comp.Currency = silver.NullStringPtr(rep.name), silver.NullStringPtr(rep.currency)
		comp.Payload = src
		if rep.next.Valid {
			comp.ValidUntil = &rep.next.Int64
		}
		out.comps = append(out.comps, comp)
		out.lines = append(out.lines, lines...)
	}
	return out, nil
}

// reportComposition is a report's lines as the fund's composition: each
// line above zero at its residual value over the sum of every line's, in
// percent, keyed by its name, with its region and sector where a map
// knows them. ok is false when the lines sum to zero or less. The
// weights are rounded to gold's eight places, and the largest line takes
// what the rounding leaves, so they sum to the lines' share exactly: 100
// where no line is below zero.
func reportComposition(isin string, asOf int64, lines []reportLine) (canonical.FundCompositionChange,
	[]canonical.FundLineChange, bool) {
	var total, positive float64
	for _, l := range lines {
		total += l.residual
		if l.residual > 0 {
			positive += l.residual
		}
	}
	comp := canonical.FundCompositionChange{
		FundRef: canonical.SecurityKey(isin, "", "", "", ""), AsOf: asOf, Basis: canonical.CompositionHoldings,
		Provider: reportProvider, SourceKind: "report", Origin: canonical.CompositionParsed,
	}
	if total <= 0 || comp.FundRef == "" {
		return comp, nil, false
	}
	const places = 8
	want := canonical.NewDecimalFromInt(100)
	if positive != total {
		want = canonical.NewDecimalFromFloat(100 * positive / total).Round(places)
	}
	var out []canonical.FundLineChange
	var sum canonical.Decimal
	largest := -1
	for _, l := range lines {
		if l.residual <= 0 {
			continue
		}
		weight := canonical.NewDecimalFromFloat(100 * l.residual / total).Round(places)
		line := canonical.FundLineChange{
			FundRef: comp.FundRef, AsOf: asOf, Basis: comp.Basis, LineNo: l.no,
			SecurityKey: canonical.SecurityKey("", "", "", "", l.name), Name: &l.name,
			Weight: &weight,
		}
		if region := reportRegion(l.region); region != canonical.Unknown {
			line.Region = &region
		}
		if l.sector.Valid {
			if sector := reportSector(l.sector.String); sector != canonical.Unknown {
				line.Sector = &sector
			}
		}
		if largest < 0 || weight.GreaterThan(*out[largest].Weight) {
			largest = len(out)
		}
		out = append(out, line)
		sum = sum.Add(weight)
	}
	adjusted := out[largest].Weight.Add(want.Sub(sum))
	out[largest].Weight = &adjusted
	comp.LineCount, comp.WeightSum = int64(len(out)), &want
	return comp, out, true
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
