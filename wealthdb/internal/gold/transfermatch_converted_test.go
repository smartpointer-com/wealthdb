package gold

import "testing"

// TestTheConvertedPhase pins the cross-currency window: a debit and a
// credit in two currencies pair at the debit's day's rate within the
// tolerance, and only where a narrative names the pair and each is the
// other's only candidate. Every figure is invented.
func TestTheConvertedPhase(t *testing.T) {
	rate := func(from, to string, _ int64) (float64, bool) {
		if from == "USD" && to == "CHF" {
			return 0.9, true
		}
		return 0, false
	}
	named := []string{NamedAccount("bank", "CHF1")}
	opts := TransferMatchOpts{WindowDays: 3, TolerancePct: 0.5, Rate: rate, ConvertedTolerancePct: 1}
	byPhase := func(legs []TransferLeg) map[string]string {
		got := map[string]string{}
		for _, p := range MatchTransferLegs(legs, opts) {
			got[p.Debit.ID] = p.Credit.ID + " " + string(p.By)
		}
		return got
	}

	// Named, at the rate within 1%: pairs. 1000 USD is 900 CHF; 905 is
	// inside the tolerance.
	got := byPhase([]TransferLeg{
		{Group: "broker", Owner: "USD1", ID: "OUT", Day: 10, Ccy: "USD", Amt: -1000, Names: named},
		{Group: "bank", Owner: "CHF1", ID: "IN", Day: 11, Ccy: "CHF", Amt: 905},
	})
	if got["OUT"] != "IN converted" {
		t.Errorf("a named conversion = %q, want the converted pair", got["OUT"])
	}

	// Unnamed, off the rate, outside the window, or with two candidates:
	// none pairs.
	for name, legs := range map[string][]TransferLeg{
		"unnamed": {
			{Group: "broker", Owner: "USD1", ID: "OUT", Day: 10, Ccy: "USD", Amt: -1000},
			{Group: "bank", Owner: "CHF1", ID: "IN", Day: 11, Ccy: "CHF", Amt: 900}},
		"off the rate": {
			{Group: "broker", Owner: "USD1", ID: "OUT", Day: 10, Ccy: "USD", Amt: -1000, Names: named},
			{Group: "bank", Owner: "CHF1", ID: "IN", Day: 11, Ccy: "CHF", Amt: 950}},
		"outside the window": {
			{Group: "broker", Owner: "USD1", ID: "OUT", Day: 10, Ccy: "USD", Amt: -1000, Names: named},
			{Group: "bank", Owner: "CHF1", ID: "IN", Day: 20, Ccy: "CHF", Amt: 900}},
		"two candidates": {
			{Group: "broker", Owner: "USD1", ID: "OUT", Day: 10, Ccy: "USD", Amt: -1000, Names: named},
			{Group: "bank", Owner: "CHF1", ID: "IN", Day: 11, Ccy: "CHF", Amt: 900},
			{Group: "bank", Owner: "CHF1", ID: "IN2", Day: 12, Ccy: "CHF", Amt: 901}},
		"two debits want one credit": {
			{Group: "broker", Owner: "USD1", ID: "OUT", Day: 10, Ccy: "USD", Amt: -1000, Names: named},
			{Group: "broker", Owner: "USD2", ID: "OUT2", Day: 10, Ccy: "USD", Amt: -1001, Names: named},
			{Group: "bank", Owner: "CHF1", ID: "IN", Day: 11, Ccy: "CHF", Amt: 900}},
		"no rate": {
			{Group: "broker", Owner: "EUR1", ID: "OUT", Day: 10, Ccy: "EUR", Amt: -1000, Names: named},
			{Group: "bank", Owner: "CHF1", ID: "IN", Day: 11, Ccy: "CHF", Amt: 900}},
	} {
		if got := byPhase(legs); got["OUT"] != "" {
			t.Errorf("%s: OUT paired with %q, want no pair", name, got["OUT"])
		}
	}

	// The credit naming the debit's account is enough.
	if got := byPhase([]TransferLeg{
		{Group: "broker", Owner: "USD1", ID: "OUT", Day: 10, Ccy: "USD", Amt: -1000},
		{Group: "bank", Owner: "CHF1", ID: "IN", Day: 11, Ccy: "CHF", Amt: 900, Names: []string{"broker"}},
	}); got["OUT"] != "IN converted" {
		t.Errorf("a credit naming the debit: %q, want the converted pair", got["OUT"])
	}

	// The phases around it, with the reversal phase on as the spending
	// caller runs it. A debit the amount phase paired is not offered to
	// the converted phase, and a converted debit is not offered to the
	// reversal phase: each debit pairs once, and the other credit stays
	// free.
	opts.SameAccountReversals = true
	for name, c := range map[string]struct {
		legs []TransferLeg
		want string
	}{
		"amount first": {[]TransferLeg{
			{Group: "broker", Owner: "USD1", ID: "OUT", Day: 10, Ccy: "USD", Amt: -1000},
			{Group: "bank", Owner: "USD2", ID: "SAME", Day: 11, Ccy: "USD", Amt: 1000},
			{Group: "bank", Owner: "CHF1", ID: "IN", Day: 11, Ccy: "CHF", Amt: 900, Names: []string{"broker"}},
		}, "SAME amount"},
		"converted before reversal": {[]TransferLeg{
			{Group: "broker", Owner: "USD1", ID: "OUT", Day: 10, Ccy: "USD", Amt: -1000, Names: named, Signature: "WIRE"},
			{Group: "broker", Owner: "USD1", ID: "BACK", Day: 12, Ccy: "USD", Amt: 1000, Signature: "WIRE"},
			{Group: "bank", Owner: "CHF1", ID: "IN", Day: 11, Ccy: "CHF", Amt: 900},
		}, "IN converted"},
	} {
		pairs := MatchTransferLegs(c.legs, opts)
		if len(pairs) != 1 || pairs[0].Debit.ID+" "+pairs[0].Credit.ID+" "+string(pairs[0].By) != "OUT "+c.want {
			t.Errorf("%s: pairs = %+v, want OUT to %s alone", name, pairs, c.want)
		}
	}
	opts.SameAccountReversals = false

	// Without a Rate the phase is off: the returns caller's matching.
	opts.Rate = nil
	if got := byPhase([]TransferLeg{
		{Group: "broker", Owner: "USD1", ID: "OUT", Day: 10, Ccy: "USD", Amt: -1000, Names: named},
		{Group: "bank", Owner: "CHF1", ID: "IN", Day: 11, Ccy: "CHF", Amt: 900},
	}); got["OUT"] != "" {
		t.Errorf("no Rate: OUT paired with %q, want no pair", got["OUT"])
	}
}
