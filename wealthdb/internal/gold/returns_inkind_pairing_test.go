package gold

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// TestNetLegsPairsInKindByHolding pins the in-kind rule on the netter's
// candidates: a leg that moved a holding pairs by holding and quantity,
// never by value, and a leg of cash by value alone.
func TestNetLegsPairsInKindByHolding(t *testing.T) {
	t.Parallel()
	type leg struct {
		day     int64
		amount  float64
		holding string // "" for cash
		qty     float64
	}
	for _, tc := range []struct {
		name  string
		legs  []leg
		pairs [][2]int // indexes into legs, positive leg first
		// transit: the first pair carries its transit, as the value
		// netter's rule would not pair it.
		transit bool
	}{
		{"one coin out, one coin in at a moved price",
			[]leg{{10, -1000, "BTC", 1}, {12, 1080, "BTC", 1}}, [][2]int{{1, 0}}, true},
		{"a week apart pairs",
			[]leg{{10, -1000, "BTC", 1}, {17, 1000, "BTC", 1}}, [][2]int{{1, 0}}, true},
		{"eight days apart does not",
			[]leg{{10, -1000, "BTC", 1}, {18, 1000, "BTC", 1}}, nil, false},
		{"a fee in kind inside the tolerance pairs",
			[]leg{{10, -1000, "BTC", 1}, {10, 960, "BTC", 0.96}}, [][2]int{{1, 0}}, true},
		{"a quantity short by more does not",
			[]leg{{10, -1000, "BTC", 1}, {10, 940, "BTC", 0.94}}, nil, false},
		{"two holdings of one value do not",
			[]leg{{10, -1000, "BTC", 1}, {10, 1000, "ETH", 20}}, nil, false},
		{"a holding and cash of one value do not",
			[]leg{{10, -1000, "BTC", 1}, {10, 1000, "", 0}}, nil, false},
		{"a leg that states no quantity does not",
			[]leg{{10, -1000, "BTC", 0}, {10, 1000, "BTC", 1}}, nil, false},
		{"a leg takes the open leg of the closest quantity",
			[]leg{{10, -1000, "BTC", 1}, {10, -990, "BTC", 0.99}, {11, 990, "BTC", 0.99}},
			[][2]int{{2, 1}}, false},
		{"1.0 BTC out, 0.9995 in, a day later at a moved price",
			[]leg{{10, -1000, "BTC", 1}, {11, 1050, "BTC", 0.9995}}, [][2]int{{1, 0}}, true},
		{"100 shares out, 99 in: no pair",
			[]leg{{10, -1000, "SHARE", 100}, {11, 1050, "SHARE", 99}}, nil, false},
		{"100 shares out, 100 in at a moved price",
			[]leg{{10, -1000, "SHARE", 100}, {11, 1050, "SHARE", 100}}, [][2]int{{1, 0}}, true},
		{"100 shares out, 99 in at one value: the value netter may not pair them either",
			[]leg{{10, -1000, "SHARE", 100}, {10, 1000, "SHARE", 99}}, nil, false},
		{"cash pairs by value",
			[]leg{{10, -1000, "", 0}, {12, 1003, "", 0}}, [][2]int{{1, 0}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &accountData{src: "s", acct: "A", inKind: map[string]heldMove{}}
			var cand []flowLeg
			ids := map[string]int{}
			for i, l := range tc.legs {
				id := string(rune('a' + i))
				ids[id] = i
				if l.holding != "" {
					// A holding named SHARE is a security: exact quantities.
					a.inKind[id] = heldMove{holding: l.holding, qty: l.qty, exact: l.holding == "SHARE"}
				}
				cand = append(cand, flowLeg{Flow: returns.Flow{Day: l.day, Amount: l.amount, ID: id}, acct: a, nettable: true})
			}
			var got [][2]int
			var transit []bool
			kept, unmatched := netLegs(cand, func(pos, neg flowLeg, tr bool) {
				got = append(got, [2]int{ids[pos.ID], ids[neg.ID]})
				transit = append(transit, tr)
			})
			if len(got) != len(tc.pairs) || (len(got) > 0 && (got[0] != tc.pairs[0] || transit[0] != tc.transit)) {
				t.Errorf("pairs = %v, transit %v; want %v, transit %t", got, transit, tc.pairs, tc.transit)
			}
			if want := len(tc.legs) - 2*len(tc.pairs); unmatched != want || len(kept) != want {
				t.Errorf("unmatched = %d, kept %d; want %d", unmatched, len(kept), want)
			}
		})
	}
}

