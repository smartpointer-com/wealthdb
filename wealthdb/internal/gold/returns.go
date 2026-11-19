package gold

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// acctKey is the in-memory map key for an account: source id + a NUL separator
// + account id (NUL can't appear in an id, so the join is unambiguous). Reused
// for every key of the same shape: the source+portfolio grouping key, the
// change accumulator's dimension keys, NamedAccount.
func acctKey(a, b string) string { return a + "\x00" + b }

// ReturnRow is one row of `wealthdb returns`: an entity's return over one
// reporting bucket (or the since-inception summary). Money columns are decimal
// strings (nil when unresolved); rate columns are nil when n/a, with the reason
// in Quality.
type ReturnRow struct {
	SilverSourceID string
	EntityID       string // account / portfolio / source id; "" for global and per-source orphan bucket
	EntityLabel    string // display label (falls back to the id)
	Period         string // "2025-Q1" / "2025-03" / "2025" / "total"
	IsSummary      bool   // the since-inception cumulative row

	StartDay, EndDay int64 // epoch days

	StartValue *string // output-currency boundary values
	EndValue   *string
	NetFlow    *string // net external flow over the row's span (outCcy)

	TWR           *float64
	TWRAnnualized *float64
	MWR           *float64
	MWRAnnualized *float64

	Quality []string

	// Benchmark is the benchmark beside the row, where a run asks for one
	// (RunReturnsBenchmark).
	Benchmark *BenchmarkRow
}

// ReturnParams configures a returns run.
type ReturnParams struct {
	Level     string // accounts | portfolios | sources | global
	FromEpoch int64  // window start, Unix seconds; <=0 means "since inception" (clamp per entity)
	ToEpoch   int64  // window end, Unix seconds
	OutCcy    string
	Method    string // twr | mwr | both
	Period    string // monthly | quarterly | annual | total
	Annualize string // auto | always | never
	Netting   bool   // net internal transfers at coarse grains (heuristic netter + stored pairs)
	Inception string // full | strict
	ReturnsSettings
}

// ReturnsSettings are wealthdb.cfg's returns settings, which every run of
// the engine applies alike: a CLI report, the MCP tool and web-materialize.
type ReturnsSettings struct {
	// InceptionOverrides carries user-configured per-entity inception
	// floors from wealthdb.cfg. Nil ⇒ every entity keeps its data-derived
	// inception (the default). See entityWindow.
	InceptionOverrides *InceptionOverrides
	// ReturnsExclude omits accounts/portfolios from higher-grain aggregates
	// (sources, global). Nil ⇒ nothing excluded. See groupAccounts.
	ReturnsExclude *ReturnsExclude
	// ReturnsHide suppresses accounts'/portfolios' own display rows at every
	// grain while keeping their values and flows in every aggregate — the
	// display mirror of ReturnsExclude, which removes an entity from the
	// coarse-grain math instead. Composes with the cash accounts and the
	// policy-side AccountsGrainHidden mode, which are hidden regardless.
	// Nil ⇒ nothing hidden beyond those. See entityHidden.
	ReturnsHide *ReturnsHide
	// PolicyOverrides adjusts per-source ReturnsPolicies from wealthdb.cfg's
	// returns_policy_overrides block, keyed by silver_source_id. Nil/absent ⇒
	// registered policies apply unchanged. See newAccountData.
	PolicyOverrides map[string]ReturnsPolicyOverride
	// TransferMatching is wealthdb.cfg's returns_transfer_matching
	// switch: returns nets the transfer pairs the load's enrichment pass
	// stored in transfer_links, the override ledger applied. A pair binds
	// in the entities that hold both legs in the window (storedPairs),
	// while finer grains keep counting each leg as the real boundary flow
	// it is for them. False (the default) ⇒ no link is read. See
	// bindStoredLinks and docs/DESIGN.md §5.8.
	TransferMatching bool
}

// defaultRun is the run a bare `wealthdb returns <level> --period <period>
// --method both -x <ccy>` makes over [from, to]: annualized where the window
// is long enough, netted, from the full inception, with these settings.
func (s ReturnsSettings) defaultRun(level, period, ccy string, from, to int64) ReturnParams {
	return ReturnParams{Level: level, FromEpoch: from, ToEpoch: to, OutCcy: ccy,
		Method: "both", Period: period, Annualize: "auto", Netting: true, Inception: "full",
		ReturnsSettings: s}
}

// ReturnsHide holds the source-keyed membership sets of accounts and
// portfolios whose OWN rows are suppressed at every grain while their values
// and flows stay inside every aggregate, built from wealthdb.cfg's
// returns_hide block. Nil ⇒ nothing hidden config-side.
type ReturnsHide struct {
	Portfolios map[string]map[string]bool // source_id -> portfolio_external_id -> true
	Accounts   map[string]map[string]bool // source_id -> account_external_id -> true
}

// hiddenConstituent reports whether one account's own display presence is
// suppressed: it is a cash account, its source policy declares the accounts
// grain hidden (AccountsGrainHidden plumbing), it is listed in returns_hide,
// or it belongs to a listed portfolio. A nil receiver hides nothing
// config-side.
//
// A cash account is plumbing whatever source it comes from: money passes
// through it between other holdings, so a return of its own is noise — n/a
// on a drained base, or a chained −100% on one drained and refilled.
func (h *ReturnsHide) hiddenConstituent(a *accountData) bool {
	if a.kind == string(canonical.AccountKindCash) ||
		a.rpolicy.AccountsGrain == returns.AccountsGrainHidden {
		return true
	}
	if h == nil {
		return false
	}
	return h.Accounts[a.src][a.acct] || h.Portfolios[a.src][a.portfolio]
}

// entityHidden reports whether a computed entity emits no rows: every
// constituent is display-hidden and the grain is not the whole — hidden
// plumbing still aggregates, and the global row always shows it doing so.
func entityHidden(g *grain, members []*accountData, h *ReturnsHide) bool {
	if g.whole || len(members) == 0 {
		return false
	}
	for _, a := range members {
		if !h.hiddenConstituent(a) {
			return false
		}
	}
	return true
}

// ReturnsPolicyOverride carries one source's config-side policy adjustments
// (wealthdb.cfg `returns_policy_overrides`, translated by the CLI). Nil
// pointer fields keep the registered policy's values. A FlowRegime override
// replaces the source's flow completeness (returns.NewFlowPolicy); it also
// marks the policy Known — an
// explicitly declared classification is not an unknown-adapter condition.
type ReturnsPolicyOverride struct {
	FlowRegime    *returns.Regime
	AccountsGrain *returns.AccountsGrainMode
}

// ReturnsExclude holds the source-keyed membership sets of accounts and
// portfolios to omit from higher-grain return aggregates, built from
// wealthdb.cfg's returns_exclude block. Nil ⇒ nothing excluded.
type ReturnsExclude struct {
	Portfolios map[string]map[string]bool // source_id -> portfolio_external_id -> true
	Accounts   map[string]map[string]bool // source_id -> account_external_id -> true
}

// excludes reports whether e omits an account from its group at the grain
// g. An account is never excluded from its OWN grain: the accounts grain
// always shows every account, and the portfolios grain still shows an
// excluded PORTFOLIO's own row (only an excluded ACCOUNT drops out of its
// portfolio there). At the sources and global grains, both an excluded
// account and any account of an excluded portfolio drop out. A nil e
// excludes nothing.
func (g *grain) excludes(e *ReturnsExclude, src, portfolio, acct string) bool {
	if e == nil {
		return false
	}
	return (g.excludesAccounts && e.Accounts[src][acct]) || (g.excludesPortfolios && e.Portfolios[src][portfolio])
}

