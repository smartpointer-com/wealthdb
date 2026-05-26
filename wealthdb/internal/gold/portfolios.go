package gold

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
)

// PortfolioRow is one row of the `wealthdb portfolios` output.
// Each row aggregates the positions and cash of every account
// whose portfolio_external_id matches the row's portfolio.
//
// For each silver_source there is additionally a sentinel row
// with PortfolioExternalID == "" that aggregates every account
// in that source whose portfolio_external_id is NULL. This is
// where Schwab, Swissquote, and any portfolio-less UBS accounts
// land — the invariant
//
//   sum(portfolios.total_value_<CCY>) == sum(accounts.total_value_<CCY>)
//                                     == positions --with-cash total
//
// holds precisely because of the sentinel.
type PortfolioRow struct {
	SilverSourceID      string
	PortfolioExternalID string // empty string for the sentinel row
	DisplayName         *string
	BaseCurrency        *string
	RelationshipID      *string
	Nickname            *string

	// TaxWrapper is the portfolio-level wrapper rolled up from
	// its component accounts. STRICT semantics: non-nil only when
	// every component account carries a non-NULL tax_wrapper AND
	// all values agree. Any disagreement OR any unclassified
	// component renders the portfolio's wrapper as NULL — the
	// column is sensitive enough that "ambiguous" is preferable
	// to "possibly wrong".
	TaxWrapper *string
	// ManagementStyle answers "what mandate is this portfolio
	// under?". Computed from non-overlay component accounts only
	// (overlay accounts are synthetic per-portfolio buckets the
	// UBS adapter emits for forward contracts and OTC positions
	// the bank attributes directly to the portfolio with no sub-
	// account; their per-row management_style stays self_directed
	// even when the surrounding mandate is discretionary, so they
	// shouldn't poison the rollup). Non-nil only when every non-
	// overlay component has a non-NULL management_style AND all
	// values agree. The propagation pass in the UBS adapter
	// already lifts the safekeeping-account mandate to its sibling
	// cash accounts, so the named-mandate portfolios naturally
	// collapse to one style; "general banking" portfolios with
	// residual advisory securities resolve to NULL (correct — no
	// single mandate covers everything).
	ManagementStyle *string

	PositionsValueBase *string
	CashBalanceBase    *string
	TotalValueBase     *string

	PositionsValueOutCcy *string
	CashBalanceOutCcy    *string
	TotalValueOutCcy     *string

	// SnapshotAt is the latest snapshot_at across all lines that
	// rolled into this portfolio (positions + cash across every
	// child account). When no lines rolled into the portfolio,
	// falls back to the silver_source's latest observed snapshot
	// — an empty portfolio at a known silver snapshot is honestly
	// zero, not "unknown". 0 only when the silver source has
	// produced no data at all.
	SnapshotAt int64
}

