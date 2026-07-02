package gold

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/returns"
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
	Netting   bool   // net heuristically-matched internal transfers at coarse grains
	Inception string // full | strict
}

// netting tolerances (proposal §2.2 / locked decision 5).
const (
	nettingEpsFloor  = 1.00  // absolute floor, output currency
	nettingEpsRel    = 0.005 // 0.5% of the larger leg
	nettingWindowDay = 3     // ±3 calendar days
)

// RunReturns computes returns at the requested grain. All grains are built from
// the per-account value spine (report_accounts_history) aggregated in Go, so the
// staggered-inception synthetic-onboarding mechanism applies uniformly.
func RunReturns(ctx context.Context, db *sql.DB, p ReturnParams) ([]ReturnRow, error) {
	fx, err := loadFxBounds(ctx, db)
	if err != nil {
		return nil, err
	}
	accts, err := loadAccountData(ctx, db, p.OutCcy)
	if err != nil {
		return nil, err
	}
	if err := attachFlows(ctx, db, p.OutCcy, accts, fx); err != nil {
		return nil, err
	}

	// The spine's latest emitted day across all accounts (≈ today). An account
	// whose own series ends before this dropped out of a later same-source
	// snapshot (closed / feed-dropped) — its value is 0 thereafter (matching the
	// macro), and we flag it if it dropped while still holding value (review #1).
	var globalMax int64
	for _, a := range accts {
		if d := a.lastDay(); d > globalMax {
			globalMax = d
		}
	}
	for _, a := range accts {
		if a.lastDay() < globalMax && math.Abs(a.lastVal()) > valueTol {
			a.droppedNonzero = true
		}
	}

	// Never value past the latest available data (also guards a future ToEpoch).
	toDay := p.ToEpoch / 86400
	if toDay > globalMax {
		toDay = globalMax
	}

	groups, order := groupAccounts(p.Level, accts)
	var out []ReturnRow
	for _, key := range order {
		members := groups[key]
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
		rows := computeEntityReturn(assets, p, toDay, fx)
		// AccountsGrainMeaningless: per-wallet (accounts-grain) rows for a crypto-
		// sweep source are economically meaningless (coins sweep between wallets on
		// arrival), so keep their start/end values but blank TWR/MWR to n/a and flag
		// it. Gate STRICTLY on the group's per-constituent rpolicy so ONLY that
		// source's wallet rows change; the portfolios/sources/global grains are NEVER
		// gated (they aggregate coherent units, which ARE valid). One row per wallet
		// is still emitted.
		if p.Level == "accounts" && accountsGrainMeaningless(assets) {
			for i := range rows {
				rows[i].TWR, rows[i].TWRAnnualized = nil, nil
				rows[i].MWR, rows[i].MWRAnnualized = nil, nil
				rows[i].Quality = dedupeStrings(append(rows[i].Quality, "accounts_grain_meaningless"))
			}
		}
		out = append(out, rows...)
	}
	return out, nil
}

// accountsGrainMeaningless reports whether the group's constituent policy marks
// the per-wallet (accounts) grain meaningless (AccountsGrainMeaningless). At the
// accounts grain every constituent of a group shares one source (groupAccounts
// keys on src), so the whole group carries one rpolicy; reading the first
// constituent is exact and source-scoped. Default policy (false) leaves every
// other source untouched.
func accountsGrainMeaningless(assets []*accountData) bool {
	return len(assets) > 0 && assets[0].rpolicy.AccountsGrainMeaningless
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
	rpolicy        returns.ReturnsPolicy // full per-source policy incl. forward knobs (OnboardScope/Inception/ConduitKinds/ExternalOnly)
	nonTransfer    []returns.Flow        // external deposit/withdrawal — always kept (never netted)
	transferLike   []returns.Flow        // transfer_in/out/journal — netting candidates at coarse grains
	journalPresent bool
	cryptoExcluded bool
	hasClampedFlow bool // a flow was valued at the migration-0023 day-0 clamped FX rate
	droppedNonzero bool // dropped out of a later same-source snapshot while still holding value
}

