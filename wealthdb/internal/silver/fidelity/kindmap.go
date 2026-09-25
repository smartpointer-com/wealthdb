package fidelity

import (
	"encoding/json"
	"strings"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// kindFor maps fidelity-web's `transactions.kind` (the
// first word of Fidelity's "Action" column, e.g. "BUY",
// "DIVIDEND", "CASH_SWEEP_IN") to canonical TxKind values.
// Some kinds need more than the raw verb. DISTRIBUTION reads the
// quantity and the row's Action text, because Fidelity overloads it.
// WIRE and DIRECT_DEBIT / DIRECT_DEPOSIT read the sign of `amount`,
// because their verbs do not state the direction from this account's
// side — WIRE carries none at all, and the DIRECT_* pair names the
// leg the originating bank saw. See the cases below.
//
// Fidelity's signed `amount` already follows the single-entry
// convention from the account's perspective (positive = cash in,
// negative = cash out), and the adapter keeps it on every kind
// (transactions.go). A row signed against its kind is therefore a
// correction, never a sign to repair: the kind says what was
// corrected and the sign nets it against the booking it corrects.
//
// The two CASH_SWEEP_* kinds are core-position shuffles (cash
// ↔ money-market fund); we route them as TxKindOther so the
// source sign is preserved verbatim — gold's cash_balances roll-
// up already reflects the resulting balance via the snapshot
// path, so we don't want the sign helper to second-guess these.
//
// Unrecognised values land as TxKindOther with the raw kind
// preserved in payload.
func kindFor(raw string, quantity, amount *canonical.Decimal, action string) canonical.TxKind {
	switch raw {
	case "BUY", "REINVESTMENT":
		// REINVESTMENT is the share-purchase leg of a reinvested
		// dividend (the cash leg is the matching DIVIDEND row);
		// canonical sign for both is negative-cash.
		return canonical.TxKindBuy
	case "SELL", "REDEMPTION":
		return canonical.TxKindSell
	case "DIVIDEND":
		return canonical.TxKindDividend
	case "DISTRIBUTION":
		// Fidelity overloads DISTRIBUTION: pooled-fund capital-
		// gain payouts are pure-cash rows, but the share legs of
		// stock splits, ADR ratio changes and spinoffs also book
		// as DISTRIBUTION — with Amount carrying the market value
		// of the shares received even though no cash moved. Only
		// the cash rows are dividend-class income; a nonzero
		// quantity (shares received) or a SPINOFF action marks a
		// corporate action instead.
		if (quantity != nil && !quantity.IsZero()) ||
			strings.Contains(strings.ToUpper(action), "SPINOFF") {
			return canonical.TxKindCorporateAction
		}
		return canonical.TxKindDividend
	case "INTEREST":
		return canonical.TxKindInterest
	case "FEE", "ADVISOR":
		// FEE is the security-level pass-through — an ADR depositary
		// charge, booked per position. ADVISOR is the account's own
		// management fee ("ADVISOR FEE DEDUCTED Advisor Fee" /
		// "Investment Mgr Fee"), which is the household paying for a
		// service. Both are money out of the account for a fee, so
		// both are TxKindFee — and the rule tier files both under
		// BANK_FEES_INVESTMENT_FEES, because both are the cost of
		// holding the assets. The narrative is what lets it tell one
		// from the other should that ever need to change
		// (internal/spending/rules.go).
		return canonical.TxKindFee
	case "TAX":
		return canonical.TxKindTax
	case "WIRE":
		// A wire, in whichever direction the amount says. Fidelity's
		// verb does not carry one — `WIRE TRANSFER TO BANK` and its
		// inbound sibling both reduce to `WIRE` — so the SIGN is the
		// only signal, and it must be read: an inbound wire read as a
		// TxKindWithdrawal would be a credit in the spending
		// population, netting against the household's spend.
		//
		// Outbound has to reach the spending population, where the
		// internal-transfer matcher gets first refusal: a wire to an
		// account wealthdb also tracks pairs and nets out, one to an
		// account it does not is spend. Landing it in TxKindOther, as
		// an unrecognised kind would, puts it beyond both.
		if amount != nil && amount.IsPositive() {
			return canonical.TxKindDeposit
		}
		return canonical.TxKindWithdrawal
	case "DIRECT_DEBIT", "DIRECT_DEPOSIT":
		// ACH pulls and pushes, read off the SVB Wealth Advisory
		// statements. The verb names a direction, but only the one the
		// originating bank saw: the same movement books as DIRECT DEBIT
		// on the account it leaves and DIRECT DEPOSIT on the one it
		// reaches, and a reversal books under the verb of the leg it
		// undoes. The sign is what actually says which way the money
		// went, so read it, exactly as WIRE above — and for the same
		// downstream reason: an ACH out has to reach the spending
		// population, where the internal-transfer matcher pairs it
		// against the receiving leg when wealthdb tracks that account
		// too. TxKindOther, which an unrecognised kind would give, puts
		// it beyond both.
		if amount != nil && amount.IsPositive() {
			return canonical.TxKindDeposit
		}
		return canonical.TxKindWithdrawal
	case "WITHDRAWAL", "DEPOSIT":
		// The statement's own two money sections, read out of the
		// supplied PDFs because the scraped activity feed carries
		// almost no cash movement (collectors/fidelity-web
		// DESIGN.md). A section name states its direction, so unlike
		// WIRE above there is no sign to read — and a withdrawal has
		// to reach the spending population for the same reason a
		// wire out does.
		if raw == "DEPOSIT" {
			return canonical.TxKindDeposit
		}
		return canonical.TxKindWithdrawal
	case "TRANSFER", "JOURNAL":
		// Both can flow either direction; keep source sign so
		// gold doesn't lose the in/out distinction.
		return canonical.TxKindJournal
	case "CASH_SWEEP_IN", "CASH_SWEEP_OUT":
		// Cash ↔ money-market fund movements. Source-signed.
		return canonical.TxKindOther
	case "MERGER", "NAME_CHANGE", "REVERSE_SPLIT", "TENDER",
		"EXPIRATION", "CASH_IN_LIEU", "RETURN_OF_CAPITAL":
		return canonical.TxKindCorporateAction
	case "ADJUSTMENT":
		return adjustmentKind(action)
	// Donor-Advised Fund event kinds (fidelity-web DESIGN.md §12).
	// From the giving account's perspective a GRANT / Gift4Giving
	// GIFT is cash irrevocably out (external flow), a CONTRIBUTION
	// external capital in, and an EXCHANGE an internal pool shuffle.
	// CONTRIBUTION also covers the retail 529 contribution rows —
	// external capital in there too.
	case "GRANT", "GIFT":
		return canonical.TxKindWithdrawal
	case "CONTRIBUTION":
		return canonical.TxKindDeposit
	case "EXCHANGE":
		return canonical.TxKindOther
	}
	return canonical.TxKindOther
}

// adjustmentKind reads what an ADJUSTMENT corrects off its Action.
//
// The statements and the activity feed file every correction under
// the one verb: a withholding refunded in part (`ADJ FOREIGN TAX
// PAID`, `ADJ NON-RESIDENT TAX`), an ADR fee or an advisory fee given
// back (`ADJUST FEE CHARGED`, `ADJUSTMENT FEE REVERSAL`), a dividend
// clawed back (`DIVIDEND ADJUSTMENT`). One can undo part of a booking
// or all of it; the svb statement builders drop one that undoes a whole
// booking together with it (collectors/svb/DESIGN.md). Booked as the kind it
// corrects, source-signed, it nets inside that kind's category, so
// the withholding, fees and dividends read what was actually paid. As
// `other` it would reach no cash flow at all.
//
// Anything else stays TxKindOther: an adjustment whose Action names
// only a security, or a kind other than those three, says nothing
// about which kind it corrects.
func adjustmentKind(action string) canonical.TxKind {
	a := strings.ToUpper(strings.TrimSpace(action))
	switch {
	case strings.HasPrefix(a, "ADJ FOREIGN TAX PAID"),
		strings.HasPrefix(a, "ADJ NON-RESIDENT TAX"):
		return canonical.TxKindTax
	case strings.HasPrefix(a, "ADJUST FEE CHARGED"),
		strings.HasPrefix(a, "ADJUSTMENT FEE REVERSAL"):
		return canonical.TxKindFee
	case strings.HasPrefix(a, "DIVIDEND ADJUSTMENT"):
		return canonical.TxKindDividend
	}
	return canonical.TxKindOther
}

// txPayload is what the adapter reads from a silver transaction's
// payload, parsed once per row. An absent key or malformed JSON reads
// as empty — no signal, never an error.
type txPayload struct {
	Action      string `json:"Action"`
	Description string `json:"Description"`
	// InstrumentHint is what a statement builder looked the row's
	// instrument up by when its statements could not settle one (svb:
	// collectors/svb/DESIGN.md). The export's rows never carry it.
	InstrumentHint string `json:"InstrumentHint"`
}

func parseTxPayload(payload string) txPayload {
	var p txPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return txPayload{}
	}
	return p
}

// narrative is the row's narrative for gold: the Action text as
// printed, falling back to the Description.
//
// Action is the movement ("ADVISOR FEE DEDUCTED Investment Mgr Fee",
// "WIRE TRANSFER TO BANK", "FOREIGN TAX PAID <security>"); Description
// is the SECURITY, which on a fee or a withholding says nothing about
// what happened. So Action leads and Description is what is left when
// a row carries no action at all.
//
// Case is preserved — this is the string a reader sees — and the
// matching downstream is case-insensitive.
func (p txPayload) narrative() string {
	if s := strings.TrimSpace(p.Action); s != "" {
		return s
	}
	return strings.TrimSpace(p.Description)
}
