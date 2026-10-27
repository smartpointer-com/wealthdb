package fidelity

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// kindFor maps fidelity-web's `transactions.kind` (the
// first word of Fidelity's "Action" column, e.g. "BUY",
// "DIVIDEND", "CASH_SWEEP_IN") to canonical TxKind values.
// Some kinds need more than the raw verb. DISTRIBUTION reads the
// quantity and the row's Action text, because Fidelity overloads it.
// WIRE and DIRECT_DEBIT / DIRECT_DEPOSIT read the sign of `amount`,
// because their verbs do not state the direction from this account's
// side — WIRE carries none at all, and the DIRECT_* pair names the
// leg the originating bank saw. TRANSFER reads the signs and the
// `account` the row is booked on (transferKind). TRANSFER and JOURNAL
// both read the Action first, for a charge booked as a move
// (chargeKind). See the cases below.
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
func kindFor(raw string, quantity, amount *canonical.Decimal, action, account string) canonical.TxKind {
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
	case "TRANSFER":
		if k, ok := chargeKind(action); ok {
			return k
		}
		return transferKind(quantity, amount, action, account)
	case "JOURNAL":
		if k, ok := chargeKind(action); ok {
			return k
		}
		// It can flow either direction; keep source sign so gold
		// doesn't lose the in/out distinction.
		return canonical.TxKindJournal
	case "CASH_SWEEP_IN", "CASH_SWEEP_OUT":
		// Cash ↔ money-market fund movements. Source-signed.
		return canonical.TxKindOther
	case "MERGER", "NAME_CHANGE", "REVERSE_SPLIT", "CONVERSION", "TENDER",
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

// chargeKind reads a charge the account paid off the Action of a
// TRANSFER or a JOURNAL.
//
// Fidelity books an account's own tax payment and its tax-preparation
// fee as cash moved to one of its internal ledgers: `TRANSFERRED TO
// JNL… VS <ledger> TAX PAYMENT`, `JOURNALED JNL… <year> TAX PREP FEE`,
// and on the statements either verb for either charge. The verb names
// the booking, and the end of the Action names what was paid. Booked
// as a transfer or a journal, the row reaches no spending rule: the far
// leg is Fidelity's own ledger, so nothing pairs it, and a journal is
// outside the spending population.
//
// A tax payment is a withdrawal, not TxKindTax. Tax is the kind for
// tax withheld at source, which the income report shows beside the
// income it was withheld from. The rule tier files the withdrawal by
// its narrative. The preparation fee is a fee, like the account's
// other service charges.
//
// The charge must END the Action, before the account-type suffix. A
// transfer of a security whose name contains the words names that
// security after them.
func chargeKind(action string) (canonical.TxKind, bool) {
	a := strings.ToUpper(strings.TrimSpace(action))
	switch {
	case taxPaymentAction.MatchString(a):
		return canonical.TxKindWithdrawal, true
	case taxPrepFeeAction.MatchString(a):
		return canonical.TxKindFee, true
	}
	return "", false
}

var (
	taxPaymentAction = regexp.MustCompile(`\bTAX PAYMENT(?:\s+\([A-Z]+\))?$`)
	taxPrepFeeAction = regexp.MustCompile(`\bTAX PREP FEE(?:\s+\([A-Z]+\))?$`)
)

// transferKind reads a TRANSFER's direction off its signs.
//
// TRANSFER is value moving between two accounts, as cash or as shares.
// The export books it as `TRANSFERRED FROM` / `TRANSFERRED TO` between
// two Fidelity accounts. The svb statements book it for a move between
// their own accounts, for a whole-account move to or from another broker
// (`INTER BROKER DELIVER` / `RECEIVE` for the shares, `DEBIT` / `CREDIT`
// for the cash), and for shares deposited in kind
// (collectors/svb/load.py). The verb states no direction, so the signs
// must, as for WIRE. The direction is what lets the row reach the
// internal-transfer matcher, which admits no journal: as a journal the
// row could never pair with its far leg at the other account or broker.
//
// The quantity decides when shares moved. The shares are what moved, and
// a lot leaves or arrives by its quantity's sign. A cash row moves no
// shares, so its amount decides. A row with neither stays a journal. It
// has no direction to read, and a guessed one could pair with a
// stranger.
//
// An in-kind row can carry both, and then they must agree. On an svb row
// the amount is the TRAN VALUE figure, whose sign the statement parser
// normalises on the legacy template (pdf_parsers_svbwa
// `_lookahead_tran_value`). A row whose two signs disagree had one of
// them misread, so it stays a journal: a transfer kind whose amount runs
// the other way would mislead the matcher, the cash flow and the
// coverage ledger. The lot engine still reads a journal's direction off
// its quantity.
//
// A transfer that names the account it is booked on ("TRANSFERRED FROM
// VS <this account>-<n>") stays a journal too. It moves value between
// that account's own sub-accounts, like a JOURNAL, and no other account
// holds a far leg for it.
func transferKind(quantity, amount *canonical.Decimal, action, account string) canonical.TxKind {
	if t, ok := parseVSTransfer(action); ok && t.account == account {
		return canonical.TxKindJournal
	}
	q, a := signOf(quantity), signOf(amount)
	side := q
	if side == 0 {
		side = a
	}
	switch {
	case q != 0 && a != 0 && q != a, side == 0:
		return canonical.TxKindJournal
	case side > 0:
		return canonical.TxKindTransferIn
	}
	return canonical.TxKindTransferOut
}

// signOf is d's sign, 0 for a nil d.
func signOf(d *canonical.Decimal) int {
	if d == nil {
		return 0
	}
	return d.Sign()
}

// vsTransferRe is the Action of a transfer between two Fidelity
// accounts: "TRANSFERRED FROM VS <account>-<n> <security> …", or TO. The
// account prints as two digit groups, which joined are its
// account_external_id.
var vsTransferRe = regexp.MustCompile(`^TRANSFERRED (FROM|TO) VS (\d{3})-(\d{6})-\d+(?:\s+(.*))?$`)

// vsTransfer is a transfer Action that names a Fidelity account.
type vsTransfer struct {
	from    bool   // TRANSFERRED FROM: the row is the receiving leg
	account string // the account named, as its account_external_id
	rest    string // the Action after the account reference
}

func parseVSTransfer(action string) (vsTransfer, bool) {
	m := vsTransferRe.FindStringSubmatch(strings.TrimSpace(action))
	if m == nil {
		return vsTransfer{}, false
	}
	return vsTransfer{from: m[1] == "FROM", account: m[2] + m[3], rest: m[4]}, true
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