// InceptionOverrides carries user-configured per-entity inception floors
// (Unix seconds, UTC midnight), built from wealthdb.cfg's
// inception_overrides block. When an entity is computed the floor is
// resolved most-specific-first; it can only move an anchor later, never
// earlier. Nil ⇒ no overrides.
type InceptionOverrides struct {
	Sources    map[string]int64            // source_id -> epoch
	Portfolios map[string]map[string]int64 // source_id -> portfolio_external_id -> epoch
	Accounts   map[string]map[string]int64 // source_id -> account_external_id -> epoch
}

// resolve returns the configured inception floor (epoch seconds) for an
// entity at a grain, trying the most specific key first: the accounts grain
// tries the account, then its portfolio, then the source; the portfolios
// grain tries the portfolio, then the source; the sources grain tries the
// source. The global grain is never anchored. (0,false) ⇒ no override. A nil
// receiver always returns (0,false), so the default path is untouched.
func (o *InceptionOverrides) resolve(level, src, portfolio, acct string) (int64, bool) {
	if o == nil {
		return 0, false
	}
	for _, scope := range grainNamed(level).inception {
		var d int64
		var ok bool
		switch scope {
		case scopeAccount:
			d, ok = o.Accounts[src][acct]
		case scopePortfolio:
			if portfolio != "" {
				d, ok = o.Portfolios[src][portfolio]
			}
		case scopeSource:
			d, ok = o.Sources[src]
		}
		if ok {
			return d, true
		}
	}
	return 0, false
}

// returnsDataset is the loaded, currency-specific input to computeReturns: the
// per-account daily value spine and external flows in one output currency, plus
// the derived globalMax and the (currency-independent) FX bounds. Loading is
// the expensive part — several DuckDB scans — so a dataset is built once and
// reused across every (grain, period) computation for its currency; see
// MaterializeReturns, which loads every reporting currency in a single pass.
type returnsDataset struct {
	accts     map[string]*accountData
	holdings  *holdingsSpine
	fx        fxBounds
	globalMax int64 // spine's latest emitted day across all accounts (≈ today)

	// twins loads the residueCurrency dataset and links each account to its
	// twin there, once, for a dataset in another currency (residueEnds).
	twins func()
}

// linkResidueTwins links each account of ds to the same account in dec, the
// residueCurrency dataset.
func linkResidueTwins(ds, dec *returnsDataset) {
	for k, a := range ds.accts {
		a.twin = dec.accts[k]
	}
}

// ensureTwins links ds to its residueCurrency twins when it has a loader for
// them. A load that fails leaves them unlinked, and the reporting currency
// then decides the residue alone.
func (ds *returnsDataset) ensureTwins() {
	if ds.twins != nil {
		ds.twins()
	}
}

// holdingsSpine is what the value spine holds beyond the returns engine's
// accounts: the holdings side of the same rows, which the net worth bridge
// and the fee view read. The returns computation never reads it.
type holdingsSpine struct {
	cards    map[string]*accountData // each card's value series, keyed as accts
	netWorth map[int64]float64       // the sum of every account's value by day
}

func newHoldingsSpine() *holdingsSpine {
	return &holdingsSpine{cards: map[string]*accountData{}, netWorth: map[int64]float64{}}
}

// loadReturnsDataset loads one currency's dataset (loadReturnsDatasets).
func loadReturnsDataset(ctx context.Context, db *sql.DB, outCcy string, fx fxBounds, s ReturnsSettings) (*returnsDataset, error) {
	out, err := loadReturnsDatasets(ctx, db, oneCurrency(outCcy), fx, s)
	if err != nil {
		return nil, err
	}
	return out[outCcy], nil
}

// loadRunDataset loads the FX bounds and the dataset of p's output
// currency under p's returns settings: the prologue of every Run* entry
// point.
func loadRunDataset(ctx context.Context, db *sql.DB, p ReturnParams) (*returnsDataset, error) {
	fx, err := loadFxBounds(ctx, db)
	if err != nil {
		return nil, err
	}
	return loadReturnsDataset(ctx, db, p.OutCcy, fx, p.ReturnsSettings)
}

// loadReturnsDatasets loads a dataset per currency of src: the per-account
// value spine and the external flows, both converted to the currency, then
// the derived globalMax and droppedNonzero. The spine and the flows are each
// one scan for every currency; snapshot days, source kinds and portfolio
// names are read once and shared, and fx is currency-independent. Of the
// settings, only the policy overrides and transfer matching shape a
// dataset. A dataset links each account to its twin in residueCurrency: in
// the same load where it holds that currency, else in one loaded once when
// first needed (ensureTwins).
func loadReturnsDatasets(ctx context.Context, db *sql.DB, src spineSource, fx fxBounds, s ReturnsSettings) (map[string]*returnsDataset, error) {
	byCcy, holdings, err := loadSpine(ctx, db, src, s.PolicyOverrides)
	if err != nil {
		return nil, err
	}
	txns, err := loadFlowTxns(ctx, db, src)
	if err != nil {
		return nil, err
	}
	idents, err := loadInstrumentFacts(ctx, db)
	if err != nil {
		return nil, err
	}
	helds, err := heldRefs(ctx, db, txns, idents)
	if err != nil {
		return nil, err
	}
	// A link binds where both its legs are flows of a currency's dataset,
	// and the attached flows can differ by currency where FX is unresolved.
	flows := map[string]attachedFlows{}
	for k, t := range txns {
		held := helds[k]
		for i, ccy := range src.currencies {
			attached := attachOneFlow(byCcy[ccy], fx, ccy, t.src, t.acct, canonical.TxKind(t.kind),
				t.occurredAt, t.txID, t.ccy, t.values[i], t.returnsInternal, held)
			if attached && s.TransferMatching {
				if flows[ccy] == nil {
					flows[ccy] = attachedFlows{}
				}
				flows[ccy][txRef{t.src, t.txID}] = attachedFlow{acct: t.acct, day: EpochDay(t.occurredAt)}
			}
		}
	}
	var links []storedTransferLink
	if s.TransferMatching {
		if links, err = loadStoredTransferLinks(ctx, db); err != nil {
			return nil, err
		}
	}
	if err := loadDebutHoldings(ctx, db, idents, nameHoldings(byCcy), byCcy); err != nil {
		return nil, err
	}
	out := make(map[string]*returnsDataset, len(src.currencies))
	for _, ccy := range src.currencies {
		bindStoredLinks(links, flows[ccy], byCcy[ccy])
		ds := &returnsDataset{accts: byCcy[ccy], holdings: holdings[ccy], fx: fx}
		ds.finalize()
		out[ccy] = ds
	}
	dec := out[residueCurrency]
	for ccy, ds := range out {
		switch {
		case ccy == residueCurrency:
		case dec != nil:
			linkResidueTwins(ds, dec)
		default:
			ds.twins = sync.OnceFunc(func() {
				if dec, err := loadReturnsDataset(ctx, db, residueCurrency, fx, s); err == nil {
					linkResidueTwins(ds, dec)
				}
			})
		}
	}
	return out, nil
}

// finalize derives globalMax and flags accounts that dropped out of a later
// same-source snapshot while still holding value. Both depend only on the
// loaded series, not on grain/period, so they are computed once per dataset.
func (ds *returnsDataset) finalize() {
	// The spine's latest emitted day across all accounts (≈ today). An account
	// whose own series ends before this was ended there by the history's
	// ending (DESIGN.md §10.7). A run that merely did not cover it carries it
	// forward instead. Its value is 0 thereafter, matching the macro, and an
	// account that dropped while still holding value is flagged.
	var globalMax int64
	for _, a := range ds.accts {
		if d := a.lastDay(); d > globalMax {
			globalMax = d
		}
	}
	cache := &residueCache{m: map[string]map[int64]bool{}}
	for _, a := range ds.accts {
		if a.lastDay() < globalMax && math.Abs(a.lastVal()) > valueTol {
			a.droppedNonzero = true
		}
		a.cacheTail()
		a.residue = cache
	}
	ds.globalMax = globalMax
}

