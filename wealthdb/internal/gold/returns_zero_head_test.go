package gold

import (
	"context"
	"database/sql"
	"math"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// seedCoinAccount upserts a crypto wallet whose snapshots hold qty[i] of BTC
// worth snaps[i].val. A zero quantity and value is a zero valuation.
func seedCoinAccount(t *testing.T, db *sql.DB, ctx context.Context, src, acct string, snaps []snap, qty []string) {
	t.Helper()
	inst, usd := "BTC", "USD"
	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.UpsertAccounts(ctx, []canonical.AccountChange{{SilverSourceID: src, AccountExternalID: acct,
			AccountKind: canonical.AccountKindCrypto, BaseCurrency: &usd,
			FirstSeenAt: snaps[0].at, LastSeenAt: snaps[len(snaps)-1].at}}); err != nil {
			return err
		}
		var pos []canonical.PositionChange
		for i, s := range snaps {
			q, err := canonical.NewDecimalFromString(qty[i])
			if err != nil {
				return err
			}
			pos = append(pos, canonical.PositionChange{SilverSourceID: src, SnapshotAt: s.at, AccountExternalID: acct,
				PositionKey: "BTC", InstrumentExternalID: &inst, AssetClass: canonical.AssetClassCrypto,
				Vehicle: canonical.VehiclePhysical, Currency: "USD", Quantity: &q, MarketValue: decp(s.val)})
		}
		return w.InsertPositions(ctx, pos)
	})
}

// ownLegs is every flow of a, paired or not, as readZeroHead reads them.
func ownLegs(a *accountData) []returns.Flow {
	return slices.Concat(a.nonTransfer, a.transferLike)
}

// sameFlow says x and y are one flow of the entity's series: its day, amount
// and whether it is synthetic.
func sameFlow(x, y returns.Flow) bool {
	return x.Day == y.Day && x.Amount == y.Amount && x.Synthetic == y.Synthetic
}

// globalSummary is the global entity's since-inception row up to end.
func globalSummary(t *testing.T, db *sql.DB, ctx context.Context, p ReturnParams) ReturnRow {
	t.Helper()
	rows, err := RunReturns(ctx, db, p)
	if err != nil {
		t.Fatal(err)
	}
	g, ok := summaryFor(rows, "")
	if !ok {
		t.Fatal("no global summary row")
	}
	return g
}

// TestZeroDebutFundsItsFirstNonzeroValue pins that a zero first value holds
// none of the money moved in before it. The money is in transit until the
// first nonzero valuation, which is the debut it funds: the household reads
// as one account that held the money throughout, and the move books no
// capital. That holds for a pair the value netter finds, for a stored pair
// days apart, and for a coin, whose price move up to the first nonzero
// valuation is return.
func TestZeroDebutFundsItsFirstNonzeroValue(t *testing.T) {
	t.Parallel()
	d0, d1, d2, d3, d4 := dy(2024, time.January, 2), dy(2024, time.February, 1), dy(2024, time.February, 2),
		dy(2024, time.April, 30), dy(2024, time.June, 28)
	end := eod(2024, time.June, 28)
	for _, tc := range []struct {
		name string
		// moved seeds the household that moves the money; kept the same
		// household, the money never moving.
		moved, kept func(t *testing.T, db *sql.DB, ctx context.Context)
		tm          bool // read the stored pairs
	}{
		{"a pair the value netter finds",
			func(t *testing.T, db *sql.DB, ctx context.Context) {
				seedReturnsSource(t, db, ctx, "s1", "schwab")
				seedAcct(t, db, ctx, "s1", "A", canonical.AccountKindBrokerage, nil,
					[]snap{{d0, 20000}, {d1, 10000}, {d3, 10000}, {d4, 10000}},
					[]txn{{d1, canonical.TxKindTransferOut, -10000}})
				seedAcct(t, db, ctx, "s1", "B", canonical.AccountKindBrokerage, nil,
					[]snap{{d2, 0}, {d3, 10000}, {d4, 10500}}, []txn{{d1, canonical.TxKindTransferIn, 10000}})
			},
			func(t *testing.T, db *sql.DB, ctx context.Context) {
				seedReturnsSource(t, db, ctx, "s1", "schwab")
				seedAcct(t, db, ctx, "s1", "R", canonical.AccountKindBrokerage, nil,
					[]snap{{d0, 20000}, {d1, 20000}, {d2, 20000}, {d3, 20000}, {d4, 20500}}, nil)
			}, false},
		{"a stored pair weeks apart",
			func(t *testing.T, db *sql.DB, ctx context.Context) {
				seedReturnsSource(t, db, ctx, "s1", "schwab")
				seedAcct(t, db, ctx, "s1", "A", canonical.AccountKindBrokerage, nil,
					[]snap{{d0, 20000}, {d1, 10000}, {d3, 10200}, {d4, 10200}},
					[]txn{{d1, canonical.TxKindWithdrawal, -10000}})
				// B books the deposit on the day of its zero first valuation.
				in := dy(2024, time.March, 4)
				seedAcct(t, db, ctx, "s1", "B", canonical.AccountKindBrokerage, nil,
					[]snap{{in, 0}, {d3, 10000}, {d4, 10500}}, []txn{{in, canonical.TxKindDeposit, 10000}})
				seedTransferLink(t, db, ctx, "s1", "A-tx0", "s1", "B-tx0")
			},
			func(t *testing.T, db *sql.DB, ctx context.Context) {
				seedReturnsSource(t, db, ctx, "s1", "schwab")
				seedAcct(t, db, ctx, "s1", "R", canonical.AccountKindBrokerage, nil,
					[]snap{{d0, 20000}, {d1, 20000}, {dy(2024, time.March, 4), 20000}, {d3, 20200}, {d4, 20700}}, nil)
			}, true},
		{"a coin, its price moved by the first nonzero valuation",
			func(t *testing.T, db *sql.DB, ctx context.Context) {
				seedReturnsSource(t, db, ctx, "ct", "cointracking")
				seedAcct(t, db, ctx, "ct", "EX", canonical.AccountKindCrypto, nil,
					[]snap{{d0, 40000}, {d1, 10000}, {d3, 10000}, {d4, 10000}}, nil)
				seedHoldingMove(t, db, ctx, "ct", "EX", "out", d1, canonical.TxKindTransferOut, "BTC", "-1", -30000)
				seedCoinAccount(t, db, ctx, "ct", "COLD", []snap{{d2, 0}, {d3, 21000}, {d4, 24000}}, []string{"0", "1", "1"})
				seedHoldingMove(t, db, ctx, "ct", "COLD", "in", d1, canonical.TxKindTransferIn, "BTC", "1", 30000)
			},
			func(t *testing.T, db *sql.DB, ctx context.Context) {
				seedReturnsSource(t, db, ctx, "ct", "cointracking")
				seedAcct(t, db, ctx, "ct", "R", canonical.AccountKindCrypto, nil,
					[]snap{{d0, 40000}, {d1, 40000}, {d2, 40000}, {d3, 31000}, {d4, 34000}}, nil)
			}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			moved, ctx := openMigrated(t)
			tc.moved(t, moved, ctx)
			kept, _ := openMigrated(t)
			tc.kept(t, kept, ctx)
			p := params("global", 0, end)
			p.TransferMatching = tc.tm
			got, want := globalSummary(t, moved, ctx, p), globalSummary(t, kept, ctx, p)
			if got.TWR == nil || want.TWR == nil || math.Abs(*got.TWR-*want.TWR) > 1e-9 {
				t.Errorf("TWR = %s, want %s as if the money never left", rateText(got.TWR), rateText(want.TWR))
			}
			if nf := netFlowOf(t, []ReturnRow{got}, ""); math.Abs(nf) > 1e-6 {
				t.Errorf("net flow = %v, want none: the move books no capital", nf)
			}
		})
	}
}

