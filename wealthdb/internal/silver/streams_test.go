package silver

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestBatchesStreamInTimeOrder pins the batch collector: seeded times start
// empty, At hands back the batch a time already has or adds one, and the
// stream yields every batch in ascending time order.
func TestBatchesStreamInTimeOrder(t *testing.T) {
	b := NewBatches([]int64{30, 10})
	if len(b) != 2 || len(b[10].Accounts) != 0 {
		t.Fatalf("NewBatches = %v, want two empty batches", b)
	}
	if b.At(10) != b[10] {
		t.Error("At replaced a seeded batch")
	}
	b.At(20).Accounts = append(b.At(20).Accounts, canonical.AccountChange{AccountExternalID: "x"})
	if len(b) != 3 || len(b[20].Accounts) != 1 {
		t.Errorf("At(20) = %v, want one added batch carrying the account", b[20])
	}

	stream := b.Stream()
	var order []int
	for {
		batch, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, len(batch.Accounts))
		if !more {
			break
		}
	}
	if len(order) != 3 || order[1] != 1 {
		t.Errorf("stream = %v, want three batches with time 20 in the middle", order)
	}
	if got := (Batches{}).Sorted(); len(got) != 0 {
		t.Errorf("Sorted of none = %v, want none", got)
	}
}

// The mapped streams apply their fold to every batch, in order, and add
// or drop none; Lazy builds once and keeps the error with the value.
func TestMapAndLazy(t *testing.T) {
	ctx := context.Background()
	snaps := MapSnapshots(NewSnapshotStream([]canonical.SnapshotBatch{
		{Accounts: []canonical.AccountChange{{AccountExternalID: "a"}}},
		{Accounts: []canonical.AccountChange{{AccountExternalID: "b"}}},
	}), func(b *canonical.SnapshotBatch) { b.Accounts[0].AccountExternalID += "!" })
	var ids []string
	for more := true; more; {
		var b canonical.SnapshotBatch
		var err error
		if b, more, err = snaps.Next(ctx); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, b.Accounts[0].AccountExternalID)
	}
	if !slices.Equal(ids, []string{"a!", "b!"}) {
		t.Errorf("mapped snapshots = %v", ids)
	}
	txs := MapTransactions(NewTransactionStream(canonical.TransactionBatch{
		Transactions: []canonical.TransactionChange{{TransactionExternalID: "t"}},
	}), func(b *canonical.TransactionBatch) { b.Transactions[0].TransactionExternalID += "!" })
	if b, more, err := txs.Next(ctx); err != nil || more || b.Transactions[0].TransactionExternalID != "t!" {
		t.Errorf("mapped transactions = %+v %v %v", b, more, err)
	}

	var l Lazy[int]
	calls := 0
	build := func() (int, error) { calls++; return 7, errors.New("once") }
	for range 2 {
		if v, err := l.Get(build); v != 7 || err == nil || err.Error() != "once" {
			t.Errorf("Get = %d, %v", v, err)
		}
	}
	if calls != 1 {
		t.Errorf("built %d times, want once", calls)
	}
}
