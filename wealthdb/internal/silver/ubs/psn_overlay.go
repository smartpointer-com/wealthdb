package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// PSN-side overlay helpers. The orchestrator pre-fetches PSN
// metadata (per-ISIN asset class, per-day cash payloads) and
// hands it to the merge layer so per-entity append funcs don't
// have to redo the same joins.

// taxPairByISIN returns a per-ISIN taxonomy pair (exposure, vehicle)
// derived from PSN's CFI/UAC. The web overlay stamps web-emitted
// instruments with it (web has no CFI of its own), keeping gold's
// per-column upsert guard idempotent on asset_class / vehicle across
// the web→PSN cutover while letting web's later Name + Currency win.
func (r *psnReader) taxPairByISIN(ctx context.Context) (map[string]taxPair, error) {
	if r == nil {
		return nil, nil
	}
	meta, err := r.instrumentMetaByISIN(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]taxPair, len(meta))
	for isin, m := range meta {
		out[isin] = taxPair{AssetClass: m.AssetClass, Vehicle: m.Vehicle}
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
// portfolio_external_id uses, so they join directly. The mapping has
// to be unambiguous to retroactively attribute a PDF security — which
// knows only its portfolio — to a single account, so a portfolio is
// included only when exactly one of its safekeeping accounts can be
// the one that held it. Where more than one can, the portfolio is
// omitted and the caller leaves its securities on the overlay.
//
// Counting the accounts is the wrong question, though, because a
// safekeeping account need not hold securities: UBS opens one per
// service line, and some of those lines hold nothing a Statement of
// assets would print. PSN's `holdings` tells them apart — it reports a
// position against the account that owns it — so the candidates are
// narrowed to the accounts PSN has ever reported a holding for. When
// that narrowing would leave nothing (PSN carries no holdings yet, or
// none for this portfolio) it is not applied, so a portfolio whose one
// account is quiet maps exactly as it did before.
//
// Opening dates are deliberately NOT used to narrow this. The roster
// dates every opening, which makes them look like the discriminator,
// but they answer only half the question: PSN states a closing STATUS
// (`AcctClsgSts`) rather than a closing date, and a portfolio element's
// `PrtflElmtEndDt` stays open-ended until the element actually ends. A
// rule that can see an account appear but never disappear resolves the
// span before a second account opens and leaves the span after it
// ambiguous — one continuous series cut in two, with a seam on a date
// where nothing happened to the holding.
//
// Built from the latest roster snapshot (the portfolio↔safekeeping
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
	holdsSecurities, err := r.safekeepingAccountsWithHoldings(ctx)
	if err != nil {
		return nil, err
	}
	const q = `
SELECT account_external_id, payload
  FROM safekeeping_accounts
 WHERE snapshot_at = ?
 ORDER BY account_external_id`
	rows, err := r.db.QueryContext(ctx, q, latest.Int64)
	if err != nil {
		return nil, fmt.Errorf("psn safekeepingByPortfolio: %w", err)
	}
	defer rows.Close()
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
		if witnessed := filterHoldsSecurities(accts, holdsSecurities); len(witnessed) > 0 {
			accts = witnessed
		}
		if len(accts) == 1 {
			out[portfolio] = accts[0]
		}
	}
	return out, nil
}

// filterHoldsSecurities keeps the accounts PSN has reported a holding
// for, in the order given. An empty result means the question cannot
// be answered from holdings, which the caller treats as no narrowing
// rather than as no candidates.
func filterHoldsSecurities(accts []string, holdsSecurities map[string]bool) []string {
	var out []string
	for _, a := range accts {
		if holdsSecurities[a] {
			out = append(out, a)
		}
	}
	return out
}

// safekeepingAccountsWithHoldings is the set of safekeeping accounts
// PSN has ever reported a securities position against, over every
// snapshot rather than the latest one: an account that held paper only
// in its early years is still an account that holds securities.
//
// `holdings` is the only witness that answers this. The roster's
// account-type text describes the service, not what is in the account,
// and `pending_securities` is no witness at all: PSN emits one
// statement per safekeeping account per day whether or not anything is
// pending, so every account appears in it.
func (r *psnReader) safekeepingAccountsWithHoldings(ctx context.Context) (map[string]bool, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT DISTINCT safekeeping_external_id FROM holdings`)
	if err != nil {
		return nil, fmt.Errorf("psn safekeepingAccountsWithHoldings: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
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

// instrumentMetaByISIN builds the same isin→meta lookup as
// appendInstruments, minus the in-window InstrumentChange emission the
// overlay path doesn't need. It duplicates that loop on purpose:
// sharing it would drag appendInstruments' byTime pipeline into the
// overlay for no real gain. Keep the taxonomy mapping here in sync with
// appendInstruments if it ever changes.
func (r *psnReader) instrumentMetaByISIN(ctx context.Context) (map[string]instrumentMeta, error) {
	if r == nil {
		return nil, nil
	}
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
		acNew, vehicle := taxonomyPairForInstrument(p.InstrCtgyCFI, p.UacAsstClsCd, p.InstrNm.Best())
		out[isin] = instrumentMeta{
			AssetClass: acNew,
			Vehicle:    vehicle,
			Currency:   p.GacInstrRskCcyIsoCd,
		}
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