// PortfoliosAsOf returns one PortfolioRow per registered portfolio
// in gold.portfolios, plus one sentinel row per silver_source that
// aggregates accounts whose portfolio_external_id is NULL.
// Aggregates re-use PositionsAsOf + CashAsOf and ConvertValue, in
// the same shape as AccountsAsOf — base columns use the
// portfolio's own base_currency (NULL when unknown, including
// always for sentinel rows), the _outCcy columns always populate
// when at least one underlying line resolves an FX path.
func PortfoliosAsOf(ctx context.Context, db *sql.DB, asOf int64, outCcy string, mode canonical.FxMode) ([]PortfolioRow, error) {
	portfolios, err := loadPortfolioBase(ctx, db)
	if err != nil {
		return nil, err
	}
	// Account → portfolio_external_id (or "") mapping per source.
	accountPortfolio, sourcesSeen, err := loadAccountPortfolioMap(ctx, db)
	if err != nil {
		return nil, err
	}
	// Portfolio-level rolled-up taxonomy (tax_wrapper +
	// management_style). Pulled in a single SQL pass; the per-
	// portfolio values either agree across all qualifying
	// component accounts (and become the rollup) or any
	// disagreement / NULL component leaves the rollup nil.
	taxonomy, err := loadPortfolioTaxonomy(ctx, db)
	if err != nil {
		return nil, err
	}
	// Every silver source gets a sentinel portfolio row, even
	// sources whose only accounts happen to all be inside a
	// portfolio (the sentinel will report zero). Caller can filter.
	for src := range sourcesSeen {
		portfolios = append(portfolios, PortfolioRow{SilverSourceID: src})
	}

	positions, err := PositionsAsOf(ctx, db, asOf)
	if err != nil {
		return nil, err
	}
	cash, err := CashAsOf(ctx, db, asOf)
	if err != nil {
		return nil, err
	}

	type portKey struct{ src, id string } // id "" = sentinel
	type lines struct {
		positions []lineItem
		cash      []lineItem
		maxSnap   int64
	}
	byKey := make(map[portKey]*lines)
	// Per-silver_source latest observation, mirroring AccountsAsOf.
	// Used as the snapshot_at fallback for portfolios (and the
	// sentinel row) that have no contributing lines — honestly
	// zero at the source's known snapshot, not "unknown".
	sourceMaxSnap := make(map[string]int64)
	addLine := func(src, acctID, ccy string, valueStr *string, snap int64, isCash bool) {
		if snap > sourceMaxSnap[src] {
			sourceMaxSnap[src] = snap
		}
		if valueStr == nil {
			return
		}
		v, err := canonical.NewDecimalFromString(*valueStr)
		if err != nil {
			return
		}
		// Route the line to the portfolio its account belongs to
		// (or to the sentinel if the account has no portfolio).
		portID := accountPortfolio[[2]string{src, acctID}]
		k := portKey{src, portID}
		l, ok := byKey[k]
		if !ok {
			l = &lines{}
			byKey[k] = l
		}
		item := lineItem{currency: ccy, amount: v, snapshotAt: snap}
		if isCash {
			l.cash = append(l.cash, item)
		} else {
			l.positions = append(l.positions, item)
		}
		if snap > l.maxSnap {
			l.maxSnap = snap
		}
	}
	for _, p := range positions {
		addLine(p.SilverSourceID, p.AccountExternalID, p.Currency, p.MarketValue, p.SnapshotAt, false)
	}
	for _, c := range cash {
		addLine(c.SilverSourceID, c.AccountExternalID, c.Currency, c.MarketValue, c.SnapshotAt, true)
	}

	for i := range portfolios {
		p := &portfolios[i]
		var pos, ca []lineItem
		if l, ok := byKey[portKey{p.SilverSourceID, p.PortfolioExternalID}]; ok {
			pos, ca = l.positions, l.cash
			p.SnapshotAt = l.maxSnap
		} else {
			p.SnapshotAt = sourceMaxSnap[p.SilverSourceID]
		}
		if t, ok := taxonomy[[2]string{p.SilverSourceID, p.PortfolioExternalID}]; ok {
			if t.taxWrapper != "" {
				v := t.taxWrapper
				p.TaxWrapper = &v
			}
			if t.managementStyle != "" {
				v := t.managementStyle
				p.ManagementStyle = &v
			}
		}

		if p.BaseCurrency != nil && *p.BaseCurrency != "" {
			base := *p.BaseCurrency
			pSum := sumConverted(ctx, db, pos, base, mode)
			cSum := sumConverted(ctx, db, ca, base, mode)
			p.PositionsValueBase = decimalPtrString(pSum)
			p.CashBalanceBase = decimalPtrString(cSum)
			p.TotalValueBase = decimalPtrString(addOptional(pSum, cSum))
		}
		pSum := sumConverted(ctx, db, pos, outCcy, mode)
		cSum := sumConverted(ctx, db, ca, outCcy, mode)
		p.PositionsValueOutCcy = decimalPtrString(pSum)
		p.CashBalanceOutCcy = decimalPtrString(cSum)
		p.TotalValueOutCcy = decimalPtrString(addOptional(pSum, cSum))
	}
	return portfolios, nil
}

// loadPortfolioBase reads the gold.portfolios table (no sentinel
// rows — the caller adds those). Ordered by (silver_source_id,
// portfolio_external_id) so the output is deterministic.
func loadPortfolioBase(ctx context.Context, db *sql.DB) ([]PortfolioRow, error) {
	const q = `
SELECT silver_source_id, portfolio_external_id,
       display_name, base_currency, relationship_id, nickname
  FROM portfolios
 ORDER BY silver_source_id, portfolio_external_id`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("loadPortfolioBase: %w", err)
	}
	defer rows.Close()

	var out []PortfolioRow
	for rows.Next() {
		var (
			r                                       PortfolioRow
			displayName, baseCcy, relID, nickname sql.NullString
		)
		if err := rows.Scan(&r.SilverSourceID, &r.PortfolioExternalID,
			&displayName, &baseCcy, &relID, &nickname); err != nil {
			return nil, fmt.Errorf("loadPortfolioBase scan: %w", err)
		}
		r.DisplayName = nullStringToPtr(displayName)
		r.BaseCurrency = nullStringToPtr(baseCcy)
		r.RelationshipID = nullStringToPtr(relID)
		r.Nickname = nullStringToPtr(nickname)
		out = append(out, r)
	}
	return out, rows.Err()
}

// portfolioTaxonomy carries the rolled-up tax_wrapper and
// management_style per (silver_source_id, portfolio_external_id).
// Empty string means "no rollup possible" (mixed values, or
// any qualifying component had a NULL value); the caller leaves
// the corresponding PortfolioRow field nil.
type portfolioTaxonomy struct {
	taxWrapper      string
	managementStyle string
}

