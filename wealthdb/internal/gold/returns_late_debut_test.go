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
// valuation is return. A window that ends before that valuation carries the
// money to its end.
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
			for _, to := range []int64{end, eod(2024, time.March, 31)} {
				p := params("global", 0, to)
				p.TransferMatching = tc.tm
				got, want := globalSummary(t, moved, ctx, p), globalSummary(t, kept, ctx, p)
				if got.TWR == nil || want.TWR == nil || math.Abs(*got.TWR-*want.TWR) > 1e-9 {
					t.Errorf("to %d: TWR = %s, want %s as if the money never left", EpochDay(to), rateText(got.TWR), rateText(want.TWR))
				}
				if nf := netFlowOf(t, []ReturnRow{got}, ""); math.Abs(nf) > 1e-6 {
					t.Errorf("to %d: net flow = %v, want none: the move books no capital", EpochDay(to), nf)
				}
			}
		})
	}
}

// TestLateDebutBooksWhatItsValueDoesNotHold pins how the first nonzero
// valuation funds the legs before a zero first value, as a debut would: it
// shows the money that came in, beyond the account's own legs since the
// zero, and books the rest as capital either way. The account's own later
// legs keep their own reading. Money the account paid out before its zero
// funds the zero itself.
func TestLateDebutBooksWhatItsValueDoesNotHold(t *testing.T) {
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
		{"a deposit after the zero keeps its own flow",
			[]snap{{d2, 0}, {d3, 15000}, {d4, 15000}},
			[]txn{{d1, canonical.TxKindTransferIn, 10000}, {dep, canonical.TxKindDeposit, 5000}},
			txn{d1, canonical.TxKindTransferOut, -10000},
			[]returns.Flow{{Day: EpochDay(dep), Amount: 5000}}},
		{"a first nonzero value short of the money: capital that left",
			[]snap{{d2, 0}, {d3, 4000}, {d4, 4000}},
			[]txn{{d1, canonical.TxKindTransferIn, 10000}},
			txn{d1, canonical.TxKindTransferOut, -10000},
			[]returns.Flow{{Day: EpochDay(d3), Amount: -6000, Synthetic: true}}},
		{"a first nonzero value beyond the money and the deposit: capital that came in",
			[]snap{{d2, 0}, {d3, 17000}, {d4, 17000}},
			[]txn{{d1, canonical.TxKindTransferIn, 10000}, {dep, canonical.TxKindDeposit, 5000}},
			txn{d1, canonical.TxKindTransferOut, -10000},
			[]returns.Flow{{Day: EpochDay(dep), Amount: 5000}, {Day: EpochDay(d3), Amount: 2000, Synthetic: true}}},
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
			flows, _ := globalFlows(t, db, ctx, params("global", 0, end))
			if !slices.EqualFunc(flows, tc.flows, sameFlow) {
				t.Errorf("flows = %+v, want %+v", flows, tc.flows)
			}
		})
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

// TestPerEntityOnceLateDebutCarriesItsFunding pins the late debut in a
// per-entity-once group: the group books the step on the first nonzero
// valuation, which the legs before the zero fund, and the chain carries
// their money until then.
func TestPerEntityOnceLateDebutCarriesItsFunding(t *testing.T) {
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
		a.cacheTail()
	}
	flows, _, rolled := entityFlows([]*accountData{cash, sec}, params("sources", 0, 0), 0, 300, true)
	if want := []returns.Flow{{Day: 200, Amount: 2000, Synthetic: true}}; !slices.EqualFunc(flows, want, sameFlow) {
		t.Errorf("flows = %+v, want the step on the late debut %+v", flows, want)
	}
	if want := (returns.Flow{Day: 10, ValuedOn: 200, Amount: 10000, ID: "in"}); !slices.Contains(rolled, want) {
		t.Errorf("rolled = %+v, want the carry %+v", rolled, want)
	}
}

// TestLateDebutAgreesAcrossPaths pins that web-materialize, which loads every
// currency at once, and a CLI run read a late debut alike, in every currency.
func TestLateDebutAgreesAcrossPaths(t *testing.T) {
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
		if g, _ := summaryFor(got, ""); g.TWR == nil || math.Abs(*g.TWR-0.025) > 1e-6 {
			t.Errorf("%s: TWR = %s, want the household's 2.5%%", ccy, rateText(g.TWR))
		}
	}
}
