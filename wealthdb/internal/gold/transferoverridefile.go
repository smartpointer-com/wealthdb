package gold

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// The transfer-override ledger on disk: a CSV in the same idiom as the pins
// ledger, and readable by the same person for the same reason. Columns are
// found by header name, so their order does not matter and an extra column —
// `note`, the annotation that makes the file worth keeping — is ignored.
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
	cr := csv.NewReader(r)
	cr.TrimLeadingSpace = true
	cr.FieldsPerRecord = -1

	header, err := cr.Read()
	if err == io.EOF {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, c := range requiredOverrideCols {
		if _, ok := col[c]; !ok {
			return nil, fmt.Errorf("missing required column %q", c)
		}
	}
	get := func(rec []string, name string) string {
		i, ok := col[name]
		if !ok || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}

	var out []TransferOverrideRule
	line := 1
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		line++
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if blankRecord(rec) {
			continue
		}
		rule, err := overrideFromRecord(rec, get)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		out = append(out, rule)
	}
	return out, nil
}

func overrideFromRecord(rec []string, get func([]string, string) string) (TransferOverrideRule, error) {
	verb := strings.ToLower(get(rec, "verb"))
	if verb != "match" && verb != "unmatch" {
		return TransferOverrideRule{}, fmt.Errorf("verb %q is neither match nor unmatch", get(rec, "verb"))
	}
	a, err := selectorFrom(rec, get, "")
	if err != nil {
		return TransferOverrideRule{}, fmt.Errorf("first leg: %w", err)
	}
	rule := TransferOverrideRule{Verb: verb, A: a, Note: get(rec, "note")}

	anyB := false
	for _, c := range bLegCols {
		if get(rec, c) != "" {
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
	b, err := selectorFrom(rec, get, "_b")
	if err != nil {
		return TransferOverrideRule{}, fmt.Errorf("second leg: %w", err)
	}
	rule.B = &b
	return rule, nil
}

func selectorFrom(rec []string, get func([]string, string) string, suffix string) (TransferOverrideSelector, error) {
	s := TransferOverrideSelector{
		Source:   get(rec, "silver_source_id"+suffix),
		Account:  get(rec, "account"+suffix),
		Currency: strings.ToUpper(get(rec, "currency"+suffix)),
	}
	if s.Source == "" || s.Account == "" || s.Currency == "" {
		return s, fmt.Errorf("silver_source_id, account and currency are all required")
	}
	day := get(rec, "occurred_at"+suffix)
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return s, fmt.Errorf("occurred_at %q is not YYYY-MM-DD", day)
	}
	s.Day = t.UTC().Unix()
	amt := get(rec, "amount"+suffix)
	v, err := strconv.ParseFloat(strings.ReplaceAll(amt, "'", ""), 64)
	if err != nil {
		return s, fmt.Errorf("amount %q is not a number", amt)
	}
	s.Amount = v
	return s, nil
}

func blankRecord(rec []string) bool {
	for _, f := range rec {
		if strings.TrimSpace(f) != "" {
			return false
		}
	}
	return true
}