// seedHoldingMove books a transfer of qty units of inst on (src, acct), worth
// amt dollars. The row states its asset class: crypto for a coin (BTC, ETH),
// public equity otherwise.
func seedHoldingMove(t *testing.T, db *sql.DB, ctx context.Context, src, acct, id string, at int64, kind canonical.TxKind, inst string, qty string, amt int64) {
	t.Helper()
	q, err := canonical.NewDecimalFromString(qty)
	if err != nil {
		t.Fatal(err)
	}
	class := canonical.AssetClassPublicEquity
	if inst == "BTC" || inst == "ETH" {
		class = canonical.AssetClassCrypto
	}
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertTransactions(ctx, []canonical.TransactionChange{{
			SilverSourceID: src, TransactionExternalID: id, OccurredAt: at, AccountExternalID: acct,
			InstrumentExternalID: &inst, Kind: kind, Currency: "USD", NetAmount: decp(amt), Quantity: &q,
			AssetClass: class}})
	})
}

// TestRunReturnsPairsInKindByHolding drives the in-kind rule from gold: a
// move of a holding between two accounts nets at the grain that holds both,
// whatever its price did between the legs' days, while two legs of one
// value that moved different holdings stay flows.
func TestRunReturnsPairsInKindByHolding(t *testing.T) {
	t.Parallel()
	a, out, k := dy(2024, time.January, 2), dy(2024, time.March, 1), dy(2024, time.December, 2)
	// Five days on at a moved price, outside the value netter's reach; a day
	// on at the same value, inside it.
	late, next := dy(2024, time.March, 6), dy(2024, time.March, 2)
	for _, tc := range []struct {
		name string
		// The two sources and the instruments their legs name; isin, where
		// set, is the identity gold holds for both.
		srcA, kindA, instA, srcB, kindB, instB, isin string
		in, inAmt                                    int64
		qIn                                          string // the quantity received of the 2 sent
		pairs                                        bool
	}{
		{"one coin between two wallets of one source", "ct", "cointracking", "BTC", "ct", "cointracking", "BTC", "", late, 10600, "2", true},
		{"a coin short by a network fee", "ct", "cointracking", "BTC", "ct", "cointracking", "BTC", "", late, 10600, "1.99", true},
		{"shares between brokers of one identity", "s1", "schwab", "SPX1", "s2", "ubs", "CH-SPX", "XX0000000001", late, 10600, "2", true},
		{"shares never arrive short", "s1", "schwab", "SPX1", "s2", "ubs", "CH-SPX", "XX0000000001", late, 10600, "1.99", false},
		{"one id at two kinds of source names no one holding", "s1", "schwab", "SPX1", "s2", "ubs", "SPX1", "", next, 10000, "2", false},
		{"two holdings of one value", "ct", "cointracking", "BTC", "ct", "cointracking", "ETH", "", next, 10000, "2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := openMigrated(t)
			seedReturnsSource(t, db, ctx, tc.srcA, tc.kindA)
			if tc.srcB != tc.srcA {
				seedReturnsSource(t, db, ctx, tc.srcB, tc.kindB)
			}
			seedAcct(t, db, ctx, tc.srcA, "A", canonical.AccountKindBrokerage, nil,
				[]snap{{a, 30000}, {out, 20000}, {k, 20000}}, nil)
			seedAcct(t, db, ctx, tc.srcB, "B", canonical.AccountKindBrokerage, nil,
				[]snap{{a, 5000}, {tc.in, 5000 + tc.inAmt}, {k, 5000 + tc.inAmt}}, nil)
			seedHoldingMove(t, db, ctx, tc.srcA, "A", "out", out, canonical.TxKindTransferOut, tc.instA, "-2", -10000)
			seedHoldingMove(t, db, ctx, tc.srcB, "B", "in", tc.in, canonical.TxKindTransferIn, tc.instB, tc.qIn, tc.inAmt)
			if tc.isin != "" {
				inTx(t, db, ctx, func(w *Writer) error {
					var batch []canonical.InstrumentChange
					for _, x := range [][2]string{{tc.srcA, tc.instA}, {tc.srcB, tc.instB}} {
						batch = append(batch, canonical.InstrumentChange{SilverSourceID: x[0], InstrumentExternalID: x[1],
							AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock, ISIN: &tc.isin,
							FirstSeenAt: a, LastSeenAt: k})
					}
					return w.UpsertInstruments(ctx, batch)
				})
			}
			flows, tags := globalFlows(t, db, ctx, params("global", 0, eod(2024, time.December, 2)))
			if tc.pairs {
				if len(flows) != 0 || len(tags) != 0 {
					t.Errorf("flows %v, tags %v: want the move inside, its price move return", flows, tags)
				}
				return
			}
			if len(flows) != 2 || len(tags) != 1 || tags[0] != "unmatched_transfers=2" {
				t.Errorf("flows %v, tags %v: want both legs flows, unmatched", flows, tags)
			}
		})
	}
}

