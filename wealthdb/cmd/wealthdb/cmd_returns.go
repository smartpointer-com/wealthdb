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
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/output"
)

func init() {
	register("returns", cmdReturns)
}

// returnsViews are the views `wealthdb returns <view>` supports: the
// four reconciling grains, coarsest first, then `stated`, a side view
// of the documents' stated periods beside the accounts' figures, which
// is no grain of them. Per-position gains are the gains report's.
var returnsViews = []string{"global", "sources", "portfolios", "accounts", "stated"}

// The choices of the returns options the CLI and the MCP tool share.
var (
	returnsMethods   = []string{"twr", "mwr", "both"}
	returnsPeriods   = []string{"monthly", "quarterly", "annual", "total"}
	returnsAnnualize = []string{"auto", "always", "never"}
	returnsInception = []string{"full", "strict"}
)

// cmdReturns routes `wealthdb returns <view> ...` to the view's run.
func cmdReturns(ctx context.Context, g globalFlags, subargs []string, _ io.Reader, stdout, stderr io.Writer) error {
	view, rest, err := routeView("returns", subargs, returnsViews, returnsUsage, stderr)
	if err != nil {
		return err
	}
	if view == "stated" {
		return runReturnsStated(ctx, g, rest, stdout, stderr)
	}
	return runReturnsView(ctx, g, view, rest, stdout, stderr)
}

func runReturnsView(ctx context.Context, g globalFlags, view string, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("wealthdb returns "+view, flag.ContinueOnError)
	fs.SetOutput(stderr)
	method := fs.String("method", "twr", "twr | mwr | both")
	period := fs.String("period", "quarterly", "monthly | quarterly | annual | total")
	annualize := fs.String("annualize", "auto", "auto | always | never")
	netting := fs.String("netting", "on", "on | off — net internal transfers at coarse grains (incl. cross-source matched pairs)")
	inception := fs.String("inception", "full", "full | strict — aggregate since-inception handling")
	rf := registerReportFlags(fs, true)
	fs.Usage = func() { fmt.Fprintln(stderr, returnsUsage()) }
	if err := parseFlags(fs, args, "returns"); err != nil {
		return err
	}
	for _, c := range []struct {
		flag, value string
		allowed     []string
	}{
		{"method", *method, returnsMethods},
		{"period", *period, returnsPeriods},
		{"annualize", *annualize, returnsAnnualize},
		{"netting", *netting, []string{"on", "off"}},
		{"inception", *inception, returnsInception},
	} {
		if err := checkChoice("returns", c.flag, c.value, c.allowed...); err != nil {
			return err
		}
	}
	fromEpoch, toEpoch, err := parseReturnsWindow(fs.Args(), g.clock())
	if err != nil {
		fs.Usage()
		return errs.Newf(errs.ExitUsage, "returns: %s", err.Error())
	}

	fmtChoice, cfg, outCcy, err := rf.resolve(g, "returns")
	if err != nil {
		return err
	}

	rep := returnsReport(request{view: view, currency: outCcy, from: fromEpoch, to: toEpoch,
		method: *method, period: *period, annualize: *annualize, netting: *netting == "on", inception: *inception}, cfg)
	open := func() (*sql.DB, error) { return openGoldForRead(g, cfg) }
	return writeReport(ctx, rep, *rf.cols, "returns", open, *rf.privacy, fmtChoice, stdout)
}

// returnsReport is one view of the returns family, the runner the CLI
// and the MCP server share: RunReturns with the config's overrides, so
// the two front-ends apply exactly the same settings.
func returnsReport(req request, cfg *config.Config) *report {
	kindOf, loadKinds := sourceKinds()
	nicknames := accountNicknames{}
	fetch := func(ctx context.Context, db *sql.DB) ([]gold.ReturnRow, error) {
		return gold.RunReturns(ctx, db, withReturnsCfg(cfg, gold.ReturnParams{
			Level: req.view, FromEpoch: req.from, ToEpoch: req.to, OutCcy: req.currency,
			Method: req.method, Period: req.period, Annualize: req.annualize,
			Netting: req.netting, Inception: req.inception,
		}))
	}
	loads := []func(context.Context, *sql.DB) error{loadKinds}
	// Only an account's entity reads its nickname.
	if req.view == "accounts" {
		loads = append(loads, nicknames.load)
	}
	return withLoads(newReport(buildReturnColumnRegistry(req.currency, req.view, kindOf, nicknames),
		defaultReturnColumnsFor(req.method), fetch), loads...)
}

