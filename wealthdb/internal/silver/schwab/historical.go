package schwab

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

// Historical-snapshot reader for the schwab-web silver
// migration-0002 tables (`historical_position_snapshots`,
// `historical_cash_balances`). Parsed from monthly statement
// PDFs — see schwab-web/INTEROP.md §5. These complement
// the api feed which carries only live (intra-day) positions
// per dump run.
//
// Both tables key on the statement period's natural timestamps
// (as_of_date for positions, period_end / period_start for
// cash) rather than dump times. The webReader's ChangeWindow
// extends Start back so the loader's window-DELETE covers
// existing historical gold rows before they're re-inserted on
// reload.

// snapshotsHistorical emits one batch per distinct historical
// timestamp in the window. Account ids are bridged to api
// hashValues so the rows land alongside the api-side accounts.
func (r *webReader) snapshotsHistorical(
	ctx context.Context,
	w canonical.Window,
	bridge map[string]string,
) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	ok, err := r.hasHistoricalTables(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return silver.NewSnapshotStream(nil), nil
	}

	byTime := make(map[int64]*canonical.SnapshotBatch)
	getBatch := func(t int64) *canonical.SnapshotBatch {
		if b, ok := byTime[t]; ok {
			return b
		}
		b := &canonical.SnapshotBatch{}
		byTime[t] = b
		return b
	}

	if err := r.appendHistoricalPositions(ctx, w, getBatch, bridge); err != nil {
		return nil, err
	}
	if err := r.appendHistoricalCashBalances(ctx, w, getBatch, bridge); err != nil {
		return nil, err
	}

	times := make([]int64, 0, len(byTime))
	for t := range byTime {
		times = append(times, t)
	}
	sortInt64Asc(times)

	batches := make([]canonical.SnapshotBatch, 0, len(times))
	for _, t := range times {
		batches = append(batches, *byTime[t])
	}
	return silver.NewSnapshotStream(batches), nil
}

// appendHistoricalPositions emits one PositionChange per row in
// `historical_position_snapshots`, plus one InstrumentChange per
// distinct (snapshot, instrument_key) — Schwab statements
// surface CUSIPs in some sections and tickers in others, so the
// instrument_key is used as both the gold instrument_external_id
// and the position_key. asset_class defaults to AssetClassOther
// (statement PDFs don't carry a structured type code); the api
// side's per-column upsert will overwrite with the real class
// whenever an api position references the same instrument.
//
// market_value, cost_basis, and accrued_interest are forwarded;
// quantity and market_price land in the payload via the silver
// row.
func (r *webReader) appendHistoricalPositions(
	ctx context.Context,
	w canonical.Window,
	getBatch func(int64) *canonical.SnapshotBatch,
	bridge map[string]string,
) error {
	const q = `
SELECT as_of_date, account_external_id, instrument_key,
       quantity, market_price, market_value, cost_basis,
       unrealized_gain_loss, accrued_interest, payload
  FROM historical_position_snapshots
 WHERE as_of_date BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendHistoricalPositions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			asOf                                                    int64
			suffix, instrumentKey, payload                          string
			quantity, marketPrice, marketValue, costBasis           sql.NullFloat64
			unrealized, accrued                                     sql.NullFloat64
		)
		if err := rows.Scan(&asOf, &suffix, &instrumentKey,
			&quantity, &marketPrice, &marketValue, &costBasis,
			&unrealized, &accrued, &payload); err != nil {
			return err
		}
		hash, ok := bridge[suffix]
		if !ok {
			continue
		}
		batch := getBatch(asOf)

		instrIDCopy := instrumentKey
		batch.Instruments = append(batch.Instruments, canonical.InstrumentChange{
			InstrumentExternalID: instrumentKey,
			AssetClass:           canonical.AssetClassOther,
			Symbol:               &instrIDCopy,
			FirstSeenAt:          asOf,
			LastSeenAt:           asOf,
		})

		batch.Positions = append(batch.Positions, canonical.PositionChange{
			SnapshotAt:           asOf,
			AccountExternalID:    hash,
			PositionKey:          instrumentKey,
			InstrumentExternalID: &instrIDCopy,
			AssetClass:           canonical.AssetClassOther,
			Currency:             "USD",
			Quantity:             decimalFromNullFloat(quantity),
			MarketValue:          decimalFromNullFloat(marketValue),
			BookValue:            decimalFromNullFloat(costBasis),
			AccruedInterest:      decimalFromNullFloat(accrued),
			Payload:              json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// appendHistoricalCashBalances emits opening + closing balance
// rows from `historical_cash_balances`. period_start anchors the
// opening; period_end anchors the closing. NULL-valued sides are
// skipped — the silver loader preserves NULL distinct from a
// real zero balance (see schwab-web's migration 0002 fix).
//
// total_debits / total_credits are kept in the silver row's
// payload (forwarded through PositionChange's payload field) but
// not projected to gold — they're monthly aggregates, not
// point-in-time balances.
func (r *webReader) appendHistoricalCashBalances(
	ctx context.Context,
	w canonical.Window,
	getBatch func(int64) *canonical.SnapshotBatch,
	bridge map[string]string,
) error {
	const q = `
SELECT period_end, period_start, account_external_id, currency_iso,
       opening_balance, closing_balance, payload
  FROM historical_cash_balances
 WHERE period_end BETWEEN ? AND ?
    OR period_start BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("appendHistoricalCashBalances: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			periodEnd, periodStart int64
			suffix, ccy, payload   string
			open, closing          sql.NullFloat64
		)
		if err := rows.Scan(&periodEnd, &periodStart, &suffix, &ccy,
			&open, &closing, &payload); err != nil {
			return err
		}
		hash, ok := bridge[suffix]
		if !ok {
			continue
		}

		if open.Valid && periodStart >= w.Start && periodStart <= w.End {
			batch := getBatch(periodStart)
			batch.CashBalances = append(batch.CashBalances, canonical.CashBalanceChange{
				SnapshotAt:        periodStart,
				AccountExternalID: hash,
				Currency:          ccy,
				BalanceKind:       canonical.BalanceKindOpening,
				Amount:            canonical.NewDecimalFromFloat(open.Float64),
				Payload:           json.RawMessage(payload),
			})
		}
		if closing.Valid && periodEnd >= w.Start && periodEnd <= w.End {
			batch := getBatch(periodEnd)
			batch.CashBalances = append(batch.CashBalances, canonical.CashBalanceChange{
				SnapshotAt:        periodEnd,
				AccountExternalID: hash,
				Currency:          ccy,
				BalanceKind:       canonical.BalanceKindClosing,
				Amount:            canonical.NewDecimalFromFloat(closing.Float64),
				Payload:           json.RawMessage(payload),
			})
		}
	}
	return rows.Err()
}

