package gold

import (
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestLoadFlowTransactionsMatchesTransactionsBetween pins the lean returns
// flow loader (loadFlowTransactions) to the wide TransactionsBetween readout:
// for the seven fields the returns engine consumes, both must yield the
// identical sequence — same order, same values — in every output currency.
// The lean loader drops the macro's account/instrument LEFT JOINs and 13 unused
// columns; if that ever changed the row set, order, or FX-converted value,
// RunReturns' flow classification would silently drift from the transactions
// view. Two sources carry transactions on the SAME day to exercise the
// (occurred_at, silver_source_id, transaction_external_id) tiebreak.
func TestLoadFlowTransactionsMatchesTransactionsBetween(t *testing.T) {
	db, ctx := openMigrated(t)

	d1 := dy(2023, time.March, 2)
	d2 := dy(2023, time.June, 1)
	seedAcct(t, db, ctx, "src-b", "A1", canonical.AccountKindBrokerage, nil,
		[]snap{{d1, 1000}, {d2, 1200}},
		[]txn{{d1, canonical.TxKindDeposit, 300}, {d2, canonical.TxKindWithdrawal, 100}})
	seedAcct(t, db, ctx, "src-a", "A2", canonical.AccountKindBrokerage, nil,
		[]snap{{d1, 500}, {d2, 700}},
		[]txn{{d1, canonical.TxKindDeposit, 200}})
	// FX so the cross-currency pass converts value_outccy to non-null.
	seedFX(t, db, dy(2023, time.January, 1), "USD", "CHF", "0.90")

	strEq := func(a, b *string) bool {
		if a == nil || b == nil {
			return a == b
		}
		return *a == *b
	}

	for _, outCcy := range []string{"USD", "CHF"} {
		wide, err := TransactionsBetween(ctx, db, 0, MaxEpoch, outCcy, SortAscending)
		if err != nil {
			t.Fatalf("%s: TransactionsBetween: %v", outCcy, err)
		}
		lean, err := loadFlowTransactions(ctx, db, outCcy)
		if err != nil {
			t.Fatalf("%s: loadFlowTransactions: %v", outCcy, err)
		}
		if len(lean) != len(wide) {
			t.Fatalf("%s: row count lean %d, wide %d", outCcy, len(lean), len(wide))
		}
		if len(lean) == 0 {
			t.Fatalf("%s: fixture produced no transactions", outCcy)
		}
		for i := range wide {
			w, l := wide[i], lean[i]
			if l.src != w.SilverSourceID || l.acct != w.AccountExternalID ||
				l.occurredAt != w.OccurredAt || l.kind != w.Kind ||
				l.ccy != w.Currency || l.txID != w.TransactionExternalID ||
				!strEq(l.valueOut, w.ValueOutCcy) {
				t.Errorf("%s row %d differs:\n wide %+v\n lean %+v", outCcy, i, w, l)
			}
		}
	}
}
