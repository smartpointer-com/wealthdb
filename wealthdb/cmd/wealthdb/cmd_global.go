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
)

func init() {
	register("global", cmdGlobal)
}

// cmdGlobal is the whole-portfolio rollup: a single row summing every
// account's output-currency cash, positions, and total value, plus
// the min/max of the per-account snapshot dates. The ultimate level
// of aggregation above cmd_accounts.go / cmd_portfolios.go; it reuses
// the accounts aggregation so the row reconciles with their sums.
func cmdGlobal(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb global", flag.ContinueOnError)
	fs.SetOutput(stderr)

	asOf := fs.String("d", "", "as-of date (YYYY-MM-DD; default today UTC)")
	fs.StringVar(asOf, "as-of", "", "as-of date (YYYY-MM-DD; default today UTC)")
	format := fs.String("f", "table", "output format: table | csv | csv_plain | json")
	fs.StringVar(format, "format", "table", "output format: table | csv | csv_plain | json")
	currency := fs.String("x", "", "output currency for the _<CCY> columns (default: config.default_currency)")
	fs.StringVar(currency, "currency", "", "output currency (default: config.default_currency)")
	fxMode := fs.String("fx-mode", "historic", "FX rate selection: 'historic' (rate at snapshot time, interpolated) or 'current' (latest available)")
	privacy := fs.Bool("p", false, "redact the monetary amounts in the output")
	fs.BoolVar(privacy, "privacy", false, "redact the monetary amounts in the output")
	fs.Usage = func() {
		fmt.Fprintln(stderr, globalCmdUsage())
	}
	if err := fs.Parse(subargs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errs.Newf(2, "global: bad flags")
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errs.Newf(2, "global: unexpected positional argument %q", fs.Arg(0))
	}

	mode := canonical.FxMode(*fxMode)
	if !mode.Valid() {
		return errs.Newf(2, "global: invalid --fx-mode %q (want 'historic' or 'current')", *fxMode)
	}

	fmtChoice, err := output.Parse(*format)
	if err != nil {
		return errs.Newf(2, "global: %s", err.Error())
	}

	asOfEpoch, err := parseAsOf(*asOf, time.Now())
	if err != nil {
		return errs.Newf(2, "global: %s", err.Error())
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
		return errs.Newf(2, "global: invalid -x/--currency %q (want a 3-letter ISO 4217 code)", outCcy)
	}

	db, err := openGoldForRead(g, cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	row, err := gold.GlobalAsOf(ctx, db, asOfEpoch, outCcy, mode)
	if err != nil {
		return err
	}

	cols := buildGlobalColumnRegistry(outCcy)
	return writeFormatted(stdout, fmtChoice, rowsToTable([]gold.GlobalRow{row}, cols, *privacy, fmtChoice))
}

// buildGlobalColumnRegistry is the fixed five-column shape of the
// rollup row. There is no -C/--columns flag: the whole point is a
// single, fixed, ultimate-total line.
func buildGlobalColumnRegistry(outCcy string) []columnSpec[gold.GlobalRow] {
	suffix := "_" + outCcy
	return []columnSpec[gold.GlobalRow]{
		{Name: "min_snapshot_date", Align: output.AlignLeft, Extract: func(r gold.GlobalRow) string {
			if r.MinSnapshotAt == 0 {
				return ""
			}
			return formatDate(r.MinSnapshotAt)
		}},
		{Name: "max_snapshot_date", Align: output.AlignLeft, Extract: func(r gold.GlobalRow) string {
			if r.MaxSnapshotAt == 0 {
				return ""
			}
			return formatDate(r.MaxSnapshotAt)
		}},
		{Name: "cash_balance_outccy", Header: "cash_balance" + suffix, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.GlobalRow) string { return formatCents(r.CashBalanceOutCcy) }},
		{Name: "positions_value_outccy", Header: "positions_value" + suffix, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.GlobalRow) string { return formatCents(r.PositionsValueOutCcy) }},
		{Name: "total_value_outccy", Header: "total_value" + suffix, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.GlobalRow) string { return formatCents(r.TotalValueOutCcy) }},
	}
}

func globalCmdUsage() string {
	return `usage: wealthdb global [-d YYYY-MM-DD] [-f FORMAT] [-x CCY] [--fx-mode MODE] [-p]

Roll the entire portfolio up into a single row — the ultimate level
of aggregation, summing every account's output-currency cash,
positions, and total value. Reconciles with the sum of the rows from
'wealthdb accounts'.

Columns (CUR = the -x/--currency choice, default config.default_currency):
  min_snapshot_date      earliest of the per-account latest snapshot dates
  max_snapshot_date      latest of the per-account latest snapshot dates
  cash_balance_<CUR>     summed cash balances
  positions_value_<CUR>  summed non-cash positions value
  total_value_<CUR>      summed grand total

Flags:
  -d, --as-of YYYY-MM-DD   as-of date (default: today UTC)
  -f, --format FORMAT      output format (table | csv | csv_plain | json)
  -x, --currency CCY       output currency for the _<CUR> columns (default: config.default_currency)
      --fx-mode MODE       'historic' (default; rate at snapshot time, interpolated) or 'current' (latest rate)
  -p, --privacy            redact the monetary amounts
                           (table: visible placeholders; csv: empty cells; json: keys omitted)`
}
