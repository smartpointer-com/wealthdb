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

// requiredTransferCols are the equity-transfer ledger CSV columns every
// file must carry. Lookup is by header name, so the columns may come in
// any order, and every row holds as many fields as the header;
// `quantity`, `cost_basis`, `instrument`, `note`, `holding`,
// `to_silver_source_id` and `to_account` are optional.
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
	// Holding is the source's instrument id of the private holding an
	// out-leg distributes, where the leg moves another instrument: an
	// SPV's exit in the listed shares it received. Empty on an in-leg.
	Holding string
	// PairSource and PairID name the other leg of a move one ledger
	// row states (movementLegs): its silver_source_id and synthetic
	// id. Empty on a row that states one side only.
	PairSource, PairID string
	// Line is the ledger line that states the leg, for the errors of
	// the checks that run after the parse.
	Line int
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
		// A note with an unquoted comma spills into the next column, and
		// a field of the note would read as that column's value. A short
		// row has lost a field, and which one the parse cannot tell.
		if n, want := row.Fields(), len(lr.Columns()); n > want {
			return nil, fmt.Errorf("equity_transfers: line %d: %d fields, the header names %d: quote a note that holds a comma", line, n, want)
		} else if n < want {
			return nil, fmt.Errorf("equity_transfers: line %d: %d fields, the header names %d: every row holds a field for every column, a blank one included", line, n, want)
		}
		e, err := transferFromRecord(row)
		if err != nil {
			return nil, fmt.Errorf("equity_transfers: line %d: %w", line, err)
		}
		e.Line = line
		legs, err := movementLegs(e, row.Get("to_silver_source_id"), row.Get("to_account"))
		if err != nil {
			return nil, fmt.Errorf("equity_transfers: line %d: %w", line, err)
		}
		for _, l := range legs {
			out[l.SilverSourceID] = append(out[l.SilverSourceID], l)
		}
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
		Holding:        row.Get("holding"),
	}
	if e.SilverSourceID == "" || e.Account == "" {
		return e, fmt.Errorf("silver_source_id and account are required")
	}
	if e.Direction != "in" && e.Direction != "out" {
		return e, fmt.Errorf("direction must be 'in' or 'out', got %q", e.Direction)
	}
	if e.Holding != "" && e.Direction != "out" {
		return e, fmt.Errorf("holding names the private holding an exit distributes: direction must be 'out', got %q", e.Direction)
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

// movementLegs is the legs one ledger row states. A row that names
// the receiving account (toSource, toAccount) is a whole move out of
// its own account: it states the sending leg and the receiving one,
// alike but for the account and the direction, each naming the other.
// Those two legs are the two rows a file states the move with
// otherwise, ids included. A holding is the sender's alone. A row that
// names none is its one leg.
func movementLegs(e TransferEntry, toSource, toAccount string) ([]TransferEntry, error) {
	if toSource == "" && toAccount == "" {
		return []TransferEntry{e}, nil
	}
	if toSource == "" || toAccount == "" {
		return nil, fmt.Errorf("to_silver_source_id and to_account go together")
	}
	if e.Direction != "out" {
		return nil, fmt.Errorf("a row with to_account is the sender's: direction must be 'out', got %q", e.Direction)
	}
	in := e
	in.SilverSourceID, in.Account, in.Direction, in.Holding = toSource, toAccount, "in", ""
	e.PairSource, e.PairID = in.SilverSourceID, in.syntheticID()
	in.PairSource, in.PairID = e.SilverSourceID, e.syntheticID()
	return []TransferEntry{e, in}, nil
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
// same row. Keyed on the natural content of the entry (not Value/CostBasis or
// Holding, so correcting an amount or naming a holding in place updates the
// same transaction).
func (e TransferEntry) syntheticID() string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		e.SilverSourceID, e.Account, strconv.FormatInt(e.OccurredAt, 10),
		e.Direction, e.Instrument, strconv.FormatFloat(e.Quantity, 'f', -1, 64),
	}, "|")))
	return canonical.LedgerTransferIDPrefix + fmt.Sprintf("%x", h[:8])
}

func (e TransferEntry) payload() json.RawMessage {
	b, _ := json.Marshal(struct {
		Ledger     bool    `json:"equity_transfer_ledger"`
		CostBasis  float64 `json:"cost_basis"`
		Instrument string  `json:"instrument,omitempty"`
		PairSource string  `json:"pair_silver_source_id,omitempty"`
		PairID     string  `json:"pair_transaction_external_id,omitempty"`
		Holding    string  `json:"holding,omitempty"`
	}{Ledger: true, CostBasis: e.CostBasis, Instrument: e.Instrument, PairSource: e.PairSource, PairID: e.PairID,
		Holding: e.Holding})
	return b
}

// applyTransferLedger replaces the source's ledger transfers in gold: it deletes
// any previously-injected ledger rows for the source (so edits are picked up on
// re-load) and inserts the current set, resolving each entry's account by
// account_external_id or nickname (gold.NewAccountResolver, the one resolution
// every account-keyed ledger shares). An out-leg's holding must be a private
// holding of its account, an instrument a contribution of that account names:
// the private-markets view counts the leg as that holding's distribution.
// Runs inside the load transaction, after the adapter's own transactions.
func applyTransferLedger(ctx context.Context, tx *sql.Tx, sourceID string, entries []TransferEntry) (int, error) {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM transactions WHERE silver_source_id = ? AND transaction_external_id LIKE ?`,
		sourceID, canonical.LedgerTransferIDPrefix+"%"); err != nil {
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
		if err := checkTransferHolding(ctx, tx, sourceID, acct, e); err != nil {
			return 0, err
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

// checkTransferHolding refuses an out-leg whose holding no contribution of
// its account names: the leg would count as no holding's distribution. The
// error names the row by its line, account and date, as
// checkTransferSources does.
func checkTransferHolding(ctx context.Context, tx *sql.Tx, sourceID, account string, e TransferEntry) error {
	if e.Holding == "" {
		return nil
	}
	var held bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM transactions
		 WHERE silver_source_id = ? AND account_external_id = ? AND instrument_external_id = ? AND kind = ?)`,
		sourceID, account, e.Holding, string(canonical.TxKindContribution)).Scan(&held); err != nil {
		return fmt.Errorf("apply equity_transfers: holding: %w", err)
	}
	if !held {
		return fmt.Errorf("equity_transfers: line %d: the row of account %q on %s: holding %q is no private "+
			"holding of the account: none of its contributions names it",
			e.Line, e.Account, time.Unix(e.OccurredAt, 0).UTC().Format(time.DateOnly), e.Holding)
	}
	return nil
}