// computeReturns runs the pure in-memory returns computation for one
// (grain, period) over a loaded dataset — no database access, so one dataset
// can drive every grain and period for its currency. p.OutCcy must match the
// dataset's currency (it steers the pre-FX-history flag). All grains are built
// from the per-account value spine aggregated here, so the staggered-inception
// synthetic-onboarding mechanism applies uniformly.
func computeReturns(ds *returnsDataset, p ReturnParams) []ReturnRow {
	// Never value past the latest available data (also guards a future ToEpoch).
	toDay := EpochDay(p.ToEpoch)
	if toDay > ds.globalMax {
		toDay = ds.globalMax
	}

	ds.ensureTwins()
	g := grainNamed(p.Level)
	var out []ReturnRow
	for _, e := range ds.entities(g, p.ReturnsExclude, p.ReturnsHide) {
		// Mortgage / liability accounts: excluded from coarse rollups; on the
		// accounts grain they surface as their own n/a + nonpositive_base line.
		if g.single {
			for _, m := range e.liabilities {
				out = append(out, liabilityRow(m, toDay))
			}
		}
		if len(e.assets) == 0 {
			continue
		}
		var rows []ReturnRow
		if r := e.run(p, toDay); r != nil {
			rows = r.rows(ds.fx)
		}
		// AccountsGrainBlanked: per-account (accounts-grain) rows a source's
		// policy declares meaningless keep their start/end values, with
		// TWR/MWR blanked to n/a and flagged. Gate STRICTLY on the group's
		// per-constituent rpolicy so ONLY that source's account rows change;
		// the portfolios/sources/global grains are NEVER gated (they aggregate
		// coherent units, which ARE valid). One row per account is still
		// emitted.
		if g.single && accountsGrainBlanked(e.assets) {
			for i := range rows {
				rows[i].TWR, rows[i].TWRAnnualized = nil, nil
				rows[i].MWR, rows[i].MWRAnnualized = nil, nil
				rows[i].Quality = dedupeStrings(append(rows[i].Quality, "accounts_grain_meaningless"))
			}
		}
		out = append(out, rows...)
	}
	return out
}

// RunReturns computes returns at the requested grain: load the currency's
// dataset, then compute. Callers that need several grains/periods/currencies of
// the same data load once (loadReturnsDatasets) and
// call computeReturns directly — see MaterializeReturns.
func RunReturns(ctx context.Context, db *sql.DB, p ReturnParams) ([]ReturnRow, error) {
	ds, err := loadRunDataset(ctx, db, p)
	if err != nil {
		return nil, err
	}
	return computeReturns(ds, p), nil
}

// accountsGrainBlanked reports whether the group's constituent policy blanks
// the per-account (accounts) grain (AccountsGrainBlanked). At the accounts
// grain every constituent of a group shares one source (groupAccounts keys on
// src), so the whole group carries one rpolicy; reading the first constituent
// is exact and source-scoped. The default mode leaves every other source
// untouched.
func accountsGrainBlanked(assets []*accountData) bool {
	return len(assets) > 0 && assets[0].rpolicy.AccountsGrain == returns.AccountsGrainBlanked
}

// fxBounds holds the earliest FX-rate day per currency, so the engine can flag
// conversions that fall back to fx_daily's day-0 floor rate (derived/10_fx.sql).
type fxBounds struct {
	earliest map[string]int64 // currency → earliest rate day; absent ⇒ no rate at all
}

// clampedBefore reports whether converting `ccy` to outCcy on `day` falls before
// the earliest real rate for that currency (so it used the day-0 clamp). Same
// currency, or no known rate at all, is not a clamp.
func (f fxBounds) clampedBefore(ccy, outCcy string, day int64) bool {
	if ccy == outCcy {
		return false
	}
	e, ok := f.earliest[ccy]
	return ok && day < e
}

func loadFxBounds(ctx context.Context, db *sql.DB) (fxBounds, error) {
	f := fxBounds{earliest: map[string]int64{}}
	rows, err := db.QueryContext(ctx, `
		SELECT ccy, MIN(day) FROM (
			SELECT base_currency  AS ccy, snapshot_at // 86400 AS day FROM fx_rates
			UNION ALL
			SELECT quote_currency AS ccy, snapshot_at // 86400 AS day FROM fx_rates)
		GROUP BY ccy`)
	if err != nil {
		return f, fmt.Errorf("loadFxBounds: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ccy string
		var day int64
		if err := rows.Scan(&ccy, &day); err != nil {
			return f, err
		}
		f.earliest[ccy] = day
	}
	return f, rows.Err()
}

// ---- data loading --------------------------------------------------------

type dayVal struct {
	day int64
	val float64
}

type accountData struct {
	src, acct     string
	kind          string
	srcKind       string // the source's silver_kind
	portfolio     string // portfolio_external_id ("" when none)
	portfolioName string // resolved portfolio display name ("" when none/unresolved)
	label         string
	baseCurrency  string // account base currency ("" when unknown)

	series   []dayVal // carry-forward value per emitted day, ascending
	snapDays []int64  // distinct real snapshot days, ascending

	rpolicy        returns.ReturnsPolicy // full per-source policy: the flow classification and the engine knobs (OnboardScope/Inception/ConduitKinds/ExternalOnly)
	nonTransfer    []returns.Flow        // external deposit/withdrawal — never heuristically netted; a stored pair may claim it (links)
	transferLike   []returns.Flow        // transfer_in/out/journal — netting candidates at coarse grains
	journalPresent bool
	hasClampedFlow bool // a flow was valued at fx_daily's day-0 floor rate
	droppedNonzero bool // left the spine (superseded, or past its carry horizon) while still holding value

	// links maps a flow's transaction id to its linked partner leg, filled
	// from the stored links when transfer matching is enabled (bindStoredLinks;
	// nil otherwise). Consumed per entity in entityFlows: a linked pair binds
	// only where the entity holds both legs in the window (storedPairs).
	links map[string]linkedLeg

	// inKind maps a transfer-like flow's transaction id to the holding it
	// moved, for the legs that name one; nil when none does. The netter
	// pairs such a leg by holding and quantity (pairInKind).
	inKind map[string]heldMove
	// debut is what the funded valuation holds, by holding (fundedDay): the
	// first valuation, or the first nonzero one after a zero first value.
	// It is read for an account whose transfer-like legs on or before its
	// debut moved a holding; nil otherwise, or where that valuation holds
	// cash or prices its holdings in more than one currency
	// (loadDebutHoldings).
	debut map[string]debutHolding

	// twin is the same account in residueCurrency's dataset, nil where the
	// dataset is that currency's or none is linked (linkResidueTwins).
	twin *accountData
	// residue is the dataset's cache of residue decisions (residueEnds).
	residue *residueCache

	// The zero tail and head, cached by cacheTail: lastNonzeroDay,
	// zeroingDay and lateDebut.
	tailKnown                     bool
	lastNonzero, zeroing, lateDay int64
}

// residueCache holds each entity's residue decision, read once on its
// since-inception chain and applied to every window. The benchmarks read a
// currency's dataset from goroutines of their own, so it locks.
type residueCache struct {
	mu sync.Mutex
	m  map[string]map[int64]bool
}

func (c *residueCache) get(key string, compute func() map[int64]bool) map[int64]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.m[key]; ok {
		return v
	}
	v := compute()
	c.m[key] = v
	return v
}

// linkedLeg identifies the partner leg of a stored transfer pair.
type linkedLeg struct {
	src, acct, txID string
	day             int64
}

