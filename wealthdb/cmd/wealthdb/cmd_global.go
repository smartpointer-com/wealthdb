package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/errs"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/output"
)

// cmdGlobal is the whole-portfolio rollup: a single row summing every
// account's output-currency cash, positions, and total value, plus
// the min/max of the per-account snapshot dates. The ultimate level
// of aggregation above cmd_accounts.go / cmd_portfolios.go; it reuses
// the accounts aggregation so the row reconciles with their sums.
func cmdGlobal(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb holdings global", flag.ContinueOnError)
	fs.SetOutput(stderr)

	hf := registerHoldingsFlags(fs, holdingsFlagSpec{
		cmd:            "global",
		currencyUsage:  "output currency for the _<CCY> columns (default: config.default_currency)",
		fxModeUsage:    "FX rate selection: 'historic' (nearest rate at-or-before the snapshot) or 'current' (latest available)",
		privacyUsage:   "redact the monetary amounts in the output",
		fxModeWantHint: true,
	})
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

	hv, err := hf.resolve(g)
	if err != nil {
		return err
	}

	db, err := openGoldForRead(g, hv.cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	row, err := gold.GlobalAsOf(ctx, db, hv.asOfEpoch, hv.outCcy, hv.mode)
	if err != nil {
		return err
	}

	cols := buildGlobalColumnRegistry(hv.outCcy)
	return writeFormatted(stdout, hv.fmtChoice, rowsToTable([]gold.GlobalRow{row}, cols, *hf.privacy, hv.fmtChoice))
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
	return `usage: wealthdb holdings global [-d YYYY-MM-DD] [-f FORMAT] [-x CCY] [--fx-mode MODE] [-p]

Roll the entire portfolio up into a single row — the ultimate level
of aggregation, summing every account's output-currency cash,
positions, and total value. Reconciles with the sum of the rows from
'wealthdb holdings accounts'.

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
      --fx-mode MODE       'historic' (default; nearest rate at-or-before the snapshot) or 'current' (latest rate)
  -p, --privacy            redact the monetary amounts
                           (table: visible placeholders; csv: empty cells; json: keys omitted)`
}
