package loader

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/ledgercsv"
)

// transferIDPrefix marks the synthetic transaction_external_id of every
// ledger-sourced transaction, so a load can delete-and-reinsert a source's
// ledger transfers idempotently (picking up edits) without disturbing the
// adapter-emitted rows.
const transferIDPrefix = "xfer:"

// requiredTransferCols are the equity-transfer ledger CSV columns every
// file must carry. Lookup is by header name, so the columns may come in
// any order; `quantity`, `cost_basis`, `instrument`, and `note` are
// optional.
var requiredTransferCols = []string{"silver_source_id", "account", "occurred_at", "direction", "value", "currency"}

// TransferEntry is one row of the equity-transfer ledger. Value is the market
// value of the securities at the transfer date — the capital flow that the
// returns engine books. CostBasis (the pre-transfer basis, which for an
// appreciated transfer-in is far below Value) is carried in the transaction
// payload for reference, not used by the returns math.
type TransferEntry struct {
	SilverSourceID string
	Account        string // gold account_external_id OR a nickname (resolved at load)
	OccurredAt     int64  // UTC seconds at midnight of the transfer date
	Direction      string // "in" | "out"
	Value          float64
	Currency       string
	Quantity       float64
	CostBasis      float64
	Instrument     string
	Note           string
}

// ParseTransferLedger reads the equity-transfer ledger CSV at path and returns
// its rows grouped by silver_source_id. An empty path or a missing file is not
// an error — the ledger is optional and absence means "no transfers".
func ParseTransferLedger(path string) (map[string][]TransferEntry, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("equity_transfers: open %q: %w", path, err)
	}
	defer f.Close()
	return parseTransferLedger(f)
}

func parseTransferLedger(r io.Reader) (map[string][]TransferEntry, error) {
	lr, err := ledgercsv.Open(r, requiredTransferCols)
	if err != nil {
		return nil, fmt.Errorf("equity_transfers: %w", err)
	}
	if lr == nil { // no header: an empty ledger
		return nil, nil
	}
	out := map[string][]TransferEntry{}
	for {
		row, line, err := lr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("equity_transfers: %w", err)
		}
		e, err := transferFromRecord(row)
		if err != nil {
			return nil, fmt.Errorf("equity_transfers: line %d: %w", line, err)
		}
		out[e.SilverSourceID] = append(out[e.SilverSourceID], e)
	}
	return out, nil
}

func transferFromRecord(row ledgercsv.Row) (TransferEntry, error) {
	e := TransferEntry{
		SilverSourceID: row.Get("silver_source_id"),
		Account:        row.Get("account"),
		Direction:      strings.ToLower(row.Get("direction")),
		Currency:       strings.ToUpper(row.Get("currency")),
		Instrument:     row.Get("instrument"),
		Note:           row.Get("note"),
	}
	if e.SilverSourceID == "" || e.Account == "" {
		return e, fmt.Errorf("silver_source_id and account are required")
	}
	if e.Direction != "in" && e.Direction != "out" {
		return e, fmt.Errorf("direction must be 'in' or 'out', got %q", e.Direction)
	}
	if len(e.Currency) != 3 {
		return e, fmt.Errorf("currency must be a 3-letter ISO code, got %q", row.Get("currency"))
	}
	ts, err := time.Parse("2006-01-02", row.Get("occurred_at"))
	if err != nil {
		return e, fmt.Errorf("occurred_at must be YYYY-MM-DD, got %q", row.Get("occurred_at"))
	}
	e.OccurredAt = ts.UTC().Unix()
	// value/quantity/cost_basis tolerate 0 / blank — a not-yet-known row is a
	// 0-value placeholder (it books nothing) until filled in.
	if e.Value, err = parseLedgerFloat(row.Get("value")); err != nil {
		return e, fmt.Errorf("value: %w", err)
	}
	if e.Quantity, err = parseLedgerFloat(row.Get("quantity")); err != nil {
		return e, fmt.Errorf("quantity: %w", err)
	}
	if e.CostBasis, err = parseLedgerFloat(row.Get("cost_basis")); err != nil {
		return e, fmt.Errorf("cost_basis: %w", err)
	}
	if e.Value < 0 {
		return e, fmt.Errorf("value must be non-negative (direction sets the sign)")
	}
	return e, nil
}

