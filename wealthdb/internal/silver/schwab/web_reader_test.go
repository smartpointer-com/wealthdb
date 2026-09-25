package schwab

import (
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// wtx builds a parsed web transaction tagged with its sub-feed for the
// cross-feed dedup tests. id is the TransactionExternalID used to assert which
// rows survive; day is an epoch-day (stored as seconds on OccurredAt).
func wtx(id, source string, kind canonical.TxKind, acct string, day int64, amount float64) builtWebTx {
	d := canonical.NewDecimalFromFloat(amount)
	return builtWebTx{source: source, tx: canonical.TransactionChange{
		TransactionExternalID: id,
		OccurredAt:            day * 86400,
		AccountExternalID:     acct,
		Kind:                  kind,
		NetAmount:             &d,
	}}
}

func survivingIDs(rows []canonical.TransactionChange) map[string]bool {
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		out[r.TransactionExternalID] = true
	}
	return out
}

func builtIDs(rows []builtWebTx) map[string]bool {
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		out[r.tx.TransactionExternalID] = true
	}
	return out
}

// TestSpliceNonExternalToJSON pins the feed-authority splice: within the JSON
// export's per-account coverage span the JSON copy of a non-external row wins and
// the statement-PDF copy is dropped, while PDF rows outside the span survive as
// backfill, external flows are left for the no-loss dedup, and accounts with no
// JSON keep their PDF rows.
func TestSpliceNonExternalToJSON(t *testing.T) {
	const pdf, js = sourceStatementPDF, sourceTxHistoryJSON
	in := []builtWebTx{
		// JSON defines account A's coverage span [100, 200].
		wtx("jA-lo", js, canonical.TxKindBuy, "A", 100, -500),
		wtx("jA-hi", js, canonical.TxKindSell, "A", 200, 500),
		wtx("pA-before", pdf, canonical.TxKindBuy, "A", 50, -100),         // pre-span backfill → kept
		wtx("pA-in", pdf, canonical.TxKindBuy, "A", 150, -100),            // in span → dropped
		wtx("pA-onlo", pdf, canonical.TxKindDividend, "A", 100, 10),       // on boundary → dropped
		wtx("pA-after", pdf, canonical.TxKindBuy, "A", 250, -100),         // past span → kept
		wtx("pA-extin", pdf, canonical.TxKindWithdrawal, "A", 150, -1000), // external in span → kept (dedup handles it)
		wtx("pB", pdf, canonical.TxKindBuy, "B", 150, -100),               // account B has no JSON → kept
	}
	got := builtIDs(spliceNonExternalToJSON(in, nil))
	wantKept := []string{"jA-lo", "jA-hi", "pA-before", "pA-after", "pA-extin", "pB"}
	for _, id := range wantKept {
		if !got[id] {
			t.Errorf("%q should be kept; survivors = %v", id, got)
		}
	}
	for _, id := range []string{"pA-in", "pA-onlo"} {
		if got[id] {
			t.Errorf("%q should be dropped (JSON authoritative in span); survivors = %v", id, got)
		}
	}
	if len(got) != len(wantKept) {
		t.Errorf("kept %d rows, want %d: %v", len(got), len(wantKept), got)
	}
}