// returnsCfgFiles are the files returnsCfgSettings reads beside the
// config. A refresh's input key hashes them with it.
func returnsCfgFiles(cfg *config.Config) []string {
	if m, ledger := transferMatching(cfg); m != nil {
		return []string{ledger}
	}
	return nil
}

// transferMatching is the returns transfer-matching block and the
// override ledger it reads, or nil when matching is off.
func transferMatching(cfg *config.Config) (*config.ReturnsTransferMatching, string) {
	if m := cfg.ReturnsTransferMatching; m != nil && m.Enabled {
		return m, cfg.SpendTransferOverrides()
	}
	return nil, ""
}

// withReturnsCfg is p with the config's returns settings
// (returnsCfgSettings), so every report runner applies the same ones.
func withReturnsCfg(cfg *config.Config, p gold.ReturnParams) gold.ReturnParams {
	p.InceptionOverrides, p.ReturnsExclude, p.ReturnsHide, p.PolicyOverrides, p.TransferMatching = returnsCfgSettings(cfg)
	return p
}

// returnsCfgSettings builds the engine-side inception-override, exclusion,
// hide, policy-override, and transfer-matching settings from wealthdb.cfg.
// Shared by `returns` and the hidden `web-materialize` so a materialized
// partition carries exactly the settings a CLI run applies.
func returnsCfgSettings(cfg *config.Config) (*gold.InceptionOverrides, *gold.ReturnsExclude, *gold.ReturnsHide, map[string]gold.ReturnsPolicyOverride, *gold.TransferMatching) {
	var inceptionOv *gold.InceptionOverrides
	if cfg.InceptionOverrides != nil {
		s, p, a := cfg.InceptionOverrides.Epochs()
		inceptionOv = &gold.InceptionOverrides{Sources: s, Portfolios: p, Accounts: a}
	}
	var exclude *gold.ReturnsExclude
	if cfg.ReturnsExclude != nil {
		pf, ac := cfg.ReturnsExclude.Sets()
		exclude = &gold.ReturnsExclude{Portfolios: pf, Accounts: ac}
	}
	var hide *gold.ReturnsHide
	if cfg.ReturnsHide != nil {
		pf, ac := cfg.ReturnsHide.Sets()
		hide = &gold.ReturnsHide{Portfolios: pf, Accounts: ac}
	}
	var policyOv map[string]gold.ReturnsPolicyOverride
	if len(cfg.ReturnsPolicyOverrides) > 0 {
		policyOv = make(map[string]gold.ReturnsPolicyOverride, len(cfg.ReturnsPolicyOverrides))
		for id, ov := range cfg.ReturnsPolicyOverrides {
			if ov == nil {
				continue
			}
			var g gold.ReturnsPolicyOverride
			if r, ok := ov.Regime(); ok {
				g.FlowRegime = &r
			}
			if m, ok := ov.AccountsGrainMode(); ok {
				g.AccountsGrain = &m
			}
			policyOv[id] = g
		}
	}
	var matching *gold.TransferMatching
	if m, ledger := transferMatching(cfg); m != nil {
		// A malformed ledger overrides nothing here rather than taking down
		// a returns run: `load` and `categorize` both read the same file and
		// both fail loudly on it, so the error is reported where it can be
		// acted on, and this read cannot be the first to see it in practice.
		overrideRules, _ := gold.ParseTransferOverrideLedger(ledger)
		matching = &gold.TransferMatching{
			WindowDays: m.Window(), TolerancePct: m.Tolerance(), Rules: overrideRules,
		}
	}
	return inceptionOv, exclude, hide, policyOv, matching
}

