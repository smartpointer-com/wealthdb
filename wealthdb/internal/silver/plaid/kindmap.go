package plaid

import (
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// accountKindFor maps Plaid's account type and subtype to the canonical
// kind, and reports false for an account this adapter does not project:
// any loan but one on a home, and an account of type `other`.
//
// A loan on a home is a mortgage: subtype `mortgage`, `home equity` (a
// credit line on the home), `home equity loan` (its closed-end sibling) or
// `construction`. Gold has no kind for any other loan. Projected as `other`
// its pay-down would read as a gain, and as `mortgage` it would show as
// property. Left out, it is a lender gold does not track. A payment that
// reaches it from a tracked account usually places as `debt_repayment`
// (docs/adapters/plaid.md §3).
func accountKindFor(typ, subtype string) (canonical.AccountKind, bool) {
	switch norm(typ) {
	case "depository":
		return canonical.AccountKindCash, true
	case "credit":
		return canonical.AccountKindCard, true
	case "loan":
		switch norm(subtype) {
		case "mortgage", "home equity", "home equity loan", "construction":
			return canonical.AccountKindMortgage, true
		}
		return "", false
	case "investment":
		switch norm(subtype) {
		case "crypto exchange", "non-custodial wallet":
			return canonical.AccountKindCrypto, true
		}
		return canonical.AccountKindBrokerage, true
	}
	return "", false
}

// investmentWrappers maps an investment account's Plaid subtype to its tax
// wrapper. A subtype absent here leaves the wrapper unset, never `other`.
// Both read as household, but the load and `status -v` report an unset
// wrapper, and config `account_overrides` fills it in. Plaid's `trust` is
// one of them: it covers revocable trusts, which are the holder's own, and
// irrevocable ones, which are not.
var investmentWrappers = map[string]canonical.TaxWrapper{
	"brokerage":                 canonical.TaxWrapperTaxablePersonal,
	"cash management":           canonical.TaxWrapperTaxablePersonal,
	"mutual fund":               canonical.TaxWrapperTaxablePersonal,
	"stock plan":                canonical.TaxWrapperTaxablePersonal,
	"crypto exchange":           canonical.TaxWrapperTaxablePersonal,
	"non-custodial wallet":      canonical.TaxWrapperTaxablePersonal,
	"ira":                       canonical.TaxWrapperTraditionalIRA,
	"roth":                      canonical.TaxWrapperRothIRA,
	"sep ira":                   canonical.TaxWrapperSEPIRA,
	"sarsep":                    canonical.TaxWrapperSEPIRA,
	"simple ira":                canonical.TaxWrapperSIMPLEIRA,
	"401k":                      canonical.TaxWrapper401k,
	"401a":                      canonical.TaxWrapper401k,
	"roth 401k":                 canonical.TaxWrapper401k,
	"keogh":                     canonical.TaxWrapper401k,
	"profit sharing plan":       canonical.TaxWrapper401k,
	"roth profit sharing plan":  canonical.TaxWrapper401k,
	"thrift savings plan":       canonical.TaxWrapper401k,
	"roth thrift savings plan":  canonical.TaxWrapper401k,
	"403b":                      canonical.TaxWrapper403b,
	"roth 403b":                 canonical.TaxWrapper403b,
	"457b":                      canonical.TaxWrapper457b,
	"roth 457b":                 canonical.TaxWrapper457b,
	"529":                       canonical.TaxWrapper529,
	"education savings account": canonical.TaxWrapperCoverdellESA,
	"hsa":                       canonical.TaxWrapperHSA,
	"ugma":                      canonical.TaxWrapperCustodialUGMA,
	"utma":                      canonical.TaxWrapperCustodialUTMA,
}

// wrapperFor is an account's tax wrapper, or nil where config decides.
func wrapperFor(kind canonical.AccountKind, subtype string) *canonical.TaxWrapper {
	w := canonical.TaxWrapperTaxablePersonal
	switch kind {
	case canonical.AccountKindBrokerage, canonical.AccountKindCrypto:
		var ok bool
		if w, ok = investmentWrappers[norm(subtype)]; !ok {
			return nil
		}
	case canonical.AccountKindCash:
		if norm(subtype) == "hsa" {
			w = canonical.TaxWrapperHSA
		}
	}
	return &w
}

// styleFor is who places trades: the holder, on a bank, card or loan
// account. An investment account may be advised or managed, which Plaid
// does not say, so config decides.
func styleFor(kind canonical.AccountKind) *canonical.ManagementStyle {
	switch kind {
	case canonical.AccountKindBrokerage, canonical.AccountKindCrypto:
		return nil
	}
	s := canonical.ManagementStyleSelfDirected
	return &s
}

// Personal finance category values the kind maps read (taxonomy v2).
const (
	pfcBankFees       = "BANK_FEES"
	pfcInterestCharge = "BANK_FEES_INTEREST_CHARGE"
	pfcInterestEarned = "INCOME_INTEREST_EARNED"
	pfcLoanPayments   = "LOAN_PAYMENTS"
	pfcTransferIn     = "TRANSFER_IN"
)

// bankTxKind maps a bank or card ledger row to the canonical kind, from the
// account's kind, the row's direction (silver's fleet sign: money in is
// positive) and Plaid's category.
//
// On a cash account interest earned or charged is `interest`, any other
// bank-fee debit is `fee`, and everything else is a deposit or a withdrawal
// by sign. It is never a transfer kind, in either direction: that would
// leave both the spending and the income population.
//
// On a card a debit is a purchase, a fee or interest. A credit is the bill
// paid (`card_payment`) when Plaid files it as a loan payment or a transfer
// in, and a refund otherwise. The fee test sits inside the debit case: a
// fee reversal carries the same category, and `fee` forces a negative sign.
func bankTxKind(kind canonical.AccountKind, amount canonical.Decimal, primary, detailed string) canonical.TxKind {
	debit := amount.IsNegative()
	if kind == canonical.AccountKindCard {
		switch {
		case debit && detailed == pfcInterestCharge:
			return canonical.TxKindInterest
		case debit && primary == pfcBankFees:
			return canonical.TxKindFee
		case debit:
			return canonical.TxKindPurchase
		case primary == pfcLoanPayments || primary == pfcTransferIn:
			return canonical.TxKindCardPayment
		}
		return canonical.TxKindRefund
	}
	switch {
	case detailed == pfcInterestEarned, debit && detailed == pfcInterestCharge:
		return canonical.TxKindInterest
	case debit && primary == pfcBankFees:
		return canonical.TxKindFee
	case debit:
		return canonical.TxKindWithdrawal
	}
	return canonical.TxKindDeposit
}

// investmentKinds maps an investment transaction's Plaid subtype to the
// canonical kind. Subtypes that depend on the type or on what moved are
// decided in investmentTxKind.
var investmentKinds = map[string]canonical.TxKind{
	"buy":                                  canonical.TxKindBuy,
	"buy to cover":                         canonical.TxKindBuy,
	"dividend reinvestment":                canonical.TxKindBuy,
	"interest reinvestment":                canonical.TxKindBuy,
	"long-term capital gain reinvestment":  canonical.TxKindBuy,
	"short-term capital gain reinvestment": canonical.TxKindBuy,
	"sell":                                 canonical.TxKindSell,
	"sell short":                           canonical.TxKindSell,
	"dividend":                             canonical.TxKindDividend,
	"qualified dividend":                   canonical.TxKindDividend,
	"non-qualified dividend":               canonical.TxKindDividend,
	"interest":                             canonical.TxKindInterest,
	"interest receivable":                  canonical.TxKindInterest,
	"margin expense":                       canonical.TxKindInterest,
	"long-term capital gain":               canonical.TxKindCapitalGain,
	"short-term capital gain":              canonical.TxKindCapitalGain,
	"unqualified gain":                     canonical.TxKindCapitalGain,
	"account fee":                          canonical.TxKindFee,
	"fund fee":                             canonical.TxKindFee,
	"legal fee":                            canonical.TxKindFee,
	"management fee":                       canonical.TxKindFee,
	"miscellaneous fee":                    canonical.TxKindFee,
	"transfer fee":                         canonical.TxKindFee,
	"trust fee":                            canonical.TxKindFee,
	"tax":                                  canonical.TxKindTax,
	"tax withheld":                         canonical.TxKindTax,
	"non-resident tax":                     canonical.TxKindTax,
	"merger":                               canonical.TxKindCorporateAction,
	"spin off":                             canonical.TxKindCorporateAction,
	"split":                                canonical.TxKindCorporateAction,
	"stock distribution":                   canonical.TxKindCorporateAction,
	"expire":                               canonical.TxKindCorporateAction,
	"adjustment":                           canonical.TxKindCorporateAction,
	"return of principal":                  canonical.TxKindCorporateAction,
	// One cryptocurrency for another inside the account: neither a trade
	// gold can value nor money in or out.
	"trade": canonical.TxKindOther,
}

// movements are the subtypes that move money or securities in or out of
// the account. What moved decides the kind (investmentTxKind).
var movements = map[string]bool{
	"deposit": true, "contribution": true, "withdrawal": true,
	"distribution": true, "transfer": true, "send": true, "request": true,
	"pending credit": true, "pending debit": true,
}

// investmentTxKind maps an investment transaction to the canonical kind,
// and reports whether Plaid's type and subtype were recognised. `inKind` is
// true when the row moves a security, not cash; `inward` is the direction
// of what moved.
//
// A movement of cash is a deposit or a withdrawal. A cash `transfer_in` or
// `transfer_out` is in neither the spending nor the income population, and
// cash flow leaves an unpaired one out as an in-kind leg. A movement of a
// security is a transfer in or out, valued at the securities' worth.
// Exercise and assignment follow their type: the side of a trade, or a
// corporate action. An `adjustment` of type `fee` adjusts a fee; of any
// other type, a holding. Nothing maps to `contribution`, which is a capital
// call: cash out, outside the external flows.
func investmentTxKind(typ, subtype string, inKind, inward bool) (canonical.TxKind, bool) {
	sub := norm(subtype)
	if movements[sub] {
		return movementKind(inKind, inward), true
	}
	if sub == "exercise" || sub == "assignment" {
		switch norm(typ) {
		case "buy":
			return canonical.TxKindBuy, true
		case "sell":
			return canonical.TxKindSell, true
		}
		return canonical.TxKindCorporateAction, true
	}
	if sub == "adjustment" && norm(typ) == "fee" {
		return canonical.TxKindFee, true
	}
	if k, ok := investmentKinds[sub]; ok {
		return k, true
	}
	// An unknown subtype falls back on its type where the type alone fixes
	// the kind. A `transfer` or `cash` row does not: it may move money or
	// securities either way, or nothing at all.
	switch norm(typ) {
	case "buy":
		return canonical.TxKindBuy, false
	case "sell":
		return canonical.TxKindSell, false
	case "fee":
		return canonical.TxKindFee, false
	}
	return canonical.TxKindOther, false
}

func movementKind(inKind, inward bool) canonical.TxKind {
	switch {
	case inKind && inward:
		return canonical.TxKindTransferIn
	case inKind:
		return canonical.TxKindTransferOut
	case inward:
		return canonical.TxKindDeposit
	}
	return canonical.TxKindWithdrawal
}

// norm folds a Plaid enum value for lookup: Plaid's own spellings mix case
// ("403B", "IRA" in some docs).
func norm(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
