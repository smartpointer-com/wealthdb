package gold

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// globalFlows is the global entity's flow series over its since-inception
// window.
func globalFlows(t *testing.T, db *sql.DB, ctx context.Context, p ReturnParams) ([]returns.Flow, []string) {
	t.Helper()
	ds, err := loadRunDataset(ctx, db, p)
	if err != nil {
		t.Fatal(err)
	}
	r := ds.whole(nil).run(p, min(EpochDay(p.ToEpoch), ds.globalMax))
	return r.flows, r.tags
}

// TestStoredPairFundsADebutAtAnyGap pins that a stored pair binds whatever
// the gap between its legs: a coin that leaves an exchange and lands in a
// wallet that debuts on its arrival funds that debut, days later or at a
// moved price, so the global flow series holds no flow at all.
func TestStoredPairFundsADebutAtAnyGap(t *testing.T) {
	t.Parallel()
	a, out, k := dy(2024, time.January, 2), dy(2024, time.March, 1), dy(2024, time.December, 2)
	for _, tc := range []struct {
		name  string
		in    int64
		inAmt int64
	}{
		{"ten days later", dy(2024, time.March, 11), 10000},
		{"a day later at a moved price", dy(2024, time.March, 2), 10600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := openMigrated(t)
			seedReturnsSource(t, db, ctx, "s1", "schwab")
			seedAcct(t, db, ctx, "s1", "EXCH", canonical.AccountKindBrokerage, nil,
				[]snap{{a, 30000}, {out, 20000}, {k, 20000}},
				[]txn{{out, canonical.TxKindTransferOut, -10000}})
			seedAcct(t, db, ctx, "s1", "COLD", canonical.AccountKindBrokerage, nil,
				[]snap{{tc.in, tc.inAmt}, {k, 11000}},
				[]txn{{tc.in, canonical.TxKindTransferIn, tc.inAmt}})
			seedTransferLink(t, db, ctx, "s1", "EXCH-tx0", "s1", "COLD-tx0")
			flows, tags := globalFlows(t, db, ctx, tmParams("global", 0, eod(2024, time.December, 2)))
			if len(flows) != 0 {
				t.Errorf("flows %v: the move funds the debut, no capital crosses", flows)
			}
			if len(tags) != 1 || tags[0] != "cross_source_netted=1" {
				t.Errorf("tags %v, want the stored pair netted alone", tags)
			}
			rows, err := RunReturns(ctx, db, tmParams("global", 0, eod(2024, time.December, 2)))
			if err != nil {
				t.Fatal(err)
			}
			g, _ := summaryFor(rows, "")
			// 30000 opens, 31000 closes, and no flow: the price move is return.
			if g.TWR == nil || math.Abs(*g.TWR-1.0/30) > 1e-9 {
				t.Errorf("TWR = %v, want 1/30", g.TWR)
			}
		})
	}
}

