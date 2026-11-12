package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
)

// buildCategorizeUserPrompt assembles the per-attempt user message.
// The taxonomy comes first (it is the closed vocabulary the answer
// must come from), then the anchors (this deployment's own prior
// answers), then the candidates at whatever depth the context level
// allows, then the output contract, then any feedback from the last
// attempt.
func buildCategorizeUserPrompt(fam categorizeFamily, candidates []merchantCandidate, anchors []merchantAnchor, level string, feedback []invalidRow) string {
	var b strings.Builder
	b.WriteString(fam.promptPreamble())
	for _, c := range fam.modelCategories() {
		fmt.Fprintf(&b, "  %s\t(%s)\t%s", c.Detailed, c.Primary, c.Description)
		if note := canonical.ModelNote(c.Detailed); note != "" {
			fmt.Fprintf(&b, "\tNote: %s", note)
		}
		b.WriteString("\n")
	}
	deltas := fam.deltaCategories()
	names := make([]string, 0, len(deltas))
	for _, d := range deltas {
		names = append(names, d.Detailed)
	}
	fmt.Fprintf(&b, `
Emit nothing outside that list. In particular NEVER emit %s: those are assigned elsewhere from information you do not have, and emitting one will be rejected.

`, strings.Join(names, ", "))
	if len(anchors) > 0 {
		fmt.Fprintf(&b, "Reference examples — %ss from this same dataset, already categorised. The INPUT row is the shape you are given; the OUTPUT row is the shape you must reply in.\n\nINPUT rows:\n", fam.counterparty)
		b.WriteString(formatAnchorSignatureCSV(anchors))
		b.WriteString("\nCORRECT OUTPUT rows (3 columns, this is the format your response must use):\n")
		b.WriteString(formatAnchorVerdictCSV(anchors))
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "%s to categorise — %s_signature,transaction_count:\n",
		strings.ToUpper(fam.plural()[:1])+fam.plural()[1:], fam.counterparty)
	b.WriteString(formatCandidateSignatureCSV(candidates))

	if level == config.SpendContextDescriptor || level == config.SpendContextTransaction {
		if s := formatCandidateDescriptorCSV(candidates); s != "" {
			fmt.Fprintf(&b, "\nRaw statement narratives these signatures were folded from — %s_signature,narrative:\n", fam.counterparty)
			b.WriteString(s)
		}
	}
	if level == config.SpendContextTransaction {
		if s := formatCandidateTransactionCSV(candidates); s != "" {
			fmt.Fprintf(&b, "\nExample transactions — %s_signature,date,amount,currency,account_kind,nearby_signatures:\n", fam.counterparty)
			b.WriteString(s)
		}
	}

	// The output contract, in this family's nouns. fam.outputContract()
	// is the SAME string the gauntlet quotes when it rejects a row, so
	// the instruction and the complaint cannot disagree.
	fmt.Fprintf(&b, `
Output format:
- CSV with columns: %[1]s
- No header row. Exactly three columns per row. One row per %[2]s you can confidently name.
- Double-quote any field containing a comma; backslash-escape inner quotes.
- %[2]s_signature must appear verbatim in the list above.
- %[2]s_name is the %[2]s's real-world name in normal casing (%[3]q, not %[4]q). Never repeat the signature unchanged.
- %[5]s must be one of the taxonomy values listed above.

Skip any signature you cannot confidently place. A skipped row costs nothing; a guessed one is invisible in a report and wrong forever.

Do not include explanatory prose. Output CSV only.
`, fam.outputContract(), fam.counterparty, fam.nameExample, strings.ToUpper(fam.nameExample), fam.valueColumn)
	if len(feedback) > 0 {
		b.WriteString("\nYour previous response contained the following rows that I rejected:\n")
		for _, iv := range feedback {
			fmt.Fprintf(&b, "  %v — reason: %s\n", iv.Raw, iv.Reason)
		}
		b.WriteString("\nRe-emit your full response with those invalid rows dropped or corrected. Keep the rows that WERE valid; do not add new invalid ones. Output CSV only.\n")
	}
	return b.String()
}

func formatAnchorSignatureCSV(anchors []merchantAnchor) string {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	for _, a := range anchors {
		_ = w.Write([]string{a.Signature})
	}
	w.Flush()
	return b.String()
}

func formatAnchorVerdictCSV(anchors []merchantAnchor) string {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	for _, a := range anchors {
		_ = w.Write([]string{a.Signature, a.Name, a.Detailed})
	}
	w.Flush()
	return b.String()
}

func formatCandidateSignatureCSV(cands []merchantCandidate) string {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	for _, c := range cands {
		_ = w.Write([]string{c.Signature, fmt.Sprintf("%d", c.Txns)})
	}
	w.Flush()
	return b.String()
}

func formatCandidateDescriptorCSV(cands []merchantCandidate) string {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	n := 0
	for _, c := range cands {
		for _, s := range c.Samples {
			if s.Descriptor == "" {
				continue
			}
			_ = w.Write([]string{c.Signature, s.Descriptor})
			n++
		}
	}
	w.Flush()
	if n == 0 {
		return ""
	}
	return b.String()
}

func formatCandidateTransactionCSV(cands []merchantCandidate) string {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	n := 0
	for _, c := range cands {
		for _, s := range c.Samples {
			_ = w.Write([]string{
				c.Signature,
				formatEpochDay(s.Day),
				fmt.Sprintf("%.2f", s.Amount),
				s.Currency,
				s.AccountKind,
				strings.Join(s.Neighbours, " | "),
			})
			n++
		}
	}
	w.Flush()
	if n == 0 {
		return ""
	}
	return b.String()
}
