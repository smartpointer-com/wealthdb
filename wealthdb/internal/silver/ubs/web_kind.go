package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// webProjectedNet is the signed amount a `ubs-web` cash row projects into
// gold, plus the kind and the raw column net it was derived from. It is the
// single definition of that derivation: the transaction loop emits what it
// returns and the era fold (buildEraFold) keys on it, so one booking's
// amount is the same number whichever era recorded it.
//
// It has to be a shared definition because the two web eras do not agree on
// the sign convention of the silver amount columns. The statement
// reconstruction writes the figure a statement PRINTS, and a statement
// prints a debit as a positive figure in its debit column; the export
// carries the sheet's own cell, which already states the direction in its
// sign. The raw column net therefore comes out with opposite signs for one
// booking, and a key built on it would never pair the two.
//
// What both eras do agree on is WHICH column carries the figure, and that is
// what webKind reads for direction. So the direction comes from the kind and
// the magnitude from the figure (canonical.ApplyCanonicalSign) — never from
// abs(), which would erase the difference between a payment and its
// reversal. A kind with no pinned direction (interest, fx, `other`) keeps
// the source's sign: signedCells says the cells state it (the export),
// where the debit cell is added as signed rather than subtracted as a
// printed figure.
//
// Reversal rows (webReversal) bypass the sign helper, which would otherwise
// mask the bank's correction by forcing the amount back to the base kind's
// normal direction — and a correction forced back into its base direction is
// not a correction, it is a SECOND COPY of the thing it cancels. Their kind
// still maps to the underlying canonical kind so they net against the
// originals when summed.
func webProjectedNet(descKind string, statementEra, signedCells bool, debit, credit sql.NullFloat64) (canonical.TxKind, canonical.Decimal, *canonical.Decimal) {
	var net canonical.Decimal
	if credit.Valid {
		net = net.Add(canonical.NewDecimalFromFloat(credit.Float64))
	}
	switch {
	case debit.Valid && signedCells:
		net = net.Add(canonical.NewDecimalFromFloat(debit.Float64))
	case debit.Valid:
		net = net.Sub(canonical.NewDecimalFromFloat(debit.Float64))
	}
	kind := webKind(descKind, debit.Valid, credit.Valid)
	signed := net
	if webReversal(descKind, statementEra, debit, credit) {
		return kind, net, &signed
	}
	return kind, net, canonical.ApplyCanonicalSign(kind, &signed)
}

// webReversal reports whether a row is the bank correcting a booking it has
// already made. Both places that pin a sign ask this ONE question, because
// a row that answers yes in one of them and no in the other is signed twice
// by two different rules and the second wins.
//
// A web era says it in one of two ways. The export names it in the booking
// type (`<base>;Reversal`). The statement cannot — its booking type is
// whatever the bank printed and its amount columns hold magnitudes — so it
// prints a NEGATIVE FIGURE IN THE COLUMN THE ORIGINAL WENT IN: a negative
// in the debit column is money coming back, a negative in the credit column
// is money going out again. That second reading is only available in the
// statement era, which is why the caller has to say which era the row
// belongs to; the export's cells carry the direction in their own sign, so
// there a negative debit is an ordinary payment out and means nothing of
// the kind.
func webReversal(descKind string, statementEra bool, debit, credit sql.NullFloat64) bool {
	if _, ok := stripReversalSuffix(descKind); ok {
		return true
	}
	return statementEra && (debit.Valid && debit.Float64 < 0 || credit.Valid && credit.Float64 < 0)
}

// webKindHint is the text webKind classifies a row by: the booking type
// when it is one the vocabulary knows; otherwise the first segment of
// the promoted counterparty when THAT is one. Some CSV-feed rows leave
// the booking-type column empty, or fill it with a reference — a
// safe-box number, an interest period — and carry the product in
// Description1, the very text the promotion reads as the payee. Read
// there for the kind as well as for the payee (projectWebTxText), a
// custody price or a safe-box rental is a fee and not a withdrawal.
// When neither text is a type the booking type is returned as given,
// and the direction decides.
func webKindHint(descKind, counterparty sql.NullString) string {
	if isBookingType(descKind.String) {
		return descKind.String
	}
	if head := firstSegment(counterparty.String); isBookingType(head) {
		return head
	}
	return descKind.String
}