// TestStoredPairCarriesMoneyInTransit pins that a stored pair into a debut
// leaves TWR and MWR as if the money never left the household: between the
// legs it is in neither account, and the chain carries it at its value. The
// household reads as one account that held the money throughout. (A pair
// whose both legs are live drops as it always has, and keeps its legs' own
// days.)
func TestStoredPairCarriesMoneyInTransit(t *testing.T) {
	t.Parallel()
	d0, d1, d2, d3, d4 := dy(2024, time.January, 2), dy(2024, time.February, 1), dy(2024, time.February, 15),
		dy(2024, time.March, 2), dy(2024, time.June, 3)
	end := eod(2024, time.June, 3)
	moved, ctx := openMigrated(t)
	seedReturnsSource(t, moved, ctx, "s1", "schwab")
	// A pays 10000 out on d1 that B books on d3, the day B debuts, and takes
	// a 5000 deposit from outside on d2.
	seedAcct(t, moved, ctx, "s1", "A", canonical.AccountKindBrokerage, nil,
		[]snap{{d0, 20000}, {d1, 10000}, {d2, 15500}, {d3, 15500}, {d4, 15500}},
		[]txn{{d1, canonical.TxKindWithdrawal, -10000}, {d2, canonical.TxKindDeposit, 5000}})
	seedAcct(t, moved, ctx, "s1", "B", canonical.AccountKindBrokerage, nil,
		[]snap{{d3, 10000}, {d4, 10500}}, []txn{{d3, canonical.TxKindDeposit, 10000}})
	seedTransferLink(t, moved, ctx, "s1", "A-tx0", "s1", "B-tx0")

	// The same household, the money never moving.
	kept, _ := openMigrated(t)
	seedReturnsSource(t, kept, ctx, "s1", "schwab")
	seedAcct(t, kept, ctx, "s1", "R", canonical.AccountKindBrokerage, nil,
		[]snap{{d0, 20000}, {d1, 20000}, {d2, 25500}, {d3, 25500}, {d4, 26000}},
		[]txn{{d2, canonical.TxKindDeposit, 5000}})

	global := func(db *sql.DB) ReturnRow {
		t.Helper()
		rows, err := RunReturns(ctx, db, tmParams("global", 0, end))
		if err != nil {
			t.Fatal(err)
		}
		g, _ := summaryFor(rows, "")
		return g
	}
	got, want := global(moved), global(kept)
	for _, r := range []struct {
		name      string
		got, want *float64
	}{{"TWR", got.TWR, want.TWR}, {"MWR", got.MWR, want.MWR}} {
		if r.got == nil || r.want == nil || math.Abs(*r.got-*r.want) > 1e-9 {
			t.Errorf("%s = %s, want %s as if the money never left", r.name, rateText(r.got), rateText(r.want))
		}
	}
}

// rateText is a rate for a test message, "nil" for none.
func rateText(r *float64) string {
	if r == nil {
		return "nil"
	}
	return strconv.FormatFloat(*r, 'g', -1, 64)
}

