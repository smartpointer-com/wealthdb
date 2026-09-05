package ubs

import (
	"context"
	"database/sql"
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

// webTxTextKey identifies one booking across the era seam: the bank's
// own number for the entry — the account statement's "Transaction
// no.", which web silver uses as its transaction_external_id and which
// the MT940 :61: line repeats as its bank reference — and the account
// it was booked on.
//
// The number alone would not be a key: UBS stamps both legs of an
// inter-account transfer with one number, and the two legs have
// different payees to say. The account separates them.
type webTxTextKey struct {
	account string
	txnNo   string
}

// transactionTextByKey returns the narrative columns every web
// transaction projects (projectWebTxText), keyed by (account,
// "Transaction no."). The PSN-side text fold (merge.go) reads it for
// the entries the hard cut suppresses on the web side, where the
// MT940 feed recorded the same booking as a bare code.
//
// The whole table, unwindowed and uncut, for the same reason
// buildSameDayOffsetVeto reads it whole: what an entry's narrative is
// depends only on silver's contents, never on which slice of time a
// load happens to cover. Nothing here decides whether a row is
// emitted; the hard cut still owns that.
func (r *webReader) transactionTextByKey(ctx context.Context) (map[webTxTextKey]webTxText, error) {
	if r == nil {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT transaction_external_id, account_external_id, counterparty, description_kind, payload
  FROM transactions`)
	if err != nil {
		return nil, fmt.Errorf("ubs-web transactionTextByKey: %w", err)
	}
	defer rows.Close()
	out := map[webTxTextKey]webTxText{}
	for rows.Next() {
		var (
			txID, acct, payload   string
			counterparty, kindStr sql.NullString
		)
		if err := rows.Scan(&txID, &acct, &counterparty, &kindStr, &payload); err != nil {
			return nil, fmt.Errorf("ubs-web transactionTextByKey scan: %w", err)
		}
		p, decoded := decodeWebTxPayload(payload)
		text, _, _ := projectWebTxText(counterparty.String, kindStr.String, p, !decoded || isPDFCashBackfill(p))
		out[webTxTextKey{account: acct, txnNo: txID}] = text
	}
	return out, rows.Err()
}
