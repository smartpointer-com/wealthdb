package schwab

import (
	"testing"

	"github.com/ptu/wealthdb/internal/canonical"
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
