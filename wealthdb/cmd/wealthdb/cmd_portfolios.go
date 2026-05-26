package main

import (
	"context"
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
	"github.com/ptu/wealthdb/internal/pathmode"
)

func init() {
	register("portfolios", cmdPortfolios)
}

// cmdPortfolios is the portfolio-grain rollup. One row per
// registered portfolio plus one sentinel row per silver_source
// that aggregates orphan accounts (no portfolio). The invariant
// is: sum(portfolios.total_value_<CCY>) == sum(accounts.total_value_<CCY>)
// == positions --with-cash total.
func cmdPortfolios(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb portfolios", flag.ContinueOnError)
	fs.SetOutput(stderr)

	asOf := fs.String("d", "", "as-of date (YYYY-MM-DD; default today UTC)")
	fs.StringVar(asOf, "as-of", "", "as-of date (YYYY-MM-DD; default today UTC)")
	format := fs.String("f", "table", "output format: table | csv | csv_plain | json")
	fs.StringVar(format, "format", "table", "output format: table | csv | csv_plain | json")
	cols := fs.String("C", "default", "columns: comma-separated names, or 'default' / 'all'")
	fs.StringVar(cols, "columns", "default", "columns: comma-separated names, or 'default' / 'all'")
	currency := fs.String("x", "", "output currency for the _<CCY> aggregate columns (default: config.default_currency)")
	fs.StringVar(currency, "currency", "", "output currency (default: config.default_currency)")
	fxMode := fs.String("fx-mode", "historic", "FX rate selection: 'historic' (rate at snapshot time, interpolated) or 'current' (latest available)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, portfoliosUsage())
	}
	if err := fs.Parse(splitFusedColumnsFlag(subargs)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "portfolios: bad flags")
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errs.Newf(2, "portfolios: unexpected positional argument %q", fs.Arg(0))
	}

	mode := canonical.FxMode(*fxMode)
	if !mode.Valid() {
		return errs.Newf(2, "portfolios: invalid --fx-mode %q", *fxMode)
	}

	fmtChoice, err := output.Parse(*format)
	if err != nil {
		return errs.Newf(2, "portfolios: %s", err.Error())
	}

	asOfEpoch, err := parseAsOf(*asOf, time.Now())
	if err != nil {
		return errs.Newf(2, "portfolios: %s", err.Error())
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
		return errs.Newf(2, "portfolios: invalid -x/--currency %q (want a 3-letter ISO 4217 code)", outCcy)
	}

	colSet, err := resolvePortfolioColumns(*cols, outCcy)
	if err != nil {
		return errs.Newf(2, "portfolios: %s", err.Error())
	}

	dec, err := pathmode.Detect(cfg.GoldDB, g.ForceReadOnly, false)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	if !dec.DBExists {
		return errs.Newf(errs.ExitMissingDB,
			"gold database %q does not exist. Run 'wealthdb init' first.", cfg.GoldDB)
	}

	openMode := gold.ModeReadWrite
	if dec.Mode == pathmode.ModeReadOnly {
		openMode = gold.ModeReadOnly
	}
	db, err := gold.Open(cfg.GoldDB, openMode)
	if err != nil {
		return errs.Wrap(errs.ExitOpenFailed, err)
	}
	defer db.Close()

	rows, err := gold.PortfoliosAsOf(ctx, db, asOfEpoch, outCcy, mode)
	if err != nil {
		return err
	}

	return writeFormatted(stdout, fmtChoice, rowsToTable(rows, colSet))
}

// ---- column registry -----------------------------------------------------

