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
// the suffixes, the server the kinds. The kind sets the privacy class,
// so a quantity, and a figure the names below call money, is of its
// kind, and no other kind hides as money or a quantity. Every view's
// default column set resolves against its registry.
func TestColumnKindsMatchHeaders(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{DefaultCurrency: "USD"}
	fams := []*family{marksFamily}
	for _, ts := range toolSpecs {
		if ts.family != nil {
			fams = append(fams, ts.family)
		}
	}
	check := func(label, ccy string, rep *report) {
		if _, err := rep.pick("default"); err != nil {
			t.Errorf("%s defaults: %v", label, err)
		}
		byName := map[string]reportColumn{}
		for _, c := range rep.columns {
			byName[c.name] = c
		}
		for _, c := range rep.columns {
			if (c.name == "quantity" || strings.HasPrefix(c.name, "quantity_")) && c.kind != QuantityColumn {
				t.Errorf("%s: %s is kind %d, no quantity", label, c.name, c.kind)
			}
			if moneyNames[c.name] && c.kind != MoneyColumn {
				t.Errorf("%s: %s is kind %d, no money", label, c.name, c.kind)
			}
			if c.privacy.hidesFigure() && c.kind != MoneyColumn && c.kind != QuantityColumn {
				t.Errorf("%s: %s is kind %d, yet hides as class %d", label, c.name, c.kind, c.privacy)
			}
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
						for _, opt := range []string{"", "converted", "cash", "series", "benchmark", "verbose"} {
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
							case "verbose":
								req.verbose, req.latestOnly = true, true
							}
							check(fam.name+" "+v+" "+ccy, ccy, fam.build(cfg, req))
						}
					}
				}
			}
		}
	}
}

// moneyNames are column names that are money wherever a view has them.
var moneyNames = map[string]bool{"price": true, "amount": true, "net_amount": true, "gross_amount": true,
	"cost_basis": true, "proceeds": true}

// TestTinyNegativePrintsUnsignedZero: a figure that rounds to zero from
// below prints as zero, in every format, never as -0.00, a count and a
// quantity too. A text cell keeps what it says, and so does a figure's
// cell that is no number. Under privacy a legible figure's PrivacyExtract
// unsigns the same way.
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
		kindCol("count", CountColumn, func(row) string { return "-0" }),
		kindCol("quantity", QuantityColumn, func(row) string { return "-0.000000" }),
		kindCol("dash", NumberColumn, func(row) string { return "-" }),
		kindCol("na", NumberColumn, func(row) string { return "-n/a" }),
		strCol("label", func(r row) string { return "-0.00" }),
	}
	want := []string{"0.00", "0.0000", "0.00", "0.00", "0.00", "0.00", "0.00", "0.00", "0", "0.000000", "-", "-n/a", "-0.00"}
	rows := []row{{f: -0.000001, s: "-0.001"}}
	for _, format := range []output.Format{output.FormatTable, output.FormatCSV, output.FormatCSVPlain, output.FormatJSON} {
		got := rowsToTable(rows, cols, false, format, nil).Rows[0]
		if !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", format, got, want)
		}
	}
	shown := decimalCol("duration", get)
	shown.PrivacyExtract = func(row) string { return "-0.00" }
	if got := rowsToTable(rows, []columnSpec[row]{shown}, true, output.FormatTable, nil).Rows[0][0]; got != "0.00" {
		t.Errorf("privacy: %q, want 0.00", got)
	}
}

// TestKindAlignmentAndClass: text and dates pad left and every figure
// right; money and a quantity hide by their kind, every other kind is
// legible, and a stated class wins over the kind's.
func TestKindAlignmentAndClass(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		kind  ColumnKind
		align output.Alignment
		class PrivacyClass
	}{
		{TextColumn, output.AlignLeft, PrivacyNone},
		{CountColumn, output.AlignRight, PrivacyNone},
		{NumberColumn, output.AlignRight, PrivacyNone},
		{PercentColumn, output.AlignRight, PrivacyNone},
		{MoneyColumn, output.AlignRight, PrivacyMoney},
		{QuantityColumn, output.AlignRight, PrivacyQuantity},
		{DateColumn, output.AlignLeft, PrivacyNone},
	} {
		col := kindCol("x", c.kind, func(string) string { return "" })
		if got := c.kind.align(); got != c.align {
			t.Errorf("kind %d: align %v, want %v", c.kind, got, c.align)
		}
		if got := col.class(); got != c.class {
			t.Errorf("kind %d: class %v, want %v", c.kind, got, c.class)
		}
	}
	stated := kindCol("x", MoneyColumn, func(string) string { return "" })
	stated.Privacy = PrivacyAccountID
	if got := stated.class(); got != PrivacyAccountID {
		t.Errorf("a stated class: %v, want PrivacyAccountID", got)
	}
}