// webKind maps the web silver's `description_kind` string plus
// debit/credit indicators to a canonical.TxKind. Conservative —
// unknown or ambiguous shapes route to TxKindOther rather than invent
// semantics that PSN's own MT940 events would contradict.
//
// A bank-side correction wears a `<base>;Reversal` suffix (e.g.
// `Dividend;Reversal`, a clawback of a dividend booking) and takes the
// same canonical kind as the event it reverses, so the two net out when
// summed by kind. Whether a row IS a reversal is webReversal's
// question, not this one's: the two eras state it differently, and this
// function sees neither the amounts nor the era.
//
// The type is matched as bookingOf matches it.
func webKind(descKind string, hasDebit, hasCredit bool) canonical.TxKind {
	// Strip a `;Reversal` suffix and classify the base, so a reversal
	// flavour the bank invents later needs no entry of its own.
	if base, ok := stripReversalSuffix(descKind); ok {
		return webKind(base, hasDebit, hasCredit)
	}
	b := bookingOf(descKind)
	if b.kind != "" {
		return b.kind
	}
	if b.settles == settlesTrade {
		return securitiesSide(hasDebit, hasCredit)
	}
	// Everything else, the bare payment order ("ORDER") among it, is
	// classified by direction: credit-only is a deposit, debit-only a
	// withdrawal, anything else stays "other". A payment order names
	// neither an instrument nor a side — no quantity, no price, a
	// narrative naming the party paid — and what it pays is often
	// another account of the same relationship, whose mirror books as
	// a plain credit.
	switch {
	case hasCredit && !hasDebit:
		return canonical.TxKindDeposit
	case hasDebit && !hasCredit:
		return canonical.TxKindWithdrawal
	}
	return canonical.TxKindOther
}

// securitiesSide maps a securities-settlement row to buy (cash out /
// debit) or sell (cash in / credit). Both kinds are excluded from
// the returns flow set, so the side is for analytics only.
func securitiesSide(hasDebit, hasCredit bool) canonical.TxKind {
	if hasCredit && !hasDebit {
		return canonical.TxKindSell
	}
	return canonical.TxKindBuy
}

// mt940FeedStart returns the earliest value_date carried by the MT940/CSV
// feed (rows without the PDF backfill's source marker), or 0 when the silver
// holds no MT940 rows at all. This is the era boundary for the classifier's
// rail promotion: from this day on, PDF rows are per-account gap-fills inside
// an era whose arrivals the MT940 feed counts, so they adopt MT940 semantics;
// before it, the deep backfill era stays conservative (see pdfCashIsExternal).
// A PDF-only silver (no MT940 coverage yet) therefore never promotes.
func (r *webReader) mt940FeedStart(ctx context.Context) (int64, error) {
	var start sql.NullInt64
	err := r.db.QueryRowContext(ctx, `
SELECT MIN(value_date) FROM transactions
 WHERE payload NOT LIKE '%account_statement_pdf%'`).Scan(&start)
	if err != nil {
		return 0, fmt.Errorf("mt940FeedStart: %w", err)
	}
	return start.Int64, nil
}

// webTxPayload is every field the per-row helpers read out of a web
// transaction's silver payload. The row is decoded once and the decoded
// value handed to each helper, rather than each helper decoding the
// same JSON into a struct of its own.
type webTxPayload struct {
	Source           string   `json:"source"`
	BookingType      string   `json:"booking_type"`
	InternalTransfer bool     `json:"internal_transfer"`
	CounterAccount   string   `json:"counter_account"`
	Continuation     []string `json:"continuation"`
	// SecurityValor is the Swiss valor the statement parser lifts off a
	// trade's continuation line. Statement era only: the export era
	// states its instrument as an ISIN in Description1 and takes the
	// other road. The caption the parser promotes beside it is for a
	// person reading silver, not for this adapter — the valor is the
	// identity and the caption only a label.
	SecurityValor string `json:"security_valor"`
	Description1  string `json:"Description1"`
	Description2  string `json:"Description2"`
	Description3  string `json:"Description3"`
}

// decodeWebTxPayload decodes a silver payload, discarding a partial
// fill on any error and reporting the failure rather than returning a
// value indistinguishable from a payload whose fields are absent. The
// distinction is load-bearing on the external/internal gate: an absent
// source means "not a PDF backfill", which SKIPS that gate, so a
// decode failure read as an absent field would take the permissive
// branch on the classifier that keeps owner capital from being
// fabricated. Callers treat ok=false as the conservative case; the
// zero value itself is conservative only for the fields the caller
// reaches after that (never external, no caption, no narrative lines).
func decodeWebTxPayload(payload string) (webTxPayload, bool) {
	var p webTxPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return webTxPayload{}, false
	}
	return p, true
}

// isPDFCashBackfill reports whether a transaction came from the
// Account-Statement PDF backfill (source="account_statement_pdf"). Gates the per-row external/internal
// classifier (pdfCashIsExternal); the MT940 feed carries no such
// marker and needs no per-row classifier beyond the same-day
// offset veto.
func isPDFCashBackfill(p webTxPayload) bool {
	return p.Source == "account_statement_pdf"
}

// normalizeIBAN strips spaces and upper-cases an IBAN-shaped string so a payload's
// formatted counter_account ("CH.. .... ....") compares byte-for-byte with an
// account_external_id (already no-spaces upper). Name-free.
func normalizeIBAN(s string) string {
	return strings.ToUpper(strings.ReplaceAll(s, " ", ""))
}

// stripReversalSuffix peels a `;Reversal` (case-insensitive)
// suffix off a description_kind. Returns (base, true) when a
// suffix was present, (descKind, false) otherwise.
func stripReversalSuffix(descKind string) (string, bool) {
	const suffix = ";Reversal"
	if len(descKind) > len(suffix) &&
		strings.EqualFold(descKind[len(descKind)-len(suffix):], suffix) {
		return descKind[:len(descKind)-len(suffix)], true
	}
	return descKind, false
}
