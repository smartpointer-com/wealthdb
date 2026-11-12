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

// naturalKey is how a pin and a ledger leg name a transaction: its
// source, account and currency, its UTC day (the one that starts at
// dayStart), and its amount to within pinAmountEps.
type naturalKey struct {
	source, account, currency string
	dayStart                  int64
	amount                    float64
}

// matchNaturalKeys reads every transaction each key describes, in one
// query: the keys are a VALUES list joined to transactions. The result
// holds one list per key, in key order, each in transaction_external_id
// order, with the narrative a pinned row is signed by.
func matchNaturalKeys(ctx context.Context, tx querier, keys []naturalKey) ([][]candidate, error) {
	out := make([][]candidate, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	values := make([]string, len(keys))
	args := make([]any, 0, len(keys)*7+1)
	for i, k := range keys {
		values[i] = "(CAST(? AS INTEGER), CAST(? AS VARCHAR), CAST(? AS VARCHAR), CAST(? AS VARCHAR)," +
			" CAST(? AS BIGINT), CAST(? AS BIGINT), CAST(? AS DOUBLE))"
		args = append(args, i, k.source, k.account, k.currency, k.dayStart, k.dayStart+gold.SecondsPerDay, k.amount)
	}
	args = append(args, pinAmountEps)
	rows, err := tx.QueryContext(ctx, `
SELECT k.i, t.transaction_external_id, COALESCE(t.counterparty, ''), COALESCE(t.description, '')
  FROM (VALUES `+strings.Join(values, ", ")+`) AS k(i, source, account, currency, day_from, day_to, amount)
  JOIN transactions t
    ON t.silver_source_id = k.source AND t.account_external_id = k.account AND t.currency = k.currency
   AND t.occurred_at >= k.day_from AND t.occurred_at < k.day_to
   AND abs(CAST(t.net_amount AS DOUBLE) - k.amount) <= ?
 ORDER BY k.i, t.transaction_external_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var i int
		var row candidate
		if err := rows.Scan(&i, &row.key.txID, &row.counterparty, &row.description); err != nil {
			return nil, err
		}
		row.key.source = keys[i].source
		out[i] = append(out[i], row)
	}
	return out, rows.Err()
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
	resolvers := map[string]gold.AccountResolver{}
	keys := make([]naturalKey, 0, len(pins))
	// of is the pin each key was made from.
	of := make([]int, 0, len(pins))
	unmatched := 0
	for i, p := range pins {
		resolve, ok := resolvers[p.Source]
		if !ok {
			var err error
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
		keys = append(keys, naturalKey{p.Source, account, p.Currency, p.Day, p.Amount})
		of = append(of, i)
	}
	matches, err := matchNaturalKeys(ctx, tx, keys)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: look up pins: %w", fam, err)
	}
	// In ledger order, so a later pin on a row overrides an earlier one.
	out := map[txKey]pinnedRow{}
	for j, rows := range matches {
		p := pins[of[j]]
		if len(rows) == 0 {
			unmatched++
		}
		for _, row := range rows {
			out[row.key] = pinnedRow{row: row, detailed: p.Detailed, assetClass: p.AssetClass}
		}
	}
	return out, unmatched, nil
}