// TestStoredPairsFundADebutAtSeveralGaps pins the shape of a private
// holding funded from a cash account in several moves: each withdrawal is
// stored with its deposit, days to months apart, every deposit lands on
// or before the debut, and the first value is what they paid in. Money
// also comes in from outside meanwhile. One withdrawal shows only days
// later, in a valuation that also holds a fall no flow records, so the
// chain cannot read its move early, and another account is valued and
// takes money from outside in between. The household reads as one account that held the money
// throughout.
func TestStoredPairsFundADebutAtSeveralGaps(t *testing.T) {
	t.Parallel()
	day := func(m time.Month, d int) int64 { return dy(2024, m, d) }
	end := eod(2024, time.June, 28)
	moved, ctx := openMigrated(t)
	seedReturnsSource(t, moved, ctx, "s1", "schwab")
	// C pays 10000 on Feb 1, 5000 on Mar 1 and 8000 on Mar 11, and takes
	// 20000 from outside on Apr 1. Its Mar 15 valuation also shows a 5000
	// fall no flow records.
	seedAcct(t, moved, ctx, "s1", "C", canonical.AccountKindBrokerage, nil,
		[]snap{{day(time.January, 2), 100000}, {day(time.February, 1), 90000}, {day(time.March, 1), 86000},
			{day(time.March, 15), 73000}, {day(time.April, 1), 93500}, {day(time.May, 10), 94000}, {day(time.June, 28), 96000}},
		[]txn{{day(time.February, 1), canonical.TxKindWithdrawal, -10000}, {day(time.March, 1), canonical.TxKindWithdrawal, -5000},
			{day(time.March, 11), canonical.TxKindWithdrawal, -8000}, {day(time.April, 1), canonical.TxKindDeposit, 20000}})
	// E books the three deposits 6, 38 and 52 days on and debuts on May 10
	// at what they paid in.
	seedAcct(t, moved, ctx, "s1", "E", canonical.AccountKindCustody, nil,
		[]snap{{day(time.May, 10), 23000}, {day(time.June, 28), 23500}},
		[]txn{{day(time.February, 7), canonical.TxKindDeposit, 10000}, {day(time.April, 8), canonical.TxKindDeposit, 5000},
			{day(time.May, 2), canonical.TxKindDeposit, 8000}})
	for i := range 3 {
		seedTransferLink(t, moved, ctx, "s1", fmt.Sprintf("C-tx%d", i), "s1", fmt.Sprintf("E-tx%d", i))
	}
	// O is valued in between, and takes 10000 from outside on Mar 14.
	other := []snap{{day(time.January, 2), 1000}, {day(time.March, 13), 1000}, {day(time.March, 15), 11000},
		{day(time.June, 28), 11000}}
	otherIn := []txn{{day(time.March, 14), canonical.TxKindDeposit, 10000}}
	seedAcct(t, moved, ctx, "s1", "O", canonical.AccountKindBrokerage, nil, other, otherIn)

	// The same household, the money never moving.
	kept, _ := openMigrated(t)
	seedReturnsSource(t, kept, ctx, "s1", "schwab")
	seedAcct(t, kept, ctx, "s1", "R", canonical.AccountKindBrokerage, nil,
		[]snap{{day(time.January, 2), 100000}, {day(time.February, 1), 100000}, {day(time.March, 1), 101000},
			{day(time.March, 13), 101000}, {day(time.March, 15), 96000}, {day(time.April, 1), 116500},
			{day(time.May, 10), 117000}, {day(time.June, 28), 119500}},
		[]txn{{day(time.April, 1), canonical.TxKindDeposit, 20000}})
	seedAcct(t, kept, ctx, "s1", "O", canonical.AccountKindBrokerage, nil, other, otherIn)

	global := func(db *sql.DB) ReturnRow {
		t.Helper()
		rows, err := RunReturns(ctx, db, tmParams("global", 0, end))
		if err != nil {
			t.Fatal(err)
		}
		g, _ := summaryFor(rows, "")
		return g
	}
	got, want := global(moved), global(kept)
	for _, r := range []struct {
		name      string
		got, want *float64
	}{{"TWR", got.TWR, want.TWR}, {"MWR", got.MWR, want.MWR}} {
		if r.got == nil || r.want == nil || math.Abs(*r.got-*r.want) > 1e-9 {
			t.Errorf("%s = %s, want %s as if the money never left", r.name, rateText(r.got), rateText(r.want))
		}
	}
}

// TestRolledCarriesNoMoneyBackwards pins that a carry carries only forward:
// where the sending account shows the money gone only after the receiving
// account shows it, and its valuation does not confirm the leg, nothing is
// carried, and the receiving leg keeps its own move.
func TestRolledCarriesNoMoneyBackwards(t *testing.T) {
	t.Parallel()
	ex := &accountData{src: "s", acct: "EX", series: []dayVal{{0, 5000}, {511, 5000}}, snapDays: []int64{0, 511}}
	cold := &accountData{src: "s", acct: "COLD", series: []dayVal{{100, 1000}, {200, 1000}}, snapDays: []int64{100, 200}}
	out := flowLeg{Flow: returns.Flow{Day: 100, Amount: -1000, ID: "out", ValuedOn: 511}, acct: ex}
	in := flowLeg{Flow: returns.Flow{Day: 100, Amount: 1000, ID: "in", ValuedOn: 100}, acct: cold}
	m := newMovedLegs()
	m.add(ex, out.Flow)
	m.add(cold, in.Flow)
	m.carries = []carry{{out: out, in: in, to: 100}}
	got := m.rolled([]*accountData{ex, cold}, 600)
	if want := []returns.Flow{in.Flow}; !slices.Equal(got, want) {
		t.Errorf("rolled = %+v, want only the receiving leg's own move %+v", got, want)
	}
}
