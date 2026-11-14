package main

import (
	"context"
	"database/sql"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

// statedRefusedFlags are the returns flags that have no meaning on the
// stated view: the document fixes the periods, and the view computes no
// return of its own. -x is refused beside them.
var statedRefusedFlags = []string{"method", "period", "annualize", "netting", "inception", "benchmark"}

// returnsStatedReport runs the reconciliation with the config's returns
// settings, so its engine figures are the ones `returns accounts` prints.
func returnsStatedReport(req request, cfg *config.Config) *report {
	kindOf, loadKinds := sourceKinds()
	nicknames := accountNicknames{}
	fetch := func(ctx context.Context, db *sql.DB) ([]gold.StatedPeriodRow, error) {
		return gold.ReconcileStatedPeriods(ctx, db, withReturnsCfg(cfg, gold.ReturnParams{
			FromEpoch: req.from, ToEpoch: req.to, Annualize: "auto", Netting: true, Inception: "full",
		}))
	}
	return withLoads(newReport(buildStatedColumnRegistry(kindOf, nicknames), defaultStatedColumns, fetch),
		loadKinds, nicknames.load)
}

var defaultStatedColumns = []string{"silver_source", "account", "period", "currency",
	"stated_net_additions", "net_flow", "flow_gap", "stated_ending", "end_value", "value_gap", "quality"}

// buildStatedColumnRegistry is the stated view's registry. Its account
// columns read as `returns accounts` reads its entity, nickname included.
func buildStatedColumnRegistry(kindOf func(string) string, nicknames accountNicknames) []columnSpec[gold.StatedPeriodRow] {
	ref := func(r gold.StatedPeriodRow) accountRef {
		return accountRef{source: r.SilverSourceID, name: &r.AccountLabel,
			nickname: nicknames.of(r.SilverSourceID, r.AccountExternalID), id: r.AccountExternalID}
	}
	return []columnSpec[gold.StatedPeriodRow]{
		strCol("silver_source", func(r gold.StatedPeriodRow) string { return r.SilverSourceID }),
		accountCol(kindOf, ref),
		accountIDCol("account_id", kindOf, ref),
		asDate(strCol("period", func(r gold.StatedPeriodRow) string { return r.Period })),
		asDate(strCol("start_date", func(r gold.StatedPeriodRow) string { return formatEpochDay(r.StartDay) })),
		asDate(strCol("end_date", func(r gold.StatedPeriodRow) string { return formatEpochDay(r.EndDay) })),
		strCol("currency", func(r gold.StatedPeriodRow) string { return r.Currency }),
		moneyCol("stated_beginning", "", func(r gold.StatedPeriodRow) *string { return r.StatedBeginning }),
		moneyCol("stated_ending", "", func(r gold.StatedPeriodRow) *string { return r.StatedEnding }),
		moneyCol("stated_accrual", "", func(r gold.StatedPeriodRow) *string { return r.StatedAccrual }),
		moneyCol("stated_net_additions", "", func(r gold.StatedPeriodRow) *string { return r.StatedNetAdditions }),
		moneyCol("stated_net_gain", "", func(r gold.StatedPeriodRow) *string { return r.StatedNetGain }),
		moneyCol("stated_income", "", func(r gold.StatedPeriodRow) *string { return r.StatedIncome }),
		moneyCol("stated_fees", "", func(r gold.StatedPeriodRow) *string { return r.StatedFees }),
		moneyCol("start_value", "", func(r gold.StatedPeriodRow) *string { return r.StartValue }),
		moneyCol("end_value", "", func(r gold.StatedPeriodRow) *string { return r.EndValue }),
		moneyCol("net_flow", "", func(r gold.StatedPeriodRow) *string { return r.NetFlow }),
		moneyCol("flow_gap", "", func(r gold.StatedPeriodRow) *string { return r.FlowGap }),
		moneyCol("value_gap", "", func(r gold.StatedPeriodRow) *string { return r.ValueGap }),
		// The stating document's sha256 names no person, but it is the
		// handle on a private file, so -p redacts it like an id.
		{Name: "document", Align: output.AlignLeft, Privacy: PrivacyAccountID,
			Extract: func(r gold.StatedPeriodRow) string { return r.SourceSHA256 }},
		qualityCol(func(r gold.StatedPeriodRow) string { return strings.Join(r.Quality, ";") }),
	}
}

func returnsStatedUsage() string {
	return `usage: wealthdb returns stated [FROM [TO]] [-f FORMAT] [-C COLS] [-F COND]... [-p]

Each period a document states for an account, beside what the returns
engine computes for that account over the same months. The svb
collector's month-end workbook is such a document. The stated figures
are informative: they create no flow, so the engine's figures are the
ones 'returns accounts --period monthly' prints, in the stated currency.

  net_flow     the engine's net external flow over the period
  flow_gap     stated_net_additions - net_flow
  value_gap    stated_ending - end_value

A period the engine has no monthly row for reads quality no_engine_row.
The window filters on the period's last day.

Available columns:
  ` + joinColumnNames(buildStatedColumnRegistry(nil, nil)) + `

Default column set:
  ` + strings.Join(defaultStatedColumns, ", ")
}
