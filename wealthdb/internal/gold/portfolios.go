package gold

import (
	"context"
	"database/sql"
)

// PortfolioRow is one row of the `wealthdb holdings portfolios` output. Each
// row aggregates the positions and cash of every account whose
// portfolio_external_id matches the row's portfolio.
//
// For each silver_source that has at least one orphan account there
// is additionally a sentinel row with PortfolioExternalID == ""
// aggregating those accounts. An orphan is an account with no
// portfolio_external_id, or one whose portfolio_external_id names no
// row of the portfolios table, so the portfolios sum to the accounts.
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
// report_portfolios macro (migration 0024); this is the scan.
// Rows are ordered by (silver_source_id, portfolio_external_id) — ""
// sorts first within each source.
func PortfoliosAsOf(ctx context.Context, db *sql.DB, asOf int64, outCcy string) ([]PortfolioRow, error) {
	return scanRows(ctx, db, "PortfoliosAsOf", `SELECT * FROM report_portfolios(?, ?)`,
		[]any{asOf, outCcy}, func(r *PortfolioRow) []any {
			return []any{
				&r.SilverSourceID, &r.PortfolioExternalID,
				str(&r.DisplayName), str(&r.BaseCurrency), str(&r.RelationshipID), str(&r.Nickname),
				str(&r.TaxWrapper), str(&r.ManagementStyle), &r.SnapshotAt,
				dec(&r.PositionsValueBase), dec(&r.CashBalanceBase), dec(&r.TotalValueBase),
				dec(&r.PositionsValueOutCcy), dec(&r.CashBalanceOutCcy), dec(&r.TotalValueOutCcy),
			}
		})
}
