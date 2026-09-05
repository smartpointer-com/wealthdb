package spending

import (
	"strings"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// TestParsePinLedger pins the CSV contract: header-named columns in any
// order, `note` accepted and ignored, blank lines skipped, currency
// upper-cased, a thousands separator tolerated, the date folded to UTC
// midnight, and the category kept in the spelling the taxonomy uses.
// Every id and amount is invented.
func TestParsePinLedger(t *testing.T) {
	csv := `silver_source_id,account,occurred_at,amount,currency,spend_detailed,note
bank,Everyday Cash,2026-03-04,"-2,500.00",usd,investment,roll leg

bank,CASH1,2026-03-05,-1200,USD,GENERAL_SERVICES_CONSULTING_AND_LEGAL,
`
	got, err := parsePinLedger(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2 (blank line skipped)", len(got))
	}
	p := got[0]
	if p.Source != "bank" || p.Account != "Everyday Cash" || p.Amount != -2500 || p.Currency != "USD" ||
		p.Detailed != canonical.SpendDetailedInvestment {
		t.Errorf("row 0 mis-parsed: %+v", p)
	}
	if p.Day != 1772582400 { // 2026-03-04T00:00:00Z
		t.Errorf("row 0 day = %d, want UTC midnight of 2026-03-04", p.Day)
	}
	if got[1].Detailed != "GENERAL_SERVICES_CONSULTING_AND_LEGAL" {
		t.Errorf("row 1 mis-parsed: %+v", got[1])
	}

	reordered := `note,spend_detailed,currency,amount,occurred_at,account,silver_source_id
x,cash_withdrawal,chf,-300,2026-01-02,A,bank
`
	got, err = parsePinLedger(strings.NewReader(reordered))
	if err != nil {
		t.Fatalf("parse reordered: %v", err)
	}
	if p := got[0]; p.Account != "A" || p.Amount != -300 || p.Currency != "CHF" || p.Detailed != canonical.SpendDetailedCashWithdrawal {
		t.Errorf("reordered columns mis-parsed: %+v", p)
	}
}

// TestParsePinLedgerErrors: every malformed row fails the parse with a
// line number, because a ledger that half-applies is worse than one
// that refuses. The category is checked in the taxonomy's own casing
// — a pin may set any valid value, vendored or delta, but not a
// misspelling of one.
func TestParsePinLedgerErrors(t *testing.T) {
	header := "silver_source_id,account,occurred_at,amount,currency,spend_detailed\n"
	cases := map[string]string{
		"missing required column": "silver_source_id,account,occurred_at,amount,currency\nbank,A,2026-01-01,-1,USD\n",
		"bad date":                header + "bank,A,01/02/2026,-1,USD,other\n",
		"missing amount":          header + "bank,A,2026-01-01,,USD,other\n",
		"bad amount":              header + "bank,A,2026-01-01,ten,USD,other\n",
		"bad currency":            header + "bank,A,2026-01-01,-1,US,other\n",
		"missing account":         header + "bank,,2026-01-01,-1,USD,other\n",
		"unknown category":        header + "bank,A,2026-01-01,-1,USD,NOT_A_CATEGORY\n",
		"delta in the wrong case": header + "bank,A,2026-01-01,-1,USD,INVESTMENT\n",
		"vendored in lower case":  header + "bank,A,2026-01-01,-1,USD,travel_flights\n",
		"contradictory duplicate": header + "bank,A,2026-01-01,-1,USD,investment\nbank,A,2026-01-01,-1.00,USD,other\n",
	}
	for name, csv := range cases {
		_, err := parsePinLedger(strings.NewReader(csv))
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		if name != "missing required column" && !strings.Contains(err.Error(), "line 2") {
			t.Errorf("%s: error %q does not name the line", name, err)
		}
	}
	if _, err := parsePinLedger(strings.NewReader(cases["contradictory duplicate"])); err == nil ||
		!strings.Contains(err.Error(), "line 3 pins the same transaction(s) as line 2") {
		t.Errorf("a contradiction must name both lines, got %v", err)
	}
}

// TestParsePinLedgerCollapsesAgreeingDuplicates: a pin already applies
// to every row it describes, so repeating it adds nothing and is not
// an error.
func TestParsePinLedgerCollapsesAgreeingDuplicates(t *testing.T) {
	csv := `silver_source_id,account,occurred_at,amount,currency,spend_detailed
bank,A,2026-01-01,-2500,USD,investment
bank,A,2026-01-01,-2500.00,usd,investment
`
	got, err := parsePinLedger(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("rows = %d, want 1 (agreeing duplicates collapse)", len(got))
	}
}

func TestParsePinLedgerMissingFile(t *testing.T) {
	if got, err := ParsePinLedger("/no/such/pins.csv"); err != nil || got != nil {
		t.Errorf("missing file should be (nil, nil); got (%v, %v)", got, err)
	}
	if got, err := ParsePinLedger(""); err != nil || got != nil {
		t.Errorf("empty path should be (nil, nil); got (%v, %v)", got, err)
	}
	if got, err := parsePinLedger(strings.NewReader("")); err != nil || got != nil {
		t.Errorf("empty file should be (nil, nil); got (%v, %v)", got, err)
	}
}
