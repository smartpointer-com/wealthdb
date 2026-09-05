package canonical

import "testing"

// TestCanonicalSignTable pins the whole fixed-direction table: which
// kinds force a sign, which direction, and which are left to the
// source. The card kinds are read from the card account's own
// perspective — its balance is negative cash, so a purchase drives it
// further negative while a refund / payment / reward pays it back up.
func TestCanonicalSignTable(t *testing.T) {
	cases := []struct {
		k    TxKind
		want int
	}{
		// Money out.
		{TxKindBuy, -1},
		{TxKindWithdrawal, -1},
		{TxKindFee, -1},
		{TxKindTax, -1},
		{TxKindTransferOut, -1},
		{TxKindContribution, -1},
		{TxKindPurchase, -1},
		// Money in.
		{TxKindSell, +1},
		{TxKindDeposit, +1},
		{TxKindDividend, +1},
		{TxKindCoupon, +1},
		{TxKindTransferIn, +1},
		{TxKindDistribution, +1},
		{TxKindRefund, +1},
		{TxKindCardPayment, +1},
		{TxKindReward, +1},
		// Source-signed: direction depends on context.
		{TxKindInterest, 0},
		{TxKindStaking, 0},
		{TxKindCapitalGain, 0},
		{TxKindFx, 0},
		{TxKindFxForward, 0},
		{TxKindFxSwap, 0},
		{TxKindCorporateAction, 0},
		{TxKindJournal, 0},
		{TxKindOther, 0},
	}
	for _, c := range cases {
		if got := canonicalSign(c.k); got != c.want {
			t.Errorf("canonicalSign(%q) = %d, want %d", c.k, got, c.want)
		}
	}
	// Every kind in the enum is accounted for above, so a new kind
	// can't slip in without a deliberate sign decision.
	if len(cases) != len(txKindValues) {
		t.Errorf("table covers %d kinds, enum has %d — add the new kind here", len(cases), len(txKindValues))
	}
}

// TestApplyCanonicalSignCardKinds walks the card kinds through the
// helper adapters call, from both raw conventions: a source that
// reports positive magnitudes and one that reports pre-signed amounts.
// Either way the canonical direction wins.
func TestApplyCanonicalSignCardKinds(t *testing.T) {
	pos := NewDecimalFromInt(42)
	neg := pos.Neg()

	cases := []struct {
		k    TxKind
		want string
	}{
		{TxKindPurchase, "-42"},
		{TxKindRefund, "42"},
		{TxKindCardPayment, "42"},
		{TxKindReward, "42"},
	}
	for _, c := range cases {
		for _, in := range []Decimal{pos, neg} {
			raw := in
			got := ApplyCanonicalSign(c.k, &raw)
			if got == nil {
				t.Fatalf("%s: ApplyCanonicalSign(%s) returned nil", c.k, in.String())
			}
			if got.String() != c.want {
				t.Errorf("ApplyCanonicalSign(%s, %s) = %s, want %s",
					c.k, in.String(), got.String(), c.want)
			}
		}
	}

	// NULL and zero pass through untouched on the card kinds too.
	if ApplyCanonicalSign(TxKindPurchase, nil) != nil {
		t.Error("ApplyCanonicalSign(purchase, nil) must stay nil")
	}
	zero := NewDecimalFromInt(0)
	if got := ApplyCanonicalSign(TxKindPurchase, &zero); got.String() != "0" {
		t.Errorf("ApplyCanonicalSign(purchase, 0) = %s, want 0", got.String())
	}
}
