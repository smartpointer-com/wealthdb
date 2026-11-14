package main

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

func init() {
	register("returns", routeReport(returnsFamily))
}

// returnsFamily are the views `wealthdb returns <view>` supports: the
// four reconciling grains, coarsest first, then two side views, which
// are no grain of them: `stated`, the documents' stated periods beside
// the accounts' figures, and `bridge`, why net worth moved. Per-position
// gains are the gains report's.
var returnsFamily = &family{
	name: "returns",
	views: []view{
		// global's silver_source is the whole portfolio's, not one source.
		{name: "global", build: buildReturns, principal: returnsPrincipal, header: returnsHeader,
			excludes: []string{"source"}},
		{name: "sources", build: buildReturns, principal: returnsPrincipal, header: returnsHeader},
		{name: "portfolios", build: buildReturns, principal: returnsPrincipal, header: returnsHeader},
		// On accounts the entity is the account.
		{name: "accounts", build: buildReturns, principal: returnsPrincipal, header: returnsHeader,
			keyAlias: map[string]string{"account": "entity"}},
		// The stated view carries no return: "value" is the engine's
		// ending value.
		{name: "stated", side: true, principal: fixedColumn("end_value"), usage: returnsStatedUsage,
			refuses:     statedRefusedFlags,
			ownCurrency: "each period in the currency its document states",
			build: func(cfg *config.Config, req request, _ *toolArgs) *report {
				return returnsStatedReport(req, cfg)
			}},
		// On the bridge, "value" is the net worth it ends on.
		{name: "bridge", side: true, principal: fixedColumn("end_net_worth"), usage: returnsBridgeUsage,
			refuses: bridgeRefusedFlags, checkWindow: checkBridgeWindow,
			header: func(req request) []string { return []string{"period=" + req.period} },
			build: func(cfg *config.Config, req request, _ *toolArgs) *report {
				return returnsBridgeReport(req, cfg)
			}},
	},
	options: []option{
		{name: "method", cli: []string{"method"}, choices: returnsMethods, def: [2]string{"twr", "both"}},
		{name: "period", cli: []string{"period"}, choices: returnsPeriods, def: [2]string{"quarterly", "total"}},
		{name: "annualize", cli: []string{"annualize"}, choices: returnsAnnualize, def: [2]string{"auto", "auto"}},
		{name: "netting", cli: []string{"netting"}, kind: optOnOff, choices: []string{"on", "off"},
			def: [2]string{"on", "true"}},
		{name: "inception", cli: []string{"inception"}, choices: returnsInception, def: [2]string{"full", "full"}},
		{name: "benchmark", cli: []string{"benchmark"}},
	},
	window:   sinceInception,
	currency: true,
	narrow:   "from/to or a filter",
	usage:    returnsUsage,
	settle: func(_ *family, in optionSource, req *request, _ *[]string) error {
		switch req.view {
		case "stated":
			// The document fixes the periods and the currency. The CLI
			// refuses the return flags as it parses them, then -x.
			if !in.cli() {
				return refuseOptions(in, "returns", "stated", append([]string{"currency"}, statedRefusedFlags...),
					" — the periods and the currency are the document's")
			}
			if in.has("currency") {
				return errs.Newf(errs.ExitUsage, "returns: stated takes no -x/--currency — "+
					"every amount is in the currency the document states")
			}
			return nil
		case "bridge":
			if !in.cli() {
				if err := refuseOptions(in, "returns", "bridge", bridgeRefusedFlags, ""); err != nil {
					return err
				}
			}
			var err error
			if req.period, err = choose(in, "returns", "period"); err != nil {
				return err
			}
			// The CLI checks the months as it reads the window
			// (view.checkWindow).
			if !in.cli() {
				return checkBridgeWindow(req.from, req.to, req.asOf)
			}
			return nil
		}
		for _, o := range []struct {
			name string
			to   *string
		}{{"method", &req.method}, {"period", &req.period}, {"annualize", &req.annualize},
			{"netting", nil}, {"inception", &req.inception}} {
			v, err := choose(in, "returns", o.name)
			if err != nil {
				return err
			}
			if o.to != nil {
				*o.to = v
			}
		}
		req.netting = in.on("netting")
		req.benchmark = in.str("benchmark")
		return checkReturnsBenchmark(req.view, req.benchmark)
	},
}

// buildReturns is a grain's report (returnsReport).
func buildReturns(cfg *config.Config, req request, _ *toolArgs) *report {
	return returnsReport(req, cfg)
}

// returnsPrincipal is a grain's main column: on returns, "value" means
// the return itself.
func returnsPrincipal(req request) string {
	if req.method == "mwr" {
		return "mwr"
	}
	return "twr"
}

