package gold

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// acctKey is the in-memory map key for an account: source id + a NUL separator
// + account id (NUL can't appear in an id, so the join is unambiguous). Reused
// for the source+portfolio grouping key, which has the same shape.
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
	Netting   bool   // net internal transfers at coarse grains (heuristic netter + cross-source matched pairs)
	Inception string // full | strict
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
	// TransferMatching enables the opt-in cross-source transfer matcher from
	// wealthdb.cfg's returns_transfer_matching block. Nil (the default) ⇒ the
	// matcher never runs and output is byte-identical to a build without it.
	// See matchCrossTransfers.
	TransferMatching *TransferMatching
}

// TransferMatching configures the cross-source transfer matcher: an unmatched
// external leg (deposit/withdrawal/transfer/journal) whose counterparty leg
// exists in ANOTHER source — opposite sign, same native currency, equal amount
// within the tolerance, within the day window — is linked to it, and entities
// containing BOTH legs net the pair out (the money never left the entity)
// while finer grains keep counting each leg as the real boundary flow it is
// for them. See docs/DESIGN.md §5.8.
type TransferMatching struct {
	WindowDays   int     // max |day distance| between the two legs
	TolerancePct float64 // relative amount tolerance, percent of the larger leg
	// Rules are the holder's manual match / unmatch decisions, read from
	// the same ledger the spending pass reads. The matcher is shared, so a
	// pair the holder has settled must be settled the same way in both
	// reports; a movement called internal in one and external in the other
	// is worse than either answer alone.
	Rules []TransferOverrideRule
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
// constituent is display-hidden and the grain is not global — hidden plumbing
// still aggregates, and the global row always shows it doing so.
func entityHidden(level string, members []*accountData, h *ReturnsHide) bool {
	if level == "global" || len(members) == 0 {
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
// replaces the whole flow classification with the regime's canonical kind
// sets (returns.FlowPolicyForRegime); it also marks the policy Known — an
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

// excluded reports whether an account is omitted from the group at a grain. It
// is never excluded from its OWN grain: the accounts grain always shows every
// account, and the portfolios grain still shows an excluded PORTFOLIO's own row
// (only an excluded ACCOUNT drops out of its portfolio there). At the sources
// and global grains, both an excluded account and any account of an excluded
// portfolio drop out. A nil receiver excludes nothing.
func (e *ReturnsExclude) excluded(level, src, portfolio, acct string) bool {
	if e == nil {
		return false
	}
	switch level {
	case "portfolios":
		return e.Accounts[src][acct]
	case "sources", "global":
		return e.Accounts[src][acct] || e.Portfolios[src][portfolio]
	}
	return false // accounts grain: never excluded
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
	switch level {
	case "accounts":
		if m := o.Accounts[src]; m != nil {
			if d, ok := m[acct]; ok {
				return d, true
			}
		}
		if portfolio != "" {
			if m := o.Portfolios[src]; m != nil {
				if d, ok := m[portfolio]; ok {
					return d, true
				}
			}
		}
		if d, ok := o.Sources[src]; ok {
			return d, true
		}
	case "portfolios":
		if portfolio != "" {
			if m := o.Portfolios[src]; m != nil {
				if d, ok := m[portfolio]; ok {
					return d, true
				}
			}
		}
		if d, ok := o.Sources[src]; ok {
			return d, true
		}
	case "sources":
		if d, ok := o.Sources[src]; ok {
			return d, true
		}
	}
	return 0, false
}

// netting tolerances.
const (
	nettingEpsFloor  = 1.00  // absolute floor, output currency
	nettingEpsRel    = 0.005 // 0.5% of the larger leg
	nettingWindowDay = 3     // ±3 calendar days
)

// returnsDataset is the loaded, currency-specific input to computeReturns: the
// per-account daily value spine and external flows in one output currency, plus
// the derived globalMax and the (currency-independent) FX bounds. Loading is
// the expensive part — several DuckDB scans — so a dataset is built once and
// reused across every (grain, period) computation for its currency; see
// MaterializeReturns, which loads three currencies in a single pass.
type returnsDataset struct {
	accts     map[string]*accountData
	fx        fxBounds
	globalMax int64 // spine's latest emitted day across all accounts (≈ today)
}

// loadReturnsDataset loads one currency's dataset: the per-account value spine
// (report_accounts_history) and the external flows (report_transactions), both
// converted to outCcy, then the derived globalMax / droppedNonzero. fx is
// currency-independent, so a caller materializing several currencies loads it
// once and shares it.
func loadReturnsDataset(ctx context.Context, db *sql.DB, outCcy string, fx fxBounds, ov map[string]ReturnsPolicyOverride, tm *TransferMatching) (*returnsDataset, error) {
	accts, err := loadAccountData(ctx, db, outCcy, ov)
	if err != nil {
		return nil, err
	}
	if err := attachFlows(ctx, db, outCcy, accts, fx, tm); err != nil {
		return nil, err
	}
	ds := &returnsDataset{accts: accts, fx: fx}
	ds.finalize()
	return ds, nil
}

// finalize derives globalMax and flags accounts that dropped out of a later
// same-source snapshot while still holding value. Both depend only on the
// loaded series, not on grain/period, so they are computed once per dataset.
func (ds *returnsDataset) finalize() {
	// The spine's latest emitted day across all accounts (≈ today). An account
	// whose own series ends before this was ended there by the spine: a later
	// run of its source re-covered its company without it, or the source kept
	// snapshotting past it beyond the carry horizon (closed / feed-dropped; a
	// run that merely did not cover it carries it forward instead). Its value
	// is 0 thereafter (matching the macro), and we flag it if it dropped while
	// still holding value.
	var globalMax int64
	for _, a := range ds.accts {
		if d := a.lastDay(); d > globalMax {
			globalMax = d
		}
	}
	for _, a := range ds.accts {
		if a.lastDay() < globalMax && math.Abs(a.lastVal()) > valueTol {
			a.droppedNonzero = true
		}
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
	toDay := p.ToEpoch / 86400
	if toDay > ds.globalMax {
		toDay = ds.globalMax
	}

	groups, order := groupAccounts(p.Level, ds.accts, p.ReturnsExclude)
	var out []ReturnRow
	for _, key := range order {
		members := groups[key]
		// Display-hidden plumbing (cash accounts / policy AccountsGrainHidden
		// / config returns_hide): the entity emits no rows of its own — its
		// values and flows already live inside every aggregate that contains
		// it.
		if entityHidden(p.Level, members, p.ReturnsHide) {
			continue
		}
		// Mortgage / liability accounts: excluded from coarse rollups; on the
		// accounts grain they surface as their own n/a + nonpositive_base line.
		liability, assets := splitLiabilities(members)
		if p.Level == "accounts" {
			for _, m := range liability {
				out = append(out, liabilityRow(m, toDay))
			}
		}
		if len(assets) == 0 {
			continue
		}
		rows := computeEntityReturn(assets, p, toDay, ds.fx)
		// AccountsGrainBlanked: per-account (accounts-grain) rows for a sweep
		// source (crypto wallets) are economically meaningless, so keep their
		// start/end values but blank TWR/MWR to n/a and flag it. Gate STRICTLY
		// on the group's per-constituent rpolicy so ONLY that source's account
		// rows change; the portfolios/sources/global grains are NEVER gated
		// (they aggregate coherent units, which ARE valid). One row per account
		// is still emitted.
		if p.Level == "accounts" && accountsGrainBlanked(assets) {
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
// the same data load once (loadReturnsDataset or loadReturnsDatasetsMulti) and
// call computeReturns directly — see MaterializeReturns.
func RunReturns(ctx context.Context, db *sql.DB, p ReturnParams) ([]ReturnRow, error) {
	fx, err := loadFxBounds(ctx, db)
	if err != nil {
		return nil, err
	}
	ds, err := loadReturnsDataset(ctx, db, p.OutCcy, fx, p.PolicyOverrides, p.TransferMatching)
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
// conversions that fall back to the migration-0023 day-0 clamped rate.
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
	portfolio     string // portfolio_external_id ("" when none)
	portfolioName string // resolved portfolio display name ("" when none/unresolved)
	label         string
	baseCurrency  string // account base currency ("" when unknown)

	series   []dayVal // carry-forward value per emitted day, ascending
	snapDays []int64  // distinct real snapshot days, ascending

	policy         returns.FlowPolicy    // the Flow member (classification) — hot path
	rpolicy        returns.ReturnsPolicy // full per-source policy incl. the consumed engine knobs (OnboardScope/Inception/ConduitKinds/ExternalOnly)
	nonTransfer    []returns.Flow        // external deposit/withdrawal — never heuristically netted; a cross-source-matched pair may drop (crossLinks)
	transferLike   []returns.Flow        // transfer_in/out/journal — netting candidates at coarse grains
	journalPresent bool
	cryptoExcluded bool
	hasClampedFlow bool // a flow was valued at the migration-0023 day-0 clamped FX rate
	droppedNonzero bool // left the spine (superseded, or past its carry horizon) while still holding value

	// crossLinks maps a flow's transaction id to its cross-source counterparty
	// leg, filled by matchCrossTransfers when transfer matching is enabled
	// (nil otherwise). Consumed per entity in entityFlows: a linked pair nets
	// only where both legs are live members of the same entity window.
	// transferLikeIDs indexes the transferLike slice for accounts with links,
	// so per-window liveness checks resolve a partner leg's slice in O(1).
	crossLinks      map[string]crossLink
	transferLikeIDs map[string]bool
}

// crossLink identifies the counterparty leg of a cross-source-matched
// transfer pair.
type crossLink struct {
	src, acct, txID string
	day             int64
}

func (a *accountData) firstDay() int64 {
	if len(a.series) == 0 {
		return 0
	}
	return a.series[0].day
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
// last emitted row it is gone — the macro stops emitting the account once a
// later run of its source re-covered everything it was last seen with without
// it, or once its source has snapshotted past it for longer than the carry
// horizon, so carrying forward past the last row would diverge from
// report_*_history and break global == Σ accounts. Within [first,last] the
// daily spine has a row for every day.
func (a *accountData) valueAt(day int64) (float64, bool) {
	if len(a.series) == 0 || day < a.series[0].day || day > a.series[len(a.series)-1].day {
		return 0, false
	}
	i := sort.Search(len(a.series), func(k int) bool { return a.series[k].day > day })
	return a.series[i-1].val, true
}

// closureDay returns the explicit-closure day (the account's value went to ~0)
// or 0 when it merely stopped updating (staleness must NOT synthesize a
// divestment). The daily carry-forward spine extends a zeroing row into a flat
// zero tail reaching the spine's end, so this is the tail's END: the closure
// machinery engages only when the window reaches it, and the subsumption
// window (lastNonzeroDay, closureDay] spans the whole tail.
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

// lastNonzeroDay returns the day of the last carry-forward value that is still
// non-zero — the day before the daily spine drops into its terminal zero tail.
// A closing constituent's flows dated after this day land where the series
// already reads ~0 (no visible ΔV), so under the default ClosureScope they are
// subsumed as strays (the closure mirror of pre-debut); ClosureLedgerExact
// keeps them as the real exit legs. Returns the first day for an all-zero
// series.
func (a *accountData) lastNonzeroDay() int64 {
	for i := len(a.series) - 1; i >= 0; i-- {
		if math.Abs(a.series[i].val) >= valueTol {
			return a.series[i].day
		}
	}
	return a.firstDay()
}

func loadAccountData(ctx context.Context, db *sql.DB, outCcy string, ov map[string]ReturnsPolicyOverride) (map[string]*accountData, error) {
	byKey := map[string]*accountData{}

	kinds, err := SourceKinds(ctx, db)
	if err != nil {
		return nil, err
	}
	pfNames, err := loadPortfolioNames(ctx, db)
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx,
		`SELECT as_of_day, silver_source_id, account_external_id, account_kind,
		        display_name, base_currency, portfolio_external_id, total_value_outccy
		   FROM report_accounts_history(?)
		  ORDER BY silver_source_id, account_external_id, as_of_day`, outCcy)
	if err != nil {
		return nil, fmt.Errorf("RunReturns history: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			asOf            int64
			src, acct, kind string
			label, base, pf sql.NullString
			tot             sql.NullString
		)
		if err := rows.Scan(&asOf, &src, &acct, &kind, &label, &base, &pf, &tot); err != nil {
			return nil, fmt.Errorf("RunReturns scan: %w", err)
		}
		appendSeries(byKey, kinds, pfNames, ov, src, acct, kind, label, base, pf, asOf/86400, tot)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if err := loadSnapshotDays(ctx, db, byKey); err != nil {
		return nil, err
	}
	return byKey, nil
}

// newAccountData builds the currency-independent metadata record for an
// account (kind, portfolio, base currency, label, and the resolved
// ReturnsPolicy). The value series and flows are attached afterwards, per
// currency. This is the single policy-resolution point: the config-side
// override (keyed by silver_source_id, not adapter kind) composes here, so
// every downstream knob site — a.policy and a.rpolicy alike — sees the
// overridden policy at every grain.
func newAccountData(kinds, pfNames map[string]string, ov map[string]ReturnsPolicyOverride, src, acct, kind string, label, base, pf sql.NullString) *accountData {
	rp, _ := returns.ReturnsPolicyFor(kinds[src])
	rp = applyPolicyOverride(rp, ov, src)
	a := &accountData{src: src, acct: acct, kind: kind, policy: rp.Flow, rpolicy: rp}
	a.portfolio = pf.String
	a.portfolioName = pfNames[acctKey(src, pf.String)]
	a.baseCurrency = base.String
	a.label = acct
	if label.Valid && label.String != "" {
		a.label = label.String
	}
	return a
}

// returnsInvisibleKind reports whether an account_kind contributes nothing to
// any return series, at any grain. Only `card` is: a revolving-credit liability
// is a spending instrument, not an investment — its balance swings are purchases
// and payments, and running them through TWR/MWR would report shopping as
// performance. Excluding it at the loader (rather than zeroing it later) is what
// makes the exclusion total:
//
//   - the account never enters the accounts map, so no grain — accounts,
//     portfolios, sources, global — can enumerate it;
//   - attachOneFlow's nil-account gate then drops every card flow, so purchases,
//     refunds, card payments and rewards are not external flows anywhere;
//   - transfer-matching candidacy requires attachment, so a card leg can never
//     net against a real transfer and silently erase it;
//   - loadSnapshotDays and matchCrossTransfers already skip unknown accounts,
//     and groupAccounts / the materializer only ever walk the map.
//
// Deliberately NOT a ReturnsPolicy knob: policies are keyed by adapter kind and
// default to a no-op, so a card arriving from a source with no registered policy
// would leak into the aggregates. The rule belongs to the engine and keys on the
// account kind itself.
//
// Consequence of record: with cards loaded, the returns global no longer equals
// `report_global` — the returns value spine deliberately omits them. Same
// precedent as mortgages and other liabilities, which the rollups already drop
// (splitLiabilities); unlike a mortgage, a card is not even reported on the
// liability line, because it produces no row at all.
func returnsInvisibleKind(kind string) bool {
	return kind == string(canonical.AccountKindCard)
}

// appendSeries adds one account-day value to byKey, creating the account record
// on first sight. An unpriceable day (NULL total — no FX path) is omitted from
// the series, matching the single- and multi-currency loaders. Rows must arrive
// in ascending day order per account (the loaders' ORDER BY guarantees it), so
// each series is ascending for valueAt's binary search.
//
// This is also the single seam where credit cards leave the returns engine (see
// returnsInvisibleKind): the account is never created, so nothing downstream can
// see it.
func appendSeries(byKey map[string]*accountData, kinds, pfNames map[string]string, ov map[string]ReturnsPolicyOverride, src, acct, kind string, label, base, pf sql.NullString, day int64, tot sql.NullString) {
	if returnsInvisibleKind(kind) {
		return
	}
	v, ok := parseFloat(tot)
	if !ok {
		return
	}
	k := acctKey(src, acct)
	a := byKey[k]
	if a == nil {
		a = newAccountData(kinds, pfNames, ov, src, acct, kind, label, base, pf)
		byKey[k] = a
	}
	a.series = append(a.series, dayVal{day: day, val: v})
}

// applyPolicyOverride composes a source's config-side override (if any) on
// top of its registered policy. A FlowRegime override swaps in the regime's
// canonical FlowPolicy wholesale (kind sets included); unset fields leave the
// registered values untouched.
func applyPolicyOverride(rp returns.ReturnsPolicy, ov map[string]ReturnsPolicyOverride, src string) returns.ReturnsPolicy {
	o, ok := ov[src]
	if !ok {
		return rp
	}
	if o.FlowRegime != nil {
		rp.Flow = returns.FlowPolicyForRegime(*o.FlowRegime)
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
// independent, so the multi-currency loader passes all three per-currency maps
// and this scans once, distributing to whichever map holds the account.
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

// attachFlows loads transactions once and distributes the policy-external ones
// to their accounts, tagging each flow with its source id (for deterministic
// netting) and flagging any valued at the day-0 clamped FX rate. With
// transfer matching enabled it also collects the attached flows as match
// candidates and links cross-source pairs.
func attachFlows(ctx context.Context, db *sql.DB, outCcy string, byKey map[string]*accountData, fx fxBounds, tm *TransferMatching) error {
	txns, err := loadFlowTransactions(ctx, db, outCcy)
	if err != nil {
		return err
	}
	var cands []crossCandidate
	for _, t := range txns {
		attached := attachOneFlow(byKey, fx, outCcy, t.src, t.acct,
			canonical.TxKind(t.kind), t.occurredAt, t.txID, t.ccy, t.valueOut,
			t.returnsInternal)
		if attached && tm != nil {
			cands = appendCrossCandidate(cands, t.src, t.acct, t.txID, t.ccy, t.occurredAt, t.netAmt)
		}
	}
	matchCrossTransfers(cands, tm, byKey)
	return nil
}

// flowTxnRow is the lean projection of report_transactions the returns engine
// needs: only the fields attachOneFlow and the transfer matcher read, not the
// 20-column display row TransactionsBetween builds. Selecting just these lets
// DuckDB prune the macro's account/instrument LEFT JOINs, which the flow path
// never consults.
type flowTxnRow struct {
	src, acct  string
	occurredAt int64
	kind       string
	ccy        string
	txID       string
	valueOut   *string
	netAmt     *string // native-currency net amount (transfer matching)
	// returnsInternal is the adapter's own verdict that this row is
	// conduit churn rather than owner capital — read only under the
	// policy's ExternalOnly (see attachOneFlow).
	returnsInternal bool
}

// loadFlowTransactions reads every transaction's flow-relevant fields in the
// report_transactions macro's own total order (occurred_at, silver_source_id,
// transaction_external_id) — the same order TransactionsBetween's ascending
// path relies on, so the flow sequence is identical to the wide loader.
func loadFlowTransactions(ctx context.Context, db *sql.DB, outCcy string) ([]flowTxnRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT r.silver_source_id, r.account_external_id, r.occurred_at, r.kind,
		        r.currency, r.transaction_external_id, r.value_outccy, r.net_amount,
		        COALESCE(t.payload ->> 'returns_flow' = 'internal', FALSE) AS returns_internal
		   FROM report_transactions(?, ?, ?) r
		   LEFT JOIN transactions t
		          ON t.silver_source_id         = r.silver_source_id
		         AND t.transaction_external_id  = r.transaction_external_id
		  ORDER BY r.occurred_at, r.silver_source_id, r.transaction_external_id`,
		int64(0), MaxEpoch, outCcy)
	if err != nil {
		return nil, fmt.Errorf("attachFlows transactions: %w", err)
	}
	defer rows.Close()
	var out []flowTxnRow
	for rows.Next() {
		var (
			r                flowTxnRow
			valueOut, netAmt sql.NullString
		)
		if err := rows.Scan(&r.src, &r.acct, &r.occurredAt, &r.kind, &r.ccy, &r.txID,
			&valueOut, &netAmt, &r.returnsInternal); err != nil {
			return nil, fmt.Errorf("attachFlows transactions scan: %w", err)
		}
		r.valueOut = trimmedDecimalPtr(valueOut)
		r.netAmt = trimmedDecimalPtr(netAmt)
		out = append(out, r)
	}
	return out, rows.Err()
}

// attachOneFlow classifies one transaction as an external flow for its account
// in byKey (a no-op if the account isn't loaded), shared by the single- and
// multi-currency loaders, and reports whether a flow was attached (the
// transfer matcher's candidate universe — only attached flows can pair).
// valueOut is the transaction's net amount already converted to outCcy (nil ⇒
// unresolved FX ⇒ not a flow); txCcy is the transaction's own currency, for
// the day-0 clamp check.
func attachOneFlow(byKey map[string]*accountData, fx fxBounds, outCcy, src, acct string, kind canonical.TxKind, occurredAt int64, txID, txCcy string, valueOut *string, returnsInternal bool) bool {
	a := byKey[acctKey(src, acct)]
	if a == nil {
		return false
	}
	// The silver-side pre-tag ExternalOnly describes: an adapter that can
	// tell owner capital from conduit churn says so on the row. It is read
	// only under ExternalOnly, so a source that tags nothing is unaffected,
	// and it replaces the older convention of rewriting the row's KIND —
	// which the spending population reads too, and which therefore could
	// not carry a returns-only verdict without erasing the row from
	// spending as well.
	if a.rpolicy.ExternalOnly && returnsInternal {
		return false
	}
	if a.policy.Regime == returns.RegimeCryptoPartial &&
		(kind == canonical.TxKindTransferIn || kind == canonical.TxKindTransferOut) {
		a.cryptoExcluded = true
		return false
	}
	if !a.policy.IsExternal(kind) {
		return false
	}
	val, ok := parseFloatPtr(valueOut)
	if !ok {
		return false // unresolved FX on the flow — skip (documented limitation)
	}
	// ExternalOnly gates the ClassifyFlow hook: a non-nil hook may drop a
	// flow as internal; ExternalOnly without a hook is inert here — a
	// silver-side pre-tagging contract (see ReturnsPolicy.ExternalOnly).
	// Source-scoped via a.rpolicy.
	if a.rpolicy.ExternalOnly && a.rpolicy.ClassifyFlow != nil {
		if a.rpolicy.ClassifyFlow(returns.FlowCtx{Kind: kind, Amount: canonical.NewDecimalFromFloat(val)}) == returns.FlowInternal {
			return false
		}
	}
	day := occurredAt / 86400
	if kind == canonical.TxKindJournal {
		a.journalPresent = true
	}
	if fx.clampedBefore(txCcy, outCcy, day) {
		a.hasClampedFlow = true
	}
	f := returns.Flow{Day: day, Amount: val, ID: txID}
	if a.policy.IsTransferLike(kind) {
		a.transferLike = append(a.transferLike, f)
	} else {
		a.nonTransfer = append(a.nonTransfer, f)
	}
	return true
}

// crossCandidate is one attached external flow in the transfer matcher's
// candidate pool, carrying the transaction's NATIVE currency and amount:
// native amounts are identical across the three output-currency datasets, so
// every currency partition derives the same pairings (matching on converted
// amounts would let day-gap FX drift pair differently per partition).
type crossCandidate struct {
	src, acct, txID string
	day             int64
	ccy             string
	amt             float64
}

// appendCrossCandidate adds an attached flow to the matcher's candidate pool.
// Skipped: equity-transfer ledger legs (`xfer:` ids — an intentionally
// recorded pair the transferLike netter already handles), rows with no native
// amount, and zero amounts (nothing to pair).
func appendCrossCandidate(cands []crossCandidate, src, acct, txID, ccy string, occurredAt int64, netAmt *string) []crossCandidate {
	if strings.HasPrefix(txID, "xfer:") {
		return cands
	}
	amt, ok := parseFloatPtr(netAmt)
	if !ok || amt == 0 {
		return cands
	}
	return append(cands, crossCandidate{
		src: src, acct: acct, txID: txID, day: occurredAt / 86400, ccy: ccy, amt: amt,
	})
}

// matchCrossTransfers links opposite-sign external legs across DIFFERENT
// sources — same native currency, equal amount within the tolerance, within
// the day window — writing symmetric crossLink entries onto both owning
// accounts. The pairing itself is MatchTransferLegs (see transfermatch.go for
// the ranking and determinism guarantees); this is the returns binding of it,
// and it owns two side effects the engine depends on: the crossLink
// bookkeeping, and the transferLikeIDs index that crossMatchedDrops reads.
//
// Same-source pairs are out of scope (CrossGroupOnly): within a source the
// silver classifier and the transferLike netter own internality. Every
// attached flow is eligible, so no kind filter is passed — appendCrossCandidate
// has already shaped the pool. Candidates carry NATIVE amounts, so the currency
// partitions derive identical pairs whenever their attached-flow universes
// coincide (a leg whose FX is unresolved in some partition is a candidate only
// where it attached and can shift greedy pairings there).
//
// The links are only POTENTIAL internality: entityFlows nets a pair strictly
// when both legs are live members of the same entity window, so finer grains
// keep counting each leg as the boundary flow it is for them.
func matchCrossTransfers(cands []crossCandidate, tm *TransferMatching, byKey map[string]*accountData) {
	if tm == nil || len(cands) < 2 {
		return
	}
	legs := make([]TransferLeg, len(cands))
	for i, c := range cands {
		legs[i] = TransferLeg{Group: c.src, Owner: c.acct, ID: c.txID, Day: c.day, Ccy: c.ccy, Amt: c.amt}
	}
	// link records `to` as the counterparty of `from`'s leg. Accounts the
	// returns engine never loaded (a card, an excluded kind) are silently
	// skipped — a leg can be a candidate without its account being present.
	link := func(from, to TransferLeg) {
		acc := byKey[acctKey(from.Group, from.Owner)]
		if acc == nil {
			return
		}
		if acc.crossLinks == nil {
			acc.crossLinks = map[string]crossLink{}
		}
		acc.crossLinks[from.ID] = crossLink{src: to.Group, acct: to.Owner, txID: to.ID, day: to.Day}
	}
	overrides, _, err := ResolveTransferOverrides(tm.Rules, legs)
	if err != nil {
		// A malformed ledger is the caller's to report; here it simply
		// overrides nothing rather than taking down a returns run.
		overrides = TransferOverrides{}
	}
	for _, m := range MatchTransferLegs(legs, TransferMatchOpts{
		WindowDays:      tm.WindowDays,
		TolerancePct:    tm.TolerancePct,
		ToleranceMaxAbs: DefaultTransferFeeCap,
		CrossGroupOnly:  true,
		Overrides:       overrides,
	}) {
		link(m.Debit, m.Credit)
		link(m.Credit, m.Debit)
	}
	indexLinkedTransferLike(byKey)
}

// indexLinkedTransferLike indexes the transferLike slice of every account that
// carries cross-source links, so the per-window liveness check in
// crossMatchedDrops can tell which slice a partner's leg lives in without
// scanning it. Accounts without links stay unindexed (the map is sparse), and
// an existing index is left alone.
func indexLinkedTransferLike(byKey map[string]*accountData) {
	for _, acc := range byKey {
		if len(acc.crossLinks) == 0 || acc.transferLikeIDs != nil {
			continue
		}
		acc.transferLikeIDs = make(map[string]bool, len(acc.transferLike))
		for _, f := range acc.transferLike {
			acc.transferLikeIDs[f.ID] = true
		}
	}
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

// EpochDay floors a Unix-seconds timestamp to its epoch day. Floor
// rather than truncate so a pre-1970 timestamp bands with the day it
// belongs to instead of the one after.
func EpochDay(sec int64) int64 {
	if sec < 0 {
		return -((-sec + SecondsPerDay - 1) / SecondsPerDay)
	}
	return sec / SecondsPerDay
}

// ---- grouping ------------------------------------------------------------

func groupAccounts(level string, accts map[string]*accountData, excl *ReturnsExclude) (map[string][]*accountData, []string) {
	groups := map[string][]*accountData{}
	for _, a := range accts {
		if excl.excluded(level, a.src, a.portfolio, a.acct) {
			continue // omitted from this (higher) grain; still shown at its own grain
		}
		var k string
		switch level {
		case "accounts":
			k = acctKey(a.src, a.acct)
		case "sources":
			k = a.src
		case "portfolios":
			k = acctKey(a.src, a.portfolio)
		default: // global
			k = "global"
		}
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
		if m.kind == string(canonical.AccountKindMortgage) {
			liability = append(liability, m)
		} else {
			assets = append(assets, m)
		}
	}
	return
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

func decStr(v float64) *string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	return &s
}
