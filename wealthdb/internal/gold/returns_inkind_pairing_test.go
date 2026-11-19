package gold

import (
	"context"
	"database/sql"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// TestNetLegsPairsInKindByHolding pins the in-kind rule on the netter's
// candidates: a leg that moved a holding pairs by holding and quantity,
// never by value, and a leg of cash by value alone. A coin pairs by the
// coin rule: within two days, short by a fee it allows (1% of the coin
// sent where it has no price, as here, unless the leg states one).
func TestNetLegsPairsInKindByHolding(t *testing.T) {
	t.Parallel()
	type leg struct {
		day     int64
		amount  float64
		holding string // "" for cash
		qty     float64
		usd     float64 // the coin's USD price, 0 for none
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
			[]leg{{10, -1000, "BTC", 1, 0}, {12, 1080, "BTC", 1, 0}}, [][2]int{{1, 0}}, true},
		{"two days apart pairs, the receipt first",
			[]leg{{12, -1000, "BTC", 1, 0}, {10, 1080, "BTC", 1, 0}}, [][2]int{{1, 0}}, true},
		{"three days apart does not",
			[]leg{{10, -1000, "BTC", 1, 0}, {13, 1080, "BTC", 1, 0}}, nil, false},
		{"a fee within 1% of an unpriced coin pairs",
			[]leg{{10, -1000, "BTC", 1, 0}, {10, 950, "BTC", 0.995, 0}}, [][2]int{{1, 0}}, true},
		{"a quantity short by more does not",
			[]leg{{10, -1000, "BTC", 1, 0}, {10, 980, "BTC", 0.98, 0}}, nil, false},
		{"a fee worth under 100 USD pairs, however large a share",
			[]leg{{10, -150, "BTC", 0.005, 30000}, {11, 135, "BTC", 0.0045, 30000}}, [][2]int{{1, 0}}, true},
		{"a fee worth more does not",
			[]leg{{10, -1500, "BTC", 0.05, 30000}, {10, 1350, "BTC", 0.045, 30000}}, nil, false},
		{"a quarter lost is no fee, however cheap the coin",
			[]leg{{10, -10, "TOKEN", 1000, 0.01}, {10, 7, "TOKEN", 700, 0.01}}, nil, false},
		{"more received than sent within 0.1% pairs",
			[]leg{{10, -1000, "BTC", 1, 30000}, {10, 1080, "BTC", 1.0009, 30000}}, [][2]int{{1, 0}}, true},
		{"more received beyond it does not",
			[]leg{{10, -1000, "BTC", 1, 30000}, {10, 1080, "BTC", 1.002, 30000}}, nil, false},
		{"two holdings of one value do not",
			[]leg{{10, -1000, "BTC", 1, 0}, {10, 1000, "ETH", 20, 0}}, nil, false},
		{"a holding and cash of one value do not",
			[]leg{{10, -1000, "BTC", 1, 0}, {10, 1000, "", 0, 0}}, nil, false},
		{"a leg that states no quantity does not",
			[]leg{{10, -1000, "BTC", 0, 0}, {10, 1000, "BTC", 1, 0}}, nil, false},
		{"a leg takes the open leg of the smallest shortfall",
			[]leg{{10, -1000, "BTC", 1, 0}, {10, -990, "BTC", 0.99, 0}, {11, 1100, "BTC", 0.99, 0}},
			[][2]int{{2, 1}}, true},
		{"the latest open leg of one quantity on a tie",
			[]leg{{9, -1000, "BTC", 1, 0}, {10, -990, "BTC", 1, 0}, {11, 1100, "BTC", 1, 0}},
			[][2]int{{2, 1}}, true},
		{"1.0 BTC out, 0.9995 in, a day later at a moved price",
			[]leg{{10, -1000, "BTC", 1, 0}, {11, 1050, "BTC", 0.9995, 0}}, [][2]int{{1, 0}}, true},
		{"100 shares out, 99 in: no pair",
			[]leg{{10, -1000, "SHARE", 100, 0}, {11, 1050, "SHARE", 99, 0}}, nil, false},
		{"100 shares out, 100 in at a moved price",
			[]leg{{10, -1000, "SHARE", 100, 0}, {11, 1050, "SHARE", 100, 0}}, [][2]int{{1, 0}}, true},
		{"shares a week apart pair",
			[]leg{{10, -1000, "SHARE", 100, 0}, {17, 1050, "SHARE", 100, 0}}, [][2]int{{1, 0}}, true},
		{"shares eight days apart do not",
			[]leg{{10, -1000, "SHARE", 100, 0}, {18, 1050, "SHARE", 100, 0}}, nil, false},
		{"100 shares out, 99 in at one value: the value netter may not pair them either",
			[]leg{{10, -1000, "SHARE", 100, 0}, {10, 1000, "SHARE", 99, 0}}, nil, false},
		{"cash pairs by value",
			[]leg{{10, -1000, "", 0, 0}, {12, 1003, "", 0, 0}}, [][2]int{{1, 0}}, false},
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
					a.inKind[id] = heldMove{holding: l.holding, qty: l.qty, exact: l.holding == "SHARE", usd: l.usd}
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
// amt dollars. The row states its asset class and vehicle: a coin held
// physical for BTC and ETH, a stock otherwise.
func seedHoldingMove(t *testing.T, db *sql.DB, ctx context.Context, src, acct, id string, at int64, kind canonical.TxKind, inst string, qty string, amt int64) {
	t.Helper()
	q, err := canonical.NewDecimalFromString(qty)
	if err != nil {
		t.Fatal(err)
	}
	class, vehicle := canonical.AssetClassPublicEquity, canonical.VehicleStock
	if inst == "BTC" || inst == "ETH" {
		class, vehicle = canonical.AssetClassCrypto, canonical.VehiclePhysical
	}
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertTransactions(ctx, []canonical.TransactionChange{{
			SilverSourceID: src, TransactionExternalID: id, OccurredAt: at, AccountExternalID: acct,
			InstrumentExternalID: &inst, Kind: kind, Currency: "USD", NetAmount: decp(amt), Quantity: &q,
			AssetClass: class, Vehicle: vehicle}})
	})
}

