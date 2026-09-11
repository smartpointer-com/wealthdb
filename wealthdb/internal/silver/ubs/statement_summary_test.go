package ubs

import (
	"context"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// A statement's period summary is not a booking. Before its closing balance
// an Account Statement prints "Turnover total <debits> <credits>", a line
// without a date that the collector's parser attaches to the booking that
// precedes it — at a period close the zero-amount service-price or interest
// line. The adapter drops that row and keeps the same line's non-zero
// siblings as the fee or interest bookings they are, composed from the
// booking type alone. Every amount here is synthetic.

func emitWebStream(t *testing.T, r *webReader) interface {
	Next(context.Context) (canonical.TransactionBatch, bool, error)
} {
	t.Helper()
	stream, _, err := r.transactionsBeforePSNStart(context.Background(),
		canonical.Window{Start: 0, End: 1 << 40, HasChanges: true}, nil, nil)
	if err != nil {
		t.Fatalf("transactionsBeforePSNStart: %v", err)
	}
	t.Cleanup(func() { stream.Close() })
	return stream
}

func pdfTurnoverPayload(bookingType string, continuation ...string) string {
	lines := ""
	for i, c := range continuation {
		if i > 0 {
			lines += ","
		}
		lines += `"` + c + `"`
	}
	return `{"source":"account_statement_pdf","booking_type":"` + bookingType +
		`","internal_transfer":false,"counter_account":null,"continuation":[` + lines + `]}`
}

func seedStatementSummaryRows(t *testing.T, r *webReader) {
	t.Helper()
	seedWebAccount(t, r, textAcct)
	seedRailEraAnchor(t, r)
	// The period summaries: a zero figure in either column, the turnover
	// line as the only narrative, under both period-close types.
	seedWebTextRow(t, r, "S1", 200*86400, 0.0, nil, "Turnover total 4 321.00 1 234.50", "BALANCE CLOSING OF SERVICE PRICES",
		pdfTurnoverPayload("BALANCE CLOSING OF SERVICE PRICES", "Turnover total 4 321.00 1 234.50"))
	seedWebTextRow(t, r, "S2", 231*86400, nil, 0.0, "Turnover total 0 0", "INTEREST CALCULATION BALANCE",
		pdfTurnoverPayload("INTEREST CALCULATION BALANCE", "Turnover total 0 0"))
	// A real fee and a real interest booking at a period close: the same
	// shape with an amount.
	seedWebTextRow(t, r, "F1", 262*86400, 7.5, nil, "Turnover total 2 000.00 500.00", "BALANCE CLOSING OF SERVICE PRICES",
		pdfTurnoverPayload("BALANCE CLOSING OF SERVICE PRICES", "Turnover total 2 000.00 500.00"))
	seedWebTextRow(t, r, "I1", 293*86400, nil, 1.25, "Turnover total 10.00 20.00", "INTEREST CALCULATION BALANCE",
		pdfTurnoverPayload("INTEREST CALCULATION BALANCE", "Turnover total 10.00 20.00"))
	// A zero-amount booking with its own narrative is not a summary.
	seedWebTextRow(t, r, "Z1", 300*86400, 0.0, nil, "EXAMPLE PAYEE", "E-BANKING PAYMENT ORDER",
		pdfTurnoverPayload("E-BANKING PAYMENT ORDER", "EXAMPLE PAYEE"))
	// The line attached behind an ordinary booking's own text: stripped,
	// the booking untouched otherwise.
	seedWebTextRow(t, r, "P1", 305*86400, 50.0, nil, "EXAMPLE GROCER", "E-BANKING PAYMENT ORDER",
		pdfTurnoverPayload("E-BANKING PAYMENT ORDER", "EXAMPLE GROCER", "EXAMPLE CITY", "Turnover total 1 000.00 2 000.00"))
}

// TestStatementSummaryRowIsDropped: the zero-amount, turnover-only rows never
// reach gold; every other row does.
func TestStatementSummaryRowIsDropped(t *testing.T) {
	r := newWebTxFixture(t)
	seedStatementSummaryRows(t, r)
	got := drainTx(t, emitWebStream(t, r))
	for _, id := range []string{"S1", "S2"} {
		if _, ok := got[id+"@"+textAcct]; ok {
			t.Errorf("%s: statement summary row emitted as a transaction", id)
		}
	}
	for _, id := range []string{"F1", "I1", "Z1", "P1", "ANCHOR"} {
		if _, ok := got[id+"@"+textAcct]; !ok {
			t.Errorf("%s: booking missing", id)
		}
	}
}

// TestPeriodCloseBookingKeepsTypeOnly: a fee or interest booking at a period
// close keeps its kind and amount, its description is the booking type alone,
// and the provider category still carries the type for the provider map to
// place. Its payee is the BANK: both booking types are charges for the
// bank's own services, and the turnover line the statement attaches behind
// them is not a party. A trailing turnover line behind an ordinary booking
// is stripped without touching the payee.
func TestPeriodCloseBookingKeepsTypeOnly(t *testing.T) {
	r := newWebTxFixture(t)
	seedStatementSummaryRows(t, r)
	got := drainTx(t, emitWebStream(t, r))
	checkText(t, got, map[string]textCase{
		"F1@" + textAcct: {"BALANCE CLOSING OF SERVICE PRICES", bankName, "BALANCE CLOSING OF SERVICE PRICES"},
		"I1@" + textAcct: {"INTEREST CALCULATION BALANCE", bankName, "INTEREST CALCULATION BALANCE"},
		"Z1@" + textAcct: {"E-BANKING PAYMENT ORDER; EXAMPLE PAYEE", "EXAMPLE PAYEE", "E-BANKING PAYMENT ORDER"},
		"P1@" + textAcct: {"E-BANKING PAYMENT ORDER; EXAMPLE GROCER; EXAMPLE CITY", "EXAMPLE GROCER", "E-BANKING PAYMENT ORDER"},
	})
	want := map[string]struct {
		kind canonical.TxKind
		net  string
	}{
		"F1@" + textAcct: {canonical.TxKindFee, "-7.5"},
		"I1@" + textAcct: {canonical.TxKindInterest, "1.25"},
		"P1@" + textAcct: {canonical.TxKindWithdrawal, "-50"},
	}
	for id, w := range want {
		tx, ok := got[id]
		if !ok {
			t.Errorf("missing tx %s", id)
			continue
		}
		if tx.Kind != w.kind {
			t.Errorf("%s Kind = %q, want %q", id, tx.Kind, w.kind)
		}
		if tx.NetAmount == nil || tx.NetAmount.String() != w.net {
			t.Errorf("%s NetAmount = %v, want %s", id, tx.NetAmount, w.net)
		}
	}
}

// TestIsTurnoverTotalLine pins the line test: the two words in any case
// followed by figures and nothing else; a word after them, or glued to them,
// is some other line.
func TestIsTurnoverTotalLine(t *testing.T) {
	cases := map[string]bool{
		"Turnover total 4 321.00 1 234.50": true,
		"TURNOVER TOTAL 0 0":               true,
		"turnover total 16.05- 0.00":       true,
		"  Turnover total 1'234.50 0.00 ":  true,
		"Turnover total":                   true,
		"Turnover totals for the year":     false,
		"Turnover totalizer":               false,
		"Turnover":                         false,
		"EXAMPLE PAYEE":                    false,
		"":                                 false,
	}
	for line, want := range cases {
		if got := isTurnoverTotalLine(line); got != want {
			t.Errorf("isTurnoverTotalLine(%q) = %v, want %v", line, got, want)
		}
	}
}