func parseLedgerFloat(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	return strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
}

// toChange converts a ledger entry to a canonical transfer transaction.
// AccountExternalID must already be resolved to the gold id. NetAmount carries
// the market value with the canonical sign for the direction; cost basis and
// the source marker live in the payload.
func (e TransferEntry) toChange(account string) canonical.TransactionChange {
	kind := canonical.TxKindTransferIn
	if e.Direction == "out" {
		kind = canonical.TxKindTransferOut
	}
	val := canonical.NewDecimalFromFloat(e.Value)
	ch := canonical.TransactionChange{
		TransactionExternalID: e.syntheticID(),
		OccurredAt:            e.OccurredAt,
		AccountExternalID:     account,
		Kind:                  kind,
		Currency:              e.Currency,
		NetAmount:             canonical.ApplyCanonicalSign(kind, &val),
		Payload:               e.payload(),
	}
	if e.Quantity != 0 {
		q := canonical.NewDecimalFromFloat(e.Quantity)
		ch.Quantity = &q
	}
	if e.Instrument != "" {
		ins := e.Instrument
		ch.InstrumentExternalID = &ins
	}
	if e.Note != "" {
		d := e.Note
		ch.Description = &d
	}
	return ch
}

// syntheticID is a deterministic, ledger-marked id so re-loads collapse onto the
// same row. Keyed on the natural content of the entry (not Value/CostBasis, so
// correcting an amount in place updates the same transaction).
func (e TransferEntry) syntheticID() string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		e.SilverSourceID, e.Account, strconv.FormatInt(e.OccurredAt, 10),
		e.Direction, e.Instrument, strconv.FormatFloat(e.Quantity, 'f', -1, 64),
	}, "|")))
	return transferIDPrefix + fmt.Sprintf("%x", h[:8])
}

func (e TransferEntry) payload() json.RawMessage {
	b, _ := json.Marshal(struct {
		Ledger     bool    `json:"equity_transfer_ledger"`
		CostBasis  float64 `json:"cost_basis"`
		Instrument string  `json:"instrument,omitempty"`
	}{Ledger: true, CostBasis: e.CostBasis, Instrument: e.Instrument})
	return b
}

// applyTransferLedger replaces the source's ledger transfers in gold: it deletes
// any previously-injected ledger rows for the source (so edits are picked up on
// re-load) and inserts the current set, resolving each entry's account by
// account_external_id or nickname (gold.NewAccountResolver, the one resolution
// every account-keyed ledger shares). Runs inside the load transaction.
func applyTransferLedger(ctx context.Context, tx *sql.Tx, sourceID string, entries []TransferEntry) (int, error) {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM transactions WHERE silver_source_id = ? AND transaction_external_id LIKE ?`,
		sourceID, transferIDPrefix+"%"); err != nil {
		return 0, fmt.Errorf("apply equity_transfers: clear prior: %w", err)
	}
	if len(entries) == 0 {
		return 0, nil
	}
	resolve, err := gold.NewAccountResolver(ctx, tx, sourceID)
	if err != nil {
		return 0, fmt.Errorf("apply equity_transfers: %w", err)
	}
	batch := make([]canonical.TransactionChange, 0, len(entries))
	for _, e := range entries {
		acct, err := resolve(e.Account)
		if err != nil {
			return 0, fmt.Errorf("apply equity_transfers: %w", err)
		}
		ch := e.toChange(acct)
		ch.SilverSourceID = sourceID
		batch = append(batch, ch)
	}
	if err := gold.NewWriter(tx).InsertTransactions(ctx, batch); err != nil {
		return 0, fmt.Errorf("apply equity_transfers: insert: %w", err)
	}
	return len(batch), nil
}