// TestRunReturnsPairsInKindByHolding drives the in-kind rule from gold: a
// move of a holding between two accounts nets at the grain that holds both,
// whatever its price did between the legs' days, while two legs of one
// value that moved different holdings stay flows.
func TestRunReturnsPairsInKindByHolding(t *testing.T) {
	t.Parallel()
	a, out, k := dy(2024, time.January, 2), dy(2024, time.March, 1), dy(2024, time.December, 2)
	// Five days on at a moved price, outside the value netter's reach; two
	// days on at a moved price, outside it too; a day on at the same value,
	// inside it.
	late, soon, next := dy(2024, time.March, 6), dy(2024, time.March, 3), dy(2024, time.March, 2)
	for _, tc := range []struct {
		name string
		// The two sources and the instruments their legs name; isin, where
		// set, is the identity gold holds for both.
		srcA, kindA, instA, srcB, kindB, instB, isin string
		in, inAmt                                    int64
		qIn                                          string // the quantity received of the 2 sent
		pairs                                        bool
		price                                        int64 // the coin's USD price, 0 for none
	}{
		{"one coin between two wallets of one source", "ct", "cointracking", "BTC", "ct", "cointracking", "BTC", "", soon, 10600, "2", true, 0},
		{"a coin short by a network fee", "ct", "cointracking", "BTC", "ct", "cointracking", "BTC", "", soon, 10600, "1.99", true, 0},
		{"a coin five days on", "ct", "cointracking", "BTC", "ct", "cointracking", "BTC", "", late, 10600, "2", false, 0},
		{"a coin short by a fee its price allows", "ct", "cointracking", "BTC", "ct", "cointracking", "BTC", "", soon, 10600, "1.95", true, 1000},
		{"the same fee where the coin has no price", "ct", "cointracking", "BTC", "ct", "cointracking", "BTC", "", soon, 10600, "1.95", false, 0},
		{"shares between brokers of one identity", "s1", "schwab", "SPX1", "s2", "ubs", "CH-SPX", "XX0000000001", late, 10600, "2", true, 0},
		{"shares never arrive short", "s1", "schwab", "SPX1", "s2", "ubs", "CH-SPX", "XX0000000001", late, 10600, "1.99", false, 0},
		{"one id at two kinds of source names no one holding", "s1", "schwab", "SPX1", "s2", "ubs", "SPX1", "", next, 10000, "2", false, 0},
		{"two holdings of one value", "ct", "cointracking", "BTC", "ct", "cointracking", "ETH", "", next, 10000, "2", false, 0},
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
			if tc.price > 0 {
				inTx(t, db, ctx, func(w *Writer) error {
					return w.InsertFxRates(ctx, []canonical.FxRateChange{{SilverSourceID: tc.srcA, SnapshotAt: out,
						BaseCurrency: "USD", QuoteCurrency: tc.instA, MidRate: canonical.NewDecimalFromInt(tc.price)}})
				})
			}
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
// the coin rule at the coin's price on the funding leg's day, the legs are
// not one unit of one holding, and the leg funds the debut by its value.
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
		price   int64 // the coin's USD price on the move's day, 0 for none
	}{{"the price falls", "1", 21000, false, 0}, {"a fee in kind and a price fall", "0.9995", 19990, false, 0},
		{"three hundredths short of what moved in", "0.97", 20370, true, 0},
		{"twice the quantity moved in", "2", 42000, true, 0},
		{"half a percent short where the coin has no price", "0.995", 20900, false, 0},
		{"half a percent short, beyond the fee its price allows", "0.995", 20900, true, 30000}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			end := eod(2024, time.June, 3)
			moved, ctx := openMigrated(t)
			seedReturnsSource(t, moved, ctx, "ct", "cointracking")
			if tc.price > 0 {
				inTx(t, moved, ctx, func(w *Writer) error {
					return w.InsertFxRates(ctx, []canonical.FxRateChange{{SilverSourceID: "ct", SnapshotAt: d1,
						BaseCurrency: "USD", QuoteCurrency: "BTC", MidRate: canonical.NewDecimalFromInt(tc.price)}})
				})
			}
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
// holding, the quantity moved within the coin rule. Otherwise it funds by its
// value, and a first value below it books the difference as capital that
// left, as the value netter does: when the first valuation also holds
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

// TestHoldingReadsItsAssetClass pins where a leg's asset class and vehicle
// come from, each the row's own where it states one, else its instrument's,
// and that only a coin, class crypto held physical, pairs by the coin
// rule: a share of a fund that holds coins, any other class, and a class
// or vehicle gold does not state pair on the exact quantity.
func TestHoldingReadsItsAssetClass(t *testing.T) {
	t.Parallel()
	crypto, physical := string(canonical.AssetClassCrypto), string(canonical.VehiclePhysical)
	facts := map[string]instrumentFacts{
		acctKey("s", "BTC"):  {class: crypto, vehicle: physical},
		acctKey("s", "BETF"): {class: crypto, vehicle: string(canonical.VehicleETF)},
		acctKey("s", "BX"):   {class: crypto},
		acctKey("s", "SPX"):  {class: string(canonical.AssetClassPublicEquity), vehicle: string(canonical.VehicleStock)},
	}
	for _, tc := range []struct {
		inst, rowClass, rowVehicle string
		exact                      bool
	}{
		{"BTC", "", "", false},
		{"BETF", "", "", true},
		{"BX", "", "", true},
		{"BX", "", physical, false},
		{"SPX", "", "", true},
		{"BTC", string(canonical.AssetClassPublicEquity), "", true},
		{"SPX", crypto, physical, false},
		{"NONE", "", "", true},
	} {
		r := flowTxnRow{src: "s", inst: tc.inst, class: tc.rowClass, vehicle: tc.rowVehicle, qty: 1}
		if got := r.holding(facts).move("k").exact; got != tc.exact {
			t.Errorf("%s with row class %q, vehicle %q: exact %t, want %t", tc.inst, tc.rowClass, tc.rowVehicle, got, tc.exact)
		}
	}
}

// TestStoredSecurityPairLeavesItsRestUnpaired pins that a stored pair of a
// security in unequal quantities pairs what both legs hold: 100 shares
// leave A and 99 land in B, linked. The 99 stay inside the household, the
// 100th leaves it, a flow at its share of the leg's value, counted unmatched.
// A coin short within the coin rule pairs whole: the shortfall is a fee.
// So does a pair whose legs name two holdings, whose quantities say nothing
// of each other.
func TestStoredSecurityPairLeavesItsRestUnpaired(t *testing.T) {
	t.Parallel()
	a, out, in, k := dy(2024, time.January, 2), dy(2024, time.March, 1), dy(2024, time.March, 2), dy(2024, time.December, 2)
	for _, tc := range []struct {
		name, inst, instIn, qOut, qIn string
		inAmt                         int64
		flow                          float64 // the global flow the rest books
	}{
		{"shares", "SPX1", "SPX1", "-100", "99", 9900, -100},
		{"a coin within the coin rule", "BTC", "BTC", "-100", "99", 9900, 0},
		{"more shares received", "SPX1", "SPX1", "-99", "100", 10100, 101},
		{"two holdings", "SPX1", "SPX2", "-100", "1000", 9900, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := openMigrated(t)
			seedReturnsSource(t, db, ctx, "s1", "schwab")
			seedAcct(t, db, ctx, "s1", "A", canonical.AccountKindBrokerage, nil, []snap{{a, 30000}, {out, 20000}, {k, 20000}}, nil)
			seedAcct(t, db, ctx, "s1", "B", canonical.AccountKindBrokerage, nil,
				[]snap{{a, 5000}, {in, 5000 + tc.inAmt}, {k, 5000 + tc.inAmt}}, nil)
			seedHoldingMove(t, db, ctx, "s1", "A", "out", out, canonical.TxKindTransferOut, tc.inst, tc.qOut, -10000)
			seedHoldingMove(t, db, ctx, "s1", "B", "in", in, canonical.TxKindTransferIn, tc.instIn, tc.qIn, tc.inAmt)
			seedTransferLink(t, db, ctx, "s1", "out", "s1", "in")
			flows, tags := globalFlows(t, db, ctx, tmParams("global", 0, eod(2024, time.December, 2)))
			if got := sumFlows(flows); math.Abs(got-tc.flow) > 1e-9 {
				t.Errorf("global flows %v sum to %v, want %v", flows, got, tc.flow)
			}
			wantTags := []string{"cross_source_netted=1"}
			if tc.flow != 0 {
				wantTags = append(wantTags, "unmatched_transfers=1")
			}
			if !slices.Equal(tags, wantTags) {
				t.Errorf("tags %v, want %v", tags, wantTags)
			}
		})
	}
}

// TestStoredPairRestFundsNoDebut pins that the rest of a stored pair funds
// no debut by quantity: 99 shares leave A, linked to 100 that land in B on
// its debut. The 99 fund B's first value by their value; the 100th is new
// to the household, and the onboarding books it.
func TestStoredPairRestFundsNoDebut(t *testing.T) {
	t.Parallel()
	a, out, in, k := dy(2024, time.January, 2), dy(2024, time.March, 1), dy(2024, time.March, 2), dy(2024, time.December, 2)
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "s1", "schwab")
	seedAcct(t, db, ctx, "s1", "A", canonical.AccountKindBrokerage, nil, []snap{{a, 30000}, {out, 20100}, {k, 20100}}, nil)
	inst, usd := "SPX1", "USD"
	q := canonical.NewDecimalFromInt(100)
	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.UpsertAccounts(ctx, []canonical.AccountChange{{SilverSourceID: "s1", AccountExternalID: "B",
			AccountKind: canonical.AccountKindBrokerage, BaseCurrency: &usd, FirstSeenAt: in, LastSeenAt: k}}); err != nil {
			return err
		}
		var pos []canonical.PositionChange
		for _, at := range []int64{in, k} {
			pos = append(pos, canonical.PositionChange{SilverSourceID: "s1", SnapshotAt: at, AccountExternalID: "B",
				PositionKey: inst, InstrumentExternalID: &inst, AssetClass: canonical.AssetClassPublicEquity,
				Vehicle: canonical.VehicleStock, Currency: "USD", Quantity: &q, MarketValue: decp(10000)})
		}
		return w.InsertPositions(ctx, pos)
	})
	seedHoldingMove(t, db, ctx, "s1", "A", "out", out, canonical.TxKindTransferOut, inst, "-99", -9900)
	seedHoldingMove(t, db, ctx, "s1", "B", "in", in, canonical.TxKindTransferIn, inst, "100", 10000)
	seedTransferLink(t, db, ctx, "s1", "out", "s1", "in")
	flows, tags := globalFlows(t, db, ctx, tmParams("global", 0, eod(2024, time.December, 2)))
	if got := sumFlows(flows); math.Abs(got-100) > 1e-9 {
		t.Errorf("global flows %v sum to %v, want the 100th share's 100 onboarded", flows, got)
	}
	if want := []string{"cross_source_netted=1", "unmatched_transfers=1"}; !slices.Equal(tags, want) {
		t.Errorf("tags %v, want %v", tags, want)
	}
}

