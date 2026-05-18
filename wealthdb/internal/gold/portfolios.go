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

	PositionsValueBase *string
	CashBalanceBase    *string
	TotalValueBase     *string

	PositionsValueOutCcy *string
	CashBalanceOutCcy    *string
	TotalValueOutCcy     *string
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
	}
	byKey := make(map[portKey]*lines)
	addLine := func(src, acctID, ccy string, valueStr *string, snap int64, isCash bool) {
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
