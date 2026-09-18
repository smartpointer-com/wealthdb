package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
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

// instrumentValorIndex maps a Swiss VALOR to the instrument gold holds
// under it, so a statement-era trade — which names its instrument in
// free text and carries no id — can be resolved to an identity rather
// than guessed at.
//
// Two roads to the same number, because PSN states it only sometimes:
//
//   - `payload.InstrIdtfr.Valor`, where the feed carries the identifier
//     object at all.
//   - THE ISIN ITSELF, for a Swiss line. A `CH` ISIN is `CH` plus the
//     valor zero-padded to nine digits plus a check digit, so
//     `CH0012345678` IS valor 1234567. This reaches the instruments the
//     first road cannot: the identifier object is optional in the feed,
//     and a line that omits it would otherwise be invisible to a valor
//     lookup.
//
// A valor naming more than one instrument is dropped rather than
// resolved. That should not happen — a valor identifies one security
// line, which is the whole reason this is an identity — so a collision
// means an assumption here is wrong, and the honest answer to a wrong
// assumption is no answer.
func (r *psnReader) instrumentValorIndex(ctx context.Context) (map[string]string, error) {
	// No PSN side means no instrument dimension to resolve against, and
	// an empty index resolves nothing — which is the right answer, not
	// an error.
	if r == nil {
		return nil, nil
	}
	const q = `SELECT isin, payload FROM instruments`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("psn instrumentValorIndex: %w", err)
	}
	defer rows.Close()
	out, ambiguous := map[string]string{}, map[string]bool{}
	add := func(valor, isin string) {
		if valor == "" || ambiguous[valor] {
			return
		}
		if held, ok := out[valor]; ok && held != isin {
			delete(out, valor)
			ambiguous[valor] = true
			return
		}
		out[valor] = isin
	}
	for rows.Next() {
		var isin, payload string
		if err := rows.Scan(&isin, &payload); err != nil {
			return nil, err
		}
		var p struct {
			InstrIdtfr struct {
				Valor string `json:"Valor"`
			} `json:"InstrIdtfr"`
		}
		_ = json.Unmarshal([]byte(payload), &p)
		add(normalizeValor(p.InstrIdtfr.Valor), isin)
		add(valorFromSwissISIN(isin), isin)
	}
	return out, rows.Err()
}

// valorFromSwissISIN recovers the valor a Swiss ISIN is built from: the
// nine digits between the `CH` prefix and the trailing check digit,
// with leading zeros dropped. Empty for anything that is not a
// twelve-character CH ISIN of digits.
func valorFromSwissISIN(isin string) string {
	if len(isin) != 12 || !strings.HasPrefix(isin, "CH") {
		return ""
	}
	return normalizeValor(isin[2:11])
}

// normalizeValor drops leading zeros and refuses anything that is not
// all digits, so the two roads above agree on one spelling of a number.
func normalizeValor(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return ""
		}
	}
	v = strings.TrimLeft(v, "0")
	return v
}
