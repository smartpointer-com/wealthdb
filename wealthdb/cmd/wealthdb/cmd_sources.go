package main

import (
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// ---- column registry -----------------------------------------------------

// buildSourceColumnRegistry is the sources view: one row per silver
// source, rolling up every one of its accounts. It sits between the
// portfolios and accounts views (finer) and the global row; the sum of
// total_value_<CCY> is the same at every grain.
func buildSourceColumnRegistry(outCcy string) []columnSpec[gold.SourceRow] {
	return []columnSpec[gold.SourceRow]{
		{Name: "silver_source", Align: output.AlignLeft,
			Extract: func(r gold.SourceRow) string { return r.SilverSourceID }},
		dateCol("snapshot_date", func(r gold.SourceRow) int64 { return r.SnapshotAt }),
		// The date of the latest mark the snapshot rides on: earlier than
		// snapshot_date where a read dated the snapshot after the marks.
		dateCol("valued_date", func(r gold.SourceRow) int64 { return r.ValuedAt }),
		// The date through which the source's holdings were observed:
		// later than snapshot_date where a run found nothing new.
		dateCol("observed_date", func(r gold.SourceRow) int64 { return r.ObservedAt }),
		textCol("base_currency", func(r gold.SourceRow) *string { return r.BaseCurrency }),
		// Source-level taxonomy rollups. Strict agree-or-NULL
		// semantics in the gold layer (every non-overlay account
		// agrees, none unclassified); a blank cell means
		// "mixed / unknown", not a render-time default.
		textCol("tax_wrapper", func(r gold.SourceRow) *string { return r.TaxWrapper }),
		textCol("management_style", func(r gold.SourceRow) *string { return r.ManagementStyle }),

		moneyCol("positions_value", "", func(r gold.SourceRow) *string { return r.PositionsValueBase }),
		moneyCol("cash_balance", "", func(r gold.SourceRow) *string { return r.CashBalanceBase }),
		moneyCol("total_value", "", func(r gold.SourceRow) *string { return r.TotalValueBase }),

		outCcyTwin("positions_value", outCcy, func(r gold.SourceRow) *string { return r.PositionsValueOutCcy }),
		outCcyTwin("cash_balance", outCcy, func(r gold.SourceRow) *string { return r.CashBalanceOutCcy }),
		outCcyTwin("total_value", outCcy, func(r gold.SourceRow) *string { return r.TotalValueOutCcy }),
	}
}

var defaultSourceColumns = []string{
	"silver_source", "snapshot_date",
	"tax_wrapper", "management_style", "base_currency",
	"positions_value", "cash_balance", "total_value",
	"total_value_outccy",
}

func sourcesUsage() string {
	registry := buildSourceColumnRegistry("CCY")
	return `usage: wealthdb holdings sources [-d YYYY-MM-DD] [-f FORMAT] [-C COLS] [-x CCY] [-p]

Print one row per source (an institution, or a hand-kept ledger),
rolling up every one of its accounts (positions + cash): the one
row to read for "how much is at <institution>". Sum of
total_value_<CCY> across all rows equals the same sum from
'wealthdb holdings accounts', 'portfolios', and 'global'.

snapshot_date is the date of the source's latest holdings.
valued_date (opt-in) is the date of the latest mark among them:
earlier than snapshot_date when a download found a holding gone
while the marks stayed.
observed_date (opt-in) is the date through which they were observed:
later than snapshot_date when a download found nothing new.

Flags:
  -d, --as-of YYYY-MM-DD   as-of date (default: today UTC)
  -f, --format FORMAT      output format (table | csv | csv_plain | json)
  -C, --columns COLS       comma-separated column names, 'default', 'all', or
                           a +ADD,...-REMOVE,... delta against the default set
                           (e.g. -C+positions_value_outccy-base_currency)
  -x, --currency CCY       output currency for the _<CCY> aggregate columns (default: config.default_currency)
  -p, --privacy            redact the monetary amounts
                           (table: visible placeholders; csv: empty cells; json: keys omitted)

Available columns:
  ` + joinColumnNames(registry) + `

  ('positions_value_outccy', 'cash_balance_outccy',
   'total_value_outccy' render as positions_value_<CCY> etc.)

Default column set:
  ` + strings.Join(defaultSourceColumns, ", ")
}
