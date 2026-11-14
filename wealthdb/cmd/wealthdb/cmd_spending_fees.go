package main

import (
	"context"
	"database/sql"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// feePeriods are the fee view's buckets: a return and a rate need more
// than a day or a week to mean anything.
var feePeriods = returnsPeriods

// feesRefusedFlags are the spending flags that have no meaning on the
// fee view: its rows are entities, not categories. A category filter is
// refused as every view's filters are (filterViewError).
var feesRefusedFlags = []string{"level"}

// spendingFeesReport is the fee view, the runner the CLI and the MCP
// server share.
func spendingFeesReport(req request, cfg *config.Config) *report {
	kindOf, loadKinds := sourceKinds()
	nicknames := accountNicknames{}
	fetch := func(ctx context.Context, db *sql.DB) ([]gold.FeeRow, error) {
		return gold.RunFees(ctx, db, withReturnsCfg(cfg, gold.ReturnParams{
			FromEpoch: req.from, ToEpoch: req.to, OutCcy: req.currency, Period: req.period,
		}))
	}
	return withLoads(newReport(buildFeeColumnRegistry(req.currency, kindOf, nicknames), defaultFeeColumns, fetch),
		loadKinds, nicknames.load)
}

var defaultFeeColumns = []string{"period", "silver_source", "entity", "fees", "mean_value",
	"fee_rate_pct", "twr_pct", "quality"}

// buildFeeColumnRegistry is the fee view's registry. Its entity is a
// portfolio or an account, and -p shows each as its own report does.
func buildFeeColumnRegistry(outCcy string, kindOf func(string) string, nicknames accountNicknames) []columnSpec[gold.FeeRow] {
	asPortfolio := func(r gold.FeeRow) bool { return r.EntityKind == "portfolio" }
	account, portfolio := entityCols(kindOf, nicknames, func(r gold.FeeRow) (string, string, string) {
		return r.SilverSourceID, r.EntityID, r.EntityLabel
	})
	return []columnSpec[gold.FeeRow]{
		asDate(strCol("period", func(r gold.FeeRow) string { return r.Period })),
		asDate(strCol("start_date", func(r gold.FeeRow) string { return formatEpochDay(r.StartDay) })),
		asDate(strCol("end_date", func(r gold.FeeRow) string { return formatEpochDay(r.EndDay) })),
		strCol("silver_source", func(r gold.FeeRow) string { return r.SilverSourceID }),
		strCol("entity_kind", func(r gold.FeeRow) string { return r.EntityKind }),
		eitherCol("entity", asPortfolio, portfolio[0], account[0]),
		eitherCol("entity_id", asPortfolio, portfolio[1], account[1]),
		outCcyCol("fees", outCcy, func(r gold.FeeRow) *string { return r.Fees }),
		outCcyCol("fees_investment", outCcy, func(r gold.FeeRow) *string { return r.FeesInvestment }),
		outCcyCol("fees_bank", outCcy, func(r gold.FeeRow) *string { return r.FeesBank }),
		outCcyCol("fees_atm", outCcy, func(r gold.FeeRow) *string { return r.FeesATM }),
		outCcyCol("fees_other", outCcy, func(r gold.FeeRow) *string { return r.FeesOther }),
		outCcyCol("mean_value", outCcy, func(r gold.FeeRow) *string { return r.MeanValue }),
		ratioOrBlankCol("fee_rate_pct", "", func(r gold.FeeRow) *float64 { return r.FeeRate }),
		ratioOrBlankCol("twr_pct", "", func(r gold.FeeRow) *float64 { return r.TWR }),
		qualityCol(func(r gold.FeeRow) string { return strings.Join(r.Quality, ";") }),
	}
}

func spendingFeesUsage() string {
	return `usage: wealthdb spending fees [FROM [TO]] [--period P]
                             [-f FORMAT] [-C COLS] [-F COND]... [-x CCY] [-p]

What each portfolio cost, in money and as a share of the value it held,
beside its return. One row per bucket and entity with a fee line: a
portfolio, else an account outside any portfolio. The largest cost comes
first.

  fees             the spending lines in primary BANK_FEES, net of refunds
  fees_investment, fees_bank, fees_atm, fees_other
                   the same lines by detailed group; they sum to fees
  mean_value       the entity's mean daily value, over the days it held one.
                   A portfolio's leaves out its mortgages and cards, as its
                   twr_pct does
  fee_rate_pct     fees over mean_value, annualized (365 days). Blank where
                   the mean value is not above zero
  twr_pct          the entity's monthly returns from 'returns portfolios' or
                   'returns accounts --period monthly', since inception,
                   chained over the months that end inside the window: the
                   Spending dashboard's figure. It can differ from a run over
                   the window itself ('returns accounts FROM TO --period
                   total'), which chains on valuation days. Blank where
                   returns has no month, as for plumbing cash accounts

It reads the spending lines, never the cash flow statement, so a vehicle's
fees count: a trust's, a pension plan's. Over one window the rows sum to
the 'Bank fees' row of 'spending categories'. Returns stay net of fees.

What the figures miss:
  - Only booked fee lines count. Trading costs sit inside the trades.
  - Fund costs sit inside fund prices.
  - A fee debited from another account lands on that account. Grouping by
    portfolio fixes this where a portfolio exists.
  - A yearly fee in a short window annualizes as if it recurred.

The quality column says what a row misses:
  no_mean_value      the mean value is not above zero, so there is no rate
  part_window_value  the entity held a value on only part of the window
  no_return          returns has no monthly return for the entity in the window
  unpriced_fees=N    N lines have no rate to the output currency
and the tags of the months chained.

Window (positional, optional; default: the trailing twelve months):
  YYYY / YYYY-MM / YYYY-MM-DD   that calendar period
  FROM TO                       explicit range; '-' is open-ended

Flags:
  --period P        monthly | quarterly | annual | total (default)
  -f, --format      table | csv | csv_plain | json
  -C, --columns     comma-separated names, 'default', 'all', or a +ADD,-REMOVE delta
  -F, --filter COND keep the rows where COND holds; repeatable;
                    wealthdb help filter lists the operators
  -x, --currency    output currency (default: config.default_currency)
  -p, --privacy     redact amounts, ids and account names as the returns views do;
                    rates and returns stay visible

Available columns:
  ` + joinColumnNames(buildFeeColumnRegistry("CCY", nil, nil)) + `

Default column set:
  ` + strings.Join(defaultFeeColumns, ", ")
}