// TestZeroDebutInsideTheWindowsOpening pins a window that opens at a zero
// first value, or between it and the first nonzero one. The window holds
// none of the legs that moved the money in, and its opening value none of
// the money: the first nonzero valuation shows it, and onboards it, as a
// debut after the window opens onboards what moved in before. The cash
// household then reads as if the money never left; a window that opens
// before the zero reads as TestZeroDebutFundsItsFirstNonzeroValue pins.
func TestZeroDebutInsideTheWindowsOpening(t *testing.T) {
	t.Parallel()
	d0, d1, d2, d3, d4 := dy(2024, time.January, 2), dy(2024, time.February, 1), dy(2024, time.February, 2),
		dy(2024, time.April, 30), dy(2024, time.June, 28)
	end := eod(2024, time.June, 28)
	moved, ctx := openMigrated(t)
	seedReturnsSource(t, moved, ctx, "s1", "schwab")
	seedAcct(t, moved, ctx, "s1", "A", canonical.AccountKindBrokerage, nil,
		[]snap{{d0, 20000}, {d1, 10000}, {d3, 10000}, {d4, 10000}}, []txn{{d1, canonical.TxKindTransferOut, -10000}})
	seedAcct(t, moved, ctx, "s1", "B", canonical.AccountKindBrokerage, nil,
		[]snap{{d2, 0}, {d3, 10000}, {d4, 10500}}, []txn{{d1, canonical.TxKindTransferIn, 10000}})
	kept, _ := openMigrated(t)
	seedReturnsSource(t, kept, ctx, "s1", "schwab")
	seedAcct(t, kept, ctx, "s1", "R", canonical.AccountKindBrokerage, nil,
		[]snap{{d0, 20000}, {d1, 20000}, {d2, 20000}, {d3, 20000}, {d4, 20500}}, nil)
	for _, tc := range []struct {
		name  string
		from  int64
		flows []returns.Flow
	}{
		{"before the zero", dy(2024, time.January, 20), nil},
		{"at the zero", d2, []returns.Flow{{Day: EpochDay(d3), Amount: 10000, Synthetic: true}}},
		{"inside the gap", dy(2024, time.April, 1), []returns.Flow{{Day: EpochDay(d3), Amount: 10000, Synthetic: true}}},
		{"after the debut", dy(2024, time.May, 15), nil},
	} {
		p := params("global", tc.from, end)
		if flows, _ := globalFlows(t, moved, ctx, p); !slices.EqualFunc(flows, tc.flows, sameFlow) {
			t.Errorf("%s: flows = %+v, want %+v", tc.name, flows, tc.flows)
		}
		got, want := globalSummary(t, moved, ctx, p), globalSummary(t, kept, ctx, p)
		if got.TWR == nil || want.TWR == nil || math.Abs(*got.TWR-*want.TWR) > 1e-9 {
			t.Errorf("%s: TWR = %s, want %s as if the money never left", tc.name, rateText(got.TWR), rateText(want.TWR))
		}
	}
}

