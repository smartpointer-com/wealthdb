package gold

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/ledgercsv"
)

// The transfer-override ledger on disk: a CSV in the same idiom as the pins
// ledger, and readable by the same person for the same reason. Columns are
// found by header name, so their order does not matter; `note` is the
// annotation that makes the file worth keeping. A column the format does not
// know is an error, as in the pins: a misspelled `_b` header would otherwise
// turn a two-leg unmatch into a one-leg one, a far broader statement.
// Unlike the pins, `account` is the gold account_external_id itself; a
// nickname names nothing here.
//
// One line names one or two legs:
//
//	verb,silver_source_id,account,occurred_at,amount,currency,silver_source_id_b,account_b,occurred_at_b,amount_b,currency_b,note
//	unmatch,chase,CHECKING,2099-01-04,-10000.00,USD,chase,CHECKING,2099-01-05,9954.00,USD,a refund is not the other half
//	unmatch,chase,CHECKING,2099-02-01,-1234.56,USD,,,,,,"this cheque is spending, never a transfer"
//	match,chase,CHECKING,2099-03-10,-10000.00,USD,cointracking,Wallet,2099-03-04,10000.00,USD,the ACH posted six days after the credit
//
// An `unmatch` with only the first leg isolates it: the row is not half of a
// movement at all. With both legs it forbids just that pairing, leaving each
// leg free to find its real partner — the narrower statement, and the one a
// coincidence usually calls for.

var requiredOverrideCols = []string{"verb", "silver_source_id", "account", "occurred_at", "amount", "currency"}

// bLegCols are the second leg's columns; all blank means "no second leg".
var bLegCols = []string{"silver_source_id_b", "account_b", "occurred_at_b", "amount_b", "currency_b"}

// ParseTransferOverrideLedger reads the ledger at path. A missing file is not
// an error — the override surface is optional, and its absence is the normal
// case — but an unreadable or malformed one is.
func ParseTransferOverrideLedger(path string) ([]TransferOverrideRule, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("transfer overrides: open %s: %w", path, err)
	}
	defer f.Close()
	rules, err := parseTransferOverrideLedger(f)
	if err != nil {
		return nil, fmt.Errorf("transfer overrides: %s: %w", path, err)
	}
	return rules, nil
}

func parseTransferOverrideLedger(r io.Reader) ([]TransferOverrideRule, error) {
	lr, err := ledgercsv.Open(r, requiredOverrideCols)
	if err != nil {
		return nil, err
	}
	if lr == nil { // no header: an empty ledger
		return nil, nil
	}
	known := map[string]bool{"note": true}
	for _, c := range append(append([]string{}, requiredOverrideCols...), bLegCols...) {
		known[c] = true
	}
	for _, name := range lr.Columns() {
		if name != "" && !known[name] {
			return nil, fmt.Errorf("unknown column %q; the format is %v, the second leg's %v, and note",
				name, requiredOverrideCols, bLegCols)
		}
	}
	var out []TransferOverrideRule
	for {
		row, line, err := lr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		rule, err := overrideFromRecord(row)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		out = append(out, rule)
	}
	return out, nil
}

func overrideFromRecord(row ledgercsv.Row) (TransferOverrideRule, error) {
	verb := strings.ToLower(row.Get("verb"))
	if verb != "match" && verb != "unmatch" {
		return TransferOverrideRule{}, fmt.Errorf("verb %q is neither match nor unmatch", row.Get("verb"))
	}
	a, err := selectorFrom(row, "")
	if err != nil {
		return TransferOverrideRule{}, fmt.Errorf("first leg: %w", err)
	}
	rule := TransferOverrideRule{Verb: verb, A: a, Note: row.Get("note")}

	anyB := false
	for _, c := range bLegCols {
		if row.Get(c) != "" {
			anyB = true
			break
		}
	}
	if !anyB {
		if verb == "match" {
			return TransferOverrideRule{}, fmt.Errorf("match needs both legs")
		}
		return rule, nil
	}
	b, err := selectorFrom(row, "_b")
	if err != nil {
		return TransferOverrideRule{}, fmt.Errorf("second leg: %w", err)
	}
	rule.B = &b
	return rule, nil
}

func selectorFrom(row ledgercsv.Row, suffix string) (TransferOverrideSelector, error) {
	s := TransferOverrideSelector{
		Source:   row.Get("silver_source_id" + suffix),
		Account:  row.Get("account" + suffix),
		Currency: strings.ToUpper(row.Get("currency" + suffix)),
	}
	if s.Source == "" || s.Account == "" || s.Currency == "" {
		return s, fmt.Errorf("silver_source_id, account and currency are all required")
	}
	day := row.Get("occurred_at" + suffix)
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return s, fmt.Errorf("occurred_at %q is not YYYY-MM-DD", day)
	}
	s.Day = t.UTC().Unix()
	amt := row.Get("amount" + suffix)
	v, err := strconv.ParseFloat(strings.ReplaceAll(amt, "'", ""), 64)
	if err != nil {
		return s, fmt.Errorf("amount %q is not a number", amt)
	}
	s.Amount = v
	return s, nil
}