// TestDedupeCrossFeedExternalFlows pins the statement-PDF ↔ tx-history-JSON
// cross-feed dedup: external-flow twins (within ±3 days / 0.5% / $1) collapse to
// the JSON copy, while trades, beyond-tolerance flows, opposite signs, and
// feed-unique flows all pass through untouched.
func TestDedupeCrossFeedExternalFlows(t *testing.T) {
	const pdf, js = sourceStatementPDF, sourceTxHistoryJSON
	cases := []struct {
		name    string
		in      []builtWebTx
		wantIDs []string // surviving TransactionExternalIDs, any order
	}{
		{
			name: "exact cross-feed twin drops the PDF copy",
			in: []builtWebTx{
				wtx("j1", js, canonical.TxKindWithdrawal, "A", 100, -1000),
				wtx("p1", pdf, canonical.TxKindWithdrawal, "A", 100, -1000),
			},
			wantIDs: []string{"j1"},
		},
		{
			name: "settlement-date offset within the window still matches (PDF Withdrawal vs JSON Journal)",
			in: []builtWebTx{
				wtx("j1", js, canonical.TxKindJournal, "A", 97, -5000),
				wtx("p1", pdf, canonical.TxKindWithdrawal, "A", 100, -5000),
			},
			wantIDs: []string{"j1"},
		},
		{
			name: "date offset beyond the window keeps both",
			in: []builtWebTx{
				wtx("j1", js, canonical.TxKindWithdrawal, "A", 96, -5000),
				wtx("p1", pdf, canonical.TxKindWithdrawal, "A", 100, -5000),
			},
			wantIDs: []string{"j1", "p1"},
		},
		{
			name: "sub-dollar rounding within tolerance matches",
			in: []builtWebTx{
				wtx("j1", js, canonical.TxKindWithdrawal, "A", 100, -1000.49),
				wtx("p1", pdf, canonical.TxKindWithdrawal, "A", 100, -1000),
			},
			wantIDs: []string{"j1"},
		},
		{
			name: "amount beyond tolerance keeps both",
			in: []builtWebTx{
				wtx("j1", js, canonical.TxKindWithdrawal, "A", 100, -1100),
				wtx("p1", pdf, canonical.TxKindWithdrawal, "A", 100, -1000),
			},
			wantIDs: []string{"j1", "p1"},
		},
		{
			name: "opposite signs do not match",
			in: []builtWebTx{
				wtx("j1", js, canonical.TxKindDeposit, "A", 100, 1000),
				wtx("p1", pdf, canonical.TxKindWithdrawal, "A", 100, -1000),
			},
			wantIDs: []string{"j1", "p1"},
		},
		{
			name: "trades are never deduped",
			in: []builtWebTx{
				wtx("j1", js, canonical.TxKindBuy, "A", 100, -1000),
				wtx("p1", pdf, canonical.TxKindBuy, "A", 100, -1000),
			},
			wantIDs: []string{"j1", "p1"},
		},
		{
			name: "feed-unique PDF external flow is kept",
			in: []builtWebTx{
				wtx("p1", pdf, canonical.TxKindDeposit, "A", 100, 1000),
			},
			wantIDs: []string{"p1"},
		},
		{
			name: "one JSON twin consumes only one of two identical PDF legs",
			in: []builtWebTx{
				wtx("j1", js, canonical.TxKindWithdrawal, "A", 100, -1000),
				wtx("p1", pdf, canonical.TxKindWithdrawal, "A", 100, -1000),
				wtx("p2", pdf, canonical.TxKindWithdrawal, "A", 100, -1000),
			},
			wantIDs: []string{"j1", "p2"}, // p1 consumes the lone twin; p2 survives
		},
		{
			name: "different accounts do not match",
			in: []builtWebTx{
				wtx("j1", js, canonical.TxKindWithdrawal, "A", 100, -1000),
				wtx("p1", pdf, canonical.TxKindWithdrawal, "B", 100, -1000),
			},
			wantIDs: []string{"j1", "p1"},
		},
		{
			name: "nil-amount external flow is kept (no panic, no match)",
			in: []builtWebTx{
				{source: pdf, tx: canonical.TransactionChange{
					TransactionExternalID: "p1", OccurredAt: 100 * 86400,
					AccountExternalID: "A", Kind: canonical.TxKindDeposit, NetAmount: nil,
				}},
			},
			wantIDs: []string{"p1"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := survivingIDs(dedupeCrossFeedExternalFlows(c.in))
			if len(got) != len(c.wantIDs) {
				t.Fatalf("survivors = %v, want %v", got, c.wantIDs)
			}
			for _, id := range c.wantIDs {
				if !got[id] {
					t.Errorf("expected %q to survive; survivors = %v", id, got)
				}
			}
		})
	}
}

