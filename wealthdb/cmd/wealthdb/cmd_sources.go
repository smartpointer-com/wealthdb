package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/config"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/errs"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/output"
)

// cmdSources is the silver-source-grain rollup: one row per silver
// source, aggregating all of that source's accounts. It sits between
// cmd_portfolios.go / cmd_accounts.go (finer) and cmd_global.go (the
// whole portfolio). The invariant is: sum(sources.total_value_<CCY>)
// == sum(accounts.total_value_<CCY>) == sum(portfolios.total_value_<CCY>).
func cmdSources(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb holdings sources", flag.ContinueOnError)
	fs.SetOutput(stderr)

	asOf := fs.String("d", "", "as-of date (YYYY-MM-DD; default today UTC)")
	fs.StringVar(asOf, "as-of", "", "as-of date (YYYY-MM-DD; default today UTC)")
	format := fs.String("f", "table", "output format: table | csv | csv_plain | json")
	fs.StringVar(format, "format", "table", "output format: table | csv | csv_plain | json")
	cols := fs.String("C", "default", "columns: comma-separated names, or 'default' / 'all'")
	fs.StringVar(cols, "columns", "default", "columns: comma-separated names, or 'default' / 'all'")
	currency := fs.String("x", "", "output currency for the _<CCY> aggregate columns (default: config.default_currency)")
	fs.StringVar(currency, "currency", "", "output currency (default: config.default_currency)")
	fxMode := fs.String("fx-mode", "historic", "FX rate selection: 'historic' (nearest rate at-or-before the snapshot) or 'current' (latest available)")
	privacy := fs.Bool("p", false, "redact the monetary amounts in the output")
	fs.BoolVar(privacy, "privacy", false, "redact the monetary amounts in the output")
	fs.Usage = func() {
		fmt.Fprintln(stderr, sourcesUsage())
	}
	if err := fs.Parse(splitFusedColumnsFlag(subargs)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "sources: bad flags")
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errs.Newf(2, "sources: unexpected positional argument %q", fs.Arg(0))
	}

	mode := canonical.FxMode(*fxMode)
	if !mode.Valid() {
		return errs.Newf(2, "sources: invalid --fx-mode %q", *fxMode)
	}

	fmtChoice, err := output.Parse(*format)
	if err != nil {
		return errs.Newf(2, "sources: %s", err.Error())
	}

	asOfEpoch, err := parseAsOf(*asOf, time.Now())
	if err != nil {
		return errs.Newf(2, "sources: %s", err.Error())
	}

	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return err
	}

	outCcy := strings.ToUpper(*currency)
	if outCcy == "" {
		outCcy = cfg.DefaultCurrency
	}
	if len(outCcy) != 3 {
		return errs.Newf(2, "sources: invalid -x/--currency %q (want a 3-letter ISO 4217 code)", outCcy)
	}

	colSet, err := resolveSourceColumns(*cols, outCcy)
	if err != nil {
		return errs.Newf(2, "sources: %s", err.Error())
	}

	db, err := openGoldForRead(g, cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := gold.SourcesAsOf(ctx, db, asOfEpoch, outCcy, mode)
	if err != nil {
		return err
	}

	return writeFormatted(stdout, fmtChoice, rowsToTable(rows, colSet, *privacy, fmtChoice))
}

// ---- column registry -----------------------------------------------------

func buildSourceColumnRegistry(outCcy string) []columnSpec[gold.SourceRow] {
	suffix := "_" + outCcy
	return []columnSpec[gold.SourceRow]{
		{Name: "silver_source", Align: output.AlignLeft,
			Extract: func(r gold.SourceRow) string { return r.SilverSourceID }},
		{Name: "snapshot_date", Align: output.AlignLeft, Extract: func(r gold.SourceRow) string {
			if r.SnapshotAt == 0 {
				return ""
			}
			return formatDate(r.SnapshotAt)
		}},
		{Name: "base_currency", Align: output.AlignLeft,
			Extract: func(r gold.SourceRow) string { return strOrEmpty(r.BaseCurrency) }},
		// Source-level taxonomy rollups. Strict agree-or-NULL
		// semantics in the gold layer (every non-overlay account
		// agrees, none unclassified); a blank cell means
		// "mixed / unknown", not a render-time default.
		{Name: "tax_wrapper", Align: output.AlignLeft,
			Extract: func(r gold.SourceRow) string { return strOrEmpty(r.TaxWrapper) }},
		{Name: "management_style", Align: output.AlignLeft,
			Extract: func(r gold.SourceRow) string { return strOrEmpty(r.ManagementStyle) }},

		{Name: "positions_value", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.SourceRow) string { return formatCents(r.PositionsValueBase) }},
		{Name: "cash_balance", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.SourceRow) string { return formatCents(r.CashBalanceBase) }},
		{Name: "total_value", Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.SourceRow) string { return formatCents(r.TotalValueBase) }},

		{Name: "positions_value_outccy", Header: "positions_value" + suffix, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.SourceRow) string { return formatCents(r.PositionsValueOutCcy) }},
		{Name: "cash_balance_outccy", Header: "cash_balance" + suffix, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.SourceRow) string { return formatCents(r.CashBalanceOutCcy) }},
		{Name: "total_value_outccy", Header: "total_value" + suffix, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.SourceRow) string { return formatCents(r.TotalValueOutCcy) }},
	}
}

var defaultSourceColumns = []string{
	"silver_source", "snapshot_date",
	"tax_wrapper", "management_style", "base_currency",
	"positions_value", "cash_balance", "total_value",
	"total_value_outccy",
}

func resolveSourceColumns(flagValue, outCcy string) ([]columnSpec[gold.SourceRow], error) {
	return resolveColumns(flagValue, defaultSourceColumns, buildSourceColumnRegistry(outCcy))
}

func sourcesUsage() string {
	registry := buildSourceColumnRegistry("CCY")
	return `usage: wealthdb holdings sources [-d YYYY-MM-DD] [-f FORMAT] [-C COLS] [-x CCY] [--fx-mode MODE] [-p]

Print one row per silver source, rolling up every one of its
accounts (positions + cash). The source-grain level between
'wealthdb holdings accounts' / 'portfolios' and 'holdings global'.
Sum of total_value_<CCY> across all rows equals the same sum from
'wealthdb holdings accounts', 'portfolios', and 'global'.

Flags:
  -d, --as-of YYYY-MM-DD   as-of date (default: today UTC)
  -f, --format FORMAT      output format (table | csv | csv_plain | json)
  -C, --columns COLS       comma-separated column names, 'default', 'all', or
                           a +ADD,...-REMOVE,... delta against the default set
                           (e.g. -C+positions_value_outccy-base_currency)
  -x, --currency CCY       output currency for the _<CCY> aggregate columns (default: config.default_currency)
      --fx-mode MODE       'historic' (default) or 'current'
  -p, --privacy            redact the monetary amounts
                           (table: visible placeholders; csv: empty cells; json: keys omitted)

Available columns:
  ` + joinColumnNames(registry) + `

  ('positions_value_outccy', 'cash_balance_outccy',
   'total_value_outccy' render as positions_value_<CCY> etc.)

Default column set:
  ` + strings.Join(defaultSourceColumns, ", ")
}