// TestDebutHoldingsNeedOneCurrencyAndValues pins that a first valuation's
// holdings are read only where every position states a value, all in one
// currency: a share of the value means nothing otherwise.
func TestDebutHoldingsNeedOneCurrencyAndValues(t *testing.T) {
	t.Parallel()
	d1 := dy(2024, time.March, 1)
	for _, tc := range []struct {
		name, ccy string // the second position's currency
		value     int64  // and its market value
		read      bool
	}{
		{"one currency", "USD", 100, true},
		{"two currencies", "EUR", 100, false},
		{"a position of no value", "USD", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := openMigrated(t)
			seedReturnsSource(t, db, ctx, "ct", "cointracking")
			q := canonical.NewDecimalFromInt(1)
			inTx(t, db, ctx, func(w *Writer) error {
				var pos []canonical.PositionChange
				for _, p := range []struct {
					inst, ccy string
					value     int64
				}{{"BTC", "USD", 100}, {"ETH", tc.ccy, tc.value}} {
					inst := p.inst
					pos = append(pos, canonical.PositionChange{SilverSourceID: "ct", SnapshotAt: d1, AccountExternalID: "COLD",
						PositionKey: inst, InstrumentExternalID: &inst, AssetClass: canonical.AssetClassCrypto,
						Vehicle: canonical.VehiclePhysical, Currency: p.ccy, Quantity: &q, MarketValue: decp(p.value)})
				}
				return w.InsertPositions(ctx, pos)
			})
			a := &accountData{src: "ct", acct: "COLD", srcKind: "cointracking", series: []dayVal{{EpochDay(d1), 1}},
				transferLike: []returns.Flow{{Day: EpochDay(d1), Amount: 1, ID: "in"}},
				inKind:       map[string]heldMove{"in": {holding: "BTC", qty: 1}}}
			if err := loadDebutHoldings(ctx, db, nil, newHoldingUnion(), map[string]map[string]*accountData{"USD": {"COLD": a}}); err != nil {
				t.Fatal(err)
			}
			if read := len(a.debut) == 2; read != tc.read || (!read && a.debut != nil) {
				t.Errorf("debut %v: read %t, want %t", a.debut, read, tc.read)
			}
		})
	}
}

