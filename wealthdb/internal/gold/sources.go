package gold

import (
	"context"
	"database/sql"
)

// SourceRow is one row of the `wealthdb holdings sources` output:
// every account of a silver source rolled into a single line — the
// source-grain rollup sitting between `holdings accounts` /
// `holdings portfolios` (finer) and `holdings global` (the whole
// portfolio).
//
// Aggregation, the base/taxonomy rollup, FX, and ordering are all
// the report_sources macro (migration 0123); this is the scan.
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

	// ValuedAt is the latest mark that snapshot rides on: the snapshot
	// itself, unless the source dates a snapshot at a read later than
	// its marks (positions.valued_at). 0 with SnapshotAt.
	ValuedAt int64

	// ObservedAt is the instant through which the source's holdings
	// were observed (silver_sources), capped at the as-of date: a quiet
	// source is current through its latest run although its latest
	// snapshot is older. 0 when unknown.
	ObservedAt int64

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
func SourcesAsOf(ctx context.Context, db *sql.DB, asOf int64, outCcy string) ([]SourceRow, error) {
	return scanRows(ctx, db, "SourcesAsOf", `
SELECT r.*, CASE WHEN o.observed_snapshots_through IS NULL THEN 0
                 ELSE LEAST(o.observed_snapshots_through, CAST(? AS BIGINT)) END
  FROM report_sources(?, ?) r
  LEFT JOIN source_observed o USING (silver_source_id)
 ORDER BY r.silver_source_id`, []any{asOf, asOf, outCcy}, func(r *SourceRow) []any {
		return []any{
			&r.SilverSourceID, str(&r.BaseCurrency), str(&r.TaxWrapper), str(&r.ManagementStyle),
			&r.SnapshotAt, &r.ValuedAt,
			dec(&r.PositionsValueBase), dec(&r.CashBalanceBase), dec(&r.TotalValueBase),
			dec(&r.PositionsValueOutCcy), dec(&r.CashBalanceOutCcy), dec(&r.TotalValueOutCcy),
			&r.ObservedAt,
		}
	})
}
