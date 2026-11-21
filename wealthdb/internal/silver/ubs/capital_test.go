package ubs

import (
	"context"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// TestCapitalAccountsReadTheQuarterlyReports: each report dated in the
// window is the fund's capital account on its date, reconciled or not,
// named by its document: two reports of one date, as two portfolios'
// on one fund, are both kept. A report that prints no currency is none.
func TestCapitalAccountsReadTheQuarterlyReports(t *testing.T) {
	r := newReportFixture(t)
	for _, row := range []struct {
		token, isin, ccy string
		asOf             int64
		reconciled       int
		commitment       any
	}{
		{"doc-b", "XX0000000017", "USD", 1000, 0, 175000.0},
		{"doc-a", "XX0000000017", "USD", 1000, 1, 170285.1},
		{"doc-c", "XX0000000017", "USD", 2000, 1, 170285.1},
		{"doc-d", "XX0000000025", "", 1000, 1, 50000.0},
		{"doc-e", "XX0000000033", "EUR", 9000, 1, 50000.0},
	} {
		silvertest.Exec(t, r.db, `INSERT INTO private_market_reports (source_doc_token, instrument_isin, as_of,
		    currency_iso, commitment, capital_called, valuation, line_count, lines_reconciled, payload)
		    VALUES (?, ?, ?, NULLIF(?, ''), ?, 120000.5, 131000, 0, ?, '{}')`,
			row.token, row.isin, row.asOf, row.ccy, row.commitment, row.reconciled)
	}
	got, err := r.capitalAccounts(context.Background(), canonical.Window{HasChanges: true, Start: 0, End: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("capital accounts = %+v, want the fund's three reports", got)
	}
	for i, want := range []struct {
		asOf            int64
		ref, commitment string
	}{{1000, "doc-a", "170285.1"}, {1000, "doc-b", "175000"}, {2000, "doc-c", "170285.1"}} {
		ca := got[i]
		if ca.InstrumentExternalID != "XX0000000017" || ca.AccountExternalID != nil || ca.AsOf != want.asOf ||
			ca.SourceRef != want.ref ||
			ca.Currency != "USD" || ca.Commitment.String() != want.commitment || ca.Called.String() != "120000.5" ||
			ca.NAV.String() != "131000" || ca.Distributed != nil || ca.TVPI != nil {
			t.Errorf("capital account %d = %+v", i, ca)
		}
	}
}
