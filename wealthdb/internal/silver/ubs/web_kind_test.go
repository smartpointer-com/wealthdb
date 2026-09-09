package ubs

import (
	"database/sql"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// TestWebKindClassification pins the description_kind → TxKind
// mapping for BOTH transaction sources that share the ubs-web
// `transactions` table:
//
//   - the MT940 CSV feed (2024-01-02 onward), whose genuine
//     deposit/withdrawal rows must stay classified as before so the
//     PDF backfill never perturbs the post-2024 flow set; its FX
//     legs are the deliberate exception — reclassified out of the
//     flow set into non-flow fx kinds (see below); and
//   - the pre-2024 Account-Statement PDF backfill, whose booking
//     types must land in the right kind so settlements / dividends /
//     fees / FX are excluded from flows and only genuine
//     deposits/withdrawals count.
func TestWebKindClassification(t *testing.T) {
	const (
		D = true  // hasDebit
		C = true  // hasCredit
		N = false // absent
	)
	cases := []struct {
		name      string
		desc      string
		hasDebit  bool
		hasCredit bool
		want      canonical.TxKind
	}{
		// ---- MT940 feed: MUST be unchanged (regression guard). ----
		{"mt940 dividend", "Dividend", N, C, canonical.TxKindDividend},
		{"mt940 dividend reversal", "Dividend;Reversal", D, N, canonical.TxKindDividend},
		{"mt940 credit", "credit", N, C, canonical.TxKindDeposit},
		{"mt940 ebanking payment", "e-banking payment order", D, N, canonical.TxKindWithdrawal},
		{"mt940 ebanking credit", "e-banking credit", N, C, canonical.TxKindDeposit},
		{"mt940 transfer prefix", "TRANSFER; e-banking payment order", D, N, canonical.TxKindWithdrawal},
		// FX legs in the MT940 feed classify to non-flow fx kinds by
		// instrument (spot/forward/swap) so the conversion legs never
		// enter net_flow. Both cash directions occur per instrument.
		// Precious-metal spot trades are securities, not fx.
		{"mt940 fx spot sale", "Sale FX Spot", N, C, canonical.TxKindFx},
		{"mt940 fx spot purchase", "Purchase FX Spot", D, N, canonical.TxKindFx},
		{"mt940 fx forward purchase", "Purchase FX Forward", D, N, canonical.TxKindFxForward},
		{"mt940 fx forward sale", "Sale FX Forward", N, C, canonical.TxKindFxForward},
		{"mt940 fx swap sale", "Sale from FX Swap", N, C, canonical.TxKindFxSwap},
		{"mt940 fx swap purchase", "Purchase from FX Swap", D, N, canonical.TxKindFxSwap},
		{"mt940 pm spot sell", "Sell PM spot w/o VAT", N, C, canonical.TxKindSell},
		{"mt940 order prefixed", "UCCDD00000000001; order", D, N, canonical.TxKindWithdrawal},
		{"mt940 capital gain", "Capital gain", N, C, canonical.TxKindDeposit},
		{"mt940 issue without rights", "Issue without rights", D, N, canonical.TxKindWithdrawal},
		// TWINT in the CSV feed's mixed case: money moving, by type.
		{"mt940 twint payment", "Payment UBS TWINT", D, N, canonical.TxKindWithdrawal},
		{"mt940 twint debit", "Debit UBS TWINT", D, N, canonical.TxKindWithdrawal},
		{"mt940 twint credit", "Credit UBS TWINT", N, C, canonical.TxKindDeposit},
		{"mt940 twint reversal", "Reversal UBS TWINT", N, C, canonical.TxKindDeposit},

		// ---- PDF backfill: correct classification. ----
		// Genuine external flows.
		{"pdf credit", "CREDIT", N, C, canonical.TxKindDeposit},
		{"pdf debit", "DEBIT", D, N, canonical.TxKindWithdrawal},
		{"pdf ebanking payment order", "E-BANKING PAYMENT ORDER", D, N, canonical.TxKindWithdrawal},
		{"pdf ebanking credit", "E-BANKING CREDIT", N, C, canonical.TxKindDeposit},
		{"pdf salary", "SALARY PAYMENT", N, C, canonical.TxKindDeposit},
		{"pdf atm", "ATM WITHDRAWAL", D, N, canonical.TxKindWithdrawal},
		{"pdf paynet", "PAYNET ORDER", D, N, canonical.TxKindWithdrawal},
		// TWINT: the two outflow types are withdrawals, the two inflow
		// types deposits — by type, not by column, so a reversal printed
		// with a trailing minus (a negative debit figure) is still the
		// inflow it is.
		{"pdf twint payment", "PAYMENT UBS TWINT", D, N, canonical.TxKindWithdrawal},
		{"pdf twint debit", "DEBIT UBS TWINT", D, N, canonical.TxKindWithdrawal},
		{"pdf twint credit", "CREDIT UBS TWINT", N, C, canonical.TxKindDeposit},
		{"pdf twint reversal", "REVERSAL UBS TWINT", N, C, canonical.TxKindDeposit},
		{"pdf twint reversal as negative debit", "REVERSAL UBS TWINT", D, N, canonical.TxKindDeposit},
		// Income / cost — excluded from flows.
		{"pdf dividend", "DIVIDEND", N, C, canonical.TxKindDividend},
		{"pdf reversal dividend", "REVERSAL DIVIDEND", D, N, canonical.TxKindDividend},
		{"pdf custody fee", "CUSTODY PRICE", D, N, canonical.TxKindFee},
		{"pdf adr fee", "ADR/GDR HANDLING FEES", D, N, canonical.TxKindFee},
		{"pdf service fee", "BALANCE CLOSING OF SERVICE PRICES", D, N, canonical.TxKindFee},
		{"pdf call deposit interest", "CALL DEPOSIT INTEREST PAYMENT", N, C, canonical.TxKindInterest},
		// FX conversion — internal, excluded.
		{"pdf forex purchase", "FOREX PURCHASE", D, N, canonical.TxKindFx},
		{"pdf forex sale", "FOREX SALE", N, C, canonical.TxKindFx},
		// Securities settlements — buy/sell by direction, excluded.
		{"pdf share buy", "SHARE", D, N, canonical.TxKindBuy},
		{"pdf share sell", "SHARE", N, C, canonical.TxKindSell},
		{"pdf mutual funds buy", "MUTUAL FUNDS", D, N, canonical.TxKindBuy},
		{"pdf ubs funds sell", "UBS INVESTMENT FUNDS", N, C, canonical.TxKindSell},
		{"pdf purchase", "PURCHASE", D, N, canonical.TxKindBuy},
		{"pdf sale", "SALE", N, C, canonical.TxKindSell},
		{"pdf order buy", "ORDER", D, N, canonical.TxKindBuy},
		{"pdf ubs manage buy", "UBS MANAGE", D, N, canonical.TxKindBuy},
		{"pdf precious metal sell", "PRECIOUS METAL SELL", N, C, canonical.TxKindSell},
		// Mortgage principal payoff — classified by direction
		// (a debit → withdrawal).
		{"pdf extraord amortization", "EXTRAORD. AMORTIZATION", D, N, canonical.TxKindWithdrawal},
		{"pdf mortgage closing", "CLOSING", D, N, canonical.TxKindWithdrawal},
		// Unknown / no direction hint.
		{"unknown no direction", "MYSTERY", N, N, canonical.TxKindOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := webKind(tc.desc, tc.hasDebit, tc.hasCredit)
			if got != tc.want {
				t.Fatalf("webKind(%q, debit=%v, credit=%v) = %q, want %q",
					tc.desc, tc.hasDebit, tc.hasCredit, got, tc.want)
			}
		})
	}
}

