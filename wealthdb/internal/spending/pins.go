package spending

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/ledgercsv"
)

// The pins ledger: per-transaction category pins, the top of the
// precedence lattice. A pin keys on what a statement shows (source,
// account, day, amount, currency), applies to every row it describes,
// may set any valid value, and is re-stamped by every pass with
// provenance `manual` (docs/SPENDING.md §3, "Pins").

// Pin is one row of the ledger.
type Pin struct {
	Source   string
	Account  string  // gold account_external_id OR a nickname, resolved at pass time
	Day      int64   // UTC midnight of occurred_at, Unix seconds
	Amount   float64 // net_amount as gold stores it: canonical sign, so a debit is negative
	Currency string
	Detailed string
	// AssetClass is what the capital went INTO, set only on a pin whose
	// verdict is this family's one investing value. Empty everywhere
	// else, and empty is NULL in the overlay rather than a blank string
	// — a blank would win the resolution's COALESCE and mint a class
	// node with no name.
	AssetClass string
}

// requiredPinCols are the ledger CSV columns. Lookup is by header
// name, so they may be given in any order, and a column outside this
// list is accepted only if this format knows it: `note`, a free-text
// annotation kept for the ledger's own readability, and `asset_class`,
// the exposure a pin may state beside an investing verdict. Anything
// else fails the parse: a column CHANGES the answer, so a misspelled
// header would be a silent no-op that still applied its verdict, and
// there is no diagnostic anywhere downstream that would show it.
//
// The value column is the family's: `spend_detailed` in the spending
// ledger, `income_detailed` in the income one. Everything else about
// the format is shared, because a pin identifies a transaction the same
// way whichever question is being answered about it.
var requiredPinCols = []string{"silver_source_id", "account", "occurred_at", "amount", "currency"}

// pinAmountEps is how far a pin's amount may sit from the stored
// net_amount and still describe it: a cent, the absolute floor gold's
// transfer matcher applies. A ledger is written from a statement
// showing two decimals; gold stores four.
const pinAmountEps = gold.TransferMatchMinEps

// naturalKeySQL follows a SELECT of transactions' columns with the
// natural key a pin and a ledger leg name a transaction by: its source,
// account and currency, its UTC day, and its amount to within
// pinAmountEps. naturalKeyArgs gives its arguments.
const naturalKeySQL = `
  FROM transactions
 WHERE silver_source_id = ? AND account_external_id = ? AND currency = ?
   AND occurred_at >= ? AND occurred_at < ?
   AND abs(CAST(net_amount AS DOUBLE) - ?) <= ?`

// naturalKeyArgs are naturalKeySQL's arguments: a transaction of the
// source, account and currency, on the UTC day that starts at dayStart,
// of the amount.
func naturalKeyArgs(source, account, currency string, dayStart int64, amount float64) []any {
	return []any{source, account, currency, dayStart, dayStart + gold.SecondsPerDay, amount, pinAmountEps}
}

// ParsePinLedger reads the pins ledger CSV at path. An empty path or a
// missing file is not an error — the ledger is optional and absence
// means "no pins".
func ParsePinLedger(path string) ([]Pin, error) {
	return ParsePinLedgerAs(path, "spending", "spend_detailed",
		canonical.SpendDetailedInvestment, canonical.ValidSpendDetailed)
}

// ParsePinLedgerAs is ParsePinLedger for a named family: the same
// format and the same transaction key, read for a different value
// column and validated against a different vocabulary. A spending
// value in the income ledger is refused at config load, which is the
// only place a person's typo can still be cheap to fix.
//
// `investingValue` is the one value of this family's vocabulary whose
// cash flow section is `investing`, and the only one an `asset_class`
// may accompany.
func ParsePinLedgerAs(path, family, valueCol, investingValue string, valid func(string) bool) ([]Pin, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s.pins: open %q: %w", family, path, err)
	}
	defer f.Close()
	return parsePinLedger(f, family, valueCol, investingValue, valid)
}