func (a *accountData) firstDay() int64 {
	if len(a.series) == 0 {
		return 0
	}
	return a.series[0].day
}

// valuedOn is the first real snapshot day on or after day, 0 when none is.
func (a *accountData) valuedOn(day int64) int64 {
	i := sort.Search(len(a.snapDays), func(k int) bool { return a.snapDays[k] >= day })
	if i == len(a.snapDays) {
		return 0
	}
	return a.snapDays[i]
}

// isConduit reports whether this account is a conduit (plumbing) under its
// source's ReturnsPolicy: it feeds the aggregate value spine but emits no
// per-account synthetic onboarding. Default policy (empty ConduitKinds) => false
// for every account, so non-conduit sources are unaffected.
func (a *accountData) isConduit() bool {
	return a.rpolicy.IsConduit(canonical.AccountKind(a.kind))
}

// firstRealSnapshotDay returns the earliest real snapshot day for this account
// (the first entry in snapDays), or its spine firstDay() when no real snapshot
// day was loaded. This is the anchor used under Inception=first-real-snapshot.
func (a *accountData) firstRealSnapshotDay() int64 {
	if len(a.snapDays) > 0 {
		return a.snapDays[0]
	}
	return a.firstDay()
}

func (a *accountData) lastDay() int64 {
	if len(a.series) == 0 {
		return 0
	}
	return a.series[len(a.series)-1].day
}

func (a *accountData) lastVal() float64 {
	if len(a.series) == 0 {
		return 0
	}
	return a.series[len(a.series)-1].val
}

// valueAt returns the value on `day` and whether the account was present then.
// Before its first emitted row it is NULL ("not yet alive"); AFTER its
// last emitted row it is gone: the macro stops emitting the account where
// its last observation ends (DESIGN.md §10.7), so carrying forward past the
// last row would diverge from report_*_history and break global == Σ
// accounts. Within [first,last] the daily spine has a row for every day.
func (a *accountData) valueAt(day int64) (float64, bool) {
	if len(a.series) == 0 || day < a.series[0].day || day > a.series[len(a.series)-1].day {
		return 0, false
	}
	i := sort.Search(len(a.series), func(k int) bool { return a.series[k].day > day })
	return a.series[i-1].val, true
}

// changeDays appends to days each day in (from, to] on which a's value
// changes: the days of its series there, and its last day's next, where
// it ends inside the span and reads nothing from then on.
func (a *accountData) changeDays(from, to int64, days []int64) []int64 {
	i := sort.Search(len(a.series), func(k int) bool { return a.series[k].day > from })
	for ; i < len(a.series) && a.series[i].day <= to; i++ {
		days = append(days, a.series[i].day)
	}
	if n := len(a.series); n > 0 && a.series[n-1].day >= from && a.series[n-1].day < to {
		days = append(days, a.series[n-1].day+1)
	}
	return days
}

// closureDay returns the explicit-closure day (the account's value went to ~0)
// or 0 when it merely stopped updating (staleness must NOT synthesize a
// divestment). The daily carry-forward spine extends a zeroing row into a flat
// zero tail reaching the spine's end, so this is the tail's END. Subsumption
// covers (lastNonzeroDay, closureDay] as far as the window reaches; the closure
// flow keys on zeroingDay.
func (a *accountData) closureDay() int64 {
	if len(a.series) == 0 {
		return 0
	}
	last := a.series[len(a.series)-1]
	if math.Abs(last.val) < valueTol {
		return last.day
	}
	return 0
}

// zeroingDay is the first day of a's terminal zero tail, the valuation that shows
// it empty: the day after lastNonzeroDay. 0 when a has no explicit closure or
// never held value.
func (a *accountData) zeroingDay() int64 {
	if a.tailKnown {
		return a.zeroing
	}
	return a.findZeroingDay()
}

func (a *accountData) findZeroingDay() int64 {
	if a.closureDay() == 0 {
		return 0
	}
	last := a.lastNonzeroDay()
	if v, _ := a.valueAt(last); math.Abs(v) < valueTol {
		return 0
	}
	return last + 1
}

// cacheTail stores lastNonzeroDay and zeroingDay, which walk back over the
// zero tail and are read for every flow of a closed account, and lateDebut,
// which walks over a zero head. The series must not change afterwards
// (finalize calls it).
func (a *accountData) cacheTail() {
	a.tailKnown = false
	a.lastNonzero, a.zeroing, a.lateDay = a.findLastNonzeroDay(), a.findZeroingDay(), a.findLateDebut()
	a.tailKnown = true
}

// lateDebut is the first nonzero valuation of an account whose first value is
// zero, 0 where the first value is not zero or no nonzero value follows it.
// A zero first value holds none of the money that came in before it: that
// money funds this valuation instead, the first to show it, and is in
// transit until then.
func (a *accountData) lateDebut() int64 {
	if a.tailKnown {
		return a.lateDay
	}
	return a.findLateDebut()
}

func (a *accountData) findLateDebut() int64 {
	if len(a.series) == 0 || math.Abs(a.series[0].val) >= valueTol {
		return 0
	}
	for _, dv := range a.series[1:] {
		if math.Abs(dv.val) >= valueTol {
			return dv.day
		}
	}
	return 0
}

// fundedDay is the valuation the money that came in on or before a's debut
// funds: the late debut where there is one, else the debut.
func (a *accountData) fundedDay() int64 {
	if d := a.lateDebut(); d != 0 {
		return d
	}
	return a.firstDay()
}

// lastNonzeroDay returns the day of the last carry-forward value that is still
// non-zero — the day before the daily spine drops into its terminal zero tail.
// A closing constituent's flows dated after this day land where the series
// already reads ~0 (no visible ΔV), so under the default ClosureScope they are
// subsumed and leave in the closure flow on the zeroing day; ClosureLedgerExact
// keeps them as the real exit legs. Returns the first day for an all-zero
// series.
func (a *accountData) lastNonzeroDay() int64 {
	if a.tailKnown {
		return a.lastNonzero
	}
	return a.findLastNonzeroDay()
}

func (a *accountData) findLastNonzeroDay() int64 {
	for i := len(a.series) - 1; i >= 0; i-- {
		if math.Abs(a.series[i].val) >= valueTol {
			return a.series[i].day
		}
	}
	return a.firstDay()
}

