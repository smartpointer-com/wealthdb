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

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/config"
	"github.com/ptu/wealthdb/internal/errs"
	"github.com/ptu/wealthdb/internal/gold"
	"github.com/ptu/wealthdb/internal/output"
)

func init() {
	register("positions", cmdPositions)
}

func cmdPositions(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb positions", flag.ContinueOnError)
	fs.SetOutput(stderr)

	asOf := fs.String("d", "", "as-of date (YYYY-MM-DD; default today UTC)")
	fs.StringVar(asOf, "as-of", "", "as-of date (YYYY-MM-DD; default today UTC)")
	format := fs.String("f", "table", "output format: table | csv | csv_plain | json")
	fs.StringVar(format, "format", "table", "output format: table | csv | csv_plain | json")
	cols := fs.String("C", "default", "columns: comma-separated names, or 'default' / 'all'")
	fs.StringVar(cols, "columns", "default", "columns: comma-separated names, or 'default' / 'all'")
	currency := fs.String("x", "", "output currency for the value column (default: config.default_currency)")
	fs.StringVar(currency, "currency", "", "output currency (default: config.default_currency)")
	fxMode := fs.String("fx-mode", "historic", "FX rate selection: 'historic' (rate at snapshot time, interpolated) or 'current' (latest available)")
	withCash := fs.Bool("with-cash", false, "also emit one synthetic row per account+currency with non-zero cash")
	privacy := fs.Bool("p", false, "redact account IDs / share quantities / monetary amounts in the output")
	fs.BoolVar(privacy, "privacy", false, "redact account IDs / share quantities / monetary amounts in the output")
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

	mode := canonical.FxMode(*fxMode)
	if !mode.Valid() {
		return errs.Newf(2, "positions: invalid --fx-mode %q (want 'historic' or 'current')", *fxMode)
	}

	fmtChoice, err := output.Parse(*format)
	if err != nil {
		return errs.Newf(2, "positions: %s", err.Error())
	}

	asOfEpoch, err := parseAsOf(*asOf, time.Now())
	if err != nil {
		return errs.Newf(2, "positions: %s", err.Error())
	}

	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}
	gold.SetFxSourceOrder(cfg.FxSourceOrder())

	outCcy := strings.ToUpper(*currency)
	if outCcy == "" {
		outCcy = cfg.DefaultCurrency
	}
	if len(outCcy) != 3 {
		return errs.Newf(2, "positions: invalid -x/--currency %q (want a 3-letter ISO 4217 code)", outCcy)
	}

	colSet, err := resolvePositionColumns(*cols, outCcy)
	if err != nil {
		return errs.Newf(2, "positions: %s", err.Error())
	}

	db, err := openGoldForRead(g, cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := gold.PositionsAsOf(ctx, db, asOfEpoch)
	if err != nil {
		return err
	}
	if *withCash {
		cash, err := gold.CashAsOf(ctx, db, asOfEpoch)
		if err != nil {
			return err
		}
		rows = mergeSorted(rows, cash)
	}

	rendered, err := convertAll(ctx, db, rows, outCcy, mode)
	if err != nil {
		return err
	}

	return writeFormatted(stdout, fmtChoice, rowsToTable(rendered, colSet, *privacy, fmtChoice))
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

// renderedRow pairs a raw position with its market value converted
// to the user's requested output currency. The column extractors
// pull from one or the other depending on which column they
// represent.
type renderedRow struct {
	Row            gold.PositionRow
	ConvertedValue *canonical.Decimal // nil when market_value was NULL or no rate was found
}

// convertAll resolves the per-row converted market value once,
// up front. Failures to find an FX rate are not fatal — the row
// is still emitted with ConvertedValue=nil so the user sees the
// hole rather than the whole report aborting. The single
// exception: if EVERY row failed to convert, that's almost
// certainly a misconfiguration (e.g. asking for an output
// currency we have no rates for) and we surface a fail with a
// hint to look at the config.
func convertAll(ctx context.Context, db *sql.DB, rows []gold.PositionRow, outCcy string, mode canonical.FxMode) ([]renderedRow, error) {
	out := make([]renderedRow, len(rows))
	anyConverted := false
	anyAttempted := false
	for i, r := range rows {
		out[i].Row = r
		if r.MarketValue == nil {
			continue
		}
		anyAttempted = true
		v, err := canonical.NewDecimalFromString(*r.MarketValue)
		if err != nil {
			continue
		}
		conv, err := gold.ConvertValue(ctx, db, r.SnapshotAt, v, r.Currency, outCcy, mode)
		if err != nil {
			// Per-row missing rate — leave nil and move on.
			continue
		}
		out[i].ConvertedValue = &conv
		anyConverted = true
	}
	if anyAttempted && !anyConverted {
		return out, fmt.Errorf("positions: no FX rates available to convert to %q (mode=%s). Check that your silvers carry the right currency pairs", outCcy, mode)
	}
	return out, nil
}

// ---- column registry -----------------------------------------------------

// buildColumnRegistry returns the full set of available columns
// for the given output currency. Most entries are constant; the
// dynamic `value` column embeds the currency in its header
// (`value_USD`, `value_CHF`, ...) and looks up the converted
// amount on each row.
func buildColumnRegistry(outCcy string) []columnSpec[renderedRow] {
	return []columnSpec[renderedRow]{
		{Name: "silver_source", Align: output.AlignLeft,
			Extract: func(rr renderedRow) string { return rr.Row.SilverSourceID }},
		{Name: "snapshot_date", Align: output.AlignLeft,
			Extract: func(rr renderedRow) string { return formatDate(rr.Row.SnapshotAt) }},
		{Name: "account", Align: output.AlignLeft, Privacy: PrivacyAccountID, Extract: func(rr renderedRow) string {
			// Display name preferred (Schwab accountNumber);
			// fall back to the raw external_id when no display
			// name is known (UBS IBAN, Swissquote customer ID).
			if rr.Row.DisplayName != nil && *rr.Row.DisplayName != "" {
				return *rr.Row.DisplayName
			}
			return rr.Row.AccountExternalID
		}},
		{Name: "account_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(rr renderedRow) string { return rr.Row.AccountExternalID }},
		// position_key is the gold-side instrument identifier
		// (CUSIP / ISIN / ticker); instrument identifiers stay
		// legible under -p / --privacy. See columns.go for the
		// per-class privacy contract.
		{Name: "position_key", Align: output.AlignLeft,
			Extract: func(rr renderedRow) string { return rr.Row.PositionKey }},
		{Name: "symbol", Align: output.AlignLeft,
			Extract: func(rr renderedRow) string { return strOrEmpty(rr.Row.Symbol) }},
		{Name: "name", Align: output.AlignLeft,
			Extract: func(rr renderedRow) string { return strOrEmpty(rr.Row.Name) }},
		{Name: "asset_class", Align: output.AlignLeft,
			Extract: func(rr renderedRow) string { return rr.Row.AssetClass }},
		{Name: "currency", Align: output.AlignLeft,
			Extract: func(rr renderedRow) string { return rr.Row.Currency }},
		{Name: "quantity", Align: output.AlignRight, Privacy: PrivacyQuantity,
			Extract: func(rr renderedRow) string { return strOrEmpty(rr.Row.Quantity) }},
		{Name: "market_value", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(rr renderedRow) string { return formatCents(rr.Row.MarketValue) }},
		{Name: "value", Header: "value_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(rr renderedRow) string {
				if rr.ConvertedValue == nil {
					return ""
				}
				return rr.ConvertedValue.StringFixed(2)
			}},
		{Name: "relationship_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(rr renderedRow) string { return strOrEmpty(rr.Row.RelationshipID) }},
		{Name: "account_nickname", Align: output.AlignLeft,
			Extract: func(rr renderedRow) string { return strOrEmpty(rr.Row.Nickname) }},
		{Name: "account_category", Align: output.AlignLeft,
			Extract: func(rr renderedRow) string { return strOrEmpty(rr.Row.AccountCategory) }},
	}
}

// formatCents renders a decimal-string-as-pointer with exactly
// two fractional digits — the canonical convention for money
// columns. Returns "" for nil so empty cells stay empty (not
// "0.00"). On parse failure falls back to the raw string rather
// than erroring (defensive — the value came from DuckDB and
// should always be valid, but garbage-in shouldn't crash the
// table render).
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

// defaultColumns is what `wealthdb positions` shows when --columns
// isn't passed (or when --columns=default). Tracks user feedback
// from M8 — `account` shows the human-readable identifier and
// `symbol` is included separately from `position_key`. The
// dynamic `value` column (named value_<CCY> in the header) is
// appended so users see both the natural-currency market value
// and the converted value side by side.
var defaultColumns = []string{
	"silver_source", "snapshot_date", "account", "symbol",
	"position_key", "asset_class", "currency", "quantity",
	"market_value", "value",
}

func resolvePositionColumns(flagValue, outCcy string) ([]columnSpec[renderedRow], error) {
	return resolveColumns(flagValue, defaultColumns, buildColumnRegistry(outCcy))
}

func positionsUsage() string {
	// We don't know the user's chosen output currency at usage-
	// print time; show a placeholder for the dynamic column.
	registry := buildColumnRegistry("CCY")
	return `usage: wealthdb positions [-d YYYY-MM-DD] [-f FORMAT] [-C COLS] [-x CCY] [--fx-mode MODE] [-p]

Print consolidated positions as of a date. For each silver source,
the latest snapshot ≤ the as-of date is used. Default: today UTC,
table format, default column set, output currency from
config.default_currency, historic FX mode.

Flags:
  -d, --as-of YYYY-MM-DD   as-of date (default: today UTC)
  -f, --format FORMAT      output format (default: table; csv / csv_plain / json land in M10)
  -C, --columns COLS       comma-separated column names, 'default', 'all', or
                           a +ADD,...-REMOVE,... delta against the default set
                           (e.g. -C+account_id-market_value)
  -x, --currency CCY       output currency for the value column (default: config.default_currency)
      --fx-mode MODE       'historic' (default; rate at snapshot time, interpolated) or 'current' (latest rate)
  -p, --privacy            redact account IDs, share quantities, and monetary amounts
                           (table: visible placeholders; csv: empty cells; json: keys omitted)
      --with-cash          also emit one row per account+currency with non-zero cash

Available columns:
  ` + joinColumnNames(registry) + `

  (The 'value' column renders as 'value_<CCY>' in the header,
   reflecting your -x/--currency choice.)

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
// position_key) ordering shared with gold.PositionsAsOf /
// gold.CashAsOf's SQL ORDER BY.
func positionLess(x, y gold.PositionRow) bool {
	if x.SilverSourceID != y.SilverSourceID {
		return x.SilverSourceID < y.SilverSourceID
	}
	if x.AccountExternalID != y.AccountExternalID {
		return x.AccountExternalID < y.AccountExternalID
	}
	return x.PositionKey < y.PositionKey
}

