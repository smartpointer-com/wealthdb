package main

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

func init() {
	register("returns", routeReport(returnsFamily))
}

// returnsOptions are the returns family's options.
var returnsOptions = []option{
	{name: "method", cli: []string{"method"}, choices: returnsMethods, def: [2]string{"twr", "both"}},
	{name: "period", cli: []string{"period"}, choices: returnsPeriods, def: [2]string{"quarterly", "total"}},
	{name: "annualize", cli: []string{"annualize"}, choices: returnsAnnualize, def: [2]string{"auto", "auto"}},
	{name: "netting", cli: []string{"netting"}, kind: optOnOff, choices: []string{"on", "off"},
		def: [2]string{"on", "true"}},
	{name: "inception", cli: []string{"inception"}, choices: returnsInception, def: [2]string{"full", "full"}},
	{name: "benchmark", cli: []string{"benchmark"}},
	// The private view's as-of day; no other view takes it.
	{name: "as_of", cli: []string{"d", "as-of"}, only: true},
}

// returnsOptionNames are the name of every returns option the grains
// take: what the stated and private views refuse. The stated view's
// document fixes the periods and computes no return of its own (-x is
// refused beside them); the private view reads every leg since each
// holding's first call, and its return is the money-weighted one.
var returnsOptionNames = func() []string {
	var names []string
	for _, o := range returnsOptions {
		if !o.only {
			names = append(names, o.name)
		}
	}
	return names
}()

