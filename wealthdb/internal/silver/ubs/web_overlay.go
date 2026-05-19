package ubs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// Web-side overlay helpers for iteration 2 of the web/PSN merge.
// The orchestrator pre-fetches these once and hands them to both
// readers so the merge stays in one place.

// webPosKey is the per-(UTC date, ISIN) match key for the
// position-payload overlay. Web silver flattens all securities
// under a single portfolio, so we can't key
// by (account, portfolio, ISIN) without ambiguity. ISIN alone
// plus the day is enough granularity per source — overlay
// payload semantics don't depend on uniqueness, only on having
// useful cost_price/lending_value to fold.
type webPosKey struct {
	utcDate int64
	isin    string
}

// positionPayloadByKey returns web's per-(UTC date, ISIN) JSON
// payload. Used by the orchestrator's PSN-side fold to inject
// web-only fields (cost_price, lending_value, lending_value_ratio,
// market_value_base) into PSN's position payloads during the
// overlap window.
func (r *webReader) positionPayloadByKey(ctx context.Context, wStart, wEnd int64) (map[webPosKey]string, error) {
	if r == nil {
		return nil, nil
	}
	const q = `
SELECT snapshot_at, instrument_isin, payload
  FROM positions
 WHERE instrument_isin IS NOT NULL
   AND snapshot_at BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, wStart, wEnd)
	if err != nil {
		return nil, fmt.Errorf("web positionPayloadByKey: %w", err)
	}
	defer rows.Close()
	out := make(map[webPosKey]string)
	for rows.Next() {
		var snap int64
		var isin, payload string
		if err := rows.Scan(&snap, &isin, &payload); err != nil {
			return nil, err
		}
		out[webPosKey{utcDate: utcDay(snap), isin: isin}] = payload
	}
	return out, rows.Err()
}

// webCashKey is the (UTC date, account, currency) key for web's
// cash-position rows. Web uses IBAN as account_external_id so
// this matches PSN's cash_balances directly post-canonicalisation.
type webCashKey struct {
	utcDate  int64
	account  string
	currency string
}

// cashPayloadByKey returns web's per-(UTC date, account, ccy)
// JSON payload for cash positions. Folded into PSN's cash
// balances during the overlap.
func (r *webReader) cashPayloadByKey(ctx context.Context, wStart, wEnd int64) (map[webCashKey]string, error) {
	if r == nil {
		return nil, nil
	}
	const q = `
SELECT snapshot_at, account_external_id, currency_iso, payload
  FROM positions
 WHERE instrument_isin IS NULL
   AND snapshot_at BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, wStart, wEnd)
	if err != nil {
		return nil, fmt.Errorf("web cashPayloadByKey: %w", err)
	}
	defer rows.Close()
	out := make(map[webCashKey]string)
	for rows.Next() {
		var snap int64
		var acct, ccy, payload string
		if err := rows.Scan(&snap, &acct, &ccy, &payload); err != nil {
			return nil, err
		}
		out[webCashKey{utcDate: utcDay(snap), account: acct, currency: ccy}] = payload
	}
	return out, rows.Err()
}

// snapshotDatesInWindow returns the UTC day-buckets that contain
// at least one web dump_runs row in [wStart, wEnd]. Used by the
// orchestrator to drop PSN snapshots on dates web also covers.
func (r *webReader) snapshotDatesInWindow(ctx context.Context, wStart, wEnd int64) (map[int64]bool, error) {
	if r == nil {
		return nil, nil
	}
	const q = `SELECT DISTINCT snapshot_at FROM dump_runs WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, wStart, wEnd)
	if err != nil {
		return nil, fmt.Errorf("web snapshotDatesInWindow: %w", err)
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

// foldPSNPayload merges a PSN payload string under a "psn" key
// inside the web JSON payload. Returns:
//
//   - The original web payload unchanged when psn is empty.
//   - {"psn": <psn>} when web is empty / non-object — wraps PSN
//     payload in a minimal envelope so the key path is uniform.
//   - merged object with "psn" key replacing any existing key.
//
// The wrapping keeps web identity / structured fields untouched
// and gives downstream queries a stable JSON path
// (`payload.psn.*`) for PSN-only fields during the overlap window.
func foldPSNPayload(web json.RawMessage, psn string) json.RawMessage {
	if psn == "" {
		return web
	}
	webTrim := bytes.TrimSpace(web)
	// Non-object web payload (null, array, string, empty): emit
	// the PSN envelope alone — better than silently dropping the
	// PSN extension.
	if len(webTrim) == 0 || webTrim[0] != '{' {
		return json.RawMessage(`{"psn":` + psn + `}`)
	}
	// Object web payload: parse, set psn, re-marshal. Using
	// json.RawMessage for the PSN value preserves its shape
	// (object/array/scalar) without re-marshalling.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(webTrim, &m); err != nil {
		// Couldn't parse web payload: prefer the safer fallback
		// over silently corrupting bytes.
		return json.RawMessage(`{"psn":` + psn + `}`)
	}
	m["psn"] = json.RawMessage(psn)
	out, err := json.Marshal(m)
	if err != nil {
		return webTrim
	}
	return out
}