// returnsHeader names a grain's options in force.
func returnsHeader(req request) []string {
	h := []string{"method=" + req.method, "period=" + req.period}
	if req.annualize != "auto" {
		h = append(h, "annualize="+req.annualize)
	}
	if !req.netting {
		h = append(h, "netting off")
	}
	if req.inception != "full" {
		h = append(h, "inception="+req.inception)
	}
	if req.benchmark != "" {
		h = append(h, "benchmark="+req.benchmark)
	}
	return h
}

// The choices of the returns options the CLI and the MCP tool share.
var (
	returnsMethods   = []string{"twr", "mwr", "both"}
	returnsPeriods   = []string{"monthly", "quarterly", "annual", "total"}
	returnsAnnualize = []string{"auto", "always", "never"}
	returnsInception = []string{"full", "strict"}
)

// returnsReport is one view of the returns family, the runner the CLI
// and the MCP server share: RunReturns with the config's overrides, so
// the two front-ends apply exactly the same settings.
func returnsReport(req request, cfg *config.Config) *report {
	kindOf, loadKinds := sourceKinds()
	nicknames := accountNicknames{}
	fetch := func(ctx context.Context, db *sql.DB) ([]gold.ReturnRow, error) {
		p := withReturnsCfg(cfg, gold.ReturnParams{
			Level: req.view, FromEpoch: req.from, ToEpoch: req.to, OutCcy: req.currency,
			Method: req.method, Period: req.period, Annualize: req.annualize,
			Netting: req.netting, Inception: req.inception,
		})
		if req.benchmark != "" {
			return gold.RunReturnsBenchmark(ctx, db, p, req.benchmark)
		}
		return gold.RunReturns(ctx, db, p)
	}
	loads := []func(context.Context, *sql.DB) error{loadKinds}
	// Only an account's entity reads its nickname.
	if req.view == "accounts" {
		loads = append(loads, nicknames.load)
	}
	defaults := defaultReturnColumnsFor(req.method)
	if req.benchmark != "" {
		defaults = benchmarkReturnColumnsFor(req.method)
	}
	return withLoads(newReport(buildReturnColumnRegistry(req.currency, req.view, kindOf, nicknames), defaults, fetch),
		loads...)
}

// checkReturnsBenchmark refuses a benchmark on a view other than global:
// a benchmark stands beside the whole portfolio, as on the dashboard.
func checkReturnsBenchmark(view, benchmark string) error {
	if benchmark != "" && view != "global" {
		return errs.Newf(errs.ExitUsage, "returns: --benchmark belongs to the global view")
	}
	return nil
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
	entity := privateCol("entity", PrivacyAccountID, func(r gold.ReturnRow) string { return r.EntityLabel })
	entityID := privateCol("entity_id", PrivacyAccountID, func(r gold.ReturnRow) string { return r.EntityID })
	account, portfolio := entityCols(kindOf, nicknames, func(r gold.ReturnRow) (string, string, string) {
		return r.SilverSourceID, r.EntityID, r.EntityLabel
	})
	switch level {
	case "accounts":
		entity, entityID = account[0], account[1]
	case "portfolios":
		entity, entityID = portfolio[0], portfolio[1]
	}
	return []columnSpec[gold.ReturnRow]{
		strCol("silver_source", func(r gold.ReturnRow) string { return r.SilverSourceID }),
		entity,
		entityID,
		asDate(strCol("period", func(r gold.ReturnRow) string { return r.Period })),
		asDate(strCol("start_date", func(r gold.ReturnRow) string { return formatEpochDay(r.StartDay) })),
		asDate(strCol("end_date", func(r gold.ReturnRow) string { return formatEpochDay(r.EndDay) })),
		{Name: "start_value", Header: "start_" + outCcy, Kind: MoneyColumn, Unit: OutputCurrency,
			Extract: func(r gold.ReturnRow) string { return formatCents(r.StartValue) }},
		{Name: "end_value", Header: "end_" + outCcy, Kind: MoneyColumn, Unit: OutputCurrency,
			Extract: func(r gold.ReturnRow) string { return formatCents(r.EndValue) }},
		outCcyCol("net_flow", outCcy, func(r gold.ReturnRow) *string { return r.NetFlow }),
		{Name: "gain", Header: "gain_" + outCcy, Kind: MoneyColumn, Unit: OutputCurrency,
			Extract: func(r gold.ReturnRow) string { return formatCents(r.Gain()) }},
		{Name: "twr", Header: "twr_pct", Kind: PercentColumn,
			Extract: func(r gold.ReturnRow) string { return formatPct(r.TWR) }},
		{Name: "twr_annualized", Header: "twr_ann_pct", Kind: PercentColumn,
			Extract: func(r gold.ReturnRow) string { return formatPct(r.TWRAnnualized) }},
		{Name: "mwr", Header: "mwr_pct", Kind: PercentColumn,
			Extract: func(r gold.ReturnRow) string { return formatPct(r.MWR) }},
		{Name: "mwr_annualized", Header: "mwr_ann_pct", Kind: PercentColumn,
			Extract: func(r gold.ReturnRow) string { return formatPct(r.MWRAnnualized) }},
		{Name: "benchmark_twr", Header: "benchmark_twr_pct", Kind: PercentColumn,
			Extract: benchmarkFigure(func(b *gold.BenchmarkRow) *float64 { return b.TWR })},
		{Name: "compared_twr", Header: "compared_twr_pct", Kind: PercentColumn,
			Extract: benchmarkFigure(func(b *gold.BenchmarkRow) *float64 { return b.PortfolioTWR })},
		{Name: "excess", Header: "excess_pct", Kind: PercentColumn,
			Extract: benchmarkFigure(func(b *gold.BenchmarkRow) *float64 { return b.Excess })},
		{Name: "benchmark_mwr", Header: "benchmark_mwr_pct", Kind: PercentColumn,
			Extract: benchmarkFigure(func(b *gold.BenchmarkRow) *float64 { return b.MWR })},
		{Name: "compared_mwr", Header: "compared_mwr_pct", Kind: PercentColumn,
			Extract: benchmarkFigure(func(b *gold.BenchmarkRow) *float64 { return b.PortfolioMWR })},
		asDate(strCol("compared_from", benchmarkDay(func(b *gold.BenchmarkRow) int64 { return b.StartDay }))),
		asDate(strCol("compared_to", benchmarkDay(func(b *gold.BenchmarkRow) int64 { return b.EndDay }))),
		strCol("benchmark_quality", func(r gold.ReturnRow) string {
			if r.Benchmark == nil {
				return ""
			}
			return strings.Join(r.Benchmark.Quality, ";")
		}),
		strCol("quality", func(r gold.ReturnRow) string { return strings.Join(r.Quality, ";") }),
	}
}