// historicalRange returns MIN/MAX time across the historical
// tables. Both -1 when the silver carries no historical rows.
// Used by ChangeWindow to extend Start backwards so the loader's
// window-DELETE covers existing historical rows before re-insert.
func (r *webReader) historicalRange(ctx context.Context) (int64, int64, error) {
	var (
		posMin, posMax   sql.NullInt64
		cashMin, cashMax sql.NullInt64
	)
	if err := r.db.QueryRowContext(ctx,
		`SELECT MIN(as_of_date), MAX(as_of_date) FROM historical_position_snapshots`,
	).Scan(&posMin, &posMax); err != nil {
		return -1, -1, fmt.Errorf("schwab-web historicalRange positions: %w", err)
	}
	if err := r.db.QueryRowContext(ctx,
		`SELECT MIN(period_start), MAX(period_end) FROM historical_cash_balances`,
	).Scan(&cashMin, &cashMax); err != nil {
		return -1, -1, fmt.Errorf("schwab-web historicalRange cash: %w", err)
	}
	lo, hi := int64(-1), int64(-1)
	merge := func(n sql.NullInt64) {
		if !n.Valid {
			return
		}
		if lo == -1 || n.Int64 < lo {
			lo = n.Int64
		}
		if hi == -1 || n.Int64 > hi {
			hi = n.Int64
		}
	}
	merge(posMin)
	merge(posMax)
	merge(cashMin)
	merge(cashMax)
	return lo, hi, nil
}

// hasHistoricalTables reports whether the schwab-web silver
// carries the migration-0002 historical tables. Older silvers
// (rebuilt against 0001 only) won't have them, and the adapter
// must still load — falling back to the live-only stream.
func (r *webReader) hasHistoricalTables(ctx context.Context) (bool, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM sqlite_master
 WHERE type = 'table'
   AND name IN ('historical_position_snapshots', 'historical_cash_balances')`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("schwab-web hasHistoricalTables: %w", err)
	}
	return n == 2, nil
}

func sortInt64Asc(xs []int64) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}

func decimalFromNullFloat(n sql.NullFloat64) *canonical.Decimal {
	if !n.Valid {
		return nil
	}
	d := canonical.NewDecimalFromFloat(n.Float64)
	return &d
}
