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
	accts, err := loadAccountData(ctx, db, p.OutCcy)
	if err != nil {
		return nil, err
	}
	if err := attachFlows(ctx, db, p.OutCcy, accts); err != nil {
		return nil, err
	}

	toDay := p.ToEpoch / 86400
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
		out = append(out, computeEntityReturn(assets, p, toDay)...)
	}
	return out, nil
}

// ---- data loading --------------------------------------------------------

type dayVal struct {
	day int64
	val float64
}

type accountData struct {
	src, acct string
	kind      string
	portfolio string // "" when none
	label     string

	series   []dayVal // carry-forward value per emitted day, ascending
	snapDays []int64  // distinct real snapshot days, ascending

	policy         returns.FlowPolicy
	nonTransfer    []returns.Flow // external deposit/withdrawal — always kept (never netted)
	transferLike   []returns.Flow // transfer_in/out/journal — netting candidates at coarse grains
	journalPresent bool
	cryptoExcluded bool
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

// valueAt returns the carry-forward value on `day` and whether the account was
// alive then (a day before its first snapshot is NULL, not 0 — Fix #4).
func (a *accountData) valueAt(day int64) (float64, bool) {
	if len(a.series) == 0 || day < a.series[0].day {
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

func loadAccountData(ctx context.Context, db *sql.DB, outCcy string) (map[string]*accountData, error) {
	byKey := map[string]*accountData{}

	rows, err := db.QueryContext(ctx,
		`SELECT as_of_day, silver_source_id, account_external_id, account_kind,
		        display_name, portfolio_external_id, total_value_outccy
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

	for rows.Next() {
		var (
			asOf            int64
			src, acct, kind string
			label, pf, tot  sql.NullString
		)
		if err := rows.Scan(&asOf, &src, &acct, &kind, &label, &pf, &tot); err != nil {
			return nil, fmt.Errorf("RunReturns scan: %w", err)
		}
		v, ok := parseFloat(tot)
		if !ok {
			continue // unpriceable account-day (no FX path) — omit from the series
		}
		k := acctKey(src, acct)
		a := byKey[k]
		if a == nil {
			a = &accountData{src: src, acct: acct, kind: kind, policy: returns.FlowPolicyFor(kinds[src])}
			a.portfolio = pf.String
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
// to their accounts.
func attachFlows(ctx context.Context, db *sql.DB, outCcy string, byKey map[string]*accountData) error {
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
		if kind == canonical.TxKindJournal {
			a.journalPresent = true
		}
		f := returns.Flow{Day: t.OccurredAt / 86400, Amount: val}
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
