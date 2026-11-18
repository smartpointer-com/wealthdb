package ubs

import (
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// bookingFacts is what the adapter reads off one of the bank's booking
// types. A type the table does not hold states none of it: its kind
// follows the cash direction (webKind), and it is no rail, deposit
// product or charge of the bank's.
type bookingFacts struct {
	// kind is the kind the type books as whatever its direction; empty
	// where the direction decides.
	kind canonical.TxKind
	// settles is what the type settles, where that decides how it books.
	settles settlement
	// rail is the payment rail the type moves money on, where it names
	// one (pdfCashIsExternal).
	rail paymentRail
	// transfer marks a type that moves cash, under which a move between
	// own accounts can book (pdfInternalTransfer). A securities
	// settlement, a dividend or a fee referencing a portfolio is not a
	// flow at all, so it is never one.
	transfer bool
	// deposit marks a type that moves a deposit product's principal
	// (isDepositProductBooking).
	deposit bool
	// ownCharge marks a type the bank bills for its own services: it
	// names a product rather than a party, because the party is the bank
	// (isOwnServiceCharge).
	ownCharge bool
	// class and vehicle are what a securities type says it traded, where
	// the instrument itself is not known (unlinkedSecurityTaxonomy).
	class   canonical.AssetClass
	vehicle canonical.Vehicle
}

// settlement is what a booking type settles.
type settlement int

const (
	// The zero value: a type that states no settlement.
	_ settlement = iota
	// settlesTrade: a securities trade, whose side (buy or sell) follows
	// the cash direction (securitiesSide).
	settlesTrade
	// settlesNothing: securities delivered or received free of payment,
	// valued but with no cash leg (portfolioCashKind).
	settlesNothing
)

// paymentRail is the direction of a payment rail a booking type moves
// money on.
type paymentRail int

const (
	noRail paymentRail = iota
	// railOut: an owner-initiated payment out through the domestic rails
	// or a debit channel.
	railOut
	// railIn: a payment arriving through the interbank rails.
	railIn
)

// bookingOf is what the table says of a booking type, matched exactly on
// the trimmed, upper-cased type. The two feeds spell one type
// differently — the statement era shouts it, the CSV export title-cases
// it — so the fold classifies both, and an exact match keeps the two
// vocabularies apart: the MT940 feed's multi-token forms ("UCCDD…;
// order") never equal a bare statement type ("ORDER").
func bookingOf(bookingType string) bookingFacts {
	return bookingTypes[strings.ToUpper(strings.TrimSpace(bookingType))]
}

// bookingTypes is the bank's booking-type vocabulary, upper-cased: every
// fact the adapter reads off a type, in one place.
var bookingTypes = map[string]bookingFacts{
	// Income and cost: no capital flows, so excluded from returns.
	"DIVIDEND":                            {kind: canonical.TxKindDividend},
	"REVERSAL DIVIDEND":                   {kind: canonical.TxKindDividend},
	"COUPON":                              {kind: canonical.TxKindCoupon},
	"INTEREST":                            {kind: canonical.TxKindInterest},
	"INTEREST CALCULATION BALANCE":        {kind: canonical.TxKindInterest, ownCharge: true},
	"CALL DEPOSIT INTEREST PAYMENT":       {kind: canonical.TxKindInterest},
	"FIXED TERM DEPOSIT INTEREST PAYMENT": {kind: canonical.TxKindInterest},
	"FEE":                                 {kind: canonical.TxKindFee},
	"FEES":                                {kind: canonical.TxKindFee},
	"CUSTODY PRICE":                       {kind: canonical.TxKindFee, ownCharge: true},
	"RENTAL FEE SAFE BOX":                 {kind: canonical.TxKindFee, ownCharge: true},
	"BALANCE CLOSING OF SERVICE PRICES":   {kind: canonical.TxKindFee, ownCharge: true},
	"ADVICE":                              {kind: canonical.TxKindFee},
	"UBS ADVICE":                          {kind: canonical.TxKindFee, ownCharge: true},
	// Two charges the bank collects on someone else's behalf, so they
	// are not its own: a depositary's handling fee passed through, and a
	// third party's charge, as its name says. Attributing them to the
	// bank would misstate who was paid.
	"ADR/GDR HANDLING FEES": {kind: canonical.TxKindFee},
	"THIRD-PARTY CHARGES":   {kind: canonical.TxKindFee},
	// The discretionary mandate's periodic management charge. Like
	// "Custody Price" it names the product charged for and carries no
	// instrument, quantity or price. `CAN` cancels a charge already
	// billed and `REC` re-bills the corrected figure; the
	// cancellation's inflow survives because the statement era prints
	// it as a negative debit, which webReversal reads. The payee column
	// of all three holds the relationship the mandate runs under, which
	// is a reference and not a party: the bank is the party.
	"UBS MANAGE":     {kind: canonical.TxKindFee, ownCharge: true},
	"CAN UBS MANAGE": {kind: canonical.TxKindFee, ownCharge: true},
	"REC UBS MANAGE": {kind: canonical.TxKindFee, ownCharge: true},

	// Currency conversion between the holder's own accounts — an
	// internal reshuffle, not a capital flow. The MT940 feed names the
	// instrument, the older PDF backfill only says "FOREX". Enumerated
	// so the multi-token MT940 forms do not fall through to the
	// direction fallback and book as deposits / withdrawals.
	"FOREX PURCHASE":        {kind: canonical.TxKindFx},
	"FOREX SALE":            {kind: canonical.TxKindFx},
	"PURCHASE FX SPOT":      {kind: canonical.TxKindFx},
	"SALE FX SPOT":          {kind: canonical.TxKindFx},
	"PURCHASE FX FORWARD":   {kind: canonical.TxKindFxForward},
	"SALE FX FORWARD":       {kind: canonical.TxKindFxForward},
	"PURCHASE FROM FX SWAP": {kind: canonical.TxKindFxSwap},
	"SALE FROM FX SWAP":     {kind: canonical.TxKindFxSwap},
	"BUY":                   {kind: canonical.TxKindBuy},
	"SECURITIES PURCHASE":   {kind: canonical.TxKindBuy},
	"SELL":                  {kind: canonical.TxKindSell},
	"SECURITIES SALE":       {kind: canonical.TxKindSell},

	// The types that settle a securities trade. They reallocate between
	// cash and instruments and are excluded from flows.
	//
	// Private-market vehicles settle the same way under their own
	// vocabulary: a capital call buys fund units and a distribution sells
	// them, both against the cash account in the same portfolio as the
	// units. Left to the direction fallback they would be deposits and
	// withdrawals, which returns count as external capital — each leg of
	// one internal move booked as though the other did not exist.
	//
	// Where the instrument is not known, the type still says what KIND
	// of thing it traded: `PRECIOUS METAL SELL` sold a metal whether or
	// not anything can say which bar. The pairs are the 2-D taxonomy's
	// (exposure, wrapper) and match what this adapter emits for the same
	// things as positions: a precious metal is (metal, physical), and a
	// private-markets fund is (private_equity, fund) — docs/TAXONOMY.md
	// §5, where `private_fund` is the intermediate that maps to that
	// pair. `private_equity` is the exposure whether the holding is one
	// company or a fund of them; it is not a claim that a capital call
	// bought a single company.
	"SHARE":                               {settles: settlesTrade, class: canonical.AssetClassPublicEquity, vehicle: canonical.VehicleStock},
	"MUTUAL FUNDS":                        {settles: settlesTrade},
	"INVESTMENT FUNDS":                    {settles: settlesTrade},
	"UBS INVESTMENT FUNDS":                {settles: settlesTrade},
	"STRUCTURED PRODUCTS":                 {settles: settlesTrade},
	"PURCHASE":                            {settles: settlesTrade},
	"SALE":                                {settles: settlesTrade},
	"PRECIOUS METAL BUY":                  {settles: settlesTrade},
	"PRECIOUS METAL SELL":                 {settles: settlesTrade, class: canonical.AssetClassMetal, vehicle: canonical.VehiclePhysical},
	"BUY PM SPOT W/O VAT":                 {settles: settlesTrade},
	"SELL PM SPOT W/O VAT":                {settles: settlesTrade},
	"SUBSCRIPTION RIGHT":                  {settles: settlesTrade, class: canonical.AssetClassPublicEquity, vehicle: canonical.VehicleRight},
	"CAPITAL CALL":                        {settles: settlesTrade, class: canonical.AssetClassPrivateEquity, vehicle: canonical.VehicleFund},
	"ISSUE WITHOUT RIGHTS":                {settles: settlesTrade},
	"PURCHASE FROM ISSUE WITH PREPAYMENT": {settles: settlesTrade},
	"CASH SETTLEMENT":                     {settles: settlesTrade},
	"CASH DISTRIBUTION":                   {settles: settlesTrade},
	"PRECIOUS METAL PURCHASE":             {class: canonical.AssetClassMetal, vehicle: canonical.VehiclePhysical},

	// Owner-initiated payments through the domestic payment rails and
	// debit channels: payment orders (e-banking / paper / telephone /
	// standing), e-bill (PayNet), direct debit (LSV), QR-bill, ATM cash,
	// card and TWINT debits. These pay a beneficiary OUTSIDE the
	// relationship by construction — moves between own accounts book as
	// transfer forms the internal-transfer rule reads (pdfInternalTransfer),
	// carry an own counter IBAN, or mirror on the receiving own account and
	// fall to the same-day offset veto — so a debit with one of these
	// types that survives those checks is a boundary-crossing payment
	// even when the statement records no counter IBAN (it usually
	// doesn't: the beneficiary appears in free text only).
	"E-BANKING PAYMENT ORDER":    {rail: railOut, transfer: true},
	"MULTI E-BANKING ORDER":      {rail: railOut, transfer: true},
	"PAYMENT ORDER":              {rail: railOut, transfer: true},
	"SPECIAL PAYMENT ORDER":      {rail: railOut, transfer: true},
	"PAYMENT ORDER BY TELEPHONE": {rail: railOut, transfer: true},
	"VARIOUS STANDING ORDERS":    {rail: railOut},
	"PAYNET ORDER":               {rail: railOut, transfer: true},
	"MULTI PAYNET ORDER":         {rail: railOut, transfer: true},
	"DIRECT DEBIT":               {rail: railOut},
	"QR-BILL (INSTANT PAYMENT)":  {rail: railOut},
	"ATM WITHDRAWAL":             {rail: railOut},
	"UBS BANCOMAT WITHDRAWAL":    {rail: railOut},
	"PAYMENT TO CARD":            {rail: railOut},
	"DEBIT CARD PAYMENT":         {rail: railOut},

	// Payments ARRIVING through the interbank rails: the generic
	// credit-transfer booking (an incoming SIC/SWIFT wire), its e-banking
	// flavour, and salary — the mirror of the outbound rails. The MT940
	// feed counts the identical arrivals as deposits, so demoting the PDF
	// era's copies would understate inflows against counted outflows and
	// fabricate return. Own-product cash parkings (the deposit products
	// below) deliberately stay off the rails: they settle an own product
	// inside the relationship.
	"CREDIT":           {rail: railIn, transfer: true},
	"E-BANKING CREDIT": {rail: railIn, transfer: true},
	"SALARY PAYMENT":   {rail: railIn},
	// The statement's bare debit: no rail of its own (its direction
	// decides the kind), but cash moves under it.
	"DEBIT": {transfer: true},

	// Mobile payments cross the relationship boundary like a card
	// payment, on the rails of both directions: a payment made, and a
	// payment received or an outbound one reversed. Named so the kind
	// follows the booking type: the silver row carries an unsigned
	// figure in either column, and ApplyCanonicalSign orients the amount
	// by the kind, so a reversal keeps its inflow whichever column
	// printed it.
	"PAYMENT UBS TWINT":  {kind: canonical.TxKindWithdrawal, rail: railOut},
	"DEBIT UBS TWINT":    {kind: canonical.TxKindWithdrawal, rail: railOut},
	"CREDIT UBS TWINT":   {kind: canonical.TxKindDeposit, rail: railIn},
	"REVERSAL UBS TWINT": {kind: canonical.TxKindDeposit, rail: railIn},

	// The types that move PRINCIPAL in or out of one of the bank's own
	// cash-parking products — a call deposit, a fixed-term deposit, a
	// notice account. The bank books every one of them on the account
	// that funds the product and never lists the product as an account
	// of its own, so these rows are the only trace of it there is.
	//
	// The interest payment is deliberately not one. It is the one
	// movement of a deposit that is not a transfer: the money is new, it
	// is income, and a list that swept it in with the rest would take a
	// year of interest out of the income statement.
	"CALL DEPOSIT NEW INVESTMENT":       {deposit: true},
	"CALL DEPOSIT INCREASE":             {deposit: true},
	"CALL DEPOSIT DECREASE":             {deposit: true},
	"CALL DEPOSIT REPAYMENT":            {deposit: true},
	"FIXED TERM DEPOSIT NEW INVESTMENT": {deposit: true},
	"FIXED TERM DEPOSIT INCREASE":       {deposit: true},
	"FIXED TERM DEPOSIT DECREASE":       {deposit: true},
	"FIXED TERM DEPOSIT REPAYMENT":      {deposit: true},

	// The portfolio list's types that state a value and move no money:
	// securities delivered to or received from another custody account
	// with no cash leg. ISO 15022 calls that delivery or receipt free of
	// payment, and UBS spells it out in the type. They are named because
	// nothing else shows them: every other row that settles nothing says
	// so by leaving the value cell empty; these are valued, because the
	// securities are worth something, and booking that value as cash
	// would invent a payment on both accounts at once.
	"CUSTODY ACCOUNT TRANSFER DELIVERY WITHOUT PAYMENT": {settles: settlesNothing},
	"CUSTODY ACCOUNT TRANSFER RECEIPT WITHOUT PAYMENT":  {settles: settlesNothing},
}
