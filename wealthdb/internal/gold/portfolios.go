package gold

import (
	"context"
	"database/sql"
	"fmt"
)

// PortfolioRow is one row of the `wealthdb portfolios` output. Each
// row aggregates the positions and cash of every account whose
// portfolio_external_id matches the row's portfolio.
//
// For each silver_source that has at least one orphan account
// (portfolio_external_id IS NULL) there is additionally a sentinel
// row with PortfolioExternalID == "" aggregating those accounts.
// This is where Schwab, Swissquote, and any portfolio-less UBS
// accounts land. (Accounts whose portfolio_external_id names a
// portfolio with no row in the portfolios table are NOT bucketed —
// they appear only in `wealthdb accounts`.)
type PortfolioRow struct {
	SilverSourceID      string
	PortfolioExternalID string // empty string for the sentinel row
	DisplayName         *string
	BaseCurrency        *string
	RelationshipID      *string
	Nickname            *string

	// TaxWrapper / ManagementStyle are rolled up from the portfolio's
	// non-overlay component accounts (NULL tax_wrapper treated as
	// 'taxable_personal', NULL management_style as 'self_directed' for
	// the agreement check). Non-nil only when all components agree.
	TaxWrapper      *string
	ManagementStyle *string

	PositionsValueBase *string
	CashBalanceBase    *string
	TotalValueBase     *string

	PositionsValueOutCcy *string
	CashBalanceOutCcy    *string
	TotalValueOutCcy     *string

	// SnapshotAt is the latest snapshot_at across the portfolio's
	// lines; falls back to the silver source's latest snapshot when
	// the portfolio has no lines.
	SnapshotAt int64
}

// PortfoliosAsOf returns one PortfolioRow per registered portfolio
// plus a per-source sentinel (PortfolioExternalID == "") catching
// accounts with no registered portfolio — both NULL and an
// unregistered portfolio_external_id (migration 0023). Aggregation,
// taxonomy rollup, sentinel generation, FX, and ordering are all the
// report_portfolios macro (migrations 0021/0023); this is the scan.
// Rows are ordered by (silver_source_id, portfolio_external_id) — ""
// sorts first within each source.
func PortfoliosAsOf(ctx context.Context, db *sql.DB, asOf int64, outCcy string) ([]PortfolioRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT * FROM report_portfolios(?, ?)`, asOf, outCcy)
	if err != nil {
		return nil, fmt.Errorf("PortfoliosAsOf: %w", err)
	}
	defer rows.Close()

	var out []PortfolioRow
	for rows.Next() {
		var (
			r                                     PortfolioRow
			displayName, baseCcy, relID, nickname sql.NullString
			taxWrapper, mgmtStyle                 sql.NullString
			pvb, cvb, tvb, pvo, cvo, tvo          sql.NullString
		)
		if err := rows.Scan(
			&r.SilverSourceID, &r.PortfolioExternalID,
			&displayName, &baseCcy, &relID, &nickname,
			&taxWrapper, &mgmtStyle, &r.SnapshotAt,
			&pvb, &cvb, &tvb, &pvo, &cvo, &tvo,
		); err != nil {
			return nil, fmt.Errorf("PortfoliosAsOf scan: %w", err)
		}
		r.DisplayName = nullStringToPtr(displayName)
		r.BaseCurrency = nullStringToPtr(baseCcy)
		r.RelationshipID = nullStringToPtr(relID)
		r.Nickname = nullStringToPtr(nickname)
		r.TaxWrapper = nullStringToPtr(taxWrapper)
		r.ManagementStyle = nullStringToPtr(mgmtStyle)
		r.PositionsValueBase, r.CashBalanceBase, r.TotalValueBase,
			r.PositionsValueOutCcy, r.CashBalanceOutCcy, r.TotalValueOutCcy =
			sixAggPtrs(pvb, cvb, tvb, pvo, cvo, tvo)
		out = append(out, r)
	}
	return out, rows.Err()
}
