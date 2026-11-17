package gold

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// TestReturnsDoNotFollowTheOrderOfADaysFlows: the same account, its
// same-day flows stored in another order, reads the same rows at every
// grain and period. The flows are loaded in transaction-id order within a
// day, so the two seeds differ only in that order.
func TestReturnsDoNotFollowTheOrderOfADaysFlows(t *testing.T) {
	t.Parallel()
	jan31 := dy(2023, time.January, 31)
	feb := func(d int) int64 { return dy(2023, time.February, d) }
	const (
		dep, wd = canonical.TxKindDeposit, canonical.TxKindWithdrawal
		in, out = canonical.TxKindTransferIn, canonical.TxKindTransferOut
	)
	day10 := []txn{
		{feb(10), dep, 100000001}, {feb(10), wd, -100000000}, {feb(10), dep, 7},
		{feb(10), in, 2500}, {feb(10), out, -2500}, {feb(10), wd, -3},
	}
	run := func(order []txn) map[string][]ReturnRow {
		db, ctx := openMigrated(t)
		seedReturnsSource(t, db, ctx, "bankx", "fidelity")
		x := append(slices.Clone(order), txn{feb(20), dep, 1000})
		seedAcct(t, db, ctx, "bankx", "ACCT", canonical.AccountKindBrokerage, nil,
			[]snap{{jan31, 10000}, {feb(10), 10005}, {feb(28), 11300}}, x)
		seedAcct(t, db, ctx, "bankx", "OTHER", canonical.AccountKindBrokerage, nil,
			[]snap{{jan31, 5000}, {feb(28), 5100}}, nil)
		got := map[string][]ReturnRow{}
		for _, level := range []string{"accounts", "sources", "global"} {
			for _, period := range []string{"monthly", "total"} {
				p := params(level, 0, eod(2023, time.February, 28))
				p.Period = period
				rows, err := RunReturns(ctx, db, p)
				if err != nil {
					t.Fatalf("RunReturns: %v", err)
				}
				got[level+"/"+period] = rows
			}
		}
		return got
	}
	reversed := slices.Clone(day10)
	slices.Reverse(reversed)
	a, b := run(day10), run(reversed)
	for key, rows := range a {
		if !reflect.DeepEqual(rows, b[key]) {
			t.Errorf("%s differs with the day's flows in another order:\n%+v\n%+v", key, rows, b[key])
		}
	}
}