// TestDebutHoldingsReadEachDebutDay pins that each currency's account reads
// the holdings of its own debut day, where FX starts a day later in one
// currency: the day's positions, and the cash of that day alone.
func TestDebutHoldingsReadEachDebutDay(t *testing.T) {
	t.Parallel()
	d1, d2 := dy(2024, time.March, 1), dy(2024, time.March, 2)
	for _, tc := range []struct {
		name   string
		cashOn int64 // the day cash sits beside the coin, 0 for none
		// The coin each debut reads, 0 where it reads no holdings.
		want1, want2 float64
	}{
		{"no cash", 0, 1, 2},
		{"cash on the earlier day", d1, 0, 2},
		{"cash on the later day", d2, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := openMigrated(t)
			seedReturnsSource(t, db, ctx, "ct", "cointracking")
			inst, usd := "BTC", "USD"
			inTx(t, db, ctx, func(w *Writer) error {
				if err := w.UpsertAccounts(ctx, []canonical.AccountChange{{SilverSourceID: "ct", AccountExternalID: "COLD",
					AccountKind: canonical.AccountKindCrypto, BaseCurrency: &usd, FirstSeenAt: d1, LastSeenAt: d2}}); err != nil {
					return err
				}
				var pos []canonical.PositionChange
				for i, at := range []int64{d1, d2} {
					q := canonical.NewDecimalFromInt(int64(i + 1))
					pos = append(pos, canonical.PositionChange{SilverSourceID: "ct", SnapshotAt: at, AccountExternalID: "COLD",
						PositionKey: "BTC", InstrumentExternalID: &inst, AssetClass: canonical.AssetClassCrypto,
						Vehicle: canonical.VehiclePhysical, Currency: "USD", Quantity: &q, MarketValue: decp(int64(100 * (i + 1)))})
				}
				if err := w.InsertPositions(ctx, pos); err != nil {
					return err
				}
				if tc.cashOn == 0 {
					return nil
				}
				return w.InsertCashBalances(ctx, []canonical.CashBalanceChange{{SilverSourceID: "ct", SnapshotAt: tc.cashOn,
					AccountExternalID: "COLD", Currency: "USD", BalanceKind: canonical.BalanceKindCurrent,
					Amount: canonical.NewDecimalFromInt(50)}})
			})
			// The account in two currencies, its value starting a day apart,
			// a coin moved in on each debut.
			acct := func(first int64) *accountData {
				return &accountData{src: "ct", acct: "COLD", srcKind: "cointracking", series: []dayVal{{first, 1}},
					transferLike: []returns.Flow{{Day: first, Amount: 1, ID: "in"}},
					inKind:       map[string]heldMove{"in": {holding: "BTC", qty: 1}}}
			}
			early, late := acct(EpochDay(d1)), acct(EpochDay(d2))
			byCcy := map[string]map[string]*accountData{"USD": {"COLD": early}, "CHF": {"COLD": late}}
			if err := loadDebutHoldings(ctx, db, nil, newHoldingUnion(), byCcy); err != nil {
				t.Fatal(err)
			}
			h := newHoldingUnion().key(holdingIdentityOf("cointracking", inst, instrumentFacts{}))
			for _, c := range []struct {
				a    *accountData
				want float64
			}{{early, tc.want1}, {late, tc.want2}} {
				if got := c.a.debut[h].qty; got != c.want || (c.want == 0) != (c.a.debut == nil) {
					t.Errorf("debut on day %d = %v, want %v of the coin", c.a.firstDay(), c.a.debut, c.want)
				}
			}
		})
	}
}