// TestZeroDebutInASinceYearWindow pins the materialized since-year summary
// whose 1 January falls between a zero first value and the first nonzero
// one: it reads the debut as capital, as the CLI window does, and the
// summary from a year before the zero reads as since inception.
func TestZeroDebutInASinceYearWindow(t *testing.T) {
	t.Parallel()
	d0, d1, d2, d3, d4 := dy(2023, time.November, 1), dy(2023, time.December, 1), dy(2023, time.December, 2),
		dy(2024, time.March, 29), dy(2024, time.June, 28)
	seed := func(t *testing.T, moved bool) (*sql.DB, context.Context) {
		db, ctx := openMigrated(t)
		seedReturnsSource(t, db, ctx, "s1", "schwab")
		if moved {
			seedAcct(t, db, ctx, "s1", "A", canonical.AccountKindBrokerage, nil,
				[]snap{{d0, 20000}, {d1, 10000}, {d3, 10000}, {d4, 10000}}, []txn{{d1, canonical.TxKindTransferOut, -10000}})
			seedAcct(t, db, ctx, "s1", "B", canonical.AccountKindBrokerage, nil,
				[]snap{{d2, 0}, {d3, 10000}, {d4, 10500}}, []txn{{d1, canonical.TxKindTransferIn, 10000}})
		} else {
			seedAcct(t, db, ctx, "s1", "R", canonical.AccountKindBrokerage, nil,
				[]snap{{d0, 20000}, {d1, 20000}, {d2, 20000}, {d3, 20000}, {d4, 20500}}, nil)
		}
		if _, err := MaterializeReturns(ctx, db, MaterializeParams{ToEpoch: eod(2024, time.June, 28), ComputedAt: 1}); err != nil {
			t.Fatal(err)
		}
		return db, ctx
	}
	twr := func(t *testing.T, db *sql.DB, ctx context.Context, year int) float64 {
		var v float64
		if err := db.QueryRowContext(ctx, `SELECT twr FROM report_returns WHERE grain = 'global'
			AND granularity = 'total' AND is_summary AND currency = 'USD' AND window_from_year = ?`, year).Scan(&v); err != nil {
			t.Fatalf("window %d: %v", year, err)
		}
		return v
	}
	moved, mctx := seed(t, true)
	kept, kctx := seed(t, false)
	for _, year := range []int{2023, 2024} {
		if got, want := twr(t, moved, mctx, year), twr(t, kept, kctx, year); math.Abs(got-want) > 1e-9 {
			t.Errorf("since %d: TWR = %v, want %v as if the money never left", year, got, want)
		}
	}
}

