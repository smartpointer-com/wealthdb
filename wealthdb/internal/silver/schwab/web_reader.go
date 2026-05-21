package schwab

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

// webReader reads from the schwab-web-dump silver SQLite. The web
// feed complements the api feed by:
//
//   - Backfilling transactions older than each account's
//     api-coverage-start (Trader API only goes back ~2y; web
//     statements reach further).
//   - Providing per-statement-period position snapshots and cash
//     balances that api doesn't surface at all (api emits only
//     live snapshots per dump run).
//   - Supplying the human-readable `nickname` Schwab attaches to
//     each account.
//
// Identity: web's `account_external_id` is the 3-to-5-digit
// account suffix Schwab renders in the UI. The orchestrator
// (merge.go) builds a suffix → api-hashValue bridge at adapter
// open time and the methods on this reader rewrite the suffix to
// the bridged hash before emitting downstream.
type webReader struct {
	db   *sql.DB
	path string
}

func (r *webReader) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	err := r.db.Close()
	r.db = nil
	return err
}

// Status reports the silver-side observable range for the web
// feed. Snapshot-time extrema come from dump_runs plus the
// historical tables (which use as_of_date / period_end rather
// than dump times). Transaction-time extrema come from
// transactions.timestamp. LatestChangeNumber stays a live-time
// concept — MAX(dump_runs.snapshot_at) — so a reload with no new
// dump is a no-op even when historical content is present.
func (r *webReader) Status(ctx context.Context) (canonical.Status, error) {
	out := canonical.Status{
		OldestSnapshotAt:    -1,
		LatestSnapshotAt:    -1,
		OldestTransactionAt: -1,
		LatestTransactionAt: -1,
		LatestChangeNumber:  -1,
	}
	if err := r.db.QueryRowContext(ctx, `
        SELECT COALESCE(MIN(snapshot_at), -1),
               COALESCE(MAX(snapshot_at), -1)
          FROM dump_runs`).Scan(&out.OldestSnapshotAt, &out.LatestSnapshotAt); err != nil {
		return canonical.Status{}, fmt.Errorf("schwab-web Status snapshots: %w", err)
	}
	if err := r.db.QueryRowContext(ctx, `
        SELECT COALESCE(MIN(timestamp), -1),
               COALESCE(MAX(timestamp), -1)
          FROM transactions`).Scan(&out.OldestTransactionAt, &out.LatestTransactionAt); err != nil {
		return canonical.Status{}, fmt.Errorf("schwab-web Status transactions: %w", err)
	}
	ok, err := r.hasHistoricalTables(ctx)
	if err != nil {
		return canonical.Status{}, err
	}
	if ok {
		histLo, histHi, err := r.historicalRange(ctx)
		if err != nil {
			return canonical.Status{}, err
		}
		if histLo >= 0 && (out.OldestSnapshotAt == -1 || histLo < out.OldestSnapshotAt) {
			out.OldestSnapshotAt = histLo
		}
		if histHi > out.LatestSnapshotAt {
			out.LatestSnapshotAt = histHi
		}
	}
	out.LatestChangeNumber = maxInt64(out.LatestSnapshotAt, out.LatestTransactionAt)
	return out, nil
}

// ChangeWindow returns the union of new snapshots and new
// transactions strictly after `since`, plus any historical
// content reachable by either dimension. Extends Start back to
// MIN(historical times) whenever there's any new live content so
// the loader's window-DELETE covers existing historical gold
// rows before they're re-inserted — same pattern as the UBS web
// reader.
func (r *webReader) ChangeWindow(ctx context.Context, since int64) (canonical.Window, error) {
	var (
		snapMin, snapMax sql.NullInt64
		txMin, txMax     sql.NullInt64
	)
	if err := r.db.QueryRowContext(ctx, `
        SELECT MIN(snapshot_at), MAX(snapshot_at)
          FROM dump_runs WHERE snapshot_at > ?`, since).Scan(&snapMin, &snapMax); err != nil {
		return canonical.Window{}, fmt.Errorf("schwab-web ChangeWindow snapshots: %w", err)
	}
	if err := r.db.QueryRowContext(ctx, `
        SELECT MIN(timestamp), MAX(timestamp)
          FROM transactions WHERE timestamp > ?`, since).Scan(&txMin, &txMax); err != nil {
		return canonical.Window{}, fmt.Errorf("schwab-web ChangeWindow transactions: %w", err)
	}
	w := canonical.Window{NewChangeNumber: since}
	mergeNew := func(n sql.NullInt64) {
		if !n.Valid {
			return
		}
		w.HasChanges = true
		if w.NewChangeNumber < n.Int64 {
			w.NewChangeNumber = n.Int64
		}
	}
	mins, maxs := []sql.NullInt64{snapMin, txMin}, []sql.NullInt64{snapMax, txMax}
	for _, m := range mins {
		if m.Valid && (!w.HasChanges || m.Int64 < w.Start) {
			w.Start = m.Int64
		}
	}
	for _, m := range maxs {
		if m.Valid && m.Int64 > w.End {
			w.End = m.Int64
		}
	}
	mergeNew(snapMax)
	mergeNew(txMax)

	if w.HasChanges {
		ok, err := r.hasHistoricalTables(ctx)
		if err != nil {
			return canonical.Window{}, err
		}
		if ok {
			histLo, histHi, err := r.historicalRange(ctx)
			if err != nil {
				return canonical.Window{}, err
			}
			if histLo >= 0 && histLo < w.Start {
				w.Start = histLo
			}
			if histHi > w.End {
				w.End = histHi
			}
		}
	}
	return w, nil
}