// TestRunReturnsPairsWhereAnyIdentityAgrees pins that returns finds one
// holding where any identity agrees: two sources of one kind name the
// shares by one id, and only the sender's instrument states an ISIN. The
// legs pair, five days apart at a moved price.
func TestRunReturnsPairsWhereAnyIdentityAgrees(t *testing.T) {
	t.Parallel()
	a, out, in, k := dy(2024, time.January, 2), dy(2024, time.March, 1), dy(2024, time.March, 6), dy(2024, time.December, 2)
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "s1", "schwab")
	seedReturnsSource(t, db, ctx, "s2", "schwab")
	seedAcct(t, db, ctx, "s1", "A", canonical.AccountKindBrokerage, nil, []snap{{a, 30000}, {out, 20000}, {k, 20000}}, nil)
	seedAcct(t, db, ctx, "s2", "B", canonical.AccountKindBrokerage, nil, []snap{{a, 5000}, {in, 15600}, {k, 15600}}, nil)
	seedHoldingMove(t, db, ctx, "s1", "A", "out", out, canonical.TxKindTransferOut, "SPX1", "-2", -10000)
	seedHoldingMove(t, db, ctx, "s2", "B", "in", in, canonical.TxKindTransferIn, "SPX1", "2", 10600)
	isin := "XX0000000001"
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertInstruments(ctx, []canonical.InstrumentChange{{SilverSourceID: "s1", InstrumentExternalID: "SPX1",
			AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock, ISIN: &isin, FirstSeenAt: a, LastSeenAt: k}})
	})
	if flows, tags := globalFlows(t, db, ctx, params("global", 0, eod(2024, time.December, 2))); len(flows) != 0 || len(tags) != 0 {
		t.Errorf("flows %v, tags %v: want the move inside", flows, tags)
	}
}