func parsePinLedger(r io.Reader, family, valueCol, investingValue string, valid func(string) bool) ([]Pin, error) {
	lr, err := ledgercsv.Open(r, append(append([]string(nil), requiredPinCols...), valueCol))
	if err != nil {
		return nil, fmt.Errorf("%s.pins: %w", family, err)
	}
	if lr == nil { // no header: an empty ledger
		return nil, nil
	}
	// And every header must be one this format knows — see
	// requiredPinCols on why an unknown one is an error rather than
	// ignored. A wholly empty trailing cell is not a column.
	known := map[string]struct{}{"note": {}, "asset_class": {}, valueCol: {}}
	for _, c := range requiredPinCols {
		known[c] = struct{}{}
	}
	for _, name := range lr.Columns() {
		if name == "" {
			continue
		}
		if _, ok := known[name]; !ok {
			return nil, fmt.Errorf("%s.pins: unknown column %q; the format is %v plus %q, note and asset_class",
				family, name, requiredPinCols, valueCol)
		}
	}

	// Two rows describing the same transactions must agree: the pass
	// applies pins in ledger order and the later would silently win.
	// Rows that agree collapse to one — a pin already applies to every
	// row it describes, so repeating it adds nothing.
	type seenPin struct {
		line       int
		detailed   string
		assetClass string
	}
	var out []Pin
	seen := map[string]seenPin{}
	for {
		row, line, err := lr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s.pins: %w", family, err)
		}
		p, err := pinFromRecord(row, valueCol, investingValue, valid)
		if err != nil {
			return nil, fmt.Errorf("%s.pins: line %d: %w", family, line, err)
		}
		key := p.identity()
		if prev, dup := seen[key]; dup {
			if prev.detailed != p.Detailed {
				return nil, fmt.Errorf("%s.pins: line %d pins the same transaction(s) as line %d to %q, not %q",
					family,
					line, prev.line, p.Detailed, prev.detailed)
			}
			// Agreeing on the verdict and disagreeing on the exposure is
			// the same contradiction on the other axis, and file order
			// would decide it silently.
			if prev.assetClass != p.AssetClass {
				return nil, fmt.Errorf("%s.pins: line %d pins the same transaction(s) as line %d to asset_class %q, not %q",
					family,
					line, prev.line, p.AssetClass, prev.assetClass)
			}
			continue
		}
		seen[key] = seenPin{line, p.Detailed, p.AssetClass}
		out = append(out, p)
	}
	return out, nil
}

func pinFromRecord(row ledgercsv.Row, valueCol, investingValue string, valid func(string) bool) (Pin, error) {
	p := Pin{
		Source:   row.Get("silver_source_id"),
		Account:  row.Get("account"),
		Currency: strings.ToUpper(row.Get("currency")),
		Detailed: row.Get(valueCol),
	}
	if p.Source == "" || p.Account == "" {
		return p, fmt.Errorf("silver_source_id and account are required")
	}
	ts, err := time.Parse("2006-01-02", row.Get("occurred_at"))
	if err != nil {
		return p, fmt.Errorf("occurred_at must be YYYY-MM-DD, got %q", row.Get("occurred_at"))
	}
	p.Day = ts.UTC().Unix()
	amount := row.Get("amount")
	if amount == "" {
		return p, fmt.Errorf("amount is required")
	}
	if p.Amount, err = strconv.ParseFloat(strings.ReplaceAll(amount, ",", ""), 64); err != nil {
		return p, fmt.Errorf("amount: %w", err)
	}
	if len(p.Currency) != 3 {
		return p, fmt.Errorf("currency must be a 3-letter ISO code, got %q", row.Get("currency"))
	}
	// Exact spelling: the vendored values are uppercase, the deltas
	// lowercase, and the casing is what says where a value came from.
	// A retired spelling is refused like any other non-value; the
	// refusal names what to write instead, where that is a value of this
	// family.
	if !valid(p.Detailed) {
		if r, ok := canonical.RetiredDetailed(p.Detailed); ok && valid(r.Successor) {
			return p, fmt.Errorf("%s %q is retired: use %s", valueCol, p.Detailed, r.Use)
		}
		return p, fmt.Errorf("%s %q is not a value of this family's taxonomy (vendored values are uppercase, the deltas lowercase)", valueCol, p.Detailed)
	}
	if p.AssetClass = row.Get("asset_class"); p.AssetClass != "" {
		if p.Detailed != investingValue {
			return p, fmt.Errorf("asset_class is only meaningful beside %s %q, which is this family's one investing verdict; got %q",
				valueCol, investingValue, p.Detailed)
		}
		if err := canonical.StatedExposure(p.AssetClass); err != nil {
			return p, fmt.Errorf("asset_class: %w", err)
		}
	}
	return p, nil
}

