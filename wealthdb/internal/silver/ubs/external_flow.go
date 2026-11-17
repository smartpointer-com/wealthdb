package ubs

import (
	"context"
	"database/sql"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// buildOwnIBANSet returns the set of the relationship's OWN account IBANs, keyed
// by the normalized (spaces stripped, upper-cased) IBAN — exactly the shape a
// transaction payload's counter_account normalizes to. account_external_id IS the
// IBAN (schema: "IBAN no-spaces upper"), so this is a name-free, PII-free key:
// membership decides internal-vs-external without any holder name. Used by the
// PDF-backfill cash-flow classifier to recognise inter-own-account moves as a
// SUPPLEMENT to its internal-transfer rule (pdfInternalTransfer): it can demote
// a KNOWN own counter to internal, but it cannot promote — that rule is checked
// first and is authoritative, so an own mandate/portfolio destination absent
// from `accounts` is still vetoed to internal by it, not by this set.
func (r *webReader) buildOwnIBANSet(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	if err := silver.ScanRows(ctx, r.db, "buildOwnIBANSet", `SELECT DISTINCT account_external_id FROM accounts`, func(rows *sql.Rows) error {
		var iban string
		if err := rows.Scan(&iban); err != nil {
			return err
		}
		out[normalizeIBAN(iban)] = true
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// pdfCashIsExternal decides whether a PDF-backfill cash movement is a genuine
// boundary-crossing (EXTERNAL) owner-capital flow or internal churn. outbound
// reports the row's direction (debit side); railEra reports whether the row
// falls on/after the MT940 feed's first covered day (mt940FeedStart).
//
// UBS cash/current accounts are conduits: external cash lands and is routed into
// securities / mandates / FX inside the relationship, and the securities value
// spine carries the return. Feeding that internal churn into a flow-based return
// double-counts capital. The rule is therefore CONSERVATIVE toward internal —
// default INTERNAL, mark EXTERNAL only when the movement PROVABLY crosses the
// relationship boundary — because a fabricated external double-counts capital
// (catastrophic — loose org-marker heuristics fabricate externals). It uses
// ONLY the normalized counter_account IBAN, the structured booking type, and
// the row's direction — NO holder name, NO free-text counterparty, i.e. no PII.
//
// Decision order:
//  1. an internal transfer (pdfInternalTransfer) ⇒ INTERNAL (authoritative)
//  2. own counter IBAN                           ⇒ INTERNAL
//  3. HYPOTHEK / MATURITY / CLOSING booking      ⇒ INTERNAL (own liability /
//     product settling inside the relationship)
//  4. railEra + direction-matching rail booking  ⇒ EXTERNAL (the
//     booking type's rail, railOut or railIn; bookingTypes). No
//     counter IBAN required — the MT940 feed counts the identical bookings
//     as withdrawals/deposits via its direction fallback, so demoting the
//     gap-fill PDF copies would make returns depend on which feed covered
//     the month, and demoting only ONE direction fabricates return
//     outright. The intra-relationship shapes these rails could smuggle in
//     are peeled off first: rules 1–3 here, plus the same-day offset veto
//     at the call site.
//  5. populated non-own CH/LI counter IBAN       ⇒ EXTERNAL
//  6. everything else                            ⇒ INTERNAL (own-product cash
//     parkings, unrecognized shapes — conservative default)
//
// Rule 4 is era-gated: in the DEEP backfill era (before any MT940 coverage)
// both directions stay conservative-internal. That era's inbound capital is
// carried by the engine's onboarding step-ups (relationship and sub-entity
// debuts), so counting arrival credits as deposits double-counts it — and a
// per-row shape cannot distinguish a mid-life external arrival from migration
// funding a debut books days later. Swallowing both directions keeps the two
// errors offsetting instead of fabricating one-sided return; the honest fix
// for that era is transaction-complete counterparty data, not a looser rule.
func pdfCashIsExternal(p webTxPayload, own map[string]bool, outbound, railEra bool) bool {
	// The name-free markers on the continuation lines identify this row as an
	// intra-relationship mandate-funding / book-transfer move. That is
	// authoritative and VETOES external BEFORE any promotion below: a
	// mandate/portfolio destination absent from `accounts` is a known-internal
	// row that would otherwise pass the EXTERNAL conditions and fabricate owner
	// capital (the conduit direction this model exists to prevent). own-IBAN
	// membership is a supplement that can only DEMOTE a known-own counter to
	// internal; it cannot catch such a row, so this rule must gate first.
	if pdfInternalTransfer(p) {
		return false // internal reshuffle ⇒ never external
	}
	ctr := normalizeIBAN(p.CounterAccount)
	if ctr != "" && own[ctr] {
		return false // inter-own-account move: a KNOWN own counter demotes regardless of booking shape
	}
	// Mortgage amortisation + structured-product maturity/closing net inside the
	// relationship (payoff of an own liability / roll of an own product), not owner
	// capital crossing the boundary.
	bt := strings.ToUpper(strings.TrimSpace(p.BookingType))
	if strings.Contains(bt, "HYPOTHEK") || strings.Contains(bt, "MATURITY") || strings.Contains(bt, "CLOSING") {
		return false
	}
	rail := bookingTypes[bt].rail
	if railEra && outbound && rail == railOut {
		return true // owner-initiated payment through the rails ⇒ boundary-crossing
	}
	if railEra && !outbound && rail == railIn {
		return true // interbank arrival ⇒ boundary-crossing
	}
	if ctr == "" {
		return false // no counterparty IBAN and no rail evidence ⇒ INTERNAL
	}
	if !strings.HasPrefix(ctr, "CH") && !strings.HasPrefix(ctr, "LI") {
		return false // only Swiss/Liechtenstein counterparties count; anything else stays INTERNAL
	}
	return true
}

// internalTransferMarkers are the name-free words of an INTRA-portfolio cash
// move: funding or reducing a managed mandate, or an explicit book transfer.
// They reshuffle cash between the holder's own UBS accounts and mandates, so
// they are not external capital. A genuine external credit (an incoming bank
// transfer, a SIC payment, a salary) carries none of them. All are generic
// banking terms, no personal identifiers.
var internalTransferMarkers = []string{
	"UEBERTRAG", "UMBUCHUNG", // DE: transfer / rebooking
	"MANDAT", "MANAGE", // managed-mandate operations
	"PORTFOLIO", "REDUK", "REDUCTION", "INCREAS",
}

// pdfInternalTransfer reports whether a PDF-archive cash movement is an
// intra-portfolio reshuffle (funding/reducing a managed mandate, or a book
// transfer) rather than external capital, from the continuation lines the
// payload carries: a marker of internalTransferMarkers anywhere in them,
// upper-cased. A statement row qualifies only under a booking type that
// moves cash (bookingFacts.transfer). An advice prints no booking type, and its lines
// are the purpose of the payment, which is written for exactly these moves
// ("Uebertrag <portfolio>", "Increase <name> Mandate"), so it needs no gate.
func pdfInternalTransfer(p webTxPayload) bool {
	if !isPaymentAdvice(p) && !bookingOf(p.BookingType).transfer {
		return false
	}
	blob := strings.ToUpper(strings.Join(p.Continuation, " "))
	for _, m := range internalTransferMarkers {
		if strings.Contains(blob, m) {
			return true
		}
	}
	return false
}