// returnsFamily are the views `wealthdb returns <view>` supports: the
// four reconciling grains, coarsest first, then three side views, which
// are no grain of them: `stated`, the documents' stated periods beside
// the accounts' figures, `bridge`, why net worth moved, and `private`,
// each private holding's calls, distributions and mark. Per-position
// gains are the gains report's.
var returnsFamily = &family{
	name: "returns",
	views: append(returnsGrainViews(),
		// The stated view carries no return: "value" is the engine's
		// ending value.
		view{name: "stated", side: true, principal: fixedColumn("end_value"), usage: returnsStatedUsage,
			refuses:     returnsOptionNames,
			refuseWhy:   " — the periods and the currency are the document's",
			ownCurrency: "each period in the currency its document states",
			build: func(cfg *config.Config, req request) *report {
				return returnsStatedReport(req, cfg)
			}},
		// On the bridge, "value" is the net worth it ends on.
		view{name: "bridge", side: true, principal: fixedColumn("end_net_worth"), usage: returnsBridgeUsage,
			refuses: bridgeRefusedFlags, checkWindow: checkBridgeWindow,
			header: periodHeader,
			build: func(cfg *config.Config, req request) *report {
				return returnsBridgeReport(req, cfg)
			}},
		// On the private view, "value" is what each holding is marked at.
		view{name: "private", side: true, principal: fixedColumn("nav"), usage: returnsPrivateUsage,
			refuses: returnsOptionNames, checkWindow: checkPrivateWindow, takes: []string{"as_of"},
			header: privateHeader,
			build: func(_ *config.Config, req request) *report {
				return returnsPrivateReport(req)
			}},
	),
	options:  returnsOptions,
	window:   sinceInception,
	currency: true,
	narrow:   "from/to or a filter",
	usage:    returnsUsage,
	settle: func(_ *family, in optionSource, req *request, _ *[]string) error {
		switch req.view {
		case "stated":
			// The document fixes the periods and the currency. Both front
			// ends refuse the return options before the settle, MCP the
			// currency with them (refuseMCP); the CLI refuses -x here.
			if in.cli() && in.has("currency") {
				return errs.Newf(errs.ExitUsage, "returns: stated takes no -x/--currency — "+
					"every amount is in the currency the document states")
			}
			return nil
		case "bridge":
			var err error
			req.period, err = choose(in, "returns", "period")
			return err
		case "private":
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
func buildReturns(cfg *config.Config, req request) *report {
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

// returnsGrainViews are the views of the engine's grains, coarsest first.
func returnsGrainViews() []view {
	var out []view
	for _, g := range slices.Backward(gold.ReturnsGrains()) {
		v := view{name: g.Name, build: buildReturns, principal: returnsPrincipal, header: returnsHeader}
		switch {
		case g.Whole:
			// The whole's silver_source is the whole portfolio's, not one source.
			v.excludes = []string{"source"}
		case g.Entity == "account":
			// On accounts the entity is the account.
			v.keyAlias = map[string]string{"account": "entity"}
		}
		out = append(out, v)
	}
	return out
}

// returnsEntity is what an entity of the grain named level is: "account",
// "portfolio", or "" for a source and the whole.
func returnsEntity(level string) string {
	for _, g := range gold.ReturnsGrains() {
		if g.Name == level {
			return g.Entity
		}
	}
	return ""
}

// returnsWhole reports whether the view is the returns grain of the whole
// portfolio.
func returnsWhole(level string) bool {
	for _, g := range gold.ReturnsGrains() {
		if g.Name == level {
			return g.Whole
		}
	}
	return false
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
	if returnsEntity(req.view) == "account" {
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
	if benchmark != "" && !returnsWhole(view) {
		return errs.Newf(errs.ExitUsage, "returns: --benchmark belongs to the global view")
	}
	return nil
}

// withReturnsCfg is p with the config's returns settings
// (returnsCfgSettings), so every report runner applies the same ones.
func withReturnsCfg(cfg *config.Config, p gold.ReturnParams) gold.ReturnParams {
	p.ReturnsSettings = returnsCfgSettings(cfg)
	return p
}

// returnsCfgSettings builds the engine-side inception-override, exclusion,
// hide, policy-override, and transfer-matching settings from wealthdb.cfg.
// Shared by `returns` and the hidden `web-materialize` so a materialized
// partition carries exactly the settings a CLI run applies.
func returnsCfgSettings(cfg *config.Config) gold.ReturnsSettings {
	var s gold.ReturnsSettings
	if cfg.InceptionOverrides != nil {
		src, pf, ac := cfg.InceptionOverrides.Epochs()
		s.InceptionOverrides = &gold.InceptionOverrides{Sources: src, Portfolios: pf, Accounts: ac}
	}
	if cfg.ReturnsExclude != nil {
		pf, ac := cfg.ReturnsExclude.Sets()
		s.ReturnsExclude = &gold.ReturnsExclude{Portfolios: pf, Accounts: ac}
	}
	if cfg.ReturnsHide != nil {
		pf, ac := cfg.ReturnsHide.Sets()
		s.ReturnsHide = &gold.ReturnsHide{Portfolios: pf, Accounts: ac}
	}
	if len(cfg.ReturnsPolicyOverrides) > 0 {
		policyOv := make(map[string]gold.ReturnsPolicyOverride, len(cfg.ReturnsPolicyOverrides))
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
		s.PolicyOverrides = policyOv
	}
	// The pairs come from the links the load stored, which applied the
	// override ledger when it paired, so returns reads no file of its own.
	m := cfg.ReturnsTransferMatching
	s.TransferMatching = m != nil && m.Enabled
	return s
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
	switch returnsEntity(level) {
	case "account":
		entity, entityID = account[0], account[1]
	case "portfolio":
		entity, entityID = portfolio[0], portfolio[1]
	}
	return []columnSpec[gold.ReturnRow]{
		strCol("silver_source", func(r gold.ReturnRow) string { return r.SilverSourceID }),
		entity,
		entityID,
		asDate(strCol("period", func(r gold.ReturnRow) string { return r.Period })),
		asDate(strCol("start_date", func(r gold.ReturnRow) string { return formatEpochDay(r.StartDay) })),
		asDate(strCol("end_date", func(r gold.ReturnRow) string { return formatEpochDay(r.EndDay) })),
		outCcyColAs("start_value", "start_"+outCcy, outCcy, func(r gold.ReturnRow) *string { return r.StartValue }),
		outCcyColAs("end_value", "end_"+outCcy, outCcy, func(r gold.ReturnRow) *string { return r.EndValue }),
		outCcyCol("net_flow", outCcy, func(r gold.ReturnRow) *string { return r.NetFlow }),
		outCcyColAs("gain", "gain_"+outCcy, outCcy, func(r gold.ReturnRow) *string { return r.Gain() }),
		ratioCol("twr", func(r gold.ReturnRow) *float64 { return r.TWR }),
		percentCol("twr_annualized", "twr_ann_pct", func(r gold.ReturnRow) string { return formatPct(r.TWRAnnualized) }),
		ratioCol("mwr", func(r gold.ReturnRow) *float64 { return r.MWR }),
		percentCol("mwr_annualized", "mwr_ann_pct", func(r gold.ReturnRow) string { return formatPct(r.MWRAnnualized) }),
		percentCol("benchmark_twr", "benchmark_twr_pct", benchmarkFigure(func(b *gold.BenchmarkRow) *float64 { return b.TWR })),
		percentCol("compared_twr", "compared_twr_pct",
			benchmarkFigure(func(b *gold.BenchmarkRow) *float64 { return b.PortfolioTWR })),
		percentCol("excess", "excess_pct", benchmarkFigure(func(b *gold.BenchmarkRow) *float64 { return b.Excess })),
		percentCol("benchmark_mwr", "benchmark_mwr_pct", benchmarkFigure(func(b *gold.BenchmarkRow) *float64 { return b.MWR })),
		percentCol("compared_mwr", "compared_mwr_pct",
			benchmarkFigure(func(b *gold.BenchmarkRow) *float64 { return b.PortfolioMWR })),
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
  private      each private fund, SPV, private stock or note: paid in,
               commitment, distributed, NAV, DPI, TVPI and IRR, as of
               a day (-d)
               ('wealthdb returns private -h')

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
(nonpositive_base, mwr_no_flows, dietz_degenerate, loss_beyond_total,
empty_bucket, nav_only, staggered_inception, unmatched_transfers, …).
Account-grain flow-complete returns are exact; everything else is
best-effort. Returns are NOT additive across grains (global is a value
identity over all accounts INCLUDING hidden plumbing, not a return
identity).

An account joins at its first valuation, which books its value as
capital, less what moved in from accounts already counted. A zero
first value is no valuation where the account's legs up to its first
nonzero value move money in on net. That value is then the account's
debut, and every leg on or before it funds it.

Available columns:
  ` + joinColumnNames(registry) + `

Default column set (adapts to --method; twr-only shown):
  ` + strings.Join(defaultReturnColumnsFor("twr"), ", ")
}