// TestInKindFundedDebutReadsThePriceMoveAsReturn pins that a wallet that
// debuts on coin moved in funds its debut by quantity: its first value
// stands for the coin at the debut's price, so the price move between the
// move and the first valuation is return, and a fee taken in kind is a
// loss. The household reads as one account that held the coin throughout.
// Where the first valuation holds another quantity than moved in, beyond
// the in-kind tolerance, the legs are not one unit of one holding, and the
// leg funds the debut by its value.
func TestInKindFundedDebutReadsThePriceMoveAsReturn(t *testing.T) {
	t.Parallel()
	d0, d1, d2, d3 := dy(2024, time.January, 2), dy(2024, time.February, 1), dy(2024, time.March, 1), dy(2024, time.June, 3)
	for _, tc := range []struct {
		name string
		held string // the coin the wallet's first valuation holds
		v2   int64  // its value then; the coin left at 30000
		// byValue: the quantities differ, so the leg funds by its value
		// and the debut onboards the rest.
		byValue bool
	}{{"the price falls", "1", 21000, false}, {"a fee in kind and a price fall", "0.97", 20370, false},
		{"twice the quantity moved in", "2", 42000, true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			end := eod(2024, time.June, 3)
			moved, ctx := openMigrated(t)
			seedReturnsSource(t, moved, ctx, "ct", "cointracking")
			seedAcct(t, moved, ctx, "ct", "EX", canonical.AccountKindCrypto, nil,
				[]snap{{d0, 40000}, {d1, 10000}, {d2, 10000}, {d3, 10000}}, nil)
			seedHoldingMove(t, moved, ctx, "ct", "EX", "out", d1, canonical.TxKindTransferOut, "BTC", "-1", -30000)
			inst, usd := "BTC", "USD"
			q, _ := canonical.NewDecimalFromString(tc.held)
			inTx(t, moved, ctx, func(w *Writer) error {
				if err := w.UpsertAccounts(ctx, []canonical.AccountChange{{SilverSourceID: "ct", AccountExternalID: "COLD",
					AccountKind: canonical.AccountKindCrypto, BaseCurrency: &usd, FirstSeenAt: d2, LastSeenAt: d3}}); err != nil {
					return err
				}
				var pos []canonical.PositionChange
				for _, s := range []snap{{d2, tc.v2}, {d3, tc.v2 + 3000}} {
					pos = append(pos, canonical.PositionChange{SilverSourceID: "ct", SnapshotAt: s.at, AccountExternalID: "COLD",
						PositionKey: "BTC", InstrumentExternalID: &inst, AssetClass: canonical.AssetClassCrypto,
						Vehicle: canonical.VehiclePhysical, Currency: "USD", Quantity: &q, MarketValue: decp(s.val)})
				}
				return w.InsertPositions(ctx, pos)
			})
			seedHoldingMove(t, moved, ctx, "ct", "COLD", "in", d1, canonical.TxKindTransferIn, "BTC", "1", 30000)

			kept, _ := openMigrated(t)
			seedReturnsSource(t, kept, ctx, "ct", "cointracking")
			seedAcct(t, kept, ctx, "ct", "R", canonical.AccountKindCrypto, nil,
				[]snap{{d0, 40000}, {d1, 40000}, {d2, 10000 + tc.v2}, {d3, 13000 + tc.v2}}, nil)

			global := func(db *sql.DB) ReturnRow {
				t.Helper()
				rows, err := RunReturns(ctx, db, params("global", 0, end))
				if err != nil {
					t.Fatal(err)
				}
				g, _ := summaryFor(rows, "")
				return g
			}
			got, want := global(moved), global(kept)
			if tc.byValue {
				if nf := netFlowOf(t, []ReturnRow{got}, ""); nf != float64(tc.v2-30000) {
					t.Errorf("net flow = %v, want the first value less the leg's value", nf)
				}
				return
			}
			if got.TWR == nil || want.TWR == nil || math.Abs(*got.TWR-*want.TWR) > 1e-9 {
				t.Errorf("TWR = %s, want %s as if the coin never moved", rateText(got.TWR), rateText(want.TWR))
			}
			if nf := netFlowOf(t, []ReturnRow{got}, ""); nf != 0 {
				t.Errorf("net flow = %v, want none: the move books no capital", nf)
			}
		})
	}
}

