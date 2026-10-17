package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

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
		// The date the mark is from: the snapshot date, unless a read
		// dated the snapshot after the marks it found unchanged.
		{Name: "valued_date", Align: output.AlignLeft,
			Extract: func(r gold.PositionRow) string { return formatDate(r.ValuedAt) }},
		{Name: "account", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.PositionRow) string { return accountLabel(r.DisplayName, r.AccountExternalID) }},
		{Name: "account_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.PositionRow) string { return r.AccountExternalID }},
		// position_key is the gold-side instrument identifier (CUSIP /
		// ISIN / ticker); instrument identifiers stay legible under
		// -p / --privacy. See columns.go for the per-class contract.
		{Name: "position_key", Align: output.AlignLeft,
			Extract: func(r gold.PositionRow) string { return r.PositionKey }},
		textCol("symbol", func(r gold.PositionRow) *string { return r.Symbol }),
		textCol("name", func(r gold.PositionRow) *string { return r.Name }),
		// asset_class is the 2-D exposure; vehicle the wrapper.
		{Name: "asset_class", Align: output.AlignLeft,
			Extract: func(r gold.PositionRow) string { return r.AssetClass }},
		{Name: "vehicle", Align: output.AlignLeft,
			Extract: func(r gold.PositionRow) string { return r.Vehicle }},
		{Name: "currency", Align: output.AlignLeft,
			Extract: func(r gold.PositionRow) string { return r.Currency }},
		quantityCol("quantity", func(r gold.PositionRow) *string { return r.Quantity }),
		moneyCol("market_value", "", func(r gold.PositionRow) *string { return r.MarketValue }),
		outCcyCol("value", outCcy, func(r gold.PositionRow) *string { return r.ValueOutCcy }),
		// The cost basis (gold's book_value) the source states, else the
		// one the lot engine rebuilt, and what follows from it, defined
		// in docs/GAINS.md. Blank on a cash row and wherever no basis is
		// known, unless --missing-basis zero counts the missing part as
		// 0; the unrealized gain is blank too on a line no cost basis
		// describes, such as a mortgage. basis_stamp says which notion of
		// basis the figure is.
		moneyCol("cost_basis", "", func(r gold.PositionRow) *string { return r.BookValue }),
		outCcyTwin("cost_basis", outCcy, func(r gold.PositionRow) *string { return r.BookValueOutCcy }),
		moneyCol("unrealized_gain", "", func(r gold.PositionRow) *string { return r.UnrealizedGain }),
		outCcyTwin("unrealized_gain", outCcy, func(r gold.PositionRow) *string { return r.UnrealizedOutCcy }),
		{Name: "unrealized_pct", Align: output.AlignRight,
			Extract: func(r gold.PositionRow) string { return formatPctOrBlank(r.UnrealizedRatio) }},
		textCol("basis_stamp", func(r gold.PositionRow) *string { return r.BasisStamp }),
		quantityCol("quantity_without_basis", func(r gold.PositionRow) *string { return r.QuantityWithoutBasis }),
		textCol("acquisition_date", func(r gold.PositionRow) *string { return r.AcquisitionDate }),
		moneyCol("accrued_interest", "", func(r gold.PositionRow) *string { return r.AccruedInterest }),
		moneyCol("clean_value", "", func(r gold.PositionRow) *string { return r.CleanValue }),
		{Name: "relationship_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.PositionRow) string { return strOrEmpty(r.RelationshipID) }},
		textCol("account_nickname", func(r gold.PositionRow) *string { return r.Nickname }),
		textCol("account_category", func(r gold.PositionRow) *string { return r.AccountCategory }),
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
                                  [--with-cash] [--missing-basis M]

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
      --missing-basis M    ignore (default) | zero: how a missing cost basis counts;
                           zero takes the part the lots know and counts the rest as 0
                           (default: lots.missing_basis in the config, else ignore)

Available columns:
  ` + joinColumnNames(registry) + `

  (The 'value', 'cost_basis_outccy' and 'unrealized_gain_outccy' columns
   are in the -x/--currency output currency and print as 'value_<CCY>',
   'cost_basis_<CCY>' and 'unrealized_gain_<CCY>'.)

The cost basis columns are opt-in, e.g. -C +cost_basis,unrealized_gain,basis_stamp.
unrealized_gain is the clean value (market value less accrued interest)
less the cost basis; 'wealthdb gains' reads it over a window.
quantity_without_basis is the quantity held with no known cost.
valued_date (opt-in) is the date the mark is from: the snapshot date,
unless a download dated the snapshot after marks it found unchanged.

Default column set:
  ` + strings.Join(defaultColumns, ", ")
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
