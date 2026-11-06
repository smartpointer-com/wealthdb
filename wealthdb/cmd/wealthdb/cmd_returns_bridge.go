package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// bridgeRefusedFlags are the returns flags that have no meaning on the
// bridge: it has no return to compute, and one set of flows to read.
var bridgeRefusedFlags = []string{"method", "annualize", "netting", "inception", "benchmark"}

// `wealthdb returns bridge`: why net worth moved over each bucket. The
// flags of the other views that do not apply are registered and refused
// by name, so the error says why rather than calling them unknown.
func runReturnsBridge(ctx context.Context, g globalFlags, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb returns bridge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	period := fs.String("period", "quarterly", "monthly | quarterly | annual | total")
	for _, name := range bridgeRefusedFlags {
		fs.String(name, "", "refused: the bridge has no such choice")
	}
	rf := registerReportFlags(fs, true)
	fs.Usage = func() { fmt.Fprintln(stderr, returnsBridgeUsage()) }
	if err := parseFlags(fs, args, "returns"); err != nil {
		return err
	}
	for _, name := range bridgeRefusedFlags {
		if isSet(fs, name) {
			return errs.Newf(errs.ExitUsage, "returns: bridge takes no --%s", name)
		}
	}
	if err := checkChoice("returns", "period", *period, returnsPeriods...); err != nil {
		return err
	}
	fromEpoch, toEpoch, err := parseReturnsWindow(fs.Args(), g.clock())
	if err != nil {
		fs.Usage()
		return errs.Newf(errs.ExitUsage, "returns: %s", err.Error())
	}
	if err := checkBridgeWindow(fromEpoch, toEpoch, anchorToDay(g.clock().UTC(), true).Unix()); err != nil {
		return err
	}
	fmtChoice, cfg, outCcy, err := rf.resolve(g, "returns")
	if err != nil {
		return err
	}
	req := request{view: "bridge", currency: outCcy, from: fromEpoch, to: toEpoch, period: *period}
	rep := returnsBridgeReport(req, cfg)
	open := func() (*sql.DB, error) { return openGoldForRead(g, cfg) }
	return writeReport(ctx, rep, rf.output(fmtChoice, "returns", req, cfg, g.clock()), open, stdout)
}

// checkBridgeWindow refuses a window the bridge's months cannot cut: FROM
// must be a month's first day and TO a month's last, or today or later,
// where the last month runs to the latest data. FROM 0 is since inception.
// today is the end of the current day.
func checkBridgeWindow(fromEpoch, toEpoch, today int64) error {
	from, to := time.Unix(fromEpoch, 0).UTC(), time.Unix(toEpoch, 0).UTC()
	if fromEpoch > 0 && from.Day() != 1 {
		return errs.Newf(errs.ExitUsage, "returns: bridge reads whole months: FROM %s is no month's first day",
			from.Format(time.DateOnly))
	}
	lastOfMonth := to.AddDate(0, 0, 1).Day() == 1
	if !lastOfMonth && toEpoch < today {
		return errs.Newf(errs.ExitUsage, "returns: bridge reads whole months: TO %s is no month's last day",
			to.Format(time.DateOnly))
	}
	return nil
}

// returnsBridgeReport is the bridge with the config's returns settings, the
// runner the CLI and the MCP server share.
func returnsBridgeReport(req request, cfg *config.Config) *report {
	fetch := func(ctx context.Context, db *sql.DB) ([]gold.BridgeRow, error) {
		return gold.RunBridge(ctx, db, withReturnsCfg(cfg, gold.ReturnParams{
			FromEpoch: req.from, ToEpoch: req.to, OutCcy: req.currency, Period: req.period,
		}))
	}
	return newReport(buildBridgeColumnRegistry(req.currency), defaultBridgeColumns, fetch)
}

var defaultBridgeColumns = []string{"period", "start_net_worth", "net_saved", "onboarded", "gain",
	"end_net_worth", "quality"}

