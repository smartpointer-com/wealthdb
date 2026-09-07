package spending

import "testing"

// legRail classifies the card-bill rail. What matters is the ASYMMETRY: the
// receipt side is unmistakable and demands a card payment opposite it, while
// the paying side demands nothing, because the bank's record of paying a card
// often carries only the issuer's name, the holder's own, or nothing at all.
// Every value here is synthetic.
func TestLegRail(t *testing.T) {
	cases := []struct {
		name         string
		counterparty string
		description  string
		rail         string
		partner      string
	}{
		{"a card's record of being paid demands a card payment",
			"", "PAYMENT THANK YOU MOBILE", railCardReceipt, railCardPayment},
		{"the issuers' other receipt wording, same demand",
			"", "AUTOPAY PAYMENT RECEIVED THANK YOU", railCardReceipt, railCardPayment},
		{"a bank-side card payment announces its rail and demands none",
			"", "01 31 PAYMENT TO EXAMPLE CARD ENDING IN", railCardPayment, ""},
		{"an issuer's own wording for the same",
			"", "EXAMPLE CREDIT CRD EPAY ONUS WEB ID", railCardPayment, ""},
		// The shapes that must stay unclassified: an unclassified leg
		// constrains nothing and pairs exactly as it did before.
		{"a utility bill names no rail", "Example Power", "EXAMPLE POWER BILLPAY PPD ID", "", ""},
		{"a payment to a person names no rail",
			"", "QUICKPAY WITH ZELLE PAYMENT TO EXAMPLE PERSON", "", ""},
		{"a cheque names no rail", "", "IF YOU SEE A DESCRIPTION IN THE CHECKS PAID SECTION", "", ""},
		{"a blank narrative names no rail", "", "", "", ""},
		// The counterparty column is read too: some feeds carry the wording
		// there and leave the description empty.
		{"the rail may be written in either column",
			"PAYMENT THANK YOU", "", railCardReceipt, railCardPayment},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rail, partner := legRail(tc.counterparty, tc.description)
			if rail != tc.rail || partner != tc.partner {
				t.Errorf("legRail(%q, %q) = (%q, %q), want (%q, %q)",
					tc.counterparty, tc.description, rail, partner, tc.rail, tc.partner)
			}
		})
	}
}
