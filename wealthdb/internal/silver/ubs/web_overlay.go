package ubs

import (
	"context"
	"fmt"
)

// Web-side overlay helpers. The orchestrator pre-fetches per-key
// web payloads once and hands them to the PSN-side fold stream
// (merge.go) so the cross-source join lives in one place.

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
