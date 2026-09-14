package spending

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/gold"
)

// The pins ledger: per-transaction category pins, the top of the
// precedence lattice.
//
// A pin exists for the row nothing else can classify — no descriptor
// to match a rule on, no counter-leg for the matcher to pair, no
// provider label: an FX roll settling as a bare withdrawal, a
// subscription booked as a plain debit. The only thing that identifies
// such a row is WHICH row it is, so the ledger keys on the
// transaction's observable identity — source, account, day, amount,
// currency — and never on gold's transaction_external_id, which is
// opaque, adapter-specific and not something a person can read off a
// statement.
//
// That key is deliberately not unique. Two identical rows on one day
// — two legs of the same roll, two draws of the same size — are
// indistinguishable by design, and a pin applies to every row it
// describes. Anyone who needs to pin exactly one of two identical rows
// has a distinction gold does not carry either.
//
// A pin is the override surface, so its category may be ANY valid
// value, vendored or delta: it says what the holder knows the row to
// be. It is written with provenance `manual`, after every other tier,
// and — because the ledger is config-sourced — re-stamped by every
// pass. The pass owns `manual` rows the way it owns the derived ones:
// removing a pin from the ledger removes its effect on the next pass,
// and a fresh-file rebuild needs no carry-across.

// Pin is one row of the ledger.
type Pin struct {
	Source   string
	Account  string  // gold account_external_id OR a nickname, resolved at pass time
	Day      int64   // UTC midnight of occurred_at, Unix seconds
	Amount   float64 // net_amount as gold stores it: canonical sign, so a debit is negative
	Currency string
	Detailed string
}

// requiredPinCols are the ledger CSV columns. Lookup is by header
// name, so they may be given in any order, and a column outside this
// list is accepted and ignored — `note`, a free-text annotation kept
// for the ledger's own readability, is the one the format documents.
// The value column is the family's: `spend_detailed` in the spending
// ledger, `income_detailed` in the income one. Everything else about
// the format is shared, because a pin identifies a transaction the same
// way whichever question is being answered about it.
var requiredPinCols = []string{"silver_source_id", "account", "occurred_at", "amount", "currency"}

// pinAmountEps is how far a pin's amount may sit from the stored
// net_amount and still describe it: a cent, the same absolute floor
// gold's transfer matcher applies. A ledger is written from a
// statement showing two decimals; gold stores four.
const pinAmountEps = 0.01

// ParsePinLedger reads the pins ledger CSV at path. An empty path or a
// missing file is not an error — the ledger is optional and absence
// means "no pins".
func ParsePinLedger(path string) ([]Pin, error) {
	return ParsePinLedgerAs(path, "spending", "spend_detailed", canonical.ValidSpendDetailed)
}

// ParsePinLedgerAs is ParsePinLedger for a named family: the same
// format and the same transaction key, read for a different value
// column and validated against a different vocabulary. A spending
// value in the income ledger is refused at config load, which is the
// only place a person's typo can still be cheap to fix.
func ParsePinLedgerAs(path, family, valueCol string, valid func(string) bool) ([]Pin, error) {
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
	return parsePinLedger(f, family, valueCol, valid)
}

func parsePinLedger(r io.Reader, family, valueCol string, valid func(string) bool) ([]Pin, error) {
	cr := csv.NewReader(r)
	cr.TrimLeadingSpace = true
	cr.FieldsPerRecord = -1 // tolerate trailing/blank columns; we index by header

	header, err := cr.Read()
	if err == io.EOF {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s.pins: read header: %w", family, err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, c := range append(append([]string(nil), requiredPinCols...), valueCol) {
		if _, ok := col[c]; !ok {
			return nil, fmt.Errorf("%s.pins: missing required column %q", family, c)
		}
	}
	get := func(rec []string, name string) string {
		i, ok := col[name]
		if !ok || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}

	// Two rows describing the same transactions must agree: the pass
	// applies pins in ledger order and the later would silently win.
	// Rows that agree collapse to one — a pin already applies to every
	// row it describes, so repeating it adds nothing.
	type seenPin struct {
		line     int
		detailed string
	}
	var out []Pin
	seen := map[string]seenPin{}
	line := 1
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		line++
		if err != nil {
			return nil, fmt.Errorf("%s.pins: line %d: %w", family, line, err)
		}
		if isBlankRecord(rec) {
			continue
		}
		p, err := pinFromRecord(rec, get, valueCol, valid)
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
			continue
		}
		seen[key] = seenPin{line, p.Detailed}
		out = append(out, p)
	}
	return out, nil
}

