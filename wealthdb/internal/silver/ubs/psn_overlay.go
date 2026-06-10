package ubs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ptu/wealthdb/internal/canonical"
)

// PSN-side overlay helpers. The orchestrator pre-fetches PSN
// metadata (per-ISIN asset class, per-day cash payloads) and
// hands it to the merge layer so per-entity append funcs don't
// have to redo the same joins.

// assetClassByISIN returns a per-ISIN canonical asset class
// derived from PSN's CFI. Used by the web reader to stamp web-
// emitted InstrumentChange rows with the same asset_class PSN
// would emit, so the per-column upsert guard in gold.instruments
// stays idempotent on asset_class while letting web's Name +
// Currency win on later last_seen_at.
func (r *psnReader) assetClassByISIN(ctx context.Context) (map[string]canonical.AssetClass, error) {
	if r == nil {
		return nil, nil
	}
	meta, err := r.instrumentMetaByISIN(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]canonical.AssetClass, len(meta))
	for isin, m := range meta {
		out[isin] = m.AssetClass
	}
	return out, nil
}

// instrumentMetaByISIN is a thin wrapper over the existing
// appendInstruments lookup-building logic, isolated here so the
// overlay code can reuse it without dragging in the byTime
// pipeline.
func (r *psnReader) instrumentMetaByISIN(ctx context.Context) (map[string]instrumentMeta, error) {
	const q = `SELECT snapshot_at, isin, payload FROM instruments ORDER BY snapshot_at ASC`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("psn instrumentMetaByISIN: %w", err)
	}
	defer rows.Close()
	out := make(map[string]instrumentMeta)
	for rows.Next() {
		var snap int64
		var isin, payload string
		if err := rows.Scan(&snap, &isin, &payload); err != nil {
			return nil, err
		}
		var p instrumentPayload
		_ = json.Unmarshal([]byte(payload), &p)
		out[isin] = instrumentMeta{
			AssetClass: assetClassForInstrument(p.InstrCtgyCFI, p.UacAsstClsCd),
			Currency:   p.GacInstrRskCcyIsoCd,
		}
	}
	return out, rows.Err()
}

// psnCashKey is the cash-balance counterpart to psnPosKey.
type psnCashKey struct {
	utcDate  int64
	account  string
	currency string
}

// cashPayloadByKey returns PSN's latest-per-day cash payload
// keyed by (UTC date, account_external_id, currency).
// account_external_id is the IBAN as of silver migration 0002 —
// matches cash_accounts directly.
func (r *psnReader) cashPayloadByKey(ctx context.Context, wStart, wEnd int64) (map[psnCashKey]string, error) {
	if r == nil {
		return nil, nil
	}
	const q = `
SELECT snapshot_at, account_external_id, currency_iso, payload
  FROM cash_balances
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, wStart, wEnd)
	if err != nil {
		return nil, fmt.Errorf("psn cashPayloadByKey: %w", err)
	}
	defer rows.Close()
	out := make(map[psnCashKey]string)
	for rows.Next() {
		var (
			snap            int64
			acctID, ccy, pl string
		)
		if err := rows.Scan(&snap, &acctID, &ccy, &pl); err != nil {
			return nil, err
		}
		k := psnCashKey{utcDate: utcDay(snap), account: acctID, currency: ccy}
		out[k] = pl
	}
	return out, rows.Err()
}

// utcDay rounds a Unix-seconds timestamp DOWN to UTC midnight.
// Used to bucket cross-source snapshots that fall on the same
// business day but at different wall-clock times.
func utcDay(epoch int64) int64 {
	t := time.Unix(epoch, 0).UTC()
	t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return t.Unix()
}
