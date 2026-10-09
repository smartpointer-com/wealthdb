package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

func cmdPositions(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb holdings positions", flag.ContinueOnError)
	fs.SetOutput(stderr)

	hf := registerHoldingsFlags(fs, holdingsFlagSpec{
		cmd:           "positions",
		currencyUsage: "output currency for the value column (default: config.default_currency)",
		privacyUsage:  "redact account IDs / share quantities / monetary amounts in the output",
		withColumns:   true,
		withCash:      true,
	})
	fs.Usage = func() {
		fmt.Fprintln(stderr, positionsUsage())
	}
	if err := fs.Parse(splitFusedColumnsFlag(subargs)); err != nil {
		// flag.Parse already printed usage to stderr; suppress
		// further error text for the standard -h case.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "positions: bad flags")
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errs.Newf(2, "positions: unexpected positional argument %q", fs.Arg(0))
	}

	hv, err := hf.resolve(g)
	if err != nil {
		return err
	}

	rep := holdingsReport(request{view: "positions", currency: hv.outCcy, asOf: hv.asOfEpoch, withCash: *hf.withCash})
	open := func() (*sql.DB, error) { return openGoldForRead(g, hv.cfg) }
	return writeReport(ctx, rep, *hf.cols, "positions", open, *hf.privacy, hv.fmtChoice, stdout)
}

// writeFormatted dispatches to the right output.Write* function
// for the chosen format.
func writeFormatted(w io.Writer, f output.Format, t output.Table) error {
	switch f {
	case output.FormatTable:
		return output.WriteTable(w, t)
	case output.FormatCSV:
		return output.WriteCSV(w, t)
	case output.FormatCSVPlain:
		return output.WriteCSVPlain(w, t)
	case output.FormatJSON:
		return output.WriteJSON(w, t)
	}
	return fmt.Errorf("unsupported output format %q", f)
}

// ---- column registry -----------------------------------------------------

// buildColumnRegistry returns the full set of available columns for
// the given output currency. The dynamic `value` column embeds the
// currency in its header (`value_USD`, ...) and reads the converted
// amount the report_positions / report_cash macro computed in SQL.
func buildColumnRegistry(outCcy string) []columnSpec[gold.PositionRow] {
	return []columnSpec[gold.PositionRow]{
		{Name: "silver_source", Align: output.AlignLeft,
			Extract: func(r gold.PositionRow) string { return r.SilverSourceID }},
		{Name: "snapshot_date", Align: output.AlignLeft,
			Extract: func(r gold.PositionRow) string { return formatDate(r.SnapshotAt) }},
		{Name: "account", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.PositionRow) string { return accountLabel(r.DisplayName, r.AccountExternalID) }},
		{Name: "account_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.PositionRow) string { return r.AccountExternalID }},
		// position_key is the gold-side instrument identifier (CUSIP /
		// ISIN / ticker); instrument identifiers stay legible under
		// -p / --privacy. See columns.go for the per-class contract.
		{Name: "position_key", Align: output.AlignLeft,
			Extract: func(r gold.PositionRow) string { return r.PositionKey }},
		{Name: "symbol", Align: output.AlignLeft,
			Extract: func(r gold.PositionRow) string { return strOrEmpty(r.Symbol) }},
		{Name: "name", Align: output.AlignLeft,
			Extract: func(r gold.PositionRow) string { return strOrEmpty(r.Name) }},
		// asset_class is the 2-D exposure; vehicle the wrapper.
		{Name: "asset_class", Align: output.AlignLeft,
			Extract: func(r gold.PositionRow) string { return r.AssetClass }},
		{Name: "vehicle", Align: output.AlignLeft,
			Extract: func(r gold.PositionRow) string { return r.Vehicle }},
		{Name: "currency", Align: output.AlignLeft,
			Extract: func(r gold.PositionRow) string { return r.Currency }},
		{Name: "quantity", Align: output.AlignRight, Privacy: PrivacyQuantity,
			Extract: func(r gold.PositionRow) string { return strOrEmpty(r.Quantity) }},
		{Name: "market_value", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.PositionRow) string { return formatCents(r.MarketValue) }},
		{Name: "value", Header: "value_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.PositionRow) string { return formatCents(r.ValueOutCcy) }},
		// The cost basis the source states (gold's book_value) and what
		// follows from it, defined in docs/GAINS.md. Blank on a cash
		// row and wherever the source states no basis; basis_stamp says
		// which notion of basis the figure is.
		{Name: "cost_basis", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.PositionRow) string { return formatCents(r.BookValue) }},
		{Name: "cost_basis_ccy", Header: "cost_basis_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.PositionRow) string { return formatCents(r.BookValueOutCcy) }},
		{Name: "unrealized_gain", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.PositionRow) string { return formatCents(r.UnrealizedGain) }},
		{Name: "unrealized", Header: "unrealized_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.PositionRow) string { return formatCents(r.UnrealizedOutCcy) }},
		{Name: "unrealized_pct", Align: output.AlignRight,
			Extract: func(r gold.PositionRow) string { return formatPctOrBlank(r.UnrealizedRatio) }},
		{Name: "basis_stamp", Align: output.AlignLeft,
			Extract: func(r gold.PositionRow) string { return strOrEmpty(r.BasisStamp) }},
		{Name: "acquisition_date", Align: output.AlignLeft,
			Extract: func(r gold.PositionRow) string { return strOrEmpty(r.AcquisitionDate) }},
		{Name: "accrued_interest", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.PositionRow) string { return formatCents(r.AccruedInterest) }},
		{Name: "clean_value", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.PositionRow) string { return formatCents(r.CleanValue) }},
		{Name: "relationship_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.PositionRow) string { return strOrEmpty(r.RelationshipID) }},
		{Name: "account_nickname", Align: output.AlignLeft,
			Extract: func(r gold.PositionRow) string { return strOrEmpty(r.Nickname) }},
		{Name: "account_category", Align: output.AlignLeft,
			Extract: func(r gold.PositionRow) string { return strOrEmpty(r.AccountCategory) }},
	}
}

