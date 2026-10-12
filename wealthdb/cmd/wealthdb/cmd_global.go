package main

import (
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// buildGlobalColumnRegistry is the whole-portfolio rollup: one row
// summing every account's output-currency cash, positions and total
// value, plus the min and max of the per-account snapshot dates. It
// reuses the accounts aggregation, so the row reconciles with their
// sums. Its shape is fixed, so the view takes no -C/--columns.
func buildGlobalColumnRegistry(outCcy string) []columnSpec[gold.GlobalRow] {
	return []columnSpec[gold.GlobalRow]{
		dateCol("min_snapshot_date", func(r gold.GlobalRow) int64 { return r.MinSnapshotAt }),
		dateCol("max_snapshot_date", func(r gold.GlobalRow) int64 { return r.MaxSnapshotAt }),
		outCcyTwin("cash_balance", outCcy, func(r gold.GlobalRow) *string { return r.CashBalanceOutCcy }),
		outCcyTwin("positions_value", outCcy, func(r gold.GlobalRow) *string { return r.PositionsValueOutCcy }),
		outCcyTwin("total_value", outCcy, func(r gold.GlobalRow) *string { return r.TotalValueOutCcy }),
	}
}

func globalCmdUsage() string {
	return `usage: wealthdb holdings global [-d YYYY-MM-DD] [-f FORMAT] [-x CCY] [-p]

Roll everything up into a single row: the household's cash,
positions and total value (net worth) in the output currency, every
account included, loans subtracted. Reconciles with the sum of the
rows from 'wealthdb holdings accounts'.

Columns (CCY = the -x/--currency choice, default config.default_currency):
  min_snapshot_date      earliest of the per-account latest snapshot dates
  max_snapshot_date      latest of the per-account latest snapshot dates
  cash_balance_<CCY>     summed cash balances
  positions_value_<CCY>  summed non-cash positions value
  total_value_<CCY>      summed grand total

Flags:
  -d, --as-of YYYY-MM-DD   as-of date (default: today UTC)
  -f, --format FORMAT      output format (table | csv | csv_plain | json)
  -x, --currency CCY       output currency for the _<CCY> columns (default: config.default_currency)
  -p, --privacy            redact the monetary amounts
                           (table: visible placeholders; csv: empty cells; json: keys omitted)`
}
