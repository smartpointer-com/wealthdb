package gold

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// SourceRow is one row of the `wealthdb sources` output: every
// account of a silver source rolled into a single line — the
// source-grain rollup sitting between `wealthdb accounts` /
// `portfolios` (finer) and `wealthdb global` (the whole portfolio).
//
// Aggregation, the base/taxonomy rollup, FX, and ordering are all
// the report_sources macro (migration 0025); this is the scan.
// sum(sources.total_<CCY>) == sum(accounts.total_<CCY>) ==
// sum(portfolios.total_<CCY>) by construction.
type SourceRow struct {
	SilverSourceID string

	// BaseCurrency / TaxWrapper / ManagementStyle are rolled up
	// from the source's non-overlay accounts (agree-or-NULL, NULL
	// base treated as default for the taxonomy check). Non-nil only
	// when all qualifying accounts agree — blank genuinely means
	// "mixed / unknown", not a render-time default.
	BaseCurrency    *string
	TaxWrapper      *string
	ManagementStyle *string

	// SnapshotAt is the source's latest snapshot ≤ the as-of date;
	// 0 when the source has no data as of that date.
	SnapshotAt int64

	// Aggregates in the source's rolled base currency (nil when the
	// source's accounts have no agreed base currency).
	PositionsValueBase *string
	CashBalanceBase    *string
	TotalValueBase     *string

	// Aggregates in the requested output currency (nil when no FX
	// path resolves the source's lines to outCcy).
	PositionsValueOutCcy *string
	CashBalanceOutCcy    *string
	TotalValueOutCcy     *string
}

// SourcesAsOf returns one SourceRow per silver source that has at
// least one account, each summing that source's own positions and
// cash. All aggregation + FX is the report_sources macro; this is
// the scan. Rows are ordered by silver_source_id.
func SourcesAsOf(ctx context.Context, db *sql.DB, asOf int64, outCcy string, mode canonical.FxMode) ([]SourceRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT * FROM report_sources(?, ?)`, effectiveAsOf(asOf, mode), outCcy)
	if err != nil {
		return nil, fmt.Errorf("SourcesAsOf: %w", err)
	}
	defer rows.Close()

	var out []SourceRow
	for rows.Next() {
		var (
			r                              SourceRow
			baseCcy, taxWrapper, mgmtStyle sql.NullString
			pvb, cvb, tvb, pvo, cvo, tvo   sql.NullString
		)
		if err := rows.Scan(
			&r.SilverSourceID, &baseCcy, &taxWrapper, &mgmtStyle, &r.SnapshotAt,
			&pvb, &cvb, &tvb, &pvo, &cvo, &tvo,
		); err != nil {
			return nil, fmt.Errorf("SourcesAsOf scan: %w", err)
		}
		r.BaseCurrency = nullStringToPtr(baseCcy)
		r.TaxWrapper = nullStringToPtr(taxWrapper)
		r.ManagementStyle = nullStringToPtr(mgmtStyle)
		r.PositionsValueBase = trimmedDecimalPtr(pvb)
		r.CashBalanceBase = trimmedDecimalPtr(cvb)
		r.TotalValueBase = trimmedDecimalPtr(tvb)
		r.PositionsValueOutCcy = trimmedDecimalPtr(pvo)
		r.CashBalanceOutCcy = trimmedDecimalPtr(cvo)
		r.TotalValueOutCcy = trimmedDecimalPtr(tvo)
		out = append(out, r)
	}
	return out, rows.Err()
}