func buildBridgeColumnRegistry(outCcy string) []columnSpec[gold.BridgeRow] {
	return []columnSpec[gold.BridgeRow]{
		asDate(strCol("period", func(r gold.BridgeRow) string { return r.Period })),
		asDate(strCol("start_date", func(r gold.BridgeRow) string { return formatEpochDay(r.StartDay) })),
		asDate(strCol("end_date", func(r gold.BridgeRow) string { return formatEpochDay(r.EndDay) })),
		outCcyCol("start_net_worth", outCcy, func(r gold.BridgeRow) *string { return r.StartNetWorth }),
		outCcyCol("net_saved", outCcy, func(r gold.BridgeRow) *string { return r.NetSaved }),
		outCcyCol("onboarded", outCcy, func(r gold.BridgeRow) *string { return r.Onboarded }),
		outCcyCol("gain", outCcy, func(r gold.BridgeRow) *string { return r.Gain }),
		outCcyCol("end_net_worth", outCcy, func(r gold.BridgeRow) *string { return r.EndNetWorth }),
		outCcyCol("net_flow", outCcy, func(r gold.BridgeRow) *string { return r.NetFlow }),
		outCcyCol("debt_repaid", outCcy, func(r gold.BridgeRow) *string { return r.DebtRepaid }),
		outCcyCol("bridge_gap", outCcy, func(r gold.BridgeRow) *string { return r.Gap }),
		qualityCol(func(r gold.BridgeRow) string { return strings.Join(r.Quality, ";") }),
	}
}

func returnsBridgeUsage() string {
	return `usage: wealthdb returns bridge [FROM [TO]] [--period P]
                               [-f FORMAT] [-C COLS] [-F COND]... [-x CCY] [-p]

Why net worth moved over each bucket, for the whole household. Each row
holds, to the cent:

  end_net_worth = start_net_worth + net_saved + onboarded + gain

  start_net_worth, end_net_worth
               what 'holdings global' reads at the bucket's edges
  onboarded    value that entered or left tracking, neither saved nor
               gained: returns' synthetic onboarding and closure flows,
               less any tracking move (below)
  net_flow     the net flow of 'returns global', less onboarded
  debt_repaid  the fall in what is owed on mortgages and cards. Each is
               measured in its own currency, month by month, at that
               month's end rate. It is negative when debt rose
  net_saved    net_flow + debt_repaid
  gain         the rest: the gain of 'returns global', plus the FX move
               on debt
  bridge_gap   net worth less returns' value less the mortgages and
               cards, at the worst edge of the bucket; zero when the
               three agree

A card purchase raises the debt, so it reads as money spent; the bill's
payment leaves as a flow and comes back as debt repaid. A mortgage
instalment's principal comes back the same way, and its interest stays
spent. A new loan's cash and its debt cancel.

A move into an account tracking first sees is one movement, and so is
one out of an account that closes. Where a month's net_flow and its
onboarded have opposite signs, the smaller size leaves both. Their sum
stays returns' net flow. A debut with no net outflow beside it stays
onboarded, and any net outflow beside a debut reads as a move, a
month's spending too.

The bridge reads whole months. FROM is a month's first day and TO a
month's last, or today or later; the last row then runs to the latest
data. A sale whose cash lands after month-end reads as a loss, then a
gain. gain is a residual: it is as good as the flows 'returns' finds.

Window (positional, optional; default: since the first snapshot):
  YYYY / YYYY-MM                that calendar period
  FROM TO                       explicit range; '-' is open-ended

Flags:
  --period P        monthly | quarterly (default) | annual | total
  -f, --format      table | csv | csv_plain | json
  -C, --columns     comma-separated names, 'default', 'all', or a +ADD,-REMOVE delta
  -F, --filter COND keep the rows where COND holds; repeatable;
                    wealthdb help filter lists the operators
  -x, --currency    output currency (default: config.default_currency)
  -p, --privacy     redact the amounts

--method, --annualize, --netting, --inception and --benchmark do not
apply: the bridge computes no return, and it reads the flows of the
default global run.

The quality column carries returns' tags for the bucket, and these:
  debt_first_seen   a debt appeared with no matching cash beside it, so
                    net_saved falls once by what was already owed
  debt_ended        a debt left the history while still owed, so
                    debt_repaid rises with no payment beside it
  debt_unpriced     a debt has no rate to the output currency; its fall
                    is left to gain
  bridge_gap        bridge_gap is not zero: gain holds that difference
  tracking_netted   a month's net flow and onboarded had opposite signs:
                    the smaller size is read as a move and left both

Available columns:
  ` + joinColumnNames(buildBridgeColumnRegistry("CCY")) + `

Default column set:
  ` + strings.Join(defaultBridgeColumns, ", ")
}
