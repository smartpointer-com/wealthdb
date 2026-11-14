package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// TestColumnKindsMatchHeaders holds every column of every report view to
// what its kind promises the reader: a percent column, and only one,
// prints _pct; money in the output currency, and only it, prints
// _<CCY>; a twin is in the output currency, and the column it converts,
// where its view has it, is money in the holding's own. A model reads
// the suffixes, the server the kinds.
func TestColumnKindsMatchHeaders(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{DefaultCurrency: "USD"}
	fams := []*family{marksFamily}
	for _, ts := range toolSpecs {
		if ts.family != nil {
			fams = append(fams, ts.family)
		}
	}
	args := []*toolArgs{{values: map[string]any{}}, {values: map[string]any{"verbose": true, "latest_only": true}}}
	check := func(label, ccy string, rep *report) {
		byName := map[string]reportColumn{}
		for _, c := range rep.columns {
			byName[c.name] = c
		}
		for _, c := range rep.columns {
			if got := strings.HasSuffix(c.header, "_pct"); got != (c.kind == PercentColumn) {
				t.Errorf("%s: %s is kind %d, header %s", label, c.name, c.kind, c.header)
			}
			if got := strings.HasSuffix(strings.ToLower(c.header), "_"+strings.ToLower(ccy)); got != c.converted() {
				t.Errorf("%s: %s converted %v, header %s", label, c.name, c.converted(), c.header)
			}
			if c.twin == "" {
				continue
			}
			if own, ok := byName[c.twin]; !c.converted() || ok && (own.kind != MoneyColumn || own.converted()) {
				t.Errorf("%s: %s twins %q, which is no money in its own currency", label, c.name, c.twin)
			}
		}
	}
	for _, fam := range fams {
		for _, v := range fam.viewsOrOne() {
			for _, ccy := range []string{"USD", "CHF"} {
				for _, period := range []string{"total", "monthly"} {
					for _, method := range []string{"both", "twr", "mwr"} {
						for _, opt := range []string{"", "converted", "cash", "series", "benchmark"} {
							req := request{view: v, currency: ccy, period: period, level: "primary", method: method,
								annualize: "auto", inception: "full", netting: true, investing: "whole"}
							switch opt {
							case "converted":
								req.converted = true
							case "cash":
								req.withCash = true
							case "series":
								req.series = []seriesPoint{{label: "2026-06", at: 1}}
							case "benchmark":
								req.benchmark = "bench"
							}
							for _, a := range args {
								check(fam.name+" "+v+" "+ccy, ccy, fam.build(cfg, req, a))
							}
						}
					}
				}
			}
		}
	}
}

// TestTinyNegativePrintsUnsignedZero: a figure that rounds to zero from
// below prints as zero, in every format, never as -0.00. A text cell
// keeps what it says.
func TestTinyNegativePrintsUnsignedZero(t *testing.T) {
	t.Parallel()
	type row struct {
		f float64
		s string
	}
	get := func(r row) *float64 { return &r.f }
	cols := []columnSpec[row]{
		floatMoneyCol("value", "USD", get),
		priceCol(get),
		decimalCol("duration", get),
		pctCol("share", get),
		ratioCol("twr", get),
		ratioOrBlankCol("gain_pct", "", get),
		moneyCol("amount", func(r row) *string { return &r.s }),
		kindCol("own", MoneyColumn, func(r row) string { return fmt.Sprintf("%.2f", r.f) }),
		strCol("label", func(r row) string { return "-0.00" }),
	}
	want := []string{"0.00", "0.0000", "0.00", "0.00", "0.00", "0.00", "0.00", "0.00", "-0.00"}
	rows := []row{{f: -0.000001, s: "-0.001"}}
	for _, format := range []output.Format{output.FormatTable, output.FormatCSV, output.FormatCSVPlain, output.FormatJSON} {
		got := rowsToTable(rows, cols, false, format, nil).Rows[0]
		if !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", format, got, want)
		}
	}
}