// snapshotsDimensions emits AccountChange rows from web's
// `accounts` table for every snapshot in the window, rewriting
// account_external_id from web's suffix to the bridged api
// hashValue. Web supplies the user-friendly `nickname`; api
// supplies almost everything else. The per-column upsert guard
// in gold merges them by (silver_source_id, account_external_id).
//
// Accounts whose web suffix doesn't bridge to an api hashValue
// (e.g. closed accounts no longer in api) are skipped — they
// have no api counterpart to merge with and emitting them under
// the raw suffix would create orphaned rows the rest of gold
// can't join.
func (r *webReader) snapshotsDimensions(
	ctx context.Context,
	w canonical.Window,
	byTime map[int64]*canonical.SnapshotBatch,
	bridge map[string]string,
) error {
	if !w.HasChanges {
		return nil
	}
	const q = `
SELECT snapshot_at, account_external_id, nickname, payload
  FROM accounts
 WHERE snapshot_at BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("schwab-web snapshotsDimensions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			snap          int64
			suffix        string
			nickname      sql.NullString
			payload       string
		)
		if err := rows.Scan(&snap, &suffix, &nickname, &payload); err != nil {
			return err
		}
		hash, ok := bridge[suffix]
		if !ok {
			continue
		}
		batch, ok := byTime[snap]
		if !ok {
			continue
		}
		batch.Accounts = append(batch.Accounts, canonical.AccountChange{
			AccountExternalID: hash,
			AccountKind:       canonical.AccountKindBrokerage,
			Nickname:          nullStringPtrSchwabWeb(nickname),
			FirstSeenAt:       snap,
			LastSeenAt:        snap,
			Payload:           json.RawMessage(payload),
		})
	}
	return rows.Err()
}

// transactionsBeforeAPIStart emits web transactions that are
// strictly older than each account's api-coverage start. Web
// `activity_id`s are a synthetic SHA-256 prefix — they don't
// collide with api's real activityIds. Rewrites web suffix to
// api hashValue so the row lands under the same gold account as
// the api transactions for that account.
//
// `apiStartByHash` maps api hashValue → MIN(timestamp). A web tx
// is emitted only when:
//
//   1. its account bridges to an api hashValue; AND
//   2. its timestamp is strictly less than apiStartByHash[hash]
//      (or hash has no api transactions at all, in which case
//      everything from the web side passes).
//
// Hard cut, not overlap-merge — per INTEROP §2 the two silvers'
// transaction-id spaces are disjoint so any cross-source per-row
// match would be heuristic. Inside the api window, api wins
// (real activity_id, no parser approximation).
func (r *webReader) transactionsBeforeAPIStart(
	ctx context.Context,
	w canonical.Window,
	bridge map[string]string,
	apiStartByHash map[string]int64,
) (silver.TransactionStream, error) {
	if !w.HasChanges {
		return &txStream{consumed: true}, nil
	}
	const q = `
SELECT activity_id, timestamp, account_external_id, kind, instrument_key, payload
  FROM transactions
 WHERE timestamp BETWEEN ? AND ?`
	rows, err := r.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return nil, fmt.Errorf("schwab-web Transactions: %w", err)
	}
	defer rows.Close()

	out := canonical.TransactionBatch{}
	for rows.Next() {
		var (
			activityID, suffix, kind, payload string
			ts                                int64
			instrumentKey                     sql.NullString
		)
		if err := rows.Scan(&activityID, &ts, &suffix, &kind, &instrumentKey, &payload); err != nil {
			return nil, err
		}
		hash, ok := bridge[suffix]
		if !ok {
			continue
		}
		if cutoff, ok := apiStartByHash[hash]; ok && ts >= cutoff {
			continue
		}
		var instrPtr *string
		if instrumentKey.Valid && instrumentKey.String != "" {
			s := instrumentKey.String
			instrPtr = &s
		}
		out.Transactions = append(out.Transactions, canonical.TransactionChange{
			TransactionExternalID: activityID,
			OccurredAt:            ts,
			AccountExternalID:     hash,
			InstrumentExternalID:  instrPtr,
			Kind:                  webKind(kind),
			// Currency unknown from the web row — Schwab statements
			// don't structure it. Default to USD: Schwab
			// accounts are USD-denominated here, and
			// the canonical TransactionChange.Currency field is
			// NOT NULL.
			Currency: "USD",
			Payload:  json.RawMessage(payload),
		})
	}
	return &txStream{batch: out}, rows.Err()
}

// nullStringPtrSchwabWeb mirrors helpers in other adapter packages;
// kept local to avoid a cross-package helper-collision dance.
func nullStringPtrSchwabWeb(n sql.NullString) *string {
	if !n.Valid || n.String == "" {
		return nil
	}
	s := n.String
	return &s
}

// webKind maps the web silver's `kind` discriminator (Sale,
// Purchase, CashDividend, NRATax, etc.) to canonical TxKind.
// Conservative — unmapped strings route to TxKindOther so we
// never invent semantics. Schwab uses different vocab on
// statements vs the Trader API; the api-side mapping is in
// kindmap.go.
func webKind(s string) canonical.TxKind {
	switch s {
	case "Purchase", "Buy":
		return canonical.TxKindBuy
	case "Sale", "Sell":
		return canonical.TxKindSell
	case "CashDividend", "QualDiv", "NonQualDiv":
		return canonical.TxKindDividend
	case "CreditInterest", "Interest":
		return canonical.TxKindInterest
	case "Deposit", "MoneyLinkTransfer":
		return canonical.TxKindDeposit
	case "Withdrawal":
		return canonical.TxKindWithdrawal
	case "ServiceFee", "Fee", "FundExpense":
		return canonical.TxKindFee
	case "NRATax", "Tax", "TaxWithholding":
		return canonical.TxKindTax
	case "Journal":
		return canonical.TxKindJournal
	}
	return canonical.TxKindOther
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