// formatCents renders a decimal-string-as-pointer with exactly two
// fractional digits — the canonical convention for money columns.
// Returns "" for nil so empty cells stay empty (not "0.00"). On parse
// failure falls back to the raw string rather than erroring.
func formatCents(p *string) string {
	if p == nil {
		return ""
	}
	d, err := canonical.NewDecimalFromString(*p)
	if err != nil {
		return *p
	}
	return d.StringFixed(2)
}

// defaultColumns is what `wealthdb holdings positions` shows when --columns
// isn't passed. The dynamic `value` column (value_<CCY> in the
// header) sits beside the natural-currency market_value.
var defaultColumns = []string{
	"silver_source", "snapshot_date", "account", "symbol",
	"position_key", "asset_class", "vehicle", "currency", "quantity",
	"market_value", "value",
}

func positionsUsage() string {
	// We don't know the chosen output currency at usage-print
	// time; show a placeholder for the dynamic column.
	registry := buildColumnRegistry("CCY")
	return `usage: wealthdb holdings positions [-d YYYY-MM-DD] [-f FORMAT] [-C COLS] [-x CCY] [-p]

Print every individual holding as of a date: securities, funds and
crypto, and the property, loans and private holdings recorded by
hand (a house is a position of asset_class real_estate; a loan is a
negative position). For each source, the latest snapshot ≤ the
as-of date is used. Default: today UTC, table format, default
column set, output currency from config.default_currency, historic
FX (nearest rate at-or-before the snapshot).

Flags:
  -d, --as-of YYYY-MM-DD   as-of date (default: today UTC)
  -f, --format FORMAT      output format (table | csv | csv_plain | json)
  -C, --columns COLS       comma-separated column names, 'default', 'all', or
                           a +ADD,...-REMOVE,... delta against the default set
                           (e.g. -C+account_id-market_value)
  -x, --currency CCY       output currency for the value column (default: config.default_currency)
  -p, --privacy            redact account IDs, share quantities, and monetary amounts
                           (table: visible placeholders; csv: empty cells; json: keys omitted)
      --with-cash          also emit one row per account+currency with non-zero cash

Available columns:
  ` + joinColumnNames(registry) + `

  (The 'value', 'cost_basis_ccy' and 'unrealized' columns render as
   'value_<CCY>', 'cost_basis_<CCY>' and 'unrealized_<CCY>', reflecting
   your -x/--currency choice.)

The cost basis columns are opt-in, e.g. -C +cost_basis,unrealized,basis_stamp.
unrealized_gain is the clean value (market value less accrued interest)
less the cost basis; 'wealthdb gains' reads it over a window.

Default column set:
  ` + strings.Join(defaultColumns, ", ")
}

func formatDate(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("2006-01-02")
}

func strOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// mergeSorted interleaves two already-(silver_source, account,
// position_key)-sorted row slices in the same order. Used by
// --with-cash to slot synthetic cash rows alongside their account's
// real positions without re-sorting the entire combined list.
func mergeSorted(a, b []gold.PositionRow) []gold.PositionRow {
	out := make([]gold.PositionRow, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if positionLess(a[i], b[j]) {
			out = append(out, a[i])
			i++
		} else {
			out = append(out, b[j])
			j++
		}
	}
	out = append(out, a[i:]...)
	out = append(out, b[j:]...)
	return out
}

// positionLess is the (silver_source_id, account_external_id,
// position_key) ordering shared with the report_positions /
// report_cash macros' ORDER BY.
func positionLess(x, y gold.PositionRow) bool {
	if x.SilverSourceID != y.SilverSourceID {
		return x.SilverSourceID < y.SilverSourceID
	}
	if x.AccountExternalID != y.AccountExternalID {
		return x.AccountExternalID < y.AccountExternalID
	}
	return x.PositionKey < y.PositionKey
}