// TestAFiatInstrumentNetsAsCash: a crypto tracker's leg in a fiat currency
// names that currency as its instrument, of class cash. It is money, not a
// holding moved in kind, so it nets by value as cash does: between two
// wallets short by a fee, and with a bank's cash leg.
func TestAFiatInstrumentNetsAsCash(t *testing.T) {
	t.Parallel()
	a, out, in, k := dy(2024, time.January, 2), dy(2024, time.March, 1), dy(2024, time.March, 2), dy(2024, time.December, 2)
	for _, tc := range []struct {
		name     string
		bankSide bool // the money leaves a bank's cash, not a wallet
	}{{"between two wallets, short by a fee", false}, {"from a bank's cash", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := openMigrated(t)
			seedReturnsSource(t, db, ctx, "ct", "cointracking")
			fiat := func(acct, id string, at int64, kind canonical.TxKind, qty string, amt int64) {
				q, err := canonical.NewDecimalFromString(qty)
				if err != nil {
					t.Fatal(err)
				}
				inst := "EUR"
				inTx(t, db, ctx, func(w *Writer) error {
					return w.InsertTransactions(ctx, []canonical.TransactionChange{{
						SilverSourceID: "ct", TransactionExternalID: id, OccurredAt: at, AccountExternalID: acct,
						InstrumentExternalID: &inst, Kind: kind, Currency: "USD", NetAmount: decp(amt), Quantity: &q,
						AssetClass: canonical.AssetClassCash, Vehicle: canonical.VehicleDemandDeposit}})
				})
			}
			if tc.bankSide {
				seedReturnsSource(t, db, ctx, "bk", "schwab")
				seedAcct(t, db, ctx, "bk", "A", canonical.AccountKindBrokerage, nil,
					[]snap{{a, 30000}, {out, 29000}, {k, 29000}}, []txn{{out, canonical.TxKindTransferOut, -1000}})
			} else {
				seedAcct(t, db, ctx, "ct", "A", canonical.AccountKindCrypto, nil,
					[]snap{{a, 30000}, {out, 29000}, {k, 29000}}, nil)
				fiat("A", "out", out, canonical.TxKindTransferOut, "-1000", -1000)
			}
			seedAcct(t, db, ctx, "ct", "B", canonical.AccountKindCrypto, nil,
				[]snap{{a, 5000}, {in, 5998}, {k, 5998}}, nil)
			fiat("B", "in", in, canonical.TxKindTransferIn, "998", 998)
			if flows, tags := globalFlows(t, db, ctx, params("global", 0, eod(2024, time.December, 2))); len(flows) != 0 || len(tags) != 0 {
				t.Errorf("flows %v, tags %v: want the move inside, its fee a loss", flows, tags)
			}
		})
	}
}