func buildPortfolioColumnRegistry(outCcy string) []columnSpec[gold.PortfolioRow] {
	suffix := "_" + outCcy
	return []columnSpec[gold.PortfolioRow]{
		{Name: "silver_source", Align: output.AlignLeft,
			Extract: func(r gold.PortfolioRow) string { return r.SilverSourceID }},
		{Name: "snapshot_date", Align: output.AlignLeft, Extract: func(r gold.PortfolioRow) string {
			if r.SnapshotAt == 0 {
				return ""
			}
			return formatDate(r.SnapshotAt)
		}},
		{Name: "portfolio", Align: output.AlignLeft, Extract: func(r gold.PortfolioRow) string {
			// Sentinel rows render as "(no portfolio)" so the user
			// can spot them at a glance; real portfolios show their
			// display_name if present, else their external_id.
			if r.PortfolioExternalID == "" {
				return "(no portfolio)"
			}
			if r.DisplayName != nil && *r.DisplayName != "" {
				return *r.DisplayName
			}
			return r.PortfolioExternalID
		}},
		{Name: "portfolio_id", Align: output.AlignLeft,
			Extract: func(r gold.PortfolioRow) string { return r.PortfolioExternalID }},
		{Name: "base_currency", Align: output.AlignLeft,
			Extract: func(r gold.PortfolioRow) string { return strOrEmpty(r.BaseCurrency) }},
		{Name: "relationship_id", Align: output.AlignLeft,
			Extract: func(r gold.PortfolioRow) string { return strOrEmpty(r.RelationshipID) }},
		{Name: "portfolio_nickname", Align: output.AlignLeft,
			Extract: func(r gold.PortfolioRow) string { return strOrEmpty(r.Nickname) }},
		// Portfolio-level taxonomy rollups. Strict semantics in
		// the gold layer: tax_wrapper non-nil only when every
		// component account agrees AND none is unclassified;
		// management_style non-nil only when every non-overlay
		// component agrees (and none is unclassified). Blank
		// cells genuinely mean "ambiguous / mixed / unknown" —
		// not the same as the accounts table's render-time
		// default fallback, where blank would silently show
		// taxable_personal / self_directed.
		{Name: "tax_wrapper", Align: output.AlignLeft,
			Extract: func(r gold.PortfolioRow) string { return strOrEmpty(r.TaxWrapper) }},
		{Name: "management_style", Align: output.AlignLeft,
			Extract: func(r gold.PortfolioRow) string { return strOrEmpty(r.ManagementStyle) }},

		{Name: "positions_value", Align: output.AlignRight,
			Extract: func(r gold.PortfolioRow) string { return formatCents(r.PositionsValueBase) }},
		{Name: "cash_balance", Align: output.AlignRight,
			Extract: func(r gold.PortfolioRow) string { return formatCents(r.CashBalanceBase) }},
		{Name: "total_value", Align: output.AlignRight,
			Extract: func(r gold.PortfolioRow) string { return formatCents(r.TotalValueBase) }},

		{Name: "positions_value_outccy", Header: "positions_value" + suffix, Align: output.AlignRight,
			Extract: func(r gold.PortfolioRow) string { return formatCents(r.PositionsValueOutCcy) }},
		{Name: "cash_balance_outccy", Header: "cash_balance" + suffix, Align: output.AlignRight,
			Extract: func(r gold.PortfolioRow) string { return formatCents(r.CashBalanceOutCcy) }},
		{Name: "total_value_outccy", Header: "total_value" + suffix, Align: output.AlignRight,
			Extract: func(r gold.PortfolioRow) string { return formatCents(r.TotalValueOutCcy) }},
	}
}

var defaultPortfolioColumns = []string{
	"silver_source", "snapshot_date", "portfolio",
	"tax_wrapper", "management_style", "base_currency",
	"positions_value", "cash_balance", "total_value",
	"total_value_outccy",
}

func resolvePortfolioColumns(flagValue, outCcy string) ([]columnSpec[gold.PortfolioRow], error) {
	return resolveColumns(flagValue, defaultPortfolioColumns, buildPortfolioColumnRegistry(outCcy))
}

func portfoliosUsage() string {
	registry := buildPortfolioColumnRegistry("CCY")
	return `usage: wealthdb portfolios [-d YYYY-MM-DD] [-f FORMAT] [-C COLS] [-x CCY] [--fx-mode MODE]

Print one row per portfolio (wealth-management wrapper grouping
component accounts) plus one sentinel row per silver_source that
aggregates accounts with no portfolio (Schwab, Swissquote, any
UBS account the bank didn't group). Sum of total_value_<CCY>
across all rows equals the same sum from 'wealthdb accounts',
which equals 'wealthdb positions --with-cash'.

Flags:
  -d, --as-of YYYY-MM-DD   as-of date (default: today UTC)
  -f, --format FORMAT      output format (table | csv | csv_plain | json)
  -C, --columns COLS       comma-separated column names, 'default', 'all', or
                           a +ADD,...-REMOVE,... delta against the default set
                           (e.g. -C+relationship_id-cash_balance)
  -x, --currency CCY       output currency for the _<CCY> aggregate columns (default: config.default_currency)
      --fx-mode MODE       'historic' (default) or 'current'

Available columns:
  ` + joinColumnNames(registry) + `

  ('positions_value_outccy', 'cash_balance_outccy',
   'total_value_outccy' render as positions_value_<CCY> etc.)

Default column set:
  ` + strings.Join(defaultPortfolioColumns, ", ")
}
