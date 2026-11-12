package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// categorization is one validated verdict, ready to upsert into the
// running family's verdict store. MerchantName holds the counterparty's
// name whichever family that is: a merchant's or a payer's.
type categorization struct {
	Signature    string
	MerchantName string
	Detailed     string
}

// parseAndValidateCategorizations parses the model's CSV body and
// partitions it into verdicts that may be stored and rows that may
// not. Every rejection carries a reason, which is what the next
// attempt's prompt feeds back.
//
// Unlike the resolve-symbols parser this one requires EXACTLY three
// columns. There the key is three columns and the value is one, so
// "the last column" disambiguates a padded row; here two of the three
// fields are free text and a padded row cannot be read at all without
// guessing which cell is the name and which the category.
func parseAndValidateCategorizations(fam categorizeFamily, body string, candSet map[string]bool) ([]categorization, []invalidRow) {
	body = stripCodeFences(body)
	r := csv.NewReader(strings.NewReader(body))
	r.FieldsPerRecord = -1 // tolerate ragged rows; each row is validated explicitly
	r.LazyQuotes = true

	var valid []categorization
	var invalid []invalidRow
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			invalid = append(invalid, invalidRow{Reason: fmt.Sprintf("CSV parse error: %v", err)})
			continue
		}
		if len(row) != 3 {
			invalid = append(invalid, invalidRow{Raw: row,
				Reason: fmt.Sprintf("expected exactly 3 columns (%s), got %d", fam.outputContract(), len(row))})
			continue
		}
		signature := strings.TrimSpace(row[0])
		name := strings.TrimSpace(row[1])
		category := strings.TrimSpace(row[2])

		if !candSet[signature] {
			invalid = append(invalid, invalidRow{Raw: row,
				Reason: fmt.Sprintf("signature %q was not in the candidate set", signature)})
			continue
		}
		if name == "" {
			invalid = append(invalid, invalidRow{Raw: row, Reason: fam.counterparty + "_name is empty"})
			continue
		}
		if name == signature {
			invalid = append(invalid, invalidRow{Raw: row,
				Reason: fmt.Sprintf("%s_name %q is the signature verbatim (model echoed input)", fam.counterparty, name)})
			continue
		}
		if fam.isDelta(category) {
			invalid = append(invalid, invalidRow{Raw: row,
				Reason: fmt.Sprintf("%s %q is assigned by the matcher and the rule tier, never by a model", fam.valueColumn, category)})
			continue
		}
		detailed := strings.ToUpper(category)
		if !fam.emittable(detailed) {
			reason := fmt.Sprintf("%s %q is not a value of the taxonomy", fam.valueColumn, category)
			// A spelling an earlier taxonomy held is what a model trained
			// on it reaches for, and naming its replacement is the
			// feedback that gets the next attempt right.
			if r, ok := canonical.RetiredDetailed(detailed); ok && fam.emittable(r.Successor) {
				reason = fmt.Sprintf("%s %q is retired: use %s", fam.valueColumn, category, r.Use)
			}
			invalid = append(invalid, invalidRow{Raw: row, Reason: reason})
			continue
		}
		valid = append(valid, categorization{Signature: signature, MerchantName: name, Detailed: detailed})
	}
	return valid, invalid
}
