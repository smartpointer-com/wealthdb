package schwab

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

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
		netAmount, quantity, price := extractWebTxAmounts(payload)
		txKind := webKind(kind)
		out.Transactions = append(out.Transactions, canonical.TransactionChange{
			TransactionExternalID: activityID,
			OccurredAt:            ts,
			AccountExternalID:     hash,
			InstrumentExternalID:  instrPtr,
			Kind:                  txKind,
			// Currency unknown from the web row — Schwab statements
			// don't structure it. Default to USD: Schwab
			// accounts are USD-denominated here, and
			// the canonical TransactionChange.Currency field is
			// NOT NULL.
			Currency:  "USD",
			NetAmount: canonical.ApplyCanonicalSign(txKind, netAmount),
			Quantity:  quantity,
			Price:     price,
			Payload:   json.RawMessage(payload),
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

// webKind maps the web silver's `kind` discriminator (the
// space-separated text Schwab uses on statements and in
// transaction-history exports) to canonical TxKind. Conservative
// — unmapped strings route to TxKindOther so we never invent
// semantics. Schwab uses different vocab on statements vs the
// Trader API; the api-side mapping is in kindmap.go.
//
// Reinvested dividends land as two separate transactions on
// Schwab statements: the cash-income side ("Reinvest Dividend")
// and the share-purchase side ("Reinvest" / "Reinvest Shares").
// We map them to dividend and buy respectively so the canonical
// single-entry sums stay correct (the two cancel out, net zero
// cash impact). See sign.go for the per-kind sign rules.
//
// Options trades use Schwab's open/close legs: Buy to Open and
// Buy to Close both leave cash (acquiring a long position or
// closing a short); Sell to Open and Sell to Close both add
// cash (receiving premium or closing a long). All four collapse
// to plain buy/sell at the canonical layer.
//
// Corporate actions (Spin-off, Split, Reverse Split, Exchange,
// Reorganized Issue, Return Of Capital, Cash In Lieu, etc.)
// collapse to TxKindCorporateAction. The canonical sign helper
// passes source-supplied signs through for that kind because
// the cash impact varies: cash-in-lieu yields cash, a plain
// split is zero, a cash merger pays out.
func webKind(s string) canonical.TxKind {
	switch s {
	case "Buy", "Buy to Open", "Buy to Close", "Purchase",
		"Reinvest", "Reinvest Shares":
		return canonical.TxKindBuy
	case "Sell", "Sell to Open", "Sell to Close", "Sale", "Redemption":
		return canonical.TxKindSell
	case "Cash Dividend", "Dividend", "Non-Qualified Div",
		"Qualified Dividend", "Reinvest Dividend",
		"Pr Yr Cash Div", "Special Dividend", "Special Qual Div":
		return canonical.TxKindDividend
	case "Bond Interest", "Credit Interest", "Interest",
		"Bank Interest", "Bank Interest Adj", "Margin Interest":
		return canonical.TxKindInterest
	case "Fee", "Service Fee", "ADR Mgmt Fee":
		return canonical.TxKindFee
	case "Foreign Tax Paid", "NRA Tax Adj", "Pr Yr NRA Tax", "Tax":
		return canonical.TxKindTax
	case "Deposit", "MoneyLink Deposit", "Funds Received", "Wire Received":
		return canonical.TxKindDeposit
	case "Withdrawal", "Wire Sent":
		return canonical.TxKindWithdrawal
	case "MoneyLink Transfer", "Transfer", "Security Transfer",
		"Journal", "Journaled Shares":
		return canonical.TxKindJournal
	case "Exchange", "Reorganized Issue", "Spin-off", "Split",
		"Reverse Split", "Return Of Capital", "Cash In Lieu",
		"Litigation", "Unissued Rights Redemption":
		return canonical.TxKindCorporateAction
	}
	return canonical.TxKindOther
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// schwabWebTxPayload captures the two payload shapes the silver
// produces side-by-side. Statement-PDF rows use lower-case keys
// with numeric values (`amount`, `quantity`, `price`).
// Transaction-history JSON rows use Schwab's CSV-export-style
// capitalised string keys (`Amount`, `Quantity`, `Price`) with
// values like `"$384.01"`, `"(25,000.00)"`, or `""`. Both
// payloads land in the same silver row keyed by source, so the
// adapter probes both.
type schwabWebTxPayload struct {
	Amount   *float64 `json:"amount"`
	Quantity *float64 `json:"quantity"`
	Price    *float64 `json:"price"`

	AmountStr   string `json:"Amount"`
	QuantityStr string `json:"Quantity"`
	PriceStr    string `json:"Price"`
}

// extractWebTxAmounts parses NetAmount, Quantity, and Price out
// of a schwab-web transaction payload, handling both the
// statement_pdf and tx_history_json shapes. Errors are swallowed
// — a malformed field just stays nil so one bad row doesn't
// abort the whole batch.
func extractWebTxAmounts(payload string) (netAmount, quantity, price *canonical.Decimal) {
	var p schwabWebTxPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return nil, nil, nil
	}
	netAmount = pickWebAmount(p.Amount, p.AmountStr)
	quantity = pickWebAmount(p.Quantity, p.QuantityStr)
	price = pickWebAmount(p.Price, p.PriceStr)
	return
}

func pickWebAmount(num *float64, str string) *canonical.Decimal {
	if num != nil {
		d := canonical.NewDecimalFromFloat(*num)
		return &d
	}
	d, ok := parseSchwabAmountString(str)
	if !ok {
		return nil
	}
	return &d
}

// parseSchwabAmountString parses Schwab's CSV-export string
// format: optional surrounding parens (negative), optional "$"
// prefix, optional "-" sign, comma thousands separators.
// Returns ok=false for empty or unparseable input.
func parseSchwabAmountString(s string) (canonical.Decimal, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return canonical.Decimal{}, false
	}
	negative := false
	if len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' {
		negative = true
		s = s[1 : len(s)-1]
	}
	s = strings.NewReplacer("$", "", ",", "", " ", "").Replace(s)
	if strings.HasPrefix(s, "-") {
		negative = !negative
		s = s[1:]
	}
	if s == "" {
		return canonical.Decimal{}, false
	}
	d, err := canonical.NewDecimalFromString(s)
	if err != nil {
		return canonical.Decimal{}, false
	}
	if negative {
		d = d.Neg()
	}
	return d, true
}