// The security alone cannot tell a dividend from the withholding taken
// from it, or an ADR's fee from either. The narrative leads with the
// movement each feed states, then the security; the statement's
// catch-all category says nothing and is left out.
func TestWebNarrativeLeadsWithTheMovement(t *testing.T) {
	sec := "EXAMPLE FUND ETF"
	for _, tc := range []struct {
		payload string
		want    string
	}{
		{`{"Action":"NRA Tax Adj","Description":"EXAMPLE FUND ETF"}`, "NRA Tax Adj EXAMPLE FUND ETF"},
		{`{"action":null,"category":"Fee","description":"EXAMPLE FUND ETF"}`, "Fee EXAMPLE FUND ETF"},
		{`{"action":"Reinvest","category":"Dividend","description":"EXAMPLE FUND ETF"}`, "Reinvest EXAMPLE FUND ETF"},
		{`{"category":"Unknown","description":"EXAMPLE FUND ETF"}`, "EXAMPLE FUND ETF"},
	} {
		got := webNarrative(tc.payload, &sec)
		if got == nil || *got != tc.want {
			t.Errorf("webNarrative(%s) = %v, want %q", tc.payload, got, tc.want)
		}
	}
	if got := webNarrative(`{"Action":"Journal"}`, nil); got == nil || *got != "Journal" {
		t.Errorf("a movement with no security = %v, want %q", got, "Journal")
	}
	if got := webNarrative(`{}`, nil); got != nil {
		t.Errorf("nothing stated = %q, want nil", *got)
	}
	lead := "Journaled Funds to account ...000"
	if got := webNarrative(`{"Action":"Journaled Funds"}`, &lead); got == nil || *got != lead {
		t.Errorf("a narrative already led by its movement = %v, want it unchanged", got)
	}
}

// TestWebSignedKeepsAMinusOnAnInflow: the web feeds print most figures
// as magnitudes, so the kind orients them — but a minus printed on an
// inflow is a correction and keeps its sign.
func TestWebSignedKeepsAMinusOnAnInflow(t *testing.T) {
	dec := func(s string) *canonical.Decimal {
		d, err := canonical.NewDecimalFromString(s)
		if err != nil {
			t.Fatal(err)
		}
		return &d
	}
	for _, c := range []struct {
		kind   canonical.TxKind
		amount string
		want   string
		why    string
	}{
		{canonical.TxKindDividend, "-3.00", "-3", "a clawed-back dividend nets against the dividend"},
		{canonical.TxKindDividend, "3.00", "3", "a dividend stays an inflow"},
		{canonical.TxKindBuy, "600.50", "-600.5", "a reinvestment printed as a magnitude is money out"},
		{canonical.TxKindTransferOut, "100.00", "-100", "a distribution's market value is money out"},
		{canonical.TxKindWithdrawal, "-50.00", "-50", "an outflow printed negative stays negative"},
	} {
		got := webSigned("Dividend", c.kind, dec(c.amount))
		if got == nil || got.String() != c.want {
			t.Errorf("webSigned(%s, %s) = %v, want %s: %s", c.kind, c.amount, got, c.want, c.why)
		}
	}
	if webSigned("Dividend", canonical.TxKindDividend, nil) != nil {
		t.Error("a row with no figure gained one")
	}
	// The statement parser's catch-all prints its own sign, and a shape
	// read out of it keeps that sign: a withholding reclaimed is a tax
	// row with a credit.
	if got := webSigned("Unknown", canonical.TxKindTax, dec("14.50")); got == nil || got.String() != "14.5" {
		t.Errorf("a reclaimed withholding = %v, want 14.5", got)
	}
}

// TestSpliceSpanReachesPastTheAPICutoff: the api cutoff drops the JSON
// export's later rows, so the span has to be told where the export ends.
// A trade's statement copy, dated by settlement a day after its JSON
// copy, then falls inside the span instead of just past it.
func TestSpliceSpanReachesPastTheAPICutoff(t *testing.T) {
	const pdf, js = sourceStatementPDF, sourceTxHistoryJSON
	in := []builtWebTx{
		wtx("j-trade", js, canonical.TxKindBuy, "A", 100, -500),
		wtx("p-settle", pdf, canonical.TxKindBuy, "A", 101, -500),
	}
	if got := builtIDs(spliceNonExternalToJSON(in, nil)); !got["p-settle"] {
		t.Fatalf("without the export's end the settle-dated copy is past the span and kept: %v", got)
	}
	got := builtIDs(spliceNonExternalToJSON(in, map[string]int64{"A": 120}))
	if got["p-settle"] || !got["j-trade"] {
		t.Errorf("with the export reaching day 120 the statement copy should go: %v", got)
	}
}
