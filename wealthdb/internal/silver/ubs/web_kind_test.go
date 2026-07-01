package ubs

import (
	"testing"

	"github.com/ptu/wealthdb/internal/canonical"
)

// TestWebKindClassification pins the description_kind → TxKind
// mapping for BOTH transaction sources that share the ubs-web
// `transactions` table:
//
//   - the MT940 CSV feed (2024-01-02 onward), whose vocabulary must
//     stay classified EXACTLY as before so the post-2024 flow set
//     (and hence the returns) is unchanged by the PDF backfill; and
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
		name             string
		desc             string
		hasDebit         bool
		hasCredit        bool
		want             canonical.TxKind
	}{
		// ---- MT940 feed: MUST be unchanged (regression guard). ----
		{"mt940 dividend", "Dividend", N, C, canonical.TxKindDividend},
		{"mt940 dividend reversal", "Dividend;Reversal", D, N, canonical.TxKindDividend},
		{"mt940 credit", "credit", N, C, canonical.TxKindDeposit},
		{"mt940 ebanking payment", "e-banking payment order", D, N, canonical.TxKindWithdrawal},
		{"mt940 ebanking credit", "e-banking credit", N, C, canonical.TxKindDeposit},
		{"mt940 transfer prefix", "TRANSFER; e-banking payment order", D, N, canonical.TxKindWithdrawal},
		// FX in the MT940 feed keeps its historical direction-based
		// classification (NOT reclassified to fx_spot) — the multi-
		// token form never equals the bare PDF "FOREX PURCHASE".
		{"mt940 fx spot sale", "Sale FX Spot", N, C, canonical.TxKindDeposit},
		{"mt940 fx forward purchase", "Purchase FX Forward", D, N, canonical.TxKindWithdrawal},
		{"mt940 order prefixed", "UCCDD01000001494; order", D, N, canonical.TxKindWithdrawal},
		{"mt940 capital gain", "Capital gain", N, C, canonical.TxKindDeposit},
		{"mt940 issue without rights", "Issue without rights", D, N, canonical.TxKindWithdrawal},

		// ---- PDF backfill: correct classification. ----
		// Genuine external flows.
		{"pdf credit", "CREDIT", N, C, canonical.TxKindDeposit},
		{"pdf debit", "DEBIT", D, N, canonical.TxKindWithdrawal},
		{"pdf ebanking payment order", "E-BANKING PAYMENT ORDER", D, N, canonical.TxKindWithdrawal},
		{"pdf ebanking credit", "E-BANKING CREDIT", N, C, canonical.TxKindDeposit},
		{"pdf salary", "SALARY PAYMENT", N, C, canonical.TxKindDeposit},
		{"pdf atm", "ATM WITHDRAWAL", D, N, canonical.TxKindWithdrawal},
		{"pdf paynet", "PAYNET ORDER", D, N, canonical.TxKindWithdrawal},
		// Income / cost — excluded from flows.
		{"pdf dividend", "DIVIDEND", N, C, canonical.TxKindDividend},
		{"pdf reversal dividend", "REVERSAL DIVIDEND", D, N, canonical.TxKindDividend},
		{"pdf custody fee", "CUSTODY PRICE", D, N, canonical.TxKindFee},
		{"pdf adr fee", "ADR/GDR HANDLING FEES", D, N, canonical.TxKindFee},
		{"pdf service fee", "BALANCE CLOSING OF SERVICE PRICES", D, N, canonical.TxKindFee},
		{"pdf call deposit interest", "CALL DEPOSIT INTEREST PAYMENT", N, C, canonical.TxKindInterest},
		// FX conversion — internal, excluded.
		{"pdf forex purchase", "FOREX PURCHASE", D, N, canonical.TxKindFxSpot},
		{"pdf forex sale", "FOREX SALE", N, C, canonical.TxKindFxSpot},
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
		// Mortgage principal payoff — deferred: classified by
		// direction (a debit → withdrawal) until the returns-engine
		// follow-up nets it against the vanishing liability.
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