// allExternal returns every policy-external flow (used at the accounts grain,
// where there is nothing to net).
func (a *accountData) allExternal() []returns.Flow {
	out := make([]returns.Flow, 0, len(a.nonTransfer)+len(a.transferLike))
	out = append(out, a.nonTransfer...)
	out = append(out, a.transferLike...)
	return out
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
// Before its first emitted row it is NULL ("not yet alive", Fix #4); AFTER its
// last emitted row it is gone — the macro stops emitting the account once a
// later same-source snapshot supersedes it without it, so carrying forward past
// the last row would diverge from report_*_history and break global == Σ accounts
// (review #1). Within [first,last] the daily spine has a row for every day.
func (a *accountData) valueAt(day int64) (float64, bool) {
	if len(a.series) == 0 || day < a.series[0].day || day > a.series[len(a.series)-1].day {
		return 0, false
	}
	i := sort.Search(len(a.series), func(k int) bool { return a.series[k].day > day })
	return a.series[i-1].val, true
}

// closureDay returns the explicit-closure day (the account's value went to ~0)
// or 0 when it merely stopped updating (staleness must NOT synthesize a
// divestment — proposal §2.7/§3).
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
// non-zero — the day on which the synthetic closure outflow's lastValue was
// established. The value is flat (carried) from here to the zeroing closure day,
// so a closing constituent's drains dated after this day have no visible ΔV and
// are subsumed by the closure outflow (the closure mirror of pre-debut). Returns
// the first day for an all-zero series.
func (a *accountData) lastNonzeroDay() int64 {
	for i := len(a.series) - 1; i >= 0; i-- {
		if math.Abs(a.series[i].val) >= valueTol {
			return a.series[i].day
		}
	}
	return a.firstDay()
}

func loadAccountData(ctx context.Context, db *sql.DB, outCcy string) (map[string]*accountData, error) {
	byKey := map[string]*accountData{}

	rows, err := db.QueryContext(ctx,
		`SELECT as_of_day, silver_source_id, account_external_id, account_kind,
		        display_name, base_currency, portfolio_external_id, total_value_outccy
		   FROM report_accounts_history(?)
		  ORDER BY silver_source_id, account_external_id, as_of_day`, outCcy)
	if err != nil {
		return nil, fmt.Errorf("RunReturns history: %w", err)
	}
	defer rows.Close()

	kinds, err := loadSourceKinds(ctx, db)
	if err != nil {
		return nil, err
	}

	pfNames, err := loadPortfolioNames(ctx, db)
	if err != nil {
		return nil, err
	}

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
		v, ok := parseFloat(tot)
		if !ok {
			continue // unpriceable account-day (no FX path) — omit from the series
		}
		k := acctKey(src, acct)
		a := byKey[k]
		if a == nil {
			rp, _ := returns.ReturnsPolicyFor(kinds[src])
			a = &accountData{src: src, acct: acct, kind: kind, policy: rp.Flow, rpolicy: rp}
			a.portfolio = pf.String
			a.portfolioName = pfNames[acctKey(src, pf.String)]
			a.baseCurrency = base.String
			a.label = acct
			if label.Valid && label.String != "" {
				a.label = label.String
			}
			byKey[k] = a
		}
		a.series = append(a.series, dayVal{day: asOf / 86400, val: v})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if err := loadSnapshotDays(ctx, db, byKey); err != nil {
		return nil, err
	}
	return byKey, nil
}

func loadSourceKinds(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT silver_source_id, silver_kind FROM silver_sources`)
	if err != nil {
		return nil, fmt.Errorf("RunReturns source kinds: %w", err)
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

func loadSnapshotDays(ctx context.Context, db *sql.DB, byKey map[string]*accountData) error {
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
		if a := byKey[acctKey(src, acct)]; a != nil {
			a.snapDays = append(a.snapDays, day)
		}
	}
	return rows.Err()
}

// attachFlows loads transactions once and distributes the policy-external ones
// to their accounts, tagging each flow with its source id (for deterministic
// netting) and flagging any valued at the day-0 clamped FX rate.
func attachFlows(ctx context.Context, db *sql.DB, outCcy string, byKey map[string]*accountData, fx fxBounds) error {
	txns, err := TransactionsBetween(ctx, db, 0, maxEpoch, outCcy, SortAscending)
	if err != nil {
		return err
	}
	for _, t := range txns {
		a := byKey[acctKey(t.SilverSourceID, t.AccountExternalID)]
		if a == nil {
			continue
		}
		kind := canonical.TxKind(t.Kind)
		if a.policy.Regime == returns.RegimeCryptoPartial &&
			(kind == canonical.TxKindTransferIn || kind == canonical.TxKindTransferOut) {
			a.cryptoExcluded = true
			continue
		}
		if !a.policy.IsExternal(kind) {
			continue
		}
		val, ok := parseFloatPtr(t.ValueOutCcy)
		if !ok {
			continue // unresolved FX on the flow — skip (documented limitation)
		}
		// ExternalOnly: count only boundary-crossing flows; internal churn
		// (cash<->securities settlements, inter-account transfers, FX, mandate
		// funding) is NOT a flow. The engine drops an internal flow here ONLY via a
		// non-nil ClassifyFlow hook (gated by ExternalOnly); ExternalOnly alone does
		// nothing in the engine. UBS sets ExternalOnly=true but ships NO ClassifyFlow
		// — it pre-tags the classification in silver instead (internal rows are
		// emitted under a non-flow kind, so they never reach IsExternal here), which
		// keeps the counter-account / own-IBAN logic — and any PII — entirely inside
		// the collector. For UBS, therefore, ExternalOnly is a silver-side contract
		// and this branch is inert. This is source-scoped via a.rpolicy, so non-UBS
		// sources (ExternalOnly=false) are untouched.
		if a.rpolicy.ExternalOnly && a.rpolicy.ClassifyFlow != nil {
			if a.rpolicy.ClassifyFlow(returns.FlowCtx{Kind: kind, Amount: canonical.NewDecimalFromFloat(val)}) == returns.FlowInternal {
				continue
			}
		}
		day := t.OccurredAt / 86400
		if kind == canonical.TxKindJournal {
			a.journalPresent = true
		}
		if fx.clampedBefore(t.Currency, outCcy, day) {
			a.hasClampedFlow = true
		}
		f := returns.Flow{Day: day, Amount: val, ID: t.TransactionExternalID}
		if a.policy.IsTransferLike(kind) {
			a.transferLike = append(a.transferLike, f)
		} else {
			a.nonTransfer = append(a.nonTransfer, f)
		}
	}
	return nil
}

const maxEpoch = int64(1) << 62

// ---- grouping ------------------------------------------------------------

func groupAccounts(level string, accts map[string]*accountData) (map[string][]*accountData, []string) {
	groups := map[string][]*accountData{}
	switch level {
	case "accounts":
		for _, a := range accts {
			groups[acctKey(a.src, a.acct)] = []*accountData{a}
		}
	case "sources":
		for _, a := range accts {
			groups[a.src] = append(groups[a.src], a)
		}
	case "portfolios":
		for _, a := range accts {
			k := acctKey(a.src, a.portfolio)
			groups[k] = append(groups[k], a)
		}
	default: // global
		for _, a := range accts {
			groups["global"] = append(groups["global"], a)
		}
	}
	order := make([]string, 0, len(groups))
	for k := range groups {
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