// loadPortfolioTaxonomy rolls up tax_wrapper and management_style
// from gold.accounts to the portfolio level via a single SQL
// pass. Both rollups are STRICT: a value lands only when all
// qualifying component accounts agree AND no qualifying
// component is unclassified (NULL).
//
// Both rollups exclude overlay accounts. Overlays are synthetic
// per-portfolio buckets the UBS adapter emits for forward
// contracts and OTC positions the bank attributes directly to
// the portfolio with no sub-account; the user doesn't think of
// them as separate accounts. Their per-row tax_wrapper and
// management_style are unclassified (NULL) by construction —
// including them would silently poison every rollup with a
// "disagreement" that isn't real.
//
//   tax_wrapper rollup: among non-overlay accounts, every one
//     must carry a non-NULL tax_wrapper AND all values must
//     agree. Sensitive column; "ambiguous" is preferable to
//     "possibly wrong" so any disagreement or any unclassified
//     real account → NULL.
//
//   management_style rollup: same shape, among non-overlay
//     accounts. With overlays excluded, named-mandate
//     portfolios collapse to one style (the propagation pass
//     in the UBS adapter has already lifted the safekeeping
//     mandate to the sibling cash accounts); general-banking
//     portfolios with residual advisory securities resolve to
//     NULL (correct — no single mandate covers everything).
//
// Portfolios with no qualifying accounts (e.g. a portfolio
// whose only component is an overlay) return both values
// empty.
//
// Accounts whose portfolio_external_id IS NULL are NOT
// rolled into a sentinel here; sentinels are computed in the
// caller and intentionally don't carry a wrapper/style — the
// "no portfolio" bucket aggregates accounts that may have
// disparate wrappers (Schwab's 4 wrapper variants under the
// "no portfolio" Schwab sentinel being the obvious example).
func loadPortfolioTaxonomy(ctx context.Context, db *sql.DB) (map[[2]string]portfolioTaxonomy, error) {
	const q = `
SELECT
    silver_source_id,
    portfolio_external_id,
    CASE
        WHEN COUNT(*) FILTER (WHERE account_kind != 'overlay') > 0
         AND COUNT(*) FILTER (WHERE account_kind != 'overlay' AND tax_wrapper IS NULL) = 0
         AND COUNT(DISTINCT CASE WHEN account_kind != 'overlay' THEN tax_wrapper END) = 1
        THEN MAX(CASE WHEN account_kind != 'overlay' THEN tax_wrapper END)
        ELSE NULL
    END AS rolled_tax_wrapper,
    CASE
        WHEN COUNT(*) FILTER (WHERE account_kind != 'overlay') > 0
         AND COUNT(*) FILTER (WHERE account_kind != 'overlay' AND management_style IS NULL) = 0
         AND COUNT(DISTINCT CASE WHEN account_kind != 'overlay' THEN management_style END) = 1
        THEN MAX(CASE WHEN account_kind != 'overlay' THEN management_style END)
        ELSE NULL
    END AS rolled_management_style
  FROM accounts
 WHERE portfolio_external_id IS NOT NULL
 GROUP BY silver_source_id, portfolio_external_id`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("loadPortfolioTaxonomy: %w", err)
	}
	defer rows.Close()
	out := make(map[[2]string]portfolioTaxonomy)
	for rows.Next() {
		var src, port string
		var wrapper, style sql.NullString
		if err := rows.Scan(&src, &port, &wrapper, &style); err != nil {
			return nil, fmt.Errorf("loadPortfolioTaxonomy scan: %w", err)
		}
		t := portfolioTaxonomy{}
		if wrapper.Valid {
			t.taxWrapper = wrapper.String
		}
		if style.Valid {
			t.managementStyle = style.String
		}
		out[[2]string{src, port}] = t
	}
	return out, rows.Err()
}

// loadAccountPortfolioMap returns a (silver_source_id,
// account_external_id) → portfolio_external_id lookup (empty
// string when the account isn't in a portfolio) plus the set of
// distinct silver_source_id values seen.
func loadAccountPortfolioMap(ctx context.Context, db *sql.DB) (map[[2]string]string, map[string]struct{}, error) {
	const q = `
SELECT silver_source_id, account_external_id, COALESCE(portfolio_external_id, '')
  FROM accounts`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, nil, fmt.Errorf("loadAccountPortfolioMap: %w", err)
	}
	defer rows.Close()

	out := make(map[[2]string]string)
	sources := make(map[string]struct{})
	for rows.Next() {
		var src, acct, port string
		if err := rows.Scan(&src, &acct, &port); err != nil {
			return nil, nil, err
		}
		out[[2]string{src, acct}] = port
		sources[src] = struct{}{}
	}
	return out, sources, rows.Err()
}
