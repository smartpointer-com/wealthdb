package gold

import (
	"context"
	"database/sql"
	"fmt"
)

// SourceStatus summarises the gold-side state for one silver
// source. Used by `wealthdb status` for a quick
// "what does gold currently know about this silver" view.
type SourceStatus struct {
	SilverSourceID    string
	Kind              string
	Path              string
	HighWatermark     int64
	FirstLoadedAt     int64
	LastLoadedAt      int64
	PositionsCount    int
	CashBalancesCount int
	TransactionsCount int
	FxRatesCount      int
	// OldestSnapshotAt / LatestSnapshotAt are -1 when the
	// source has no snapshot-grain rows in gold.
	OldestSnapshotAt int64
	LatestSnapshotAt int64
	// Tx-side extrema, -1 when none.
	OldestTransactionAt int64
	LatestTransactionAt int64
	// Drift markers: how many rows landed as the canonical
	// "other" bucket because the adapter couldn't categorise
	// them. Populated only when the caller asks for verbose
	// status (`wealthdb status -v`); zero otherwise.
	OtherAssetClassCount int
	OtherTxKindCount     int
	// UnmigratedTaxonomyCount is positions with a NULL 2-D pair
	// (asset_class_new) — a source that hasn't been migrated to the
	// vehicle taxonomy, or an adapter that forgot to emit it. Should
	// be 0 once every source is migrated.
	UnmigratedTaxonomyCount int
}

// StatusForSource queries one silver_sources row + aggregates
// from the fact tables. Returns (nil, nil) — no error — when the
// source isn't registered in gold; callers that want a hard
// error in that case should check the return value.
func StatusForSource(ctx context.Context, db *sql.DB, silverSourceID string, includeDrift bool) (*SourceStatus, error) {
	s := &SourceStatus{SilverSourceID: silverSourceID}

	err := db.QueryRowContext(ctx,
		`SELECT silver_kind, silver_path, high_watermark, first_loaded_at, last_loaded_at
		   FROM silver_sources WHERE silver_source_id = ?`,
		silverSourceID,
	).Scan(&s.Kind, &s.Path, &s.HighWatermark, &s.FirstLoadedAt, &s.LastLoadedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("StatusForSource(%s) silver_sources: %w", silverSourceID, err)
	}

	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COALESCE(MIN(snapshot_at), -1),
		        COALESCE(MAX(snapshot_at), -1)
		   FROM positions WHERE silver_source_id = ?`,
		silverSourceID,
	).Scan(&s.PositionsCount, &s.OldestSnapshotAt, &s.LatestSnapshotAt); err != nil {
		return nil, fmt.Errorf("StatusForSource(%s) positions agg: %w", silverSourceID, err)
	}

	// cash_balances + fx_rates contribute to the snapshot
	// extrema too (a silver might publish FX-only or cash-only
	// snapshots without any positions).
	var (
		cashMin, cashMax sql.NullInt64
		fxMin, fxMax     sql.NullInt64
	)
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        MIN(snapshot_at), MAX(snapshot_at)
		   FROM cash_balances WHERE silver_source_id = ?`,
		silverSourceID,
	).Scan(&s.CashBalancesCount, &cashMin, &cashMax); err != nil {
		return nil, fmt.Errorf("StatusForSource(%s) cash agg: %w", silverSourceID, err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        MIN(snapshot_at), MAX(snapshot_at)
		   FROM fx_rates WHERE silver_source_id = ?`,
		silverSourceID,
	).Scan(&s.FxRatesCount, &fxMin, &fxMax); err != nil {
		return nil, fmt.Errorf("StatusForSource(%s) fx agg: %w", silverSourceID, err)
	}
	s.OldestSnapshotAt = minInt64(s.OldestSnapshotAt, cashMin, fxMin)
	s.LatestSnapshotAt = maxInt64(s.LatestSnapshotAt, cashMax, fxMax)

	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COALESCE(MIN(occurred_at), -1),
		        COALESCE(MAX(occurred_at), -1)
		   FROM transactions WHERE silver_source_id = ?`,
		silverSourceID,
	).Scan(&s.TransactionsCount, &s.OldestTransactionAt, &s.LatestTransactionAt); err != nil {
		return nil, fmt.Errorf("StatusForSource(%s) tx agg: %w", silverSourceID, err)
	}

	if includeDrift {
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM positions
			  WHERE silver_source_id = ? AND asset_class = 'other'`,
			silverSourceID,
		).Scan(&s.OtherAssetClassCount); err != nil {
			return nil, err
		}
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM transactions
			  WHERE silver_source_id = ? AND kind = 'other'`,
			silverSourceID,
		).Scan(&s.OtherTxKindCount); err != nil {
			return nil, err
		}
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM positions
			  WHERE silver_source_id = ? AND asset_class_new IS NULL`,
			silverSourceID,
		).Scan(&s.UnmigratedTaxonomyCount); err != nil {
			return nil, err
		}
	}

	return s, nil
}

// LoadAuditRow is one row from gold's load_audit history.
type LoadAuditRow struct {
	LoadedAt           int64
	ChangeNumberBefore sql.NullInt64
	ChangeNumberAfter  int64
	WindowStart        int64
	WindowEnd          int64
	SnapshotsLoaded    int
	TransactionsLoaded int
}

// RecentLoadAudit returns up to `limit` most-recent load_audit
// rows for the source, newest first.
func RecentLoadAudit(ctx context.Context, db *sql.DB, silverSourceID string, limit int) ([]LoadAuditRow, error) {
	const q = `
SELECT loaded_at, change_number_before, change_number_after,
       window_start, window_end, snapshots_loaded, transactions_loaded
  FROM load_audit
 WHERE silver_source_id = ?
 ORDER BY loaded_at DESC
 LIMIT ?`
	rows, err := db.QueryContext(ctx, q, silverSourceID, limit)
	if err != nil {
		return nil, fmt.Errorf("RecentLoadAudit(%s): %w", silverSourceID, err)
	}
	defer rows.Close()
	var out []LoadAuditRow
	for rows.Next() {
		var r LoadAuditRow
		if err := rows.Scan(
			&r.LoadedAt, &r.ChangeNumberBefore, &r.ChangeNumberAfter,
			&r.WindowStart, &r.WindowEnd, &r.SnapshotsLoaded, &r.TransactionsLoaded,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// minInt64 / maxInt64 work over the SourceStatus's "-1 sentinel
// meets sql.NullInt64" mixed inputs. A -1 sentinel is treated as
// "no data on this side"; non-null cash/fx values participate
// only when present.
func minInt64(seed int64, more ...sql.NullInt64) int64 {
	out := seed
	for _, m := range more {
		if !m.Valid {
			continue
		}
		if out < 0 || m.Int64 < out {
			out = m.Int64
		}
	}
	return out
}

func maxInt64(seed int64, more ...sql.NullInt64) int64 {
	out := seed
	for _, m := range more {
		if !m.Valid {
			continue
		}
		if m.Int64 > out {
			out = m.Int64
		}
	}
	return out
}