// parseReturnsWindow defaults a bare invocation to since-inception → today
// (FROM=0 sentinel), otherwise reuses the transactions-style positional range.
func parseReturnsWindow(args []string, now time.Time) (int64, int64, error) {
	if len(args) == 0 {
		return 0, anchorToDay(now.UTC(), true).Unix(), nil
	}
	return parseDateRange(args, now)
}

// ---- column registry -----------------------------------------------------

// buildReturnColumnRegistry is the registry of the view at level. Its
// entity is an account at the accounts level, which -p shows as accountCol
// does, and a portfolio at the portfolios level, whose name -p redacts as
// portfolioNamePrivacy says and whose id as typedIDCol does.
// Elsewhere it is a source, or the whole.
func buildReturnColumnRegistry(outCcy, level string, kindOf func(string) string,
	nicknames accountNicknames) []columnSpec[gold.ReturnRow] {
	entity := columnSpec[gold.ReturnRow]{Name: "entity", Align: output.AlignLeft, Privacy: PrivacyAccountID,
		Extract: func(r gold.ReturnRow) string { return r.EntityLabel }}
	entityID := columnSpec[gold.ReturnRow]{Name: "entity_id", Align: output.AlignLeft, Privacy: PrivacyAccountID,
		Extract: func(r gold.ReturnRow) string { return r.EntityID }}
	switch level {
	case "accounts":
		ref := func(r gold.ReturnRow) accountRef {
			return accountRef{source: r.SilverSourceID, name: &r.EntityLabel,
				nickname: nicknames.of(r.SilverSourceID, r.EntityID), id: r.EntityID}
		}
		entity = accountCol(kindOf, ref)
		entity.Name = "entity"
		entityID = accountIDCol("entity_id", kindOf, ref)
	case "portfolios":
		entity = portfolioCol("entity", kindOf, func(r gold.ReturnRow) (string, string, *string) {
			return r.SilverSourceID, r.EntityID, &r.EntityLabel
		})
		entityID = typedIDCol("entity_id", kindOf, func(r gold.ReturnRow) (string, string) {
			return r.SilverSourceID, r.EntityID
		})
	}
	return []columnSpec[gold.ReturnRow]{
		{Name: "silver_source", Align: output.AlignLeft,
			Extract: func(r gold.ReturnRow) string { return r.SilverSourceID }},
		entity,
		entityID,
		{Name: "period", Align: output.AlignLeft,
			Extract: func(r gold.ReturnRow) string { return r.Period }},
		{Name: "start_date", Align: output.AlignLeft,
			Extract: func(r gold.ReturnRow) string { return formatEpochDay(r.StartDay) }},
		{Name: "end_date", Align: output.AlignLeft,
			Extract: func(r gold.ReturnRow) string { return formatEpochDay(r.EndDay) }},
		{Name: "start_value", Header: "start_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.ReturnRow) string { return formatCents(r.StartValue) }},
		{Name: "end_value", Header: "end_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.ReturnRow) string { return formatCents(r.EndValue) }},
		outCcyCol("net_flow", outCcy, func(r gold.ReturnRow) *string { return r.NetFlow }),
		{Name: "gain", Header: "gain_" + outCcy, Align: output.AlignRight, Privacy: PrivacyMoney,
			Extract: func(r gold.ReturnRow) string { return formatCents(r.Gain()) }},
		{Name: "twr", Header: "twr_pct", Align: output.AlignRight,
			Extract: func(r gold.ReturnRow) string { return formatPct(r.TWR) }},
		{Name: "twr_annualized", Header: "twr_ann_pct", Align: output.AlignRight,
			Extract: func(r gold.ReturnRow) string { return formatPct(r.TWRAnnualized) }},
		{Name: "mwr", Header: "mwr_pct", Align: output.AlignRight,
			Extract: func(r gold.ReturnRow) string { return formatPct(r.MWR) }},
		{Name: "mwr_annualized", Header: "mwr_ann_pct", Align: output.AlignRight,
			Extract: func(r gold.ReturnRow) string { return formatPct(r.MWRAnnualized) }},
		{Name: "quality", Align: output.AlignLeft,
			Extract: func(r gold.ReturnRow) string { return strings.Join(r.Quality, ";") }},
	}
}

// defaultReturnColumnsFor adapts the default column set to --method: show the
// twr column unless mwr-only, and the mwr column unless twr-only.
func defaultReturnColumnsFor(method string) []string {
	out := []string{"silver_source", "entity", "period", "start_value", "end_value", "net_flow", "gain"}
	if method != "mwr" {
		out = append(out, "twr")
	}
	if method != "twr" {
		out = append(out, "mwr")
	}
	return append(out, "quality")
}

// formatPct renders a return ratio as a percentage with two decimals, or "n/a"
// when the figure is undefined (the reason is in the quality column).
func formatPct(p *float64) string {
	if p == nil {
		return "n/a"
	}
	return formatPctOrBlank(p)
}

// formatPctOrBlank is formatPct for a ratio whose absence needs no
// reason, such as an unrealized gain on a holding with no cost basis:
// the cell stays empty.
func formatPctOrBlank(p *float64) string {
	if p == nil {
		return ""
	}
	pct := *p * 100
	return formatPercent(&pct)
}

func returnsUsage() string {
	registry := buildReturnColumnRegistry("CCY", "", nil, nil)
	return `usage: wealthdb returns <view> [FROM [TO]] [--method M] [--period P] [--annualize MODE]
                        [--netting on|off] [--inception full|strict] [-f FORMAT] [-C COLS] [-x CCY] [-p]

Time-weighted (TWR) and money-weighted (MWR/XIRR) returns. Grain is the
positional <view>; everything else is a flag. Returns use historic FX
and are net of fees and taxes paid (after-tax). The gain column is the
money behind the percentages: end value − start value − net flow.

Views (coarsest → finest):
  global       the whole tracked portfolio
  sources      one row per silver source
  portfolios   one row per portfolio (+ a per-source no-portfolio bucket)
  accounts     one row per account (exact — the headline; coarse views are best-effort)

Also:
  stated       a document's stated periods beside the engine's figures
               ('wealthdb returns stated -h')

Plumbing (cash accounts, deposit-bank sources, config returns_hide ids)
emits no rows of its own at any view, and neither does a portfolio or
source made only of it; its balances and flows still feed every
aggregate. The global view always includes everything.

Window (positional, optional; default: since first snapshot → today):
  YYYY / YYYY-MM / YYYY-MM-DD   that calendar period
  FROM TO                       explicit range; '-' is open-ended

Flags:
  --method M        twr (default) | mwr | both
  --period P        monthly | quarterly (default) | annual | total
                    (per-bucket TWR rows + a since-inception summary row; the
                    summary's cumulative TWR is chained over the entity's
                    actual snapshot days, NOT --period. total gives one row
                    per entity for the whole window)
  --annualize MODE  auto (default; only spans >= 1y) | always | never
  --netting on|off  net internal transfers at coarse grains (default on; the
                    accounts view is always exact and ignores this)
  --inception MODE  full (default; synthetic onboarding from earliest
                    constituent) | strict (latest constituent first-snapshot)
  -f, --format      table | csv | csv_plain | json
  -C, --columns     comma-separated names, 'default', 'all', or a +ADD,-REMOVE delta
  -x, --currency    output currency (default: config.default_currency)
  -p, --privacy     redact entity IDs and amounts; every name a person
                    typed prints as a pseudonym, such as manual-1, or as
                    a redacted ID
                    (returns % stay visible)

The quality column is load-bearing: n/a returns always carry a reason
(nonpositive_base, mwr_no_flows, dietz_degenerate, empty_bucket, nav_only,
staggered_inception, unmatched_transfers, …). Account-grain flow-complete
returns are exact; everything else is best-effort. Returns are NOT additive
across grains (global is a value identity over all accounts INCLUDING hidden
plumbing, not a return identity).

Available columns:
  ` + joinColumnNames(registry) + `

Default column set (adapts to --method; twr-only shown):
  ` + strings.Join(defaultReturnColumnsFor("twr"), ", ")
}
