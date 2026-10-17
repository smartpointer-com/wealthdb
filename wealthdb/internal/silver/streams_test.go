package silver

import (
	"context"
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