// benchmarkFigure is a column's extract of a benchmark's figure: n/a
// where the row has none, and benchmark_quality says why.
func benchmarkFigure(get func(*gold.BenchmarkRow) *float64) func(gold.ReturnRow) string {
	return func(r gold.ReturnRow) string {
		if r.Benchmark == nil {
			return "n/a"
		}
		return formatPct(get(r.Benchmark))
	}
}

// benchmarkDay is a column's extract of a day of the days compared,
// blank where the row compares none.
func benchmarkDay(get func(*gold.BenchmarkRow) int64) func(gold.ReturnRow) string {
	return func(r gold.ReturnRow) string {
		if r.Benchmark == nil || r.Benchmark.EndDay == 0 {
			return ""
		}
		return formatEpochDay(get(r.Benchmark))
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

// benchmarkReturnColumnsFor is the default column set beside a benchmark:
// the household's figures over the days compared, the benchmark's and
// the excess, on the rows' own window columns.
func benchmarkReturnColumnsFor(method string) []string {
	out := []string{"period"}
	if method != "mwr" {
		out = append(out, "twr", "compared_twr", "benchmark_twr", "excess")
	}
	if method != "twr" {
		out = append(out, "mwr", "compared_mwr", "benchmark_mwr")
	}
	return append(out, "compared_from", "compared_to", "benchmark_quality", "quality")
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
                        [--netting on|off] [--inception full|strict] [--benchmark NAME]
                        [-f FORMAT] [-C COLS] [-F COND]... [-x CCY] [-p]

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
  bridge       why net worth moved: saved, onboarded and gained
               ('wealthdb returns bridge -h')

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
  --benchmark NAME  the global view only: the benchmark beside each row, over
                    the days both sides have. compared_twr is the portfolio
                    over those days, excess is compared_twr less benchmark_twr,
                    in percentage points. 'wealthdb help benchmarks' lists them
  -f, --format      table | csv | csv_plain | json
  -C, --columns     comma-separated names, 'default', 'all', or a +ADD,-REMOVE delta
  -F, --filter COND keep the rows where COND holds; repeatable;
                    wealthdb help filter lists the operators
  -x, --currency    output currency (default: config.default_currency)
  -p, --privacy     redact entity IDs and amounts; every name a person
                    typed about an account prints as a pseudonym, such
                    as manual-1, or as a redacted ID
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
