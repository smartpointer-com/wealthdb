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
	if err := fs.Parse(subargs); err != nil {
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

	return writeFormatted(stdout, fmtChoice, portfoliosTable(rows, colSet))
}

// ---- column registry -----------------------------------------------------

type portfolioColumnSpec struct {
	Name    string
	Header  string
	Align   output.Alignment
	Extract func(gold.PortfolioRow) string
}

func (c portfolioColumnSpec) header() string {
	if c.Header != "" {
		return c.Header
	}
	return c.Name
}

func buildPortfolioColumnRegistry(outCcy string) []portfolioColumnSpec {
	suffix := "_" + outCcy
	return []portfolioColumnSpec{
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
	"silver_source", "snapshot_date", "portfolio", "base_currency",
	"positions_value", "cash_balance", "total_value",
	"total_value_outccy",
}

func resolvePortfolioColumns(flagValue, outCcy string) ([]portfolioColumnSpec, error) {
	registry := buildPortfolioColumnRegistry(outCcy)
	flagValue = strings.TrimSpace(flagValue)
	switch flagValue {
	case "", "default":
		return portfolioColumnsByName(defaultPortfolioColumns, registry)
	case "all":
		out := make([]portfolioColumnSpec, len(registry))
		copy(out, registry)
		return out, nil
	}
	names := strings.Split(flagValue, ",")
	for i, n := range names {
		names[i] = strings.TrimSpace(n)
	}
	return portfolioColumnsByName(names, registry)
}

func portfolioColumnsByName(names []string, registry []portfolioColumnSpec) ([]portfolioColumnSpec, error) {
	index := make(map[string]portfolioColumnSpec, len(registry))
	for _, c := range registry {
		index[c.Name] = c
	}
	out := make([]portfolioColumnSpec, 0, len(names))
	for _, n := range names {
		if n == "" {
			continue
		}
		c, ok := index[n]
		if !ok {
			return nil, fmt.Errorf("unknown column %q; available: %s", n, joinPortfolioColumnNames(registry))
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--columns produced an empty list")
	}
	return out, nil
}

func joinPortfolioColumnNames(registry []portfolioColumnSpec) string {
	names := make([]string, len(registry))
	for i, c := range registry {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}

func portfoliosTable(rows []gold.PortfolioRow, cols []portfolioColumnSpec) output.Table {
	t := output.Table{
		Columns: make([]string, len(cols)),
		Aligns:  make([]output.Alignment, len(cols)),
	}
	for i, c := range cols {
		t.Columns[i] = c.header()
		t.Aligns[i] = c.Align
	}
	for _, r := range rows {
		cells := make([]string, len(cols))
		for i, c := range cols {
			cells[i] = c.Extract(r)
		}
		t.Rows = append(t.Rows, cells)
	}
	return t
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
  -C, --columns COLS       comma-separated column names, or 'default' / 'all'
  -x, --currency CCY       output currency for the _<CCY> aggregate columns (default: config.default_currency)
      --fx-mode MODE       'historic' (default) or 'current'

Available columns:
  ` + joinPortfolioColumnNames(registry) + `

  ('positions_value_outccy', 'cash_balance_outccy',
   'total_value_outccy' render as positions_value_<CCY> etc.)

Default column set:
  ` + strings.Join(defaultPortfolioColumns, ", ")
}
