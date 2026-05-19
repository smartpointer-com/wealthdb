package ubs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ptu/wealthdb/internal/canonical"
)

// PSN-side overlay helpers. Iteration 2 of the web/PSN merge uses
// these to pre-fetch per-key PSN payloads (positions, cash,
// events) so the web reader can fold them into its emit. Reading
// silver eagerly here keeps the merge logic at a single
// orchestration point (merge.go) rather than scattering joins
// across the per-entity append funcs.

// psnPosKey identifies one position payload for the per-day
// merge. Snapshot timestamps don't line up exactly between web
// and PSN; UTC date is the right granularity.
type psnPosKey struct {
	utcDate  int64
	account  string
	position string
}

// psnPosInfo is the per-key payload-plus-structured slice the
// web reader needs to fall back to during the overlap merge.
// Web silver carries `units` (quantity) but not `market_value`
// for securities — the MT535 payload from PSN supplies it. Web
// keeps identity ownership; this struct surfaces just the bits
// web can't compute on its own.
type psnPosInfo struct {
	Payload     string
	MarketValue *canonical.Decimal
}

// positionInfoByKey returns PSN's payload and parsed market_value
// per (UTC date, account, position_key). safekeeping_external_id
// and safekeeping_accounts.account_external_id share the same
// AcctId form as of silver migration 0002 — no translation needed.
func (r *psnReader) positionInfoByKey(ctx context.Context, wStart, wEnd int64) (map[psnPosKey]psnPosInfo, error) {
	if r == nil {
		return nil, nil
	}
	instr, err := r.instrumentMetaByISIN(ctx)
	if err != nil {
		return nil, err
	}

	const q = `
SELECT snapshot_at, safekeeping_external_id, isin, payload
  FROM holdings
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, wStart, wEnd)
	if err != nil {
		return nil, fmt.Errorf("psn positionInfoByKey holdings: %w", err)
	}
	defer rows.Close()
	out := make(map[psnPosKey]psnPosInfo)
	for rows.Next() {
		var (
			snap    int64
			sk      string
			isin    string
			payload string
		)
		if err := rows.Scan(&snap, &sk, &isin, &payload); err != nil {
			return nil, err
		}
		var hp holdingsPayloadShape
		_ = json.Unmarshal([]byte(payload), &hp)
		amounts := parse19A(hp.Fields.Tag19A)
		preferredCcy := ""
		if m, ok := instr[isin]; ok {
			preferredCcy = m.Currency
		}
		mvAmt, _, mvOk := findHoldEntry(amounts, preferredCcy)
		k := psnPosKey{utcDate: utcDay(snap), account: sk, position: isin}
		info := psnPosInfo{Payload: payload}
		if mvOk {
			v := mvAmt
			info.MarketValue = &v
		}
		out[k] = info
	}
	return out, rows.Err()
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
			AssetClass: assetClassForCFI(p.InstrCtgyCFI),
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

// snapshotDatesInWindow returns the UTC day-buckets that contain
// at least one PSN dump_runs row in the window. Used so the
// orchestrator can compute the overlap with web's snapshot dates.
func (r *psnReader) snapshotDatesInWindow(ctx context.Context, wStart, wEnd int64) (map[int64]bool, error) {
	if r == nil {
		return nil, nil
	}
	const q = `SELECT DISTINCT snapshot_at FROM dump_runs WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, wStart, wEnd)
	if err != nil {
		return nil, fmt.Errorf("psn snapshotDatesInWindow: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]bool)
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out[utcDay(t)] = true
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
