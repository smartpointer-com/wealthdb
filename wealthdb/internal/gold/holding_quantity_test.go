package gold

import "testing"

// TestHoldingUnionJoinsAnyIdentity pins that two legs move one holding
// where any identity agrees, directly or through another leg, and that the
// holding's name does not follow the order the legs are read in.
func TestHoldingUnionJoinsAnyIdentity(t *testing.T) {
	t.Parallel()
	legs := []holdingIdentity{
		{isin: "XX0000000001", cusip: "000000001", id: acctKey("fred", "XYZ")}, // the sender
		{cusip: "000000001", id: acctKey("schwab", "XYZ.B")},                   // the CUSIP alone
		{id: acctKey("fred", "XYZ")},                                           // the shared id alone
		{id: acctKey("schwab", "XYZ.C")},                                       // an id of its own
		{isin: "XX0000000002", id: acctKey("ubs", "OTHER")},                    // another ISIN
	}
	name := func(order []int) []string {
		u := newHoldingUnion()
		for _, i := range order {
			u.add(legs[i])
		}
		out := make([]string, len(legs))
		for i, l := range legs {
			out[i] = u.key(l)
		}
		return out
	}
	got := name([]int{0, 1, 2, 3, 4})
	if got[0] != got[1] || got[0] != got[2] {
		t.Errorf("the sender, the CUSIP alone and the shared id alone name %q, %q and %q, want one holding", got[0], got[1], got[2])
	}
	if got[3] == got[0] || got[4] == got[0] || got[3] == got[4] {
		t.Errorf("unrelated legs share a holding: %q", got)
	}
	if again := name([]int{4, 3, 2, 1, 0}); again[0] != got[0] || again[3] != got[3] {
		t.Errorf("the names follow the order: %q, then %q", got, again)
	}
}