// identity is the key two ledger rows collide on: the transaction
// identity a pin describes, with the amount at the cent the match
// tolerates.
func (p Pin) identity() string {
	return strings.Join([]string{
		p.Source, p.Account, strconv.FormatInt(p.Day, 10),
		strconv.FormatFloat(math.Round(p.Amount*100)/100, 'f', 2, 64), p.Currency,
	}, "|")
}

// pinnedRow is a pin resolved onto one gold transaction: the row's
// narrative, so it gets a signature like any other, and the verdict.
type pinnedRow struct {
	row        candidate
	detailed   string
	assetClass string
}

// resolvePins finds every gold transaction each pin describes and
// returns the verdicts keyed by transaction, plus the number of pins
// that described nothing.
//
// A pin that matches nothing is counted, never an error: the row it
// describes may simply not have loaded yet, and the pass runs after
// every load of every source. An account the resolver does not know
// counts the same way — a source not loaded yet has no accounts at
// all — while an AMBIGUOUS nickname is an error, because that is a
// configuration fault whatever the load state.
//
// The family NAME prefixes every error, because both families call it
// with their own ledger: an ambiguous nickname reported as `spending:`
// sends a reader to the wrong file.
func resolvePins(ctx context.Context, tx *sql.Tx, fam string, pins []Pin) (map[txKey]pinnedRow, int, error) {
	if len(pins) == 0 {
		return nil, 0, nil
	}
	stmt, err := tx.PrepareContext(ctx, `
SELECT transaction_external_id, COALESCE(counterparty, ''), COALESCE(description, '')`+naturalKeySQL+`
 ORDER BY transaction_external_id`)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: prepare pin lookup: %w", fam, err)
	}
	defer stmt.Close()

	resolvers := map[string]gold.AccountResolver{}
	out := map[txKey]pinnedRow{}
	unmatched := 0
	for _, p := range pins {
		resolve, ok := resolvers[p.Source]
		if !ok {
			if resolve, err = gold.NewAccountResolver(ctx, tx, p.Source); err != nil {
				return nil, 0, fmt.Errorf("%s: resolve pin accounts for %s: %w", fam, p.Source, err)
			}
			resolvers[p.Source] = resolve
		}
		account, err := resolve(p.Account)
		if errors.Is(err, gold.ErrUnknownAccount) {
			unmatched++
			continue
		}
		if err != nil {
			return nil, 0, fmt.Errorf("%s: pin: %w", fam, err)
		}
		matches, err := pinMatches(ctx, stmt, fam, p, account)
		if err != nil {
			return nil, 0, err
		}
		if len(matches) == 0 {
			unmatched++
		}
		for _, row := range matches {
			out[row.key] = pinnedRow{row: row, detailed: p.Detailed, assetClass: p.AssetClass}
		}
	}
	return out, unmatched, nil
}

// pinMatches reads every transaction one pin describes, fully, before
// the next pin is looked up: the statement runs on the pass's
// transaction, which holds one connection.
func pinMatches(ctx context.Context, stmt *sql.Stmt, fam string, p Pin, account string) ([]candidate, error) {
	rows, err := stmt.QueryContext(ctx, naturalKeyArgs(p.Source, account, p.Currency, p.Day, p.Amount)...)
	if err != nil {
		return nil, fmt.Errorf("%s: look up pin %s/%s: %w", fam, p.Source, p.Account, err)
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		row := candidate{key: txKey{source: p.Source}}
		if err := rows.Scan(&row.key.txID, &row.counterparty, &row.description); err != nil {
			return nil, fmt.Errorf("%s: scan pin match: %w", fam, err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