// TestInKindFundingByValueKeepsCapitalOut pins that a leg funds a debut by
// quantity only where the wallet's first valuation shows it held: the same
// holding, the quantity moved within the tolerance. Otherwise it funds by its
// value, and a first value below it books the difference as capital that
// left, as the value netter always has: when the first valuation also holds
// cash, so its holdings are not read, and when it holds a ten-thousandth of
// what moved in.
func TestInKindFundingByValueKeepsCapitalOut(t *testing.T) {
	t.Parallel()
	d0, d1, d3 := dy(2024, time.January, 2), dy(2024, time.February, 1), dy(2024, time.June, 3)
	for _, tc := range []struct {
		name     string
		held     string // the coin the first valuation holds
		v1, cash int64  // its value then, and the cash beside it
	}{{"cash beside the coin", "1", 20000, 1000}, {"a ten-thousandth of what moved", "0.0001", 3, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			end := eod(2024, time.June, 3)
			db, ctx := openMigrated(t)
			seedReturnsSource(t, db, ctx, "ct", "cointracking")
			seedAcct(t, db, ctx, "ct", "EX", canonical.AccountKindCrypto, nil,
				[]snap{{d0, 40000}, {d1, 10000}, {d3, 10000}}, nil)
			seedHoldingMove(t, db, ctx, "ct", "EX", "out", d1, canonical.TxKindTransferOut, "BTC", "-1", -30000)
			inst, usd := "BTC", "USD"
			q, _ := canonical.NewDecimalFromString(tc.held)
			inTx(t, db, ctx, func(w *Writer) error {
				if err := w.UpsertAccounts(ctx, []canonical.AccountChange{{SilverSourceID: "ct", AccountExternalID: "COLD",
					AccountKind: canonical.AccountKindCrypto, BaseCurrency: &usd, FirstSeenAt: d1, LastSeenAt: d3}}); err != nil {
					return err
				}
				var pos []canonical.PositionChange
				var cash []canonical.CashBalanceChange
				for _, at := range []int64{d1, d3} {
					pos = append(pos, canonical.PositionChange{SilverSourceID: "ct", SnapshotAt: at, AccountExternalID: "COLD",
						PositionKey: "BTC", InstrumentExternalID: &inst, AssetClass: canonical.AssetClassCrypto,
						Vehicle: canonical.VehiclePhysical, Currency: "USD", Quantity: &q, MarketValue: decp(tc.v1)})
					if tc.cash > 0 {
						cash = append(cash, canonical.CashBalanceChange{SilverSourceID: "ct", SnapshotAt: at, AccountExternalID: "COLD",
							Currency: "USD", BalanceKind: canonical.BalanceKindCurrent, Amount: canonical.NewDecimalFromInt(tc.cash)})
					}
				}
				if err := w.InsertPositions(ctx, pos); err != nil {
					return err
				}
				if len(cash) == 0 {
					return nil
				}
				return w.InsertCashBalances(ctx, cash)
			})
			seedHoldingMove(t, db, ctx, "ct", "COLD", "in", d1, canonical.TxKindTransferIn, "BTC", "1", 30000)

			rows, err := RunReturns(ctx, db, params("global", 0, end))
			if err != nil {
				t.Fatal(err)
			}
			if nf, want := netFlowOf(t, rows, ""), float64(tc.v1+tc.cash-30000); nf != want {
				t.Errorf("net flow = %v, want %v: the first value less the leg's value", nf, want)
			}
		})
	}
}

// TestHoldingReadsItsAssetClass pins where a leg's asset class comes from:
// the row's own where it states one, else its instrument's, and that only a
// coin pairs within the tolerance: any other class, and one gold does not
// state, pairs on the exact quantity.
func TestHoldingReadsItsAssetClass(t *testing.T) {
	t.Parallel()
	facts := map[string]instrumentFacts{
		acctKey("s", "BTC"): {class: string(canonical.AssetClassCrypto)},
		acctKey("s", "SPX"): {class: string(canonical.AssetClassPublicEquity)},
	}
	for _, tc := range []struct {
		inst, rowClass string
		exact          bool
	}{
		{"BTC", "", false},
		{"SPX", "", true},
		{"BTC", string(canonical.AssetClassPublicEquity), true},
		{"SPX", string(canonical.AssetClassCrypto), false},
		{"NONE", "", true},
	} {
		r := flowTxnRow{src: "s", inst: tc.inst, class: tc.rowClass, qty: 1}
		if got := r.holding(facts).move("k").exact; got != tc.exact {
			t.Errorf("%s with row class %q: exact %t, want %t", tc.inst, tc.rowClass, got, tc.exact)
		}
	}
}
