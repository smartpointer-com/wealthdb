package gold

import (
	"context"
	"database/sql"
)

// AccountRow is one row of the `wealthdb holdings accounts` output: the
// gold `accounts` row's promoted columns plus derived aggregate
// columns (positions / cash / total in the account's base currency
// and in the requested output currency), computed by the
// report_accounts table macro (migration 0024).
//
// All decimals come back as their canonical string form. Aggregate
// pointers are nil when:
//
//   - the *Base aggregates: the account has no base_currency.
//   - the *OutCcy aggregates: the account has lines but no FX path
//     resolves to the requested output currency (the same per-line
//     "best-effort, show the hole" semantics as the positions
//     table's value_<CCY> column).
//
// An account with no contributing lines reports "0" (not nil).
type AccountRow struct {
	SilverSourceID      string
	AccountExternalID   string
	AccountKind         string
	DisplayName         *string
	BaseCurrency        *string
	RelationshipID      *string
	Nickname            *string
	AccountCategory     *string
	PortfolioExternalID *string
	// TaxWrapper / ManagementStyle: nil when neither adapter nor a
	// config override supplied a value. Downstream readers treat nil
	// tax_wrapper as 'taxable_personal' and nil management_style as
	// 'self_directed' for default-aware display.
	TaxWrapper      *string
	ManagementStyle *string
	// SnapshotAt is the latest snapshot_at across all contributing
	// lines; falls back to the silver source's latest snapshot for
	// accounts with no lines; 0 only when the source has no data.
	SnapshotAt int64

	// Aggregates in the account's own base_currency (nil when
	// BaseCurrency is nil).
	PositionsValueBase *string
	CashBalanceBase    *string
	TotalValueBase     *string

	// Aggregates in the requested output currency (nil when no FX
	// path is available for the account's lines).
	PositionsValueOutCcy *string
	CashBalanceOutCcy    *string
	TotalValueOutCcy     *string
}

// AccountsAsOf returns one AccountRow per gold account, each
// reporting its OWN positions and cash (no cross-account rollup), so
// summing the accounts equals the full positions-+-cash total
// exactly once. All aggregation + FX is the report_accounts macro;
// this is the scan. See docs/DESIGN.md.
func AccountsAsOf(ctx context.Context, db *sql.DB, asOf int64, outCcy string) ([]AccountRow, error) {
	return scanRows(ctx, db, "AccountsAsOf", `SELECT * FROM report_accounts(?, ?)`,
		[]any{asOf, outCcy}, func(a *AccountRow) []any {
			return []any{
				&a.SilverSourceID, &a.AccountExternalID, &a.AccountKind,
				str(&a.DisplayName), str(&a.BaseCurrency), str(&a.RelationshipID), str(&a.Nickname),
				str(&a.AccountCategory), str(&a.PortfolioExternalID), str(&a.TaxWrapper), str(&a.ManagementStyle),
				&a.SnapshotAt,
				dec(&a.PositionsValueBase), dec(&a.CashBalanceBase), dec(&a.TotalValueBase),
				dec(&a.PositionsValueOutCcy), dec(&a.CashBalanceOutCcy), dec(&a.TotalValueOutCcy),
			}
		})
}
