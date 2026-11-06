package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// feePeriods are the fee view's buckets: a return and a rate need more
// than a day or a week to mean anything.
var feePeriods = returnsPeriods

// `wealthdb spending fees`: what each portfolio cost, in money and as a
// share of the value it held, beside its return. It reads the spending
// lines, so a vehicle's fees count.
func runSpendingFees(ctx context.Context, g globalFlags, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb spending fees", flag.ContinueOnError)
	fs.SetOutput(stderr)
	period := fs.String("period", "total", strings.Join(feePeriods, " | "))
	rf := registerReportFlags(fs, true)
	fs.Usage = func() { fmt.Fprintln(stderr, spendingFeesUsage()) }
	if err := parseFlags(fs, args, "spending"); err != nil {
		return err
	}
	if err := checkChoice("spending", "period", *period, feePeriods...); err != nil {
		return err
	}
	fromEpoch, toEpoch, err := parseTrailingYearWindow(fs.Args(), g.clock())
	if err != nil {
		fs.Usage()
		return errs.Newf(errs.ExitUsage, "spending: %s", err.Error())
	}
	fmtChoice, cfg, outCcy, err := rf.resolve(g, "spending")
	if err != nil {
		return err
	}
	req := request{view: "fees", currency: outCcy, from: fromEpoch, to: toEpoch, period: *period}
	rep := spendingFeesReport(req, cfg)
	open := func() (*sql.DB, error) { return openGoldForRead(g, cfg) }
	return writeReport(ctx, rep, rf.output(fmtChoice, "spending", req, cfg, g.clock()), open, stdout)
}

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
	ref := func(r gold.FeeRow) accountRef {
		return accountRef{source: r.SilverSourceID, name: &r.EntityLabel,
			nickname: nicknames.of(r.SilverSourceID, r.EntityID), id: r.EntityID}
	}
	entity := eitherCol("entity", asPortfolio,
		portfolioCol("entity", kindOf, func(r gold.FeeRow) (string, string, *string) {
			return r.SilverSourceID, r.EntityID, &r.EntityLabel
		}), accountCol(kindOf, ref))
	entityID := eitherCol("entity_id", asPortfolio,
		typedIDCol("entity_id", kindOf, func(r gold.FeeRow) (string, string) {
			return r.SilverSourceID, r.EntityID
		}), accountIDCol("entity_id", kindOf, ref))
	return []columnSpec[gold.FeeRow]{
		asDate(strCol("period", func(r gold.FeeRow) string { return r.Period })),
		asDate(strCol("start_date", func(r gold.FeeRow) string { return formatEpochDay(r.StartDay) })),
		asDate(strCol("end_date", func(r gold.FeeRow) string { return formatEpochDay(r.EndDay) })),
		strCol("silver_source", func(r gold.FeeRow) string { return r.SilverSourceID }),
		strCol("entity_kind", func(r gold.FeeRow) string { return r.EntityKind }),
		entity,
		entityID,
		outCcyCol("fees", outCcy, func(r gold.FeeRow) *string { return r.Fees }),
		outCcyCol("fees_investment", outCcy, func(r gold.FeeRow) *string { return r.FeesInvestment }),
		outCcyCol("fees_bank", outCcy, func(r gold.FeeRow) *string { return r.FeesBank }),
		outCcyCol("fees_atm", outCcy, func(r gold.FeeRow) *string { return r.FeesATM }),
		outCcyCol("fees_other", outCcy, func(r gold.FeeRow) *string { return r.FeesOther }),
		outCcyCol("mean_value", outCcy, func(r gold.FeeRow) *string { return r.MeanValue }),
		{Name: "fee_rate_pct", Align: output.AlignRight,
			Extract: func(r gold.FeeRow) string { return formatPctOrBlank(r.FeeRate) }},
		{Name: "twr_pct", Align: output.AlignRight,
			Extract: func(r gold.FeeRow) string { return formatPctOrBlank(r.TWR) }},
		qualityCol(func(r gold.FeeRow) string { return strings.Join(r.Quality, ";") }),
	}
}

// eitherCol is a column that reads as a where first holds and as b
// elsewhere: a row's entity is one of two kinds, each with its own
// redaction. Both columns set PrivacyFunc and Pseudonym.
func eitherCol[T any](name string, first func(T) bool, a, b columnSpec[T]) columnSpec[T] {
	pick := func(r T) columnSpec[T] {
		if first(r) {
			return a
		}
		return b
	}
	return columnSpec[T]{Name: name, Align: output.AlignLeft, Privacy: PrivacyAccountID,
		Extract:     func(r T) string { return pick(r).Extract(r) },
		PrivacyFunc: func(r T) PrivacyClass { return pick(r).PrivacyFunc(r) },
		PrivacyExtract: func(r T) string {
			if c := pick(r); c.PrivacyExtract != nil {
				return c.PrivacyExtract(r)
			}
			return pick(r).Extract(r)
		},
		Pseudonym: func(r T) (string, string) { return pick(r).Pseudonym(r) }}
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
  mean_value       the entity's mean daily value, over the days it held one
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
  -p, --privacy     redact amounts, ids and names as the returns views do;
                    rates and returns stay visible

Available columns:
  ` + joinColumnNames(buildFeeColumnRegistry("CCY", nil, nil)) + `

Default column set:
  ` + strings.Join(defaultFeeColumns, ", ")
}