func pinFromRecord(rec []string, get func([]string, string) string, valueCol string, valid func(string) bool) (Pin, error) {
	p := Pin{
		Source:   get(rec, "silver_source_id"),
		Account:  get(rec, "account"),
		Currency: strings.ToUpper(get(rec, "currency")),
		Detailed: get(rec, valueCol),
	}
	if p.Source == "" || p.Account == "" {
		return p, fmt.Errorf("silver_source_id and account are required")
	}
	ts, err := time.Parse("2006-01-02", get(rec, "occurred_at"))
	if err != nil {
		return p, fmt.Errorf("occurred_at must be YYYY-MM-DD, got %q", get(rec, "occurred_at"))
	}
	p.Day = ts.UTC().Unix()
	amount := get(rec, "amount")
	if amount == "" {
		return p, fmt.Errorf("amount is required")
	}
	if p.Amount, err = strconv.ParseFloat(strings.ReplaceAll(amount, ",", ""), 64); err != nil {
		return p, fmt.Errorf("amount: %w", err)
	}
	if len(p.Currency) != 3 {
		return p, fmt.Errorf("currency must be a 3-letter ISO code, got %q", get(rec, "currency"))
	}
	// Exact spelling: the vendored values are uppercase, the deltas
	// lowercase, and the casing is what says where a value came from.
	if !valid(p.Detailed) {
		return p, fmt.Errorf("%s %q is not a value of this family's taxonomy (vendored values are uppercase, the deltas lowercase)", valueCol, p.Detailed)
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

func isBlankRecord(rec []string) bool {
	for _, f := range rec {
		if strings.TrimSpace(f) != "" {
			return false
		}
	}
	return true
}

// pinnedRow is a pin resolved onto one gold transaction: the row's
// narrative, so it gets a signature like any other, and the verdict.
type pinnedRow struct {
	row      candidate
	detailed string
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
// The family NAME prefixes every error, because both families call it
// with their own ledger: an ambiguous nickname reported as `spending:`
// sends a reader to the wrong file.
func resolvePins(ctx context.Context, tx *sql.Tx, fam string, pins []Pin) (map[txKey]pinnedRow, int, error) {
	if len(pins) == 0 {
		return nil, 0, nil
	}
	stmt, err := tx.PrepareContext(ctx, `
        SELECT transaction_external_id, COALESCE(counterparty, ''), COALESCE(description, '')
          FROM transactions
         WHERE silver_source_id = ? AND account_external_id = ? AND currency = ?
           AND occurred_at >= ? AND occurred_at < ?
           AND ABS(CAST(net_amount AS DOUBLE) - ?) <= ?
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
			out[row.key] = pinnedRow{row: row, detailed: p.Detailed}
		}
	}
	return out, unmatched, nil
}

// pinMatches reads every transaction one pin describes, fully, before
// the next pin is looked up: the statement runs on the pass's
// transaction, which holds one connection.
func pinMatches(ctx context.Context, stmt *sql.Stmt, fam string, p Pin, account string) ([]candidate, error) {
	rows, err := stmt.QueryContext(ctx, p.Source, account, p.Currency,
		p.Day, p.Day+gold.SecondsPerDay, p.Amount, pinAmountEps)
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