// TestWebProjectedNetKeepsAStatementReversal pins the second way a
// reversal announces itself. The export names it in the booking type
// (`<base>;Reversal`); a statement cannot, because its amount columns hold
// magnitudes and its booking type is whatever the bank printed — so it
// states the correction by printing a NEGATIVE FIGURE IN THE COLUMN THE
// ORIGINAL WENT IN. Forcing such a row back to its kind's normal direction
// turns a cancellation into a second copy of the booking it cancels, which
// is the error this guards.
//
// The last two cases are the boundary: the export's amount columns carry
// the direction in their own sign, so a negative debit there is an ordinary
// payment out and must still be normalised. Every value is synthetic.
func TestWebProjectedNetKeepsAStatementReversal(t *testing.T) {
	debit := func(v float64) sql.NullFloat64 { return sql.NullFloat64{Float64: v, Valid: true} }
	var none sql.NullFloat64

	for _, tc := range []struct {
		name          string
		descKind      string
		statementEra  bool
		debit, credit sql.NullFloat64
		want          float64
	}{
		{"a statement withdrawal", "E-BANKING PAYMENT ORDER", true, debit(100), none, -100},
		{"a statement dividend", "DIVIDEND", true, none, debit(100), 100},
		// The cancellations: money comes back, and money goes out again.
		{"a cancelled statement withdrawal", "CANC.MORT.MAT.", true, debit(-100), none, 100},
		{"a cancelled statement dividend", "REVERSAL DIVIDEND", true, none, debit(-100), -100},
		{"a cancelled statement purchase", "SHARE", true, debit(-100), none, 100},
		// The export states direction in the cell's own sign, and the
		// booking type is where its reversals are named.
		{"an export payment", "e-banking payment order", false, debit(-100), none, -100},
		{"an export reversal", "Dividend;Reversal", false, none, debit(-100), -100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, got := webProjectedNet(tc.descKind, tc.statementEra, tc.debit, tc.credit)
			if got == nil {
				t.Fatalf("webProjectedNet(%q) returned no signed amount", tc.descKind)
			}
			if want := canonical.NewDecimalFromFloat(tc.want); got.Cmp(want) != 0 {
				t.Errorf("webProjectedNet(%q, statementEra=%v) = %s, want %s",
					tc.descKind, tc.statementEra, got.String(), want.String())
			}
		})
	}
}
