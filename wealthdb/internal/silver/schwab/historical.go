package schwab

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Historical-snapshot reader for the schwab-web silver
// migration-0002 tables (`historical_position_snapshots`,
// `historical_cash_balances`), with the holdings' open lots
// (migration 0006, open_lots.go). Parsed from monthly statement
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
// timestamp in the window. Account ids are bridged (api hashValues,
// or the full account numbers with web alone) so the rows land on the
// accounts gold holds. alone says the web silver is configured
// without the api, which then names no instrument: the statement's
// security name names it.
func (r *webReader) snapshotsHistorical(
	ctx context.Context,
	w canonical.Window,
	bridge map[string]string,
	alone bool,
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

	byTime := silver.Batches{}
	if err := r.appendHistoricalPositions(ctx, w, byTime.At, bridge, alone); err != nil {
		return nil, err
	}
	if err := r.appendHistoricalCashBalances(ctx, w, byTime.At, bridge); err != nil {
		return nil, err
	}

	return byTime.Stream(), nil
}

// appendHistoricalPositions emits one PositionChange per row in
// `historical_position_snapshots`, plus an InstrumentChange for
// its instrument_key (one per row; gold upserts) — Schwab statements
// surface CUSIPs in some sections and tickers in others, so the
// instrument_key is used as both the gold instrument_external_id
// and the position_key. The (asset_class, vehicle) pair is derived
// from the statement shapes (statement PDFs carry no structured
// type code); the api side's per-column upsert overwrites the
// instrument dimension whenever an api position references the
// same instrument.
//
// quantity and accrued_interest are forwarded. The statement prints a
// holding's accrued interest or declared dividend apart from its market
// value; gold's market value includes it (docs/DESIGN.md §7.1), and the
// printed value stays in the payload. The book value is the printed
// cost_basis; where a holding prints none but states its market value
// and unrealized gain, it is their difference, both as printed.
// A holding's open lots (`open_lots`) follow it into the same batch,
// and the earliest lot's acquired date is the holding's. A lot whose
// holding row is missing has no position to ride beside: it is
// dropped and counted in the load log.
func (r *webReader) appendHistoricalPositions(
	ctx context.Context,
	w canonical.Window,
	getBatch func(int64) *canonical.SnapshotBatch,
	bridge map[string]string,
	alone bool,
) error {
	lots, err := r.openLotsInWindow(ctx, w)
	if err != nil {
		return err
	}
	const q = `
SELECT as_of_date, account_external_id, instrument_key,
       quantity, market_value, cost_basis,
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
			asOf                             int64
			suffix, instrumentKey, payload   string
			quantity, marketValue, costBasis sql.NullFloat64
			unrealized, accrued              sql.NullFloat64
		)
		if err := rows.Scan(&asOf, &suffix, &instrumentKey,
			&quantity, &marketValue, &costBasis,
			&unrealized, &accrued, &payload); err != nil {
			return err
		}
		key := holdingKey{asOf, suffix, instrumentKey}
		held := lots[key]
		delete(lots, key) // what remains has no holding to ride beside
		hash, ok := bridge[suffix]
		if !ok {
			continue
		}
		batch := getBatch(asOf)

		// Statement PDFs carry no structured type code, so the
		// (asset_class, vehicle) pair is derived from the section /
		// instrument-key / description shapes (payload.description
		// is the security name). The api side's per-column upsert
		// overwrites the instrument dimension whenever the same key
		// reappears source-classified.
		var hp struct {
			Section     string `json:"section"`
			Description string `json:"description"`
		}
		_ = json.Unmarshal([]byte(payload), &hp) // best-effort
		acNew, vehicle := taxonomyHistorical(hp.Section, instrumentKey, hp.Description)

		instrIDCopy := instrumentKey
		instr := canonical.InstrumentChange{
			InstrumentExternalID: instrumentKey,
			AssetClass:           acNew,
			Vehicle:              vehicle,
			Symbol:               &instrIDCopy,
			FirstSeenAt:          asOf,
			LastSeenAt:           asOf,
		}
		if alone {
			instr.Name = silver.StrPtrIfNonEmpty(hp.Description)
		}
		batch.Instruments = append(batch.Instruments, instr)

		printed := silver.DecimalPtrFromNullFloat(marketValue)
		pos := canonical.PositionChange{
			SnapshotAt:           asOf,
			AccountExternalID:    hash,
			PositionKey:          instrumentKey,
			InstrumentExternalID: &instrIDCopy,
			AssetClass:           acNew,
			Vehicle:              vehicle,
			Currency:             "USD",
			Quantity:             silver.DecimalPtrFromNullFloat(quantity),
			MarketValue:          printed,
			AccruedInterest:      silver.DecimalPtrFromNullFloat(accrued),
			Payload:              json.RawMessage(payload),
		}
		if pos.AccruedInterest != nil && printed != nil {
			pos.MarketValue = silver.WithAccrued(printed, pos.AccruedInterest)
			pos.Payload = silver.PayloadWith(payload, map[string]any{
				"printed_market_value": printed.String()})
		}
		if costBasis.Valid {
			pos.SetBookValue(silver.DecimalPtrFromNullFloat(costBasis), statementBasis)
		} else {
			short := quantity.Valid && quantity.Float64 < 0
			pos.SetBookValue(basisFromOpenPL(printed, unrealized, short), derivedBasis)
		}
		changes := make([]canonical.PositionLotChange, len(held))
		for i, l := range held {
			changes[i] = l.change(asOf, hash, instrumentKey)
		}
		pos.AcquisitionDate = canonical.EarliestLotDate(changes)
		batch.Positions = append(batch.Positions, pos)
		batch.PositionLots = append(batch.PositionLots, changes...)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	orphans := 0
	for _, held := range lots {
		orphans += len(held)
	}
	if orphans > 0 {
		log.Printf("schwab adapter: dropped %d open lot(s) whose statement holding is missing", orphans)
	}
	return nil
}

// appendHistoricalCashBalances emits opening + closing balance
// rows from `historical_cash_balances`. period_start anchors the
// opening; period_end anchors the closing. NULL-valued sides are
// skipped — the silver keeps NULL distinct from a real zero
// balance.
//
// total_debits / total_credits are kept in the silver row's
// payload (forwarded through PositionChange's payload field) but
// not projected to gold — they're monthly aggregates, not
// point-in-time balances.
//
// The interest rate the statement states for the cash (silver
// migration 0011) rides on the closing balance alone, as the
// payload's interest_rate and interest_side "credit": the
// statement states it at the period's end. A statement that states
// none, or a silver older than the column, adds neither key.
func (r *webReader) appendHistoricalCashBalances(
	ctx context.Context,
	w canonical.Window,
	getBatch func(int64) *canonical.SnapshotBatch,
	bridge map[string]string,
) error {
	hasRate, err := silver.HasColumn(ctx, r.db, "historical_cash_balances", "interest_rate")
	if err != nil {
		return fmt.Errorf("appendHistoricalCashBalances: %w", err)
	}
	rate := "NULL"
	if hasRate {
		rate = "interest_rate"
	}
	q := `
SELECT period_end, period_start, account_external_id, currency_iso,
       opening_balance, closing_balance, payload, ` + rate + `
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
			open, closing, stated  sql.NullFloat64
		)
		if err := rows.Scan(&periodEnd, &periodStart, &suffix, &ccy,
			&open, &closing, &payload, &stated); err != nil {
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
			closingPayload := json.RawMessage(payload)
			if stated.Valid {
				if closingPayload, err = silver.WithCreditRate(closingPayload,
					stated.Float64, ""); err != nil {
					return fmt.Errorf("appendHistoricalCashBalances %s: %w", suffix, err)
				}
			}
			batch := getBatch(periodEnd)
			batch.CashBalances = append(batch.CashBalances, canonical.CashBalanceChange{
				SnapshotAt:        periodEnd,
				AccountExternalID: hash,
				Currency:          ccy,
				BalanceKind:       canonical.BalanceKindClosing,
				Amount:            canonical.NewDecimalFromFloat(closing.Float64),
				Payload:           closingPayload,
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
	return silver.Extrema(ctx, r.db, "schwab-web historicalRange", []string{
		`SELECT MIN(as_of_date), MAX(as_of_date) FROM historical_position_snapshots`,
		`SELECT MIN(period_start), MAX(period_end) FROM historical_cash_balances`,
	})
}

// hasHistoricalTables reports whether the schwab-web silver
// carries the migration-0002 historical tables. Older silvers
// (rebuilt against 0001 only) won't have them, and the adapter
// must still load — falling back to the live-only stream.
func (r *webReader) hasHistoricalTables(ctx context.Context) (bool, error) {
	return silver.HasTables(ctx, r.db, "historical_position_snapshots", "historical_cash_balances")
}