// TestZeroIsNoValuation pins that a zero first value is no valuation of
// the money moved in before it. Every leg in before the zero, paired or
// not, funds the first nonzero valuation, the debut: a wire from an
// untracked bank, a transfer in from an untracked account, and a move from
// a tracked account beside a deposit. The household then reads, flow for
// flow, as the same household without the zero, in every window: one that
// opens before the legs, between the legs and the zero, at the zero,
// between the zero and the first nonzero value, on it, and after it.
func TestZeroIsNoValuation(t *testing.T) {
	t.Parallel()
	d0, leg, zero, debut, end := dy(2024, time.January, 2), dy(2024, time.January, 20), dy(2024, time.February, 2),
		dy(2024, time.April, 30), dy(2024, time.June, 28)
	steady := []snap{{d0, 20000}, {leg, 20000}, {debut, 20000}, {end, 20400}}
	for _, tc := range []struct {
		name  string
		a     []snap // the live account's valuations
		aLegs []txn
		b     []snap // the zero-debut account's valuations, the zero first
		bLegs []txn
		flows []returns.Flow // since inception
	}{
		{"a wire from an untracked bank", steady, nil,
			[]snap{{zero, 0}, {debut, 10050}, {end, 10100}},
			[]txn{{leg, canonical.TxKindDeposit, 10000}},
			[]returns.Flow{{Day: EpochDay(debut), Amount: 10050, Synthetic: true}}},
		{"a transfer in from an untracked account", steady, nil,
			[]snap{{zero, 0}, {debut, 10050}, {end, 10100}},
			[]txn{{leg, canonical.TxKindTransferIn, 10000}},
			[]returns.Flow{{Day: EpochDay(debut), Amount: 10050, Synthetic: true}}},
		{"a move from a tracked account beside a deposit",
			[]snap{{d0, 30000}, {leg, 20000}, {debut, 20000}, {end, 20400}},
			[]txn{{leg, canonical.TxKindTransferOut, -10000}},
			[]snap{{zero, 0}, {debut, 12100}, {end, 12200}},
			[]txn{{leg, canonical.TxKindTransferIn, 10000}, {leg, canonical.TxKindDeposit, 2000}},
			[]returns.Flow{{Day: EpochDay(debut), Amount: 2100, Synthetic: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			seed := func(b []snap) (*sql.DB, context.Context) {
				db, ctx := openMigrated(t)
				seedReturnsSource(t, db, ctx, "s1", "schwab")
				seedAcct(t, db, ctx, "s1", "A", canonical.AccountKindBrokerage, nil, tc.a, tc.aLegs)
				seedAcct(t, db, ctx, "s1", "B", canonical.AccountKindBrokerage, nil, b, tc.bLegs)
				return db, ctx
			}
			zeroed, ctx := seed(tc.b)
			plain, _ := seed(tc.b[1:])
			if flows, _ := globalFlows(t, zeroed, ctx, params("global", 0, end)); !slices.EqualFunc(flows, tc.flows, sameFlow) {
				t.Errorf("flows = %+v, want %+v", flows, tc.flows)
			}
			for _, from := range []int64{0, dy(2024, time.January, 25), zero, dy(2024, time.March, 15), debut, dy(2024, time.May, 15)} {
				p := params("global", from, end)
				got, _ := globalFlows(t, zeroed, ctx, p)
				want, _ := globalFlows(t, plain, ctx, p)
				if !slices.EqualFunc(got, want, sameFlow) {
					t.Errorf("from %d: flows = %+v, want %+v as without the zero", EpochDay(from), got, want)
				}
				g, w := globalSummary(t, zeroed, ctx, p), globalSummary(t, plain, ctx, p)
				if g.TWR == nil || w.TWR == nil || math.Abs(*g.TWR-*w.TWR) > 1e-9 {
					t.Errorf("from %d: TWR = %s, want %s as without the zero", EpochDay(from), rateText(g.TWR), rateText(w.TWR))
				}
			}
		})
	}
}

// TestZeroDebutKeepsItsReading pins what a zero first value leaves as it
// was: money paid out before it, where the legs by the
// first nonzero value move more out than in, leaves the later value to read
// as the account's own; money moved in before a zero no nonzero value
// follows never shows; and a first value that is not zero books what it
// shows.
func TestZeroDebutKeepsItsReading(t *testing.T) {
	t.Parallel()
	d0, leg, zero, dep, debut, end := dy(2024, time.January, 2), dy(2024, time.January, 20), dy(2024, time.February, 2),
		dy(2024, time.March, 15), dy(2024, time.April, 30), dy(2024, time.June, 28)
	for _, tc := range []struct {
		name  string
		b     []snap
		bLegs []txn
		flows []returns.Flow
	}{
		{"paid out before the zero, more than came in",
			[]snap{{zero, 0}, {debut, 3100}, {end, 3100}},
			[]txn{{leg, canonical.TxKindWithdrawal, -3500}, {dep, canonical.TxKindDeposit, 3000}},
			[]returns.Flow{{Day: EpochDay(dep), Amount: 3000}}},
		{"moved in before a zero that stays zero",
			[]snap{{zero, 0}, {end, 0}},
			[]txn{{leg, canonical.TxKindDeposit, 300}},
			nil},
		{"a first value that is not zero",
			[]snap{{zero, 10050}, {end, 10100}},
			[]txn{{leg, canonical.TxKindDeposit, 10000}},
			[]returns.Flow{{Day: EpochDay(zero), Amount: 10050, Synthetic: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := openMigrated(t)
			seedReturnsSource(t, db, ctx, "s1", "schwab")
			seedAcct(t, db, ctx, "s1", "A", canonical.AccountKindBrokerage, nil,
				[]snap{{d0, 20000}, {end, 20400}}, nil)
			seedAcct(t, db, ctx, "s1", "B", canonical.AccountKindBrokerage, nil, tc.b, tc.bLegs)
			if flows, _ := globalFlows(t, db, ctx, params("global", 0, end)); !slices.EqualFunc(flows, tc.flows, sameFlow) {
				t.Errorf("flows = %+v, want %+v", flows, tc.flows)
			}
		})
	}
}

// TestZeroHeadDebutBooksWhatItsValueDoesNotHold pins how the first nonzero
// valuation after a zero first value funds every leg before it, as a debut
// would: it shows the money that came in and books the rest as capital
// either way. Money paid out before the zero, where the legs move more out
// than in, funds the zero itself.
func TestZeroHeadDebutBooksWhatItsValueDoesNotHold(t *testing.T) {
	t.Parallel()
	d0, d1, d2, dep, d3, d4 := dy(2024, time.January, 2), dy(2024, time.February, 1), dy(2024, time.February, 2),
		dy(2024, time.March, 15), dy(2024, time.April, 30), dy(2024, time.June, 28)
	end := eod(2024, time.June, 28)
	for _, tc := range []struct {
		name  string
		b     []snap // the zero-debut account's valuations
		bLegs []txn  // its legs; the first is the move in or out
		aLeg  txn    // the live account's leg of the move
		flows []returns.Flow
	}{
		{"a deposit after the zero",
			[]snap{{d2, 0}, {d3, 15000}, {d4, 15000}},
			[]txn{{d1, canonical.TxKindTransferIn, 10000}, {dep, canonical.TxKindDeposit, 5000}},
			txn{d1, canonical.TxKindTransferOut, -10000},
			[]returns.Flow{{Day: EpochDay(d3), Amount: 5000, Synthetic: true}}},
		{"a first nonzero value short of the money: capital that left",
			[]snap{{d2, 0}, {d3, 4000}, {d4, 4000}},
			[]txn{{d1, canonical.TxKindTransferIn, 10000}},
			txn{d1, canonical.TxKindTransferOut, -10000},
			[]returns.Flow{{Day: EpochDay(d3), Amount: -6000, Synthetic: true}}},
		{"a first nonzero value beyond the money and the deposit: capital that came in",
			[]snap{{d2, 0}, {d3, 17000}, {d4, 17000}},
			[]txn{{d1, canonical.TxKindTransferIn, 10000}, {dep, canonical.TxKindDeposit, 5000}},
			txn{d1, canonical.TxKindTransferOut, -10000},
			[]returns.Flow{{Day: EpochDay(d3), Amount: 7000, Synthetic: true}}},
		{"paid out before the zero: the zero books it",
			[]snap{{d2, 0}, {d3, 5000}, {d4, 5000}},
			[]txn{{d1, canonical.TxKindTransferOut, -10000}, {dep, canonical.TxKindDeposit, 5000}},
			txn{d1, canonical.TxKindTransferIn, 10000},
			[]returns.Flow{{Day: EpochDay(d2), Amount: 10000, Synthetic: true}, {Day: EpochDay(dep), Amount: 5000}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := openMigrated(t)
			seedReturnsSource(t, db, ctx, "s1", "schwab")
			aVal := int64(20000)
			if tc.aLeg.amt > 0 {
				aVal = 30000
			}
			seedAcct(t, db, ctx, "s1", "A", canonical.AccountKindBrokerage, nil,
				[]snap{{d0, 20000}, {d1, aVal}, {d4, aVal}}, []txn{tc.aLeg})
			seedAcct(t, db, ctx, "s1", "B", canonical.AccountKindBrokerage, nil, tc.b, tc.bLegs)
			if flows, _ := globalFlows(t, db, ctx, params("global", 0, end)); !slices.EqualFunc(flows, tc.flows, sameFlow) {
				t.Errorf("flows = %+v, want %+v", flows, tc.flows)
			}
		})
	}
}

// TestZeroHeadShapes pins three shapes of legs around a zero first value:
//   - a round trip before the zero and money moved in after it;
//   - money moved in only after the zero, the first nonzero value months on;
//   - small deposits reversed before the zero, and nothing else.
//
// In the first two the legs move money in on net by the first nonzero
// valuation, which they fund: the household reads, flow for flow, as the
// same household without the zero, in every window. In the third nothing
// moved in on net: the zero stays the first valuation, and the household
// reads as if the round trips had never happened.
func TestZeroHeadShapes(t *testing.T) {
	t.Parallel()
	d0, in1, out1, zero, in2, in3, debut, end := dy(2024, time.January, 2), dy(2024, time.January, 10),
		dy(2024, time.January, 16), dy(2024, time.January, 20), dy(2024, time.February, 20),
		dy(2024, time.February, 22), dy(2024, time.March, 20), dy(2024, time.June, 28)
	dep, late, later := dy(2024, time.January, 22), dy(2024, time.December, 20), dy(2025, time.June, 27)
	steady := []snap{{d0, 20000}, {later, 20400}}
	for _, tc := range []struct {
		name  string
		a     []snap // the live account's valuations
		aLegs []txn
		b     []snap // the zero-debut account's valuations, the zero first
		bLegs []txn
		// bare is b as it reads: without its zero, or without its legs
		// where they move no money in on net.
		bare    func(b []snap, legs []txn) ([]snap, []txn)
		to      int64
		windows []int64 // window openings the household reads alike in
		flows   []returns.Flow
		nonzero bool // b debuts on its first nonzero value
	}{
		{"a round trip before the zero, money in after it",
			[]snap{{d0, 30000}, {in1, 20000}, {out1, 30000}, {in2, 24000}, {in3, 20000}, {debut, 20000}, {end, 20400}},
			[]txn{{in1, canonical.TxKindTransferOut, -10000}, {out1, canonical.TxKindTransferIn, 10000},
				{in2, canonical.TxKindTransferOut, -6000}, {in3, canonical.TxKindTransferOut, -4000}},
			[]snap{{zero, 0}, {debut, 10100}, {end, 10200}},
			[]txn{{in1, canonical.TxKindTransferIn, 10000}, {out1, canonical.TxKindTransferOut, -10000},
				{in2, canonical.TxKindTransferIn, 6000}, {in3, canonical.TxKindTransferIn, 4000}},
			func(b []snap, legs []txn) ([]snap, []txn) { return b[1:], legs },
			end, []int64{0, dy(2024, time.January, 12), zero, dy(2024, time.February, 21), debut, dy(2024, time.May, 15)},
			[]returns.Flow{{Day: EpochDay(debut), Amount: 100, Synthetic: true}},
			true},
		{"money in only after the zero",
			steady, nil,
			[]snap{{zero, 0}, {late, 10800}, {later, 10900}},
			[]txn{{dep, canonical.TxKindDeposit, 10000}},
			func(b []snap, legs []txn) ([]snap, []txn) { return b[1:], legs },
			later, []int64{0, dy(2024, time.January, 21), dy(2024, time.June, 1), late, dy(2025, time.March, 1)},
			[]returns.Flow{{Day: EpochDay(late), Amount: 10800, Synthetic: true}},
			true},
		{"small deposits reversed before the zero",
			steady, nil,
			[]snap{{zero, 0}, {debut, 500}, {later, 500}},
			[]txn{{dy(2024, time.January, 5), canonical.TxKindDeposit, 1}, {dy(2024, time.January, 6), canonical.TxKindDeposit, 2},
				{dy(2024, time.January, 8), canonical.TxKindWithdrawal, -1}, {dy(2024, time.January, 9), canonical.TxKindWithdrawal, -2}},
			func(b []snap, _ []txn) ([]snap, []txn) { return b, nil },
			later, []int64{0, dy(2024, time.January, 7), zero, dy(2024, time.February, 1), debut, dy(2024, time.May, 15)},
			nil,
			false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			seed := func(b []snap, legs []txn) (*sql.DB, context.Context) {
				db, ctx := openMigrated(t)
				seedReturnsSource(t, db, ctx, "s1", "schwab")
				seedAcct(t, db, ctx, "s1", "A", canonical.AccountKindBrokerage, nil, tc.a, tc.aLegs)
				seedAcct(t, db, ctx, "s1", "B", canonical.AccountKindBrokerage, nil, b, legs)
				return db, ctx
			}
			zeroed, ctx := seed(tc.b, tc.bLegs)
			bare, _ := seed(tc.bare(tc.b, tc.bLegs))
			if flows, _ := globalFlows(t, zeroed, ctx, params("global", 0, tc.to)); !slices.EqualFunc(flows, tc.flows, sameFlow) {
				t.Errorf("flows = %+v, want %+v", flows, tc.flows)
			}
			for _, from := range tc.windows {
				p := params("global", from, tc.to)
				got, _ := globalFlows(t, zeroed, ctx, p)
				want, _ := globalFlows(t, bare, ctx, p)
				if !slices.EqualFunc(got, want, sameFlow) {
					t.Errorf("from %d: flows = %+v, want %+v as the bare household", EpochDay(from), got, want)
				}
				g, w := globalSummary(t, zeroed, ctx, p), globalSummary(t, bare, ctx, p)
				if g.TWR == nil || w.TWR == nil || math.Abs(*g.TWR-*w.TWR) > 1e-9 {
					t.Errorf("from %d: TWR = %s, want %s as the bare household", EpochDay(from), rateText(g.TWR), rateText(w.TWR))
				}
			}
			// The account's own row opens on its debut: the first nonzero
			// value where it moved money in, else the zero.
			rows, err := RunReturns(ctx, zeroed, params("accounts", 0, tc.to))
			if err != nil {
				t.Fatal(err)
			}
			opens := EpochDay(zero)
			if tc.nonzero {
				opens = EpochDay(tc.b[1].at)
			}
			if b, ok := summaryFor(rows, "B"); !ok || b.StartDay != opens {
				t.Errorf("account B opens on %d, want %d", b.StartDay, opens)
			}
		})
	}
}

// TestMovesInByNetsTheLegs pins the net test of the legs on or before a
// day: a leg on the day counts, a round trip moves no money in, within the
// value netter's tolerance, and neither does more out than in or a leg
// after the day.
func TestMovesInByNetsTheLegs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		legs []returns.Flow
		want bool
	}{
		{"a deposit", []returns.Flow{{Day: 1, Amount: 100}}, true},
		{"a deposit on the day", []returns.Flow{{Day: 10, Amount: 100}}, true},
		{"a round trip", []returns.Flow{{Day: 1, Amount: 10000}, {Day: 3, Amount: -10000}}, false},
		{"a round trip that FX moved", []returns.Flow{{Day: 1, Amount: 10000}, {Day: 3, Amount: -9960}}, false},
		{"a round trip and money after it", []returns.Flow{{Day: 1, Amount: 10000}, {Day: 3, Amount: -10000}, {Day: 8, Amount: 6000}}, true},
		{"more out than in", []returns.Flow{{Day: 1, Amount: 500}, {Day: 3, Amount: -700}}, false},
		{"money in after the day", []returns.Flow{{Day: 1, Amount: 10000}, {Day: 3, Amount: -10000}, {Day: 11, Amount: 500}}, false},
		{"below the floor", []returns.Flow{{Day: 1, Amount: 0.5}}, false},
	} {
		if got := movesInBy(tc.legs, 10); got != tc.want {
			t.Errorf("%s: movesInBy = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestReadZeroHeadDropsOnlyAFundedZero pins which first values readZeroHead
// drops: only a zero that a nonzero value follows, where the legs on or
// before that value moved money in on net. A first value that is not zero,
// a zero no money came in before, and a zero that stays zero keep their
// series. The drop leaves every valuation and snapshot from the first
// nonzero valuation on; the daily series carries days that are no
// snapshot.
func TestReadZeroHeadDropsOnlyAFundedZero(t *testing.T) {
	t.Parallel()
	zeroHead := []dayVal{{30, 0}, {31, 0}, {120, 2000}, {121, 2000}, {200, 2100}}
	for _, tc := range []struct {
		name   string
		series []dayVal
		legs   []returns.Flow
		opens  int64 // the first valuation readZeroHead leaves
	}{
		{"a zero money moved in before", zeroHead, []returns.Flow{{Day: 29, Amount: 2000}}, 120},
		{"a zero money moved in on the first nonzero day", zeroHead, []returns.Flow{{Day: 120, Amount: 2000}}, 120},
		{"a first value that is not zero", []dayVal{{30, 10000}, {31, 10000}, {200, 10300}},
			[]returns.Flow{{Day: 29, Amount: 10000}}, 30},
		{"a zero money moved out before, more than came in", zeroHead,
			[]returns.Flow{{Day: 29, Amount: -300}, {Day: 120, Amount: -500}}, 30},
		{"a zero that stays zero", []dayVal{{30, 0}, {31, 0}, {200, 0}}, []returns.Flow{{Day: 29, Amount: 300}}, 30},
	} {
		var snaps []int64
		for _, dv := range tc.series {
			if dv.day != 31 && dv.day != 121 {
				snaps = append(snaps, dv.day)
			}
		}
		a := &accountData{series: slices.Clone(tc.series), snapDays: slices.Clone(snaps)}
		a.readZeroHead(tc.legs)
		wantSeries := slices.DeleteFunc(slices.Clone(tc.series), func(dv dayVal) bool { return dv.day < tc.opens })
		wantSnaps := slices.DeleteFunc(slices.Clone(snaps), func(d int64) bool { return d < tc.opens })
		if !slices.Equal(a.series, wantSeries) || !slices.Equal(a.snapDays, wantSnaps) {
			t.Errorf("%s: series %v, snapshots %v; want %v, %v", tc.name, a.series, a.snapDays, wantSeries, wantSnaps)
		}
	}
}

// TestZeroDebutThatStaysZeroPaysOut pins the debut no nonzero value follows:
// the zero is the account's only word on the money moved in before it, and
// says none of it is there. The legs fund that zero, so the money leaves as
// capital on it: what happens before a first valuation is capital, never
// performance. A debut whose first value is not zero funds that value.
func TestZeroDebutThatStaysZeroPaysOut(t *testing.T) {
	t.Parallel()
	d0, d1, d2, d4 := dy(2024, time.January, 2), dy(2024, time.February, 1), dy(2024, time.February, 2), dy(2024, time.June, 28)
	end := eod(2024, time.June, 28)
	for _, tc := range []struct {
		name  string
		b     []snap
		flows []returns.Flow
		twr   float64
	}{
		{"a zero that stays zero", []snap{{d2, 0}, {d4, 0}},
			[]returns.Flow{{Day: EpochDay(d2), Amount: -10000, Synthetic: true}}, 0.05},
		{"a first value short of the money", []snap{{d2, 4000}, {d4, 4000}},
			[]returns.Flow{{Day: EpochDay(d2), Amount: -6000, Synthetic: true}}, 0.05 * 10000 / 14000},
		{"a first value that holds the money", []snap{{d2, 10000}, {d4, 10000}}, nil, 0.05 * 10000 / 20000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := openMigrated(t)
			seedReturnsSource(t, db, ctx, "s1", "schwab")
			seedAcct(t, db, ctx, "s1", "A", canonical.AccountKindBrokerage, nil,
				[]snap{{d0, 20000}, {d1, 10000}, {d4, 10500}}, []txn{{d1, canonical.TxKindTransferOut, -10000}})
			seedAcct(t, db, ctx, "s1", "B", canonical.AccountKindBrokerage, nil, tc.b,
				[]txn{{d1, canonical.TxKindTransferIn, 10000}})
			p := params("global", 0, end)
			flows, _ := globalFlows(t, db, ctx, p)
			if !slices.EqualFunc(flows, tc.flows, sameFlow) {
				t.Errorf("flows = %+v, want %+v", flows, tc.flows)
			}
			if g := globalSummary(t, db, ctx, p); g.TWR == nil || math.Abs(*g.TWR-tc.twr) > 1e-9 {
				t.Errorf("TWR = %s, want %v", rateText(g.TWR), tc.twr)
			}
		})
	}
}

// TestPerEntityOnceZeroDebutCarriesItsFunding pins a zero first value in a
// per-entity-once group: the group books the step on the first nonzero
// valuation, which the legs before the zero fund, and the chain carries
// their money until then.
func TestPerEntityOnceZeroDebutCarriesItsFunding(t *testing.T) {
	t.Parallel()
	once := returns.ReturnsPolicy{OnboardScope: returns.OnboardPerEntityOnce}
	cash := &accountData{src: "u", acct: "CASH", rpolicy: once,
		series:       []dayVal{{0, 20000}, {10, 10000}, {300, 10000}},
		snapDays:     []int64{0, 10, 300},
		transferLike: []returns.Flow{{Day: 10, Amount: -10000, ID: "out"}}}
	sec := &accountData{src: "u", acct: "SEC", rpolicy: once,
		series:       []dayVal{{11, 0}, {200, 12000}, {300, 12000}},
		snapDays:     []int64{11, 200, 300},
		transferLike: []returns.Flow{{Day: 10, Amount: 10000, ID: "in"}}}
	for _, a := range []*accountData{cash, sec} {
		a.readZeroHead(ownLegs(a))
		a.cacheTail()
	}
	flows, _, rolled := entityFlows([]*accountData{cash, sec}, params("sources", 0, 0), 0, 300, true)
	if want := []returns.Flow{{Day: 200, Amount: 2000, Synthetic: true}}; !slices.EqualFunc(flows, want, sameFlow) {
		t.Errorf("flows = %+v, want the step on the first nonzero valuation %+v", flows, want)
	}
	if want := (returns.Flow{Day: 10, ValuedOn: 200, Amount: 10000, ID: "in"}); !slices.Contains(rolled, want) {
		t.Errorf("rolled = %+v, want the carry %+v", rolled, want)
	}
	// Money from outside the group funds that valuation too: the step is
	// all it shows.
	ext := &accountData{src: "u", acct: "EXT", rpolicy: once,
		series:      []dayVal{{11, 0}, {200, 3000}, {300, 3000}},
		snapDays:    []int64{11, 200, 300},
		nonTransfer: []returns.Flow{{Day: 10, Amount: 2900, ID: "dep"}}}
	idle := &accountData{src: "u", acct: "IDLE", rpolicy: once,
		series: []dayVal{{0, 20000}, {300, 20000}}, snapDays: []int64{0, 300}}
	for _, a := range []*accountData{ext, idle} {
		a.readZeroHead(ownLegs(a))
		a.cacheTail()
	}
	flows, _, _ = entityFlows([]*accountData{idle, ext}, params("sources", 0, 0), 0, 300, true)
	if want := []returns.Flow{{Day: 200, Amount: 3000, Synthetic: true}}; !slices.EqualFunc(flows, want, sameFlow) {
		t.Errorf("flows = %+v, want the step on the first nonzero valuation %+v", flows, want)
	}
}

// TestZeroHeadAgreesAcrossPaths pins that web-materialize, which loads every
// currency at once, and a CLI run read a zero first value alike, in every
// currency: one a paired leg funds, one a deposit nothing pairs funds, and
// one money moved in only after the zero funds.
func TestZeroHeadAgreesAcrossPaths(t *testing.T) {
	t.Parallel()
	d0, d1, d2, d3, d4 := dy(2024, time.January, 2), dy(2024, time.February, 1), dy(2024, time.February, 2),
		dy(2024, time.April, 30), dy(2024, time.June, 28)
	end := eod(2024, time.June, 28)
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "s1", "schwab")
	seedAcct(t, db, ctx, "s1", "A", canonical.AccountKindBrokerage, nil,
		[]snap{{d0, 20000}, {d1, 10000}, {d3, 10000}, {d4, 10000}}, []txn{{d1, canonical.TxKindTransferOut, -10000}})
	seedAcct(t, db, ctx, "s1", "B", canonical.AccountKindBrokerage, nil,
		[]snap{{d2, 0}, {d3, 10000}, {d4, 10500}}, []txn{{d1, canonical.TxKindTransferIn, 10000}})
	seedAcct(t, db, ctx, "s1", "C", canonical.AccountKindBrokerage, nil,
		[]snap{{d2, 0}, {d3, 1000}, {d4, 1000}}, []txn{{d1, canonical.TxKindDeposit, 1000}})
	seedAcct(t, db, ctx, "s1", "D", canonical.AccountKindBrokerage, nil,
		[]snap{{d2, 0}, {d3, 2000}, {d4, 2000}}, []txn{{dy(2024, time.February, 4), canonical.TxKindDeposit, 2000}})
	seedFX(t, db, dy(2024, time.January, 1), "USD", "CHF", "0.90")
	seedFX(t, db, dy(2024, time.January, 1), "USD", "EUR", "0.95")
	seedFX(t, db, dy(2024, time.January, 1), "USD", "GBP", "0.80")
	fx, err := loadFxBounds(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	multi, err := loadReturnsDatasets(ctx, db, everyCurrency(), fx, ReturnsSettings{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ccy := range materializeCurrencies {
		p := params("global", 0, end)
		p.OutCcy = ccy
		want, err := RunReturns(ctx, db, p)
		if err != nil {
			t.Fatal(err)
		}
		got := computeReturns(multi[ccy], p)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: materialized %+v, CLI %+v", ccy, got, want)
		}
		if g, _ := summaryFor(got, ""); g.TWR == nil || math.Abs(*g.TWR-500.0/23000) > 1e-6 {
			t.Errorf("%s: TWR = %s, want the household's 500 on 23000", ccy, rateText(g.TWR))
		}
	}
}

// TestZeroHeadDecidesOnceAcrossCurrencies pins that one decision reads a
// zero first value in every currency. A round trip in dollars before the
// first nonzero value moves no money in, but FX that moved between its legs
// nets it in francs. The legs are read in residueCurrency, so the account
// debuts on its zero in every currency, on the CLI and in the materialized
// load alike.
func TestZeroHeadDecidesOnceAcrossCurrencies(t *testing.T) {
	t.Parallel()
	d0, in, out, zero, nonzero, end := dy(2024, time.January, 2), dy(2024, time.January, 10),
		dy(2024, time.January, 16), dy(2024, time.January, 20), dy(2024, time.March, 20), dy(2024, time.June, 28)
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "s1", "schwab")
	seedAcct(t, db, ctx, "s1", "A", canonical.AccountKindBrokerage, nil, []snap{{d0, 20000}, {end, 20400}}, nil)
	seedAcct(t, db, ctx, "s1", "B", canonical.AccountKindBrokerage, nil,
		[]snap{{zero, 0}, {nonzero, 1000}, {end, 1000}},
		[]txn{{in, canonical.TxKindDeposit, 10000}, {out, canonical.TxKindWithdrawal, -10000}})
	// The franc buys 0.80 dollars on the deposit's day and 0.90 on the
	// withdrawal's: in francs the legs net about 1400 in, well past the
	// netting tolerance.
	seedFX(t, db, dy(2024, time.January, 1), "USD", "CHF", "0.80")
	seedFX(t, db, dy(2024, time.January, 15), "USD", "CHF", "0.90")
	seedFX(t, db, dy(2024, time.January, 1), "USD", "EUR", "0.95")
	seedFX(t, db, dy(2024, time.January, 1), "USD", "GBP", "0.80")
	fx, err := loadFxBounds(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	multi, err := loadReturnsDatasets(ctx, db, everyCurrency(), fx, ReturnsSettings{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ccy := range materializeCurrencies {
		p := params("accounts", 0, eod(2024, time.June, 28))
		p.OutCcy = ccy
		cli, err := RunReturns(ctx, db, p)
		if err != nil {
			t.Fatal(err)
		}
		for path, rows := range map[string][]ReturnRow{"CLI": cli, "materialized": computeReturns(multi[ccy], p)} {
			if b, ok := summaryFor(rows, "B"); !ok || b.StartDay != EpochDay(zero) {
				t.Errorf("%s %s: account B opens on %d, want its zero %d", ccy, path, b.StartDay, EpochDay(zero))
			}
		}
	}
}
