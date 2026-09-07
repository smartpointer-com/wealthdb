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
	// GuessedTxKindCount is the same signal for the adapters that do
	// NOT fall to `other` on an unrecognised source kind: transactions
	// whose payload carries a `source_kind`, which is where such an
	// adapter parks the raw value it could not map before kinding the
	// row by its sign instead. A row like that is a real purchase or
	// bill everywhere downstream, so nothing else counts it, and a
	// source that started publishing a new vocabulary would otherwise
	// be invisible. Rows already counted as `other` are excluded — an
	// adapter may park the raw value AND bucket the row — and so is a
	// payload whose `source_kind` is JSON null, which carries no raw
	// value to review.
	GuessedTxKindCount int
	// MissingVehicleCount is positions with a NULL vehicle — an
	// adapter that emitted an exposure but no wrapper. Should be 0.
	MissingVehicleCount int
	// UncategorizedSpendCount is spending lines this source
	// contributes that no tier could place — the model tier's
	// backlog, and the number that says how much of a spending report
	// is still "uncategorised" rather than wrong.
	UncategorizedSpendCount int
	// ExcludedUnmappedCount is transactions on this source's IN-SCOPE
	// spending accounts carrying either CATCH-ALL kind — `other` or
	// `journal` — and which therefore never reach the spending base at
	// all.
	//
	// The exclusion is deliberate — a source may demote an internal
	// conduit leg to `other`, `journal` is a bookkeeping entry, and
	// neither kind carries a reliable sign, so admitting them would
	// import noise nothing can orient — but it is the one exclusion
	// that can hide real money. An adapter that starts bucketing a
	// whole category of card rows under a catch-all would otherwise
	// shrink a spending report silently. Counting it makes that loud.
	ExcludedUnmappedCount int
	// PerKindActivity is the per-account-kind freshness breakdown,
	// populated only for sources holding more than one account kind
	// (see AccountKindActivity). Empty otherwise, and empty when the
	// caller did not ask for verbose status.
	PerKindActivity []AccountKindActivity
}

// AccountKindActivity is one account kind's freshness within a source.
//
// A source's single latest-snapshot line is an aggregate, and an
// aggregate hides the case that matters: a login carrying both deposit
// accounts and cards, whose card population quietly stops updating
// while the deposit population keeps refreshing every night. The
// source looks perfectly current, and the spending report silently
// stops at the last card row. Splitting the extrema by account kind is
// what makes that visible.
//
// Timestamps are -1 when the kind has no rows of that grain.
type AccountKindActivity struct {
	AccountKind         string
	Accounts            int
	LatestSnapshotAt    int64
	LatestTransactionAt int64
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
			`SELECT COUNT(*) FROM transactions
			  WHERE silver_source_id = ? AND kind <> 'other'
			    AND json_extract_string(payload, '$.source_kind') IS NOT NULL`,
			silverSourceID,
		).Scan(&s.GuessedTxKindCount); err != nil {
			return nil, err
		}
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM positions
			  WHERE silver_source_id = ? AND vehicle IS NULL`,
			silverSourceID,
		).Scan(&s.MissingVehicleCount); err != nil {
			return nil, err
		}
		if err := spendDrift(ctx, db, s); err != nil {
			return nil, err
		}
		if err := perKindActivity(ctx, db, s); err != nil {
			return nil, err
		}
	}

	return s, nil
}

// spendDrift fills the two spending counters. Both read the layered
// spend macros rather than restating their predicates, so a change to
// what counts as a spending account or a spending kind moves the
// status numbers with it.
//
// The second counter watches the two CATCH-ALL kinds — `other` and
// `journal` — on in-scope accounts. Those are where an adapter files a
// row it could not classify, so money landing there is money that
// silently left the spending base. The deliberate exclusions (buy,
// sell, fx, dividend, card_payment, positive interest) are not
// counted: they occur in bulk on every cash and card account, and a
// number that is permanently large says nothing.
func spendDrift(ctx context.Context, db *sql.DB, s *SourceStatus) error {
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM spending_lines_base(?, ?)
         WHERE silver_source_id = ? AND spend_detailed IS NULL`,
		int64(0), MaxEpoch, s.SilverSourceID,
	).Scan(&s.UncategorizedSpendCount); err != nil {
		return fmt.Errorf("StatusForSource(%s) uncategorised spend: %w", s.SilverSourceID, err)
	}
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*)
          FROM transactions t
          JOIN spend_scoped_accounts() sa
                 ON sa.silver_source_id    = t.silver_source_id
                AND sa.account_external_id = t.account_external_id
         WHERE t.silver_source_id = ? AND t.kind IN ('other', 'journal')`,
		s.SilverSourceID,
	).Scan(&s.ExcludedUnmappedCount); err != nil {
		return fmt.Errorf("StatusForSource(%s) excluded-unmapped spend: %w", s.SilverSourceID, err)
	}
	return nil
}

// perKindActivity fills PerKindActivity for a source holding more than
// one account kind. A single-kind source's breakdown would just repeat
// the numbers already printed above it, so it is left empty.
func perKindActivity(ctx context.Context, db *sql.DB, s *SourceStatus) error {
	rows, err := db.QueryContext(ctx, `
        WITH activity AS (
            SELECT account_external_id, snapshot_at, NULL::BIGINT AS occurred_at
              FROM positions      WHERE silver_source_id = ?
            UNION ALL
            SELECT account_external_id, snapshot_at, NULL::BIGINT
              FROM cash_balances  WHERE silver_source_id = ?
            UNION ALL
            SELECT account_external_id, NULL::BIGINT, occurred_at
              FROM transactions   WHERE silver_source_id = ?)
        SELECT a.account_kind,
               COUNT(DISTINCT a.account_external_id),
               COALESCE(MAX(x.snapshot_at), -1),
               COALESCE(MAX(x.occurred_at), -1)
          FROM accounts a
          LEFT JOIN activity x ON x.account_external_id = a.account_external_id
         WHERE a.silver_source_id = ?
         GROUP BY a.account_kind
         ORDER BY a.account_kind`,
		s.SilverSourceID, s.SilverSourceID, s.SilverSourceID, s.SilverSourceID)
	if err != nil {
		return fmt.Errorf("StatusForSource(%s) per-kind activity: %w", s.SilverSourceID, err)
	}
	defer rows.Close()
	var out []AccountKindActivity
	for rows.Next() {
		var a AccountKindActivity
		if err := rows.Scan(&a.AccountKind, &a.Accounts,
			&a.LatestSnapshotAt, &a.LatestTransactionAt); err != nil {
			return fmt.Errorf("StatusForSource(%s) scan per-kind activity: %w", s.SilverSourceID, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(out) > 1 {
		s.PerKindActivity = out
	}
	return nil
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