// loadSpine reads every account's daily value in each currency of src, and
// its real snapshot days. The row set is currency-independent; only the value
// columns and their NULL-ness differ, so appendSeries' skip-if-NULL builds
// each currency's series as a load of that currency alone does.
func loadSpine(ctx context.Context, db *sql.DB, src spineSource, ov map[string]ReturnsPolicyOverride) (map[string]map[string]*accountData, map[string]*holdingsSpine, error) {
	kinds, err := SourceKinds(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	pfNames, err := loadPortfolioNames(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	byCcy := make(map[string]map[string]*accountData, len(src.currencies))
	holdings := make(map[string]*holdingsSpine, len(src.currencies))
	for _, ccy := range src.currencies {
		byCcy[ccy] = map[string]*accountData{}
		holdings[ccy] = newHoldingsSpine()
	}

	rows, err := db.QueryContext(ctx, src.history, src.historyArgs...)
	if err != nil {
		return nil, nil, fmt.Errorf("%s history: %w", src.op, err)
	}
	var (
		asOf            int64
		sid, acct, kind string
		label, base, pf sql.NullString
	)
	tots := make([]sql.NullString, len(src.currencies))
	dest := []any{&asOf, &sid, &acct, &kind, &label, &base, &pf}
	for i := range tots {
		dest = append(dest, &tots[i])
	}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("%s history scan: %w", src.op, err)
		}
		day := EpochDay(asOf)
		for i, ccy := range src.currencies {
			appendSeries(byCcy[ccy], holdings[ccy], kinds, pfNames, ov, sid, acct, kind, label, base, pf, day, tots[i])
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}

	// Snapshot days are currency-independent: one scan, distributed to every
	// currency's map.
	maps := make([]map[string]*accountData, 0, len(src.currencies))
	for _, ccy := range src.currencies {
		maps = append(maps, byCcy[ccy])
	}
	if err := loadSnapshotDays(ctx, db, maps...); err != nil {
		return nil, nil, err
	}
	return byCcy, holdings, nil
}

// newAccountData builds the currency-independent metadata record for an
// account (kind, portfolio, base currency, label, and the resolved
// ReturnsPolicy). The value series and flows are attached afterwards, per
// currency. This is the single policy-resolution point: the config-side
// override (keyed by silver_source_id, not adapter kind) composes here, so
// every downstream knob site reads the overridden policy (a.rpolicy) at
// every grain.
func newAccountData(kinds, pfNames map[string]string, ov map[string]ReturnsPolicyOverride, src, acct, kind string, label, base, pf sql.NullString) *accountData {
	rp, _ := returns.ReturnsPolicyFor(kinds[src])
	rp = applyPolicyOverride(rp, ov, src)
	a := &accountData{src: src, acct: acct, kind: kind, srcKind: kinds[src], rpolicy: rp}
	a.portfolio = pf.String
	a.portfolioName = pfNames[acctKey(src, pf.String)]
	a.baseCurrency = base.String
	a.label = acct
	if label.Valid && label.String != "" {
		a.label = label.String
	}
	return a
}

// returnsInvisibleKind reports whether an account kind contributes nothing to
// any return series, at any grain (canonical.AccountKindRow.ReturnsInvisible):
// only `card`. appendSeries drops such an
// account at load, so no grain enumerates it and attachOneFlow drops its flows.
// docs/RETURNS-NOTES.md, "Credit cards are returns-invisible", has the why and
// the report_global contract.
func returnsInvisibleKind(kind string) bool {
	return canonical.AccountKind(kind).ReturnsInvisible()
}

// appendSeries adds one account-day value to byKey, creating the account record
// on first sight. An unpriceable day (NULL total — no FX path) is omitted from
// the series, so each currency's series holds the days it can price. Rows must arrive
// in ascending day order per account (the loaders' ORDER BY guarantees it), so
// each series is ascending for valueAt's binary search.
//
// This is also the single seam where credit cards leave the returns engine (see
// returnsInvisibleKind): the account goes to holdings instead of byKey, so nothing
// downstream of byKey can see it. holdings takes every priced row, for the
// figures that count what returns leaves out.
func appendSeries(byKey map[string]*accountData, holdings *holdingsSpine, kinds, pfNames map[string]string, ov map[string]ReturnsPolicyOverride, src, acct, kind string, label, base, pf sql.NullString, day int64, tot sql.NullString) {
	v, ok := parseFloat(tot)
	if !ok {
		return
	}
	holdings.netWorth[day] += v
	into := byKey
	if returnsInvisibleKind(kind) {
		into = holdings.cards
	}
	k := acctKey(src, acct)
	a := into[k]
	if a == nil {
		a = newAccountData(kinds, pfNames, ov, src, acct, kind, label, base, pf)
		into[k] = a
	}
	a.series = append(a.series, dayVal{day: day, val: v})
}

// applyPolicyOverride composes a source's config-side override (if any) on
// top of its registered policy. A FlowRegime override swaps in the regime's
// FlowPolicy; unset fields leave the registered values untouched.
func applyPolicyOverride(rp returns.ReturnsPolicy, ov map[string]ReturnsPolicyOverride, src string) returns.ReturnsPolicy {
	o, ok := ov[src]
	if !ok {
		return rp
	}
	if o.FlowRegime != nil {
		rp.Flow = returns.NewFlowPolicy(*o.FlowRegime)
	}
	if o.AccountsGrain != nil {
		rp.AccountsGrain = *o.AccountsGrain
	}
	return rp
}

// SourceKinds maps each configured silver_source_id to its
// silver_kind (the adapter it loads through). Config-side ids are
// free-form, so anything keying behaviour to an adapter must go
// through this map rather than match the id string.
func SourceKinds(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT silver_source_id, silver_kind FROM silver_sources`)
	if err != nil {
		return nil, fmt.Errorf("SourceKinds: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return nil, err
		}
		out[id] = kind
	}
	return out, rows.Err()
}

// loadPortfolioNames maps (silver_source_id, portfolio_external_id) to the
// portfolio's display_name so the portfolios grain reports the friendly label
// (e.g. a nickname) the holdings views already show, not the raw external id.
// Portfolios without a display_name are absent, so the label falls back to the id.
func loadPortfolioNames(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT silver_source_id, portfolio_external_id, display_name
		   FROM portfolios WHERE display_name IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("RunReturns portfolio names: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var src, pid, name string
		if err := rows.Scan(&src, &pid, &name); err != nil {
			return nil, fmt.Errorf("RunReturns portfolio-name scan: %w", err)
		}
		out[acctKey(src, pid)] = name
	}
	return out, rows.Err()
}

// loadSnapshotDays attaches each account's distinct real snapshot days (the
// inception anchor and empty-bucket detector). The day list is currency-
// independent, so a load of several currencies passes every per-currency
// map and this scans once, distributing to whichever map holds the account.
func loadSnapshotDays(ctx context.Context, db *sql.DB, byKeys ...map[string]*accountData) error {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT silver_source_id, account_external_id, snapshot_at // 86400 AS day FROM positions
		UNION
		SELECT DISTINCT silver_source_id, account_external_id, snapshot_at // 86400 AS day FROM cash_balances
		ORDER BY 1, 2, 3`)
	if err != nil {
		return fmt.Errorf("RunReturns snapshot days: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var src, acct string
		var day int64
		if err := rows.Scan(&src, &acct, &day); err != nil {
			return err
		}
		for _, byKey := range byKeys {
			if a := byKey[acctKey(src, acct)]; a != nil {
				a.snapDays = append(a.snapDays, day)
			}
		}
	}
	return rows.Err()
}

// flowTxnRow is the lean projection of report_transactions the returns engine
// needs: only the fields attachOneFlow and the link binding read, not the
// wide display row TransactionsBetween builds. Selecting just these lets
// DuckDB prune the macro's account/instrument LEFT JOINs, which the flow path
// never consults.
type flowTxnRow struct {
	src, acct  string
	occurredAt int64
	kind       string
	ccy        string
	txID       string
	values     []*string // the net amount in each currency of the load
	// returnsInternal is the adapter's own verdict that this row is
	// conduit churn rather than owner capital — read only under the
	// policy's ExternalOnly (see attachOneFlow).
	returnsInternal bool
	// inst is the instrument the row names, its id or else its hint
	// ("" for a movement of cash), class and vehicle the asset class and
	// vehicle the row states ("" for none), and qty its quantity, unsigned
	// (0 when none is stated).
	inst, class, vehicle string
	qty                  float64
}

// holding is what the row moved, with what gold states of its instrument
// (loadInstrumentFacts): its identity, and its asset class and vehicle, each
// the row's own where it states one.
func (r flowTxnRow) holding(facts map[string]instrumentFacts) heldRef {
	f := facts[acctKey(r.src, r.inst)]
	f.class, f.vehicle = cmp.Or(r.class, f.class), cmp.Or(r.vehicle, f.vehicle)
	return heldRef{inst: r.inst, facts: f, qty: r.qty}
}

// loadFlowTxns reads every transaction's flow-relevant fields, its net amount
// in each currency of src, in the report_transactions macro's own total order
// (occurred_at, silver_source_id, transaction_external_id) — the order
// TransactionsBetween's ascending path relies on, so the flow sequence is
// identical to the wide loader.
func loadFlowTxns(ctx context.Context, db *sql.DB, src spineSource) ([]flowTxnRow, error) {
	rows, err := db.QueryContext(ctx, src.transactions, src.transactionsArgs...)
	if err != nil {
		return nil, fmt.Errorf("%s transactions: %w", src.op, err)
	}
	defer rows.Close()
	var (
		out []flowTxnRow
		cur flowTxnRow
	)
	vals := make([]sql.NullString, len(src.currencies))
	dest := []any{&cur.src, &cur.acct, &cur.occurredAt, &cur.kind, &cur.ccy, &cur.txID}
	for i := range vals {
		dest = append(dest, &vals[i])
	}
	var inst, class, vehicle sql.NullString
	var qty sql.NullFloat64
	dest = append(dest, &cur.returnsInternal, &inst, &qty, &class, &vehicle)
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("%s transactions scan: %w", src.op, err)
		}
		r := cur
		r.inst, r.class, r.vehicle, r.qty = inst.String, class.String, vehicle.String, math.Abs(qty.Float64)
		r.values = make([]*string, len(vals))
		for i, v := range vals {
			r.values[i] = src.decimal(v)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// spineSource is where a dataset load reads each account's daily values and
// the transactions, both converted to its currencies: the per-currency
// report macros for one currency, or the _multi macros for every
// materialized currency at once.
type spineSource struct {
	currencies []string
	// op names the load in its errors.
	op string
	// history selects as_of_day, silver_source_id, account_external_id,
	// account_kind, display_name, base_currency, portfolio_external_id and
	// a total value per currency, ordered by account and day.
	history     string
	historyArgs []any
	// transactions selects a flowTxnRow's fields, a net amount per currency
	// in values' place, in report_transactions' own order.
	transactions     string
	transactionsArgs []any
	// decimal is a transactions query's amount as the loader keeps it.
	decimal func(sql.NullString) *string
}

// flowTxnJoin is the part of a transactions query after its value columns:
// the adapter's conduit verdict, and the instrument, quantity and asset
// class and vehicle the row moved.
const flowTxnJoin = `,
	        COALESCE(t.payload ->> '` + canonical.PayloadReturnsFlow + `' = 'internal', FALSE) AS returns_internal,
	        COALESCE(t.instrument_external_id, t.instrument_hint), CAST(t.quantity AS DOUBLE), t.asset_class, t.vehicle
	   FROM %s r
	   LEFT JOIN transactions t
	          ON t.silver_source_id        = r.silver_source_id
	         AND t.transaction_external_id = r.transaction_external_id
	  ORDER BY r.occurred_at, r.silver_source_id, r.transaction_external_id`

// oneCurrency reads report_accounts_history and report_transactions in ccy.
func oneCurrency(ccy string) spineSource {
	return spineSource{currencies: []string{ccy}, op: "RunReturns",
		history: `SELECT as_of_day, silver_source_id, account_external_id, account_kind,
		        display_name, base_currency, portfolio_external_id, total_value_outccy
		   FROM report_accounts_history(?)
		  ORDER BY silver_source_id, account_external_id, as_of_day`,
		historyArgs: []any{ccy},
		transactions: `SELECT r.silver_source_id, r.account_external_id, r.occurred_at, r.kind,
		        r.currency, r.transaction_external_id, r.value_outccy` +
			fmt.Sprintf(flowTxnJoin, "report_transactions(?, ?, ?)"),
		transactionsArgs: []any{int64(0), MaxEpoch, ccy},
		// The amounts read as the transactions view shows them.
		decimal: trimmedDecimalPtr}
}

// attachOneFlow classifies one transaction as an external flow for its account
// in byKey (a no-op if the account isn't loaded), once per currency of a
// load, and reports whether a flow was attached: a stored transfer link binds
// only between attached flows (bindStoredLinks). A transfer-like flow that
// moved a holding keeps it (accountData.inKind).
// valueOut is the transaction's net amount already converted to outCcy (nil ⇒
// unresolved FX ⇒ not a flow); txCcy is the transaction's own currency, for
// the day-0 clamp check.
func attachOneFlow(byKey map[string]*accountData, fx fxBounds, outCcy, src, acct string, kind canonical.TxKind, occurredAt int64, txID, txCcy string, valueOut *string, returnsInternal bool, held heldRef) bool {
	a := byKey[acctKey(src, acct)]
	if a == nil {
		return false
	}
	// The silver-side pre-tag ExternalOnly describes: an adapter that can
	// tell owner capital from conduit churn says so on the row. It is read
	// only under ExternalOnly, so a source that tags nothing is unaffected.
	// It is a tag rather than a rewritten KIND because the spending
	// population reads the kind too, so a kind could not carry a
	// returns-only verdict without erasing the row from spending as well.
	if a.rpolicy.ExternalOnly && returnsInternal {
		return false
	}
	if !a.rpolicy.Flow.IsExternal(kind, strings.HasPrefix(txID, canonical.LedgerTransferIDPrefix)) {
		return false
	}
	val, ok := parseFloatPtr(valueOut)
	if !ok {
		return false // unresolved FX on the flow — skip (documented limitation)
	}
	day := EpochDay(occurredAt)
	if kind == canonical.TxKindJournal {
		a.journalPresent = true
	}
	if fx.clampedBefore(txCcy, outCcy, day) {
		a.hasClampedFlow = true
	}
	f := returns.Flow{Day: day, Amount: val, ID: txID}
	if a.rpolicy.Flow.IsTransferLike(kind) {
		a.transferLike = append(a.transferLike, f)
		if held.inst != "" {
			if a.inKind == nil {
				a.inKind = map[string]heldMove{}
			}
			a.inKind[txID] = held.move(a.srcKind)
		}
	} else {
		a.nonTransfer = append(a.nonTransfer, f)
	}
	return true
}

// txRef names one transaction.
type txRef struct{ src, txID string }

// attachedFlow is where one attached flow sits: its account and day.
type attachedFlow struct {
	acct string
	day  int64
}

// attachedFlows indexes a dataset's attached flows by transaction.
type attachedFlows map[txRef]attachedFlow

// heldRef is the holding a transaction row moved, as the row names it: the
// instrument ("" for cash), what gold states of it (its identities across
// sources, and its asset class and vehicle, the row's own where it states
// them), the quantity, unsigned, and a coin's USD price on the row's day
// (CoinUSD).
type heldRef struct {
	inst  string
	facts instrumentFacts
	qty   float64
	usd   float64
}

// heldMove is what a transfer-like leg moved in kind: its holding, a key
// two legs share where they move one holding (holdingUnion, set once every
// leg is read), its identities, the quantity, unsigned (0 when the leg
// states none), whether it pairs only on the exact quantity
// (exactQuantity), and a coin's USD price on the leg's day (0 for none).
type heldMove struct {
	holding string
	ids     holdingIdentity
	qty     float64
	exact   bool
	usd     float64
}

// move is r's identities, quantity and quantity rule.
func (r heldRef) move(srcKind string) heldMove {
	return heldMove{ids: holdingIdentityOf(srcKind, r.inst, r.facts), qty: r.qty,
		exact: exactQuantity(r.facts.class, r.facts.vehicle), usd: r.usd}
}

// heldRefs is what each of txns moved (flowTxnRow.holding), a coin with its
// USD price on the row's day, read for the coins the rows move only.
func heldRefs(ctx context.Context, q querier, txns []flowTxnRow, facts map[string]instrumentFacts) ([]heldRef, error) {
	out := make([]heldRef, len(txns))
	seen := map[string]bool{}
	var coins []string
	for k, t := range txns {
		out[k] = t.holding(facts)
		if h := out[k]; h.inst != "" && IsCoin(h.facts.class, h.facts.vehicle) && !seen[h.inst] {
			seen[h.inst] = true
			coins = append(coins, h.inst)
		}
	}
	rates, err := LoadCoinUSD(ctx, q, coins)
	if err != nil || rates == nil {
		return out, err
	}
	for k, t := range txns {
		if h := &out[k]; seen[h.inst] && IsCoin(h.facts.class, h.facts.vehicle) {
			h.usd = CoinUSD(rates, h.inst, EpochDay(t.occurredAt))
		}
	}
	return out, nil
}

// nameHoldings sets the holding of every leg that moved one, over every
// currency's accounts (holdingUnion), and returns the union, which names a
// debut's holdings alike.
func nameHoldings(byCcy map[string]map[string]*accountData) *holdingUnion {
	u := newHoldingUnion()
	for _, byKey := range byCcy {
		for _, a := range byKey {
			for _, m := range a.inKind {
				u.add(m.ids)
			}
		}
	}
	for _, byKey := range byCcy {
		for _, a := range byKey {
			for id, m := range a.inKind {
				m.holding = u.key(m.ids)
				a.inKind[id] = m
			}
		}
	}
	return u
}

// debutHolding is one holding of the valuation a debut's legs fund
// (fundedDay): the quantity held and its share of the valuation's value.
type debutHolding struct{ qty, share float64 }

// loadDebutHoldings reads, for every account a leg moved a holding into on or
// before its debut, the holdings of the valuation such legs fund (fundedDay,
// accountData.debut): the positions of the day's last snapshot. A share is
// the position's market value over all of them, so it is read only where
// every position states one in a single currency and the account holds no
// cash then. A holding is named as the legs name it (u). Each currency's
// account reads its own debut day: where FX starts later in one currency,
// its value, and so its debut, starts later too.
func loadDebutHoldings(ctx context.Context, q querier, facts map[string]instrumentFacts, u *holdingUnion, byCcy map[string]map[string]*accountData) error {
	type ref struct {
		src, acct string
		day       int64
	}
	accts := map[ref][]*accountData{}
	for _, byKey := range byCcy {
		for _, a := range byKey {
			for f := range a.legs(0, a.firstDay()) {
				if _, ok := a.inKind[f.ID]; ok {
					r := ref{a.src, a.acct, a.fundedDay()}
					accts[r] = append(accts[r], a)
					break
				}
			}
		}
	}
	if len(accts) == 0 {
		return nil
	}
	refs := slices.SortedFunc(maps.Keys(accts), func(x, y ref) int {
		return cmp.Or(strings.Compare(x.src, y.src), strings.Compare(x.acct, y.acct), cmp.Compare(x.day, y.day))
	})
	var values []string
	var args []any
	for _, r := range refs {
		values = append(values, "(?, ?, ?)")
		args = append(args, r.src, r.acct, r.day)
	}
	rows, err := q.QueryContext(ctx, `
		WITH w(s, a, d) AS (VALUES `+strings.Join(values, ", ")+`),
		     cash AS (SELECT DISTINCT b.silver_source_id AS s, b.account_external_id AS a, w.d AS d
		                FROM cash_balances b
		                JOIN w ON b.silver_source_id = w.s AND b.account_external_id = w.a
		                      AND b.snapshot_at // 86400 = w.d
		               WHERE b.amount <> 0)
		SELECT p.silver_source_id, p.account_external_id, w.d, p.instrument_external_id,
		       CAST(p.quantity AS DOUBLE), CAST(p.market_value AS DOUBLE), p.currency,
		       cash.s IS NOT NULL
		  FROM positions p
		  JOIN w ON p.silver_source_id = w.s AND p.account_external_id = w.a
		        AND p.snapshot_at // 86400 = w.d
		  LEFT JOIN cash ON cash.s = p.silver_source_id AND cash.a = p.account_external_id AND cash.d = w.d
		QUALIFY p.snapshot_at = max(p.snapshot_at) OVER (PARTITION BY p.silver_source_id, p.account_external_id, w.d)
		 ORDER BY 1, 2, 4`, args...)
	if err != nil {
		return fmt.Errorf("returns debut holdings: %w", err)
	}
	defer rows.Close()
	type position struct {
		inst     sql.NullString
		qty, mv  sql.NullFloat64
		ccy      string
		withCash bool
	}
	held := map[ref][]position{}
	for rows.Next() {
		var r ref
		var p position
		if err := rows.Scan(&r.src, &r.acct, &r.day, &p.inst, &p.qty, &p.mv, &p.ccy, &p.withCash); err != nil {
			return fmt.Errorf("returns debut holdings scan: %w", err)
		}
		held[r] = append(held[r], p)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for r, ps := range held {
		var total returns.Sum
		ok := true
		for _, p := range ps {
			ok = ok && !p.withCash && p.inst.Valid && p.qty.Valid && p.mv.Valid && p.mv.Float64 > 0 && p.ccy == ps[0].ccy
			total.Add(p.mv.Float64)
		}
		if !ok {
			continue
		}
		for _, a := range accts[r] {
			a.debut = map[string]debutHolding{}
			for _, p := range ps {
				h := u.key(holdingIdentityOf(a.srcKind, p.inst.String, facts[acctKey(r.src, p.inst.String)]))
				d := a.debut[h]
				d.qty += math.Abs(p.qty.Float64)
				d.share += p.mv.Float64 / total.Total()
				a.debut[h] = d
			}
		}
	}
	return nil
}

// storedTransferLink is one leg's transfer_links row that names a partner leg.
type storedTransferLink struct {
	src, txID, farSrc, farTxID string
}

// loadStoredTransferLinks reads the transfer_links rows that name a partner
// leg, in key order. A gold without the table holds no links.
func loadStoredTransferLinks(ctx context.Context, q querier) ([]storedTransferLink, error) {
	ok, err := hasTable(ctx, q, "transfer_links")
	if err != nil || !ok {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `
		SELECT silver_source_id, transaction_external_id,
		       far_silver_source_id, far_transaction_external_id
		  FROM transfer_links
		 WHERE far_transaction_external_id IS NOT NULL
		   AND far_silver_source_id IS NOT NULL
		 ORDER BY silver_source_id, transaction_external_id`)
	if err != nil {
		return nil, fmt.Errorf("returns transfer links: %w", err)
	}
	defer rows.Close()
	var out []storedTransferLink
	for rows.Next() {
		var l storedTransferLink
		if err := rows.Scan(&l.src, &l.txID, &l.farSrc, &l.farTxID); err != nil {
			return nil, fmt.Errorf("returns transfer links scan: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// bindStoredLinks writes each stored link whose two legs are both attached
// flows as symmetric linkedLeg entries on the owning accounts. A leg
// whose partner is no flow of the dataset (a card's, an excluded kind's, one
// with no FX path) stays a plain flow.
//
// The links are only POTENTIAL internality: entityFlows binds a pair only
// where the entity holds both legs in its window (storedPairs), so finer
// grains keep counting each leg as the boundary flow it is for them.
func bindStoredLinks(links []storedTransferLink, flows attachedFlows, byKey map[string]*accountData) {
	for _, l := range links {
		from, ok := flows[txRef{l.src, l.txID}]
		if !ok {
			continue
		}
		to, ok := flows[txRef{l.farSrc, l.farTxID}]
		if !ok {
			continue
		}
		linkLeg(byKey, l.src, from.acct, l.txID, linkedLeg{src: l.farSrc, acct: to.acct, txID: l.farTxID, day: to.day})
		linkLeg(byKey, l.farSrc, to.acct, l.farTxID, linkedLeg{src: l.src, acct: from.acct, txID: l.txID, day: from.day})
	}
}

// linkLeg records `to` as the partner of the leg txID on the account (src,
// acct), where the returns engine loaded that account.
func linkLeg(byKey map[string]*accountData, src, acct, txID string, to linkedLeg) {
	acc := byKey[acctKey(src, acct)]
	if acc == nil {
		return
	}
	if acc.links == nil {
		acc.links = map[string]linkedLeg{}
	}
	acc.links[txID] = to
}

// MaxEpoch is the open upper bound for a query over the whole history.
// Gold's windowed macros and helpers take a CLOSED [from, to] window,
// so "everything" is expressed as a bound no timestamp can reach rather
// than as a special case in each caller.
const MaxEpoch = int64(1) << 62

// SecondsPerDay converts gold's Unix-seconds timestamps to epoch days.
// The day grain is a property of that timestamp convention, so it lives
// beside MaxEpoch as the shared spelling for callers.
const SecondsPerDay = 86400

// EpochDay floors a Unix-seconds timestamp to its epoch day
// (lots.EpochDay).
func EpochDay(sec int64) int64 { return lots.EpochDay(sec) }

// ---- grouping ------------------------------------------------------------

// grain is one level of the returns hierarchy: how it groups the accounts
// into entities, how an entity is named, and which config entries reach it.
type grain struct {
	name string
	// entity names what one entity is to a reader: "account" or
	// "portfolio", or "" for a source and the whole.
	entity string
	// key is the entity an account belongs to.
	key func(a *accountData) string
	// ident is an entity's id and label, read from its first asset.
	ident func(a *accountData) (id, label string)
	// excludesAccounts and excludesPortfolios say which returns_exclude
	// entries drop an account from the grain's entities. No grain drops an
	// entry of its own kind: an excluded portfolio still has its own row.
	excludesAccounts, excludesPortfolios bool
	// inception is the order in which an entity's configured inception
	// floor is looked up, most specific first. The whole is never anchored.
	inception []inceptionScope
	// single: an entity is one account. Its flows are exact, a liability
	// shows a line of its own, and a policy may blank its rates.
	single bool
	// whole: one entity of every source. It is never hidden and has no
	// single source.
	whole bool
}

// inceptionScope is a key of the inception_overrides block.
type inceptionScope uint8

const (
	scopeAccount inceptionScope = iota
	scopePortfolio
	scopeSource
)

// The grains, finest first: each coarser grain aggregates the one before.
var (
	grainAccounts = &grain{name: "accounts", entity: "account",
		key:       func(a *accountData) string { return acctKey(a.src, a.acct) },
		ident:     func(a *accountData) (string, string) { return a.acct, a.label },
		inception: []inceptionScope{scopeAccount, scopePortfolio, scopeSource}, single: true}
	grainPortfolios = &grain{name: "portfolios", entity: "portfolio",
		key:              func(a *accountData) string { return acctKey(a.src, a.portfolio) },
		ident:            portfolioIdent,
		excludesAccounts: true, inception: []inceptionScope{scopePortfolio, scopeSource}}
	grainSources = &grain{name: "sources",
		key:              func(a *accountData) string { return a.src },
		ident:            func(a *accountData) (string, string) { return a.src, a.src },
		excludesAccounts: true, excludesPortfolios: true, inception: []inceptionScope{scopeSource}}
	grainGlobal = &grain{name: "global",
		key:              func(*accountData) string { return "global" },
		ident:            func(*accountData) (string, string) { return "", "global" },
		excludesAccounts: true, excludesPortfolios: true, whole: true}
	returnsGrains = []*grain{grainAccounts, grainPortfolios, grainSources, grainGlobal}
)

// portfolioIdent is a portfolio's id and label. The id stays the stable
// portfolio_external_id; the label prefers the resolved display name and
// falls back to the id. Accounts outside any portfolio are one entity.
func portfolioIdent(a *accountData) (string, string) {
	if a.portfolio == "" {
		return "", NoPortfolioLabel
	}
	if a.portfolioName != "" {
		return a.portfolio, a.portfolioName
	}
	return a.portfolio, a.portfolio
}

// grainNamed is the grain of that name. Only code names a grain (the
// front ends offer the grains' own names), so an unknown name is a
// programming error and panics.
func grainNamed(name string) *grain {
	for _, g := range returnsGrains {
		if g.name == name {
			return g
		}
	}
	panic(fmt.Sprintf("gold: no returns grain named %q", name))
}

// grainNames are the grains' names, finest first.
func grainNames() []string {
	out := make([]string, len(returnsGrains))
	for i, g := range returnsGrains {
		out[i] = g.name
	}
	return out
}

// ReturnsGrain is a returns grain as a front end lists it. Entity is
// "account" or "portfolio" where an entity is one; Whole marks the grain
// whose one entity spans every source.
type ReturnsGrain struct {
	Name, Entity string
	Whole        bool
}

// ReturnsGrains are the returns grains, finest first.
func ReturnsGrains() []ReturnsGrain {
	out := make([]ReturnsGrain, len(returnsGrains))
	for i, g := range returnsGrains {
		out[i] = ReturnsGrain{Name: g.name, Entity: g.entity, Whole: g.whole}
	}
	return out
}

// groupAccounts groups accts into the entities of the grain named level,
// each entity's members sorted, and returns the entity keys in order.
func groupAccounts(level string, accts map[string]*accountData, excl *ReturnsExclude) (map[string][]*accountData, []string) {
	groups := map[string][]*accountData{}
	g := grainNamed(level)
	for _, a := range accts {
		if g.excludes(excl, a.src, a.portfolio, a.acct) {
			continue // omitted from this (higher) grain; still shown at its own grain
		}
		k := g.key(a)
		groups[k] = append(groups[k], a)
	}
	order := make([]string, 0, len(groups))
	for k := range groups {
		// Members accumulate in accts' (randomized) map-iteration order; sort
		// each group by (src, acct) so aggregation — float summation in `av`,
		// flow append order — is deterministic across runs, making the
		// materialized table byte-stable. (Compute is otherwise order-agnostic;
		// this only pins ULP-level wobble that never survives 2-dp rendering.)
		g := groups[k]
		sort.Slice(g, func(i, j int) bool {
			if g[i].src != g[j].src {
				return g[i].src < g[j].src
			}
			return g[i].acct < g[j].acct
		})
		order = append(order, k)
	}
	sort.Strings(order)
	return groups, order
}

func splitLiabilities(members []*accountData) (liability, assets []*accountData) {
	for _, m := range members {
		if m.liability() {
			liability = append(liability, m)
		} else {
			assets = append(assets, m)
		}
	}
	return
}

// liability reports whether the account is a debt by the kind table
// (canonical.AccountKind.Liability). Over the returns accounts that is a
// mortgage: a card never reaches them (holdingsSpine.cards).
func (a *accountData) liability() bool {
	return canonical.AccountKind(a.kind).Liability()
}

// ---- helpers -------------------------------------------------------------

func parseFloat64(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil
}

func parseFloat(n sql.NullString) (float64, bool) {
	if !n.Valid {
		return 0, false
	}
	return parseFloat64(n.String)
}

func parseFloatPtr(s *string) (float64, bool) {
	if s == nil {
		return 0, false
	}
	return parseFloat64(*s)
}

// Gain is the money the row's percentages describe: end value − start
// value − net external flow, so capital added counts as no gain. Nil
// when any of the three is missing.
func (r ReturnRow) Gain() *string {
	v0, ok0 := parseFloatPtr(r.StartValue)
	v1, ok1 := parseFloatPtr(r.EndValue)
	f, okF := parseFloatPtr(r.NetFlow)
	if !ok0 || !ok1 || !okF {
		return nil
	}
	return decStr(v1 - v0 - f)
}

func decStr(v float64) *string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	return &s
}
