package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
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

// safekeepingByPortfolio returns a per-portfolio map to the PSN
// safekeeping account_external_id that holds that portfolio's
// securities. Used to re-point ubs-web's PDF-reconstructed
// historical securities — which the Statement-of-Assets PDFs
// can't tie to a safekeeping account, so the gold adapter parks
// them on a synthetic per-portfolio overlay account — onto the
// real safekeeping account, giving account-by-account continuity
// across the web→PSN cutover.
//
// PSN's safekeeping_accounts payload carries PrtflId in the same
// 16-char BBBBAAAAAAAANN form ubs-web's historical
// portfolio_external_id uses, so they join directly. Only
// portfolios with EXACTLY ONE safekeeping account are included:
// the mapping has to be unambiguous to retroactively attribute a
// PDF security (which knows only its portfolio) to a single
// account. Portfolios with multiple safekeeping accounts are
// omitted — the caller leaves those on the overlay account.
//
// Built from the latest snapshot (the portfolio↔safekeeping
// relationship is long-lived; we apply today's structure
// retroactively to the historical PDFs).
func (r *psnReader) safekeepingByPortfolio(ctx context.Context) (map[string]string, error) {
	if r == nil {
		return nil, nil
	}
	var latest sql.NullInt64
	if err := r.db.QueryRowContext(ctx,
		`SELECT MAX(snapshot_at) FROM safekeeping_accounts`,
	).Scan(&latest); err != nil {
		return nil, fmt.Errorf("psn safekeepingByPortfolio latest: %w", err)
	}
	if !latest.Valid {
		return nil, nil
	}
	const q = `
SELECT account_external_id, payload
  FROM safekeeping_accounts
 WHERE snapshot_at = ?`
	rows, err := r.db.QueryContext(ctx, q, latest.Int64)
	if err != nil {
		return nil, fmt.Errorf("psn safekeepingByPortfolio: %w", err)
	}
	defer rows.Close()
	// accountsPerPortfolio counts safekeeping accounts seen per
	// portfolio so we can drop the ambiguous (1:many) ones.
	accountsPerPortfolio := map[string][]string{}
	for rows.Next() {
		var acctID, payload string
		if err := rows.Scan(&acctID, &payload); err != nil {
			return nil, err
		}
		var p struct {
			PrtflId string `json:"PrtflId"`
		}
		_ = json.Unmarshal([]byte(payload), &p)
		if p.PrtflId == "" {
			continue
		}
		accountsPerPortfolio[p.PrtflId] = append(
			accountsPerPortfolio[p.PrtflId], acctID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(accountsPerPortfolio))
	for portfolio, accts := range accountsPerPortfolio {
		if len(accts) == 1 {
			out[portfolio] = accts[0]
		}
	}
	return out, nil
}

// holdingsSnapshotRange returns the snapshot_at of PSN's earliest and
// latest securities-holdings batches (MIN and MAX over the holdings
// table); ok is false when PSN carries no holdings at all. PSN's cash
// and forward-contract feeds can bracket the holdings batches — they
// begin a day or two before the first MT535 batch, and after a nightly
// run they can arrive before that day's holdings land — so on those
// bracket days a PSN snapshot exists with cash/forwards but no
// securities. The merge uses this window to keep the nearest complete
// securities snapshot authoritative — see psnHoldingsGapFilter.
func (r *psnReader) holdingsSnapshotRange(ctx context.Context) (first, last int64, ok bool, err error) {
	if r == nil {
		return 0, 0, false, nil
	}
	var lo, hi sql.NullInt64
	if err := r.db.QueryRowContext(ctx,
		`SELECT MIN(snapshot_at), MAX(snapshot_at) FROM holdings`).Scan(&lo, &hi); err != nil {
		return 0, 0, false, fmt.Errorf("psn holdingsSnapshotRange: %w", err)
	}
	if !lo.Valid {
		return 0, 0, false, nil
	}
	return lo.Int64, hi.Int64, true, nil
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
			AssetClass: assetClassForInstrument(p.InstrCtgyCFI, p.UacAsstClsCd, p.InstrNm.Best()),
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
