package gold

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// seedDebtPosition upserts a mortgage held as one position in ccy, the
// shape a mortgage has in gold. All values synthetic.
func seedDebtPosition(t *testing.T, db *sql.DB, ctx context.Context, src, acct, ccy string, snaps []snap) {
	t.Helper()
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{
			SilverSourceID: src, AccountExternalID: acct, AccountKind: canonical.AccountKindMortgage,
			BaseCurrency: &ccy, FirstSeenAt: snaps[0].at, LastSeenAt: snaps[len(snaps)-1].at,
		}})
	})
	inTx(t, db, ctx, func(w *Writer) error {
		var pos []canonical.PositionChange
		for _, s := range snaps {
			pos = append(pos, canonical.PositionChange{
				SilverSourceID: src, SnapshotAt: s.at, AccountExternalID: acct, PositionKey: "M",
				AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock,
				Currency: ccy, MarketValue: decp(s.val),
			})
		}
		return w.InsertPositions(ctx, pos)
	})
}

// monthEnds are the fixture's snapshot days: December 2023 to June 2024.
func monthEnds() []int64 {
	return []int64{dy(2023, time.December, 31), dy(2024, time.January, 31), dy(2024, time.February, 29),
		dy(2024, time.March, 31), dy(2024, time.April, 30), dy(2024, time.May, 31), dy(2024, time.June, 30)}
}

func snapsOf(ends []int64, vals ...int64) []snap {
	out := make([]snap, len(vals))
	for i, v := range vals {
		out[i] = snap{ends[len(ends)-len(vals)+i], v}
	}
	return out
}

// seedBridgeHousehold is one invented household, month by month:
//
//	January   a salary of 2000 arrives, and 300 goes on the card
//	February  the card bill of 300 is paid
//	March     a mortgage instalment of 1000, 700 of it principal
//	April     a new loan of 5000 arrives as cash
//	May       a mortgage of 20000 is first observed, with no cash beside it
//	June      a brokerage account joins, holding 3000
//
// The brokerage gains 200, 100, 300, 0, 200 and 200. mortgageCcy is the
// first mortgage's currency.
func seedBridgeHousehold(t *testing.T, db *sql.DB, ctx context.Context, mortgageCcy string) {
	t.Helper()
	seedReturnsSource(t, db, ctx, "bank", "schwab")
	e := monthEnds()
	seedAcct(t, db, ctx, "bank", "BROK", canonical.AccountKindBrokerage, nil,
		snapsOf(e, 10000, 10200, 10300, 10600, 10600, 10800, 11000), nil)
	seedCashOnlyAcct(t, db, ctx, "bank", "CHK", canonical.AccountKindCash,
		snapsOf(e, 5000, 7000, 6700, 5700, 10700, 10700, 10700), []txn{
			{dy(2024, time.January, 15), canonical.TxKindDeposit, 2000},
			{dy(2024, time.February, 10), canonical.TxKindWithdrawal, -300},
			{dy(2024, time.March, 10), canonical.TxKindWithdrawal, -1000},
			{dy(2024, time.April, 15), canonical.TxKindDeposit, 5000},
		})
	seedCashOnlyAcct(t, db, ctx, "bank", "CARD", canonical.AccountKindCard,
		snapsOf(e, -100, -400, -100, -100, -100, -100, -100), nil)
	seedDebtPosition(t, db, ctx, "bank", "MORT", mortgageCcy,
		snapsOf(e, -50000, -50000, -50000, -49300, -49300, -49300, -49300))
	seedDebtPosition(t, db, ctx, "bank", "LOAN", "USD", []snap{
		{dy(2024, time.April, 15), -5000}, {e[4], -5000}, {e[5], -5000}, {e[6], -5000}})
	seedDebtPosition(t, db, ctx, "bank", "SEEN", "USD", []snap{
		{dy(2024, time.May, 20), -20000}, {e[5], -20000}, {e[6], -20000}})
	seedAcct(t, db, ctx, "bank", "LATE", canonical.AccountKindBrokerage, nil, []snap{
		{dy(2024, time.June, 10), 3000}, {e[6], 3000}}, nil)
	for i, day := range e {
		seedFX(t, db, day, "USD", "CHF", fmt.Sprintf("%.2f", 0.90-0.02*float64(i)))
		seedFX(t, db, day, "USD", "EUR", "0.95")
		seedFX(t, db, day, "USD", "GBP", "0.80")
	}
}

func bridgeParams(ccy, period string) ReturnParams {
	return ReturnParams{OutCcy: ccy, Period: period, ToEpoch: dy(2024, time.June, 30) + 43199}
}

func cents(t *testing.T, s *string) int64 {
	t.Helper()
	if s == nil {
		t.Fatalf("nil money")
	}
	f, err := strconv.ParseFloat(*s, 64)
	if err != nil {
		t.Fatalf("money %q: %v", *s, err)
	}
	return int64(math.Round(f * 100))
}

func hasTag(r BridgeRow, tag string) bool { return slices.Contains(r.Quality, tag) }

func runBridge(t *testing.T, db *sql.DB, ctx context.Context, p ReturnParams) []BridgeRow {
	t.Helper()
	rows, err := RunBridge(ctx, db, p)
	if err != nil {
		t.Fatalf("RunBridge: %v", err)
	}
	return rows
}

// checkIdentity holds every row to the bridge's two sums, to the cent.
func checkIdentity(t *testing.T, label string, rows []BridgeRow) {
	t.Helper()
	for _, r := range rows {
		start, end := cents(t, r.StartNetWorth), cents(t, r.EndNetWorth)
		saved, onboarded, gain := cents(t, r.NetSaved), cents(t, r.Onboarded), cents(t, r.Gain)
		if end != start+saved+onboarded+gain {
			t.Errorf("%s %s: %d != %d + %d + %d + %d", label, r.Period, end, start, saved, onboarded, gain)
		}
		if saved != cents(t, r.NetFlow)+cents(t, r.DebtRepaid) {
			t.Errorf("%s %s: net_saved %d is not net_flow + debt_repaid", label, r.Period, saved)
		}
	}
}

func TestBridgeReadsTheCommonCases(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedBridgeHousehold(t, db, ctx, "USD")
	rows := runBridge(t, db, ctx, bridgeParams("USD", "monthly"))
	type want struct {
		flow, repaid, onboarded, gain int64 // whole units
		tag                           string
	}
	cases := map[string]want{
		// The card purchase reads as money spent.
		"2024-01": {2000, -300, 0, 200, ""},
		// The bill's payment leaves as a flow and comes back: the two cancel.
		"2024-02": {-300, 300, 0, 100, ""},
		// The instalment's principal comes back; its interest stays spent.
		"2024-03": {-1000, 700, 0, 300, ""},
		// A new loan: its cash and its debt cancel, and no tag.
		"2024-04": {5000, -5000, 0, 0, ""},
		// A debt first seen with no cash reads as a fall in net_saved once.
		"2024-05": {0, -20000, 0, 200, "debt_first_seen"},
		// An account that joins is onboarded, neither saved nor gained.
		"2024-06": {0, 0, 3000, 200, ""},
	}
	if len(rows) != len(cases) {
		t.Fatalf("rows = %d, want %d: %+v", len(rows), len(cases), rows)
	}
	checkIdentity(t, "USD", rows)
	for _, r := range rows {
		w, ok := cases[r.Period]
		if !ok {
			t.Fatalf("no case for %s", r.Period)
		}
		got := [4]int64{cents(t, r.NetFlow), cents(t, r.DebtRepaid), cents(t, r.Onboarded), cents(t, r.Gain)}
		if got != [4]int64{w.flow * 100, w.repaid * 100, w.onboarded * 100, w.gain * 100} {
			t.Errorf("%s: flow, repaid, onboarded, gain = %v, want %+v", r.Period, got, w)
		}
		if w.tag != "" && !hasTag(r, w.tag) {
			t.Errorf("%s: quality %v lacks %s", r.Period, r.Quality, w.tag)
		}
		if r.Period == "2024-04" && hasTag(r, "debt_first_seen") {
			t.Errorf("a loan with its cash beside it is tagged: %v", r.Quality)
		}
		if cents(t, r.Gap) != 0 {
			t.Errorf("%s: net worth less returns' value is not the debts, by %s", r.Period, *r.Gap)
		}
	}
}

// Every reporting currency holds the identity in every bucket, with a
// mortgage in another currency, and months sum to quarters and years.
func TestBridgeIdentityAndSumsInEveryCurrency(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedBridgeHousehold(t, db, ctx, "CHF")
	for _, ccy := range materializeCurrencies {
		months := runBridge(t, db, ctx, bridgeParams(ccy, "monthly"))
		checkIdentity(t, ccy, months)
		for i := 1; i < len(months); i++ {
			if *months[i].StartNetWorth != *months[i-1].EndNetWorth {
				t.Errorf("%s %s starts at %s, the month before ends at %s", ccy, months[i].Period,
					*months[i].StartNetWorth, *months[i-1].EndNetWorth)
			}
		}
		for _, period := range []string{"quarterly", "annual", "total"} {
			rows := runBridge(t, db, ctx, bridgeParams(ccy, period))
			checkIdentity(t, ccy+" "+period, rows)
			var saved, onboarded, gain, flow, repaid [2]int64
			for _, m := range months {
				saved[0] += cents(t, m.NetSaved)
				onboarded[0] += cents(t, m.Onboarded)
				gain[0] += cents(t, m.Gain)
				flow[0] += cents(t, m.NetFlow)
				repaid[0] += cents(t, m.DebtRepaid)
			}
			for _, r := range rows {
				saved[1] += cents(t, r.NetSaved)
				onboarded[1] += cents(t, r.Onboarded)
				gain[1] += cents(t, r.Gain)
				flow[1] += cents(t, r.NetFlow)
				repaid[1] += cents(t, r.DebtRepaid)
			}
			for name, pair := range map[string][2]int64{"net_saved": saved, "onboarded": onboarded,
				"gain": gain, "net_flow": flow, "debt_repaid": repaid} {
				if pair[0] != pair[1] {
					t.Errorf("%s %s: %s months %d, buckets %d", ccy, period, name, pair[0], pair[1])
				}
			}
			if *rows[len(rows)-1].EndNetWorth != *months[len(months)-1].EndNetWorth {
				t.Errorf("%s %s ends elsewhere than the months", ccy, period)
			}
		}
	}
}

// gain is returns' gain plus the FX move on debt: what each line owed at the
// month's start, valued at the old rate less the new. A currency with no
// debt in another currency has no such move.
func TestBridgeGainIsReturnsGainPlusFXOnDebt(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedBridgeHousehold(t, db, ctx, "CHF")
	debts, err := loadDebtLines(ctx, db)
	if err != nil {
		t.Fatalf("loadDebtLines: %v", err)
	}
	for _, ccy := range materializeCurrencies {
		rows := runBridge(t, db, ctx, bridgeParams(ccy, "monthly"))
		rates, err := loadDebtRates(ctx, db, ccy)
		if err != nil {
			t.Fatalf("loadDebtRates: %v", err)
		}
		ret, err := RunReturns(ctx, db, ReturnParams{Level: "global", ToEpoch: dy(2024, time.June, 30) + 43199,
			OutCcy: ccy, Method: "twr", Period: "monthly", Annualize: "never", Netting: true, Inception: "full"})
		if err != nil {
			t.Fatalf("RunReturns: %v", err)
		}
		byEnd := map[int64]ReturnRow{}
		for _, r := range ret {
			if !r.IsSummary {
				byEnd[r.EndDay] = r
			}
		}
		for _, b := range rows {
			r, ok := byEnd[b.EndDay]
			if !ok {
				t.Fatalf("%s %s: no returns row", ccy, b.Period)
			}
			var move float64
			for _, d := range debts {
				move += d.owedAt(b.StartDay) * (rates[d.ccy][b.StartDay] - rates[d.ccy][b.EndDay])
			}
			want := cents(t, r.Gain()) + toCents(move)
			// Each of the bridge's four roundings can move it by half a cent.
			if got := cents(t, b.Gain); absCents(got-want) > 2 {
				t.Errorf("%s %s: gain %d, returns' gain plus FX on debt %d", ccy, b.Period, got, want)
			}
		}
	}
}

// With every debt in the output currency the two gains are one.
func TestBridgeGainEqualsReturnsGainWithNoForeignDebt(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedBridgeHousehold(t, db, ctx, "USD")
	rows := runBridge(t, db, ctx, bridgeParams("USD", "monthly"))
	ret, err := RunReturns(ctx, db, ReturnParams{Level: "global", ToEpoch: dy(2024, time.June, 30) + 43199,
		OutCcy: "USD", Method: "twr", Period: "monthly", Annualize: "never", Netting: true, Inception: "full"})
	if err != nil {
		t.Fatalf("RunReturns: %v", err)
	}
	seen := 0
	for _, r := range ret {
		if r.IsSummary {
			continue
		}
		for _, b := range rows {
			if b.EndDay == r.EndDay {
				seen++
				if cents(t, b.Gain) != cents(t, r.Gain()) {
					t.Errorf("%s: gain %s, returns' %s", b.Period, *b.Gain, *r.Gain())
				}
				var flow = cents(t, b.NetFlow) + cents(t, b.Onboarded)
				if flow != cents(t, r.NetFlow) {
					t.Errorf("%s: net_flow plus onboarded %d, returns' net flow %s", b.Period, flow, *r.NetFlow)
				}
			}
		}
	}
	if seen != len(rows) {
		t.Fatalf("matched %d of %d months", seen, len(rows))
	}
}

// The edges read what `holdings global` reads.
func TestBridgeNetWorthIsHoldingsGlobal(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedBridgeHousehold(t, db, ctx, "CHF")
	for _, ccy := range []string{"USD", "CHF"} {
		for _, r := range runBridge(t, db, ctx, bridgeParams(ccy, "monthly")) {
			g, err := GlobalAsOf(ctx, db, r.EndDay*SecondsPerDay+SecondsPerDay-1, ccy)
			if err != nil {
				t.Fatalf("GlobalAsOf: %v", err)
			}
			if cents(t, g.TotalValueOutCcy) != cents(t, r.EndNetWorth) {
				t.Errorf("%s %s: holdings global %s, bridge %s", ccy, r.Period, *g.TotalValueOutCcy, *r.EndNetWorth)
			}
		}
	}
}

// A window selects months of the one series: a month reads the same inside
// any window, and the first row of a window starts where the month before
// ended.
func TestBridgeWindowCutsTheSameMonths(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedBridgeHousehold(t, db, ctx, "USD")
	all := runBridge(t, db, ctx, bridgeParams("USD", "monthly"))
	p := bridgeParams("USD", "monthly")
	p.FromEpoch = time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC).Unix()
	p.ToEpoch = time.Date(2024, time.April, 30, 23, 59, 59, 0, time.UTC).Unix()
	got := runBridge(t, db, ctx, p)
	if len(got) != 2 || got[0].Period != "2024-03" || got[1].Period != "2024-04" {
		t.Fatalf("rows = %+v", got)
	}
	for _, r := range got {
		for _, a := range all {
			if a.Period == r.Period && (*a.Gain != *r.Gain || *a.NetSaved != *r.NetSaved || *a.StartNetWorth != *r.StartNetWorth) {
				t.Errorf("%s differs inside a window: %+v against %+v", r.Period, r, a)
			}
		}
	}
	// A window opening before the data says so on its first row.
	p.FromEpoch = time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC).Unix()
	if rows := runBridge(t, db, ctx, p); !hasTag(rows[0], "partial_window") {
		t.Errorf("quality %v lacks partial_window", rows[0].Quality)
	}
	if rows := runBridge(t, db, ctx, bridgeParams("USD", "total")); !hasTag(rows[0], "since_data_inception") {
		t.Errorf("quality %v lacks since_data_inception", rows[0].Quality)
	}
}

// An account that ends with value: returns books a zeroing on the spine's
// last day as a closure, so it leaves through onboarded, and it reads a
// dropped account's fall as a loss, so that one stays in gain and is tagged.
func TestBridgeAccountThatEndsWithValue(t *testing.T) {
	t.Parallel()
	t.Run("closed on the last day", func(t *testing.T) {
		t.Parallel()
		db, ctx := openMigrated(t)
		seedReturnsSource(t, db, ctx, "bank", "schwab")
		e := monthEnds()
		seedAcct(t, db, ctx, "bank", "KEEP", canonical.AccountKindBrokerage, nil, snapsOf(e, 1000, 1000, 1000, 1000, 1000, 1000, 1000), nil)
		seedAcct(t, db, ctx, "bank", "GONE", canonical.AccountKindBrokerage, nil, snapsOf(e, 500, 500, 500, 500, 500, 500, 0), nil)
		fx, err := loadFxBounds(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		ds, err := loadReturnsDataset(ctx, db, "USD", fx, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		// The spine reaches today; the closure machinery reads its last day.
		cutSpine(ds, EpochDay(e[6]))
		rows := computeBridge(bridgeInput{ds: ds}, bridgeParams("USD", "monthly"))
		checkIdentity(t, "closed", rows)
		last := rows[len(rows)-1]
		if cents(t, last.Onboarded) != -50000 || cents(t, last.Gain) != 0 {
			t.Errorf("onboarded %s, gain %s, want -500 and 0", *last.Onboarded, *last.Gain)
		}
	})
	t.Run("dropped from its source's snapshots", func(t *testing.T) {
		t.Parallel()
		db, ctx := openMigrated(t)
		seedReturnsSource(t, db, ctx, "bank", "schwab")
		e := monthEnds()
		seedAcct(t, db, ctx, "bank", "KEEP", canonical.AccountKindBrokerage, nil, snapsOf(e, 1000, 1000, 1000, 1000, 1000, 1000, 1000), nil)
		seedAcct(t, db, ctx, "bank", "GONE", canonical.AccountKindBrokerage, nil, []snap{{e[0], 500}, {e[1], 500}, {e[2], 500}}, nil)
		rows := runBridge(t, db, ctx, bridgeParams("USD", "monthly"))
		checkIdentity(t, "dropped", rows)
		var fell bool
		for _, r := range rows {
			if cents(t, r.Gain) == -50000 {
				fell = true
				if cents(t, r.Onboarded) != 0 {
					t.Errorf("%s: onboarded %s, want 0: returns books no flow for a dropped account", r.Period, *r.Onboarded)
				}
				if !strings.Contains(strings.Join(r.Quality, ";"), "dropped_while_nonzero") {
					t.Errorf("%s: quality %v lacks dropped_while_nonzero", r.Period, r.Quality)
				}
			}
		}
		if !fell {
			t.Errorf("no month reads the fall: %+v", rows)
		}
	})
}

// cutSpine ends every series at day, as a gold whose spine stops there.
func cutSpine(ds *returnsDataset, day int64) {
	for _, a := range ds.accts {
		i := len(a.series)
		for i > 0 && a.series[i-1].day > day {
			i--
		}
		a.series = a.series[:i]
	}
	ds.finalize()
}

// A debt that leaves the history while owed reads as repaid, and says so.
func TestBridgeDebtThatEndsWhileOwed(t *testing.T) {
	t.Parallel()
	debts := []*debtLine{{src: "s", acct: "M", ccy: "USD", series: []dayVal{{100, -500}, {110, -500}}}}
	in := bridgeInput{debts: debts, rates: map[string]map[int64]float64{"USD": {120: 1}}}
	repaid, tags := in.debtRepaid(105, 120, nil)
	if repaid != 500 || !slices.Contains(tags, "debt_ended") {
		t.Errorf("repaid %v, tags %v", repaid, tags)
	}
	// Its last row on a month end falls to the next month.
	if _, tags := in.debtRepaid(110, 120, nil); !slices.Contains(tags, "debt_ended") {
		t.Errorf("tags %v", tags)
	}
}

// A debt with no rate to the output currency stays out, and says so.
func TestBridgeDebtWithNoRate(t *testing.T) {
	t.Parallel()
	debts := []*debtLine{{src: "s", acct: "M", ccy: "XXX", series: []dayVal{{100, -500}, {120, -400}}}}
	in := bridgeInput{debts: debts, rates: map[string]map[int64]float64{}}
	repaid, tags := in.debtRepaid(100, 120, nil)
	if repaid != 0 || !slices.Contains(tags, "debt_unpriced") {
		t.Errorf("repaid %v, tags %v", repaid, tags)
	}
}

// A net real flow and a net synthetic flow of opposite signs are one movement:
// the smaller size leaves both, and the sum stays.
func TestTrackingMove(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                  string
		netFlow, onboarded    int64
		wantFlow, wantTracked int64
	}{
		{"a move in the month", -3000, 3000, 0, 0},
		{"a part move", -1000, 3000, 0, 2000},
		{"more left than arrived", -5000, 3000, -2000, 0},
		{"a debut with nothing beside it", 0, 3000, 0, 3000},
		{"a debut beside a net inflow", 1000, 3000, 1000, 3000},
		{"an outflow with no debut", -3000, 0, -3000, 0},
		{"a closure with its money arriving", 3000, -3000, 0, 0},
		{"a closure beside a smaller inflow", 1000, -3000, 0, -2000},
		{"a closure beside a larger inflow", 5000, -3000, 2000, 0},
		{"a closure beside a net outflow", -1000, -3000, -1000, -3000},
	}
	for _, c := range cases {
		moved := trackingMove(c.netFlow, c.onboarded)
		if flow, tracked := c.netFlow+moved, c.onboarded-moved; flow != c.wantFlow || tracked != c.wantTracked {
			t.Errorf("%s: flow %d, onboarded %d, want %d, %d", c.name, flow, tracked, c.wantFlow, c.wantTracked)
		}
	}
}

// A debut and a closure in one month net with each other first, so a closure
// leaves no negative onboarded behind. Its mirror, a closure with its money
// arriving, is one movement too. The spine ends on the closure's day, the one
// day returns books a closure flow.
func TestBridgeClosureInTheMonth(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		flow  int64 // the checking account's real flow in June
		debut bool
	}{
		{"beside a debut", -2500, true},
		{"with its money arriving", 500, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := openMigrated(t)
			seedReturnsSource(t, db, ctx, "bank", "schwab")
			e := monthEnds()
			kind := canonical.TxKindDeposit
			if c.flow < 0 {
				kind = canonical.TxKindWithdrawal
			}
			seedAcct(t, db, ctx, "bank", "KEEP", canonical.AccountKindBrokerage, nil, snapsOf(e, 1000, 1000, 1000, 1000, 1000, 1000, 1000), nil)
			seedAcct(t, db, ctx, "bank", "GONE", canonical.AccountKindBrokerage, nil, snapsOf(e, 500, 500, 500, 500, 500, 500, 0), nil)
			seedCashOnlyAcct(t, db, ctx, "bank", "CHK", canonical.AccountKindCash,
				snapsOf(e, 9000, 9000, 9000, 9000, 9000, 9000, 9000+c.flow), []txn{{dy(2024, time.June, 5), kind, c.flow}})
			if c.debut {
				seedAcct(t, db, ctx, "bank", "NEW", canonical.AccountKindBrokerage, nil, []snap{{dy(2024, time.June, 10), 3000}, {e[6], 3000}}, nil)
			}
			fx, err := loadFxBounds(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			ds, err := loadReturnsDataset(ctx, db, "USD", fx, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			cutSpine(ds, EpochDay(e[6]))
			rows := computeBridge(bridgeInput{ds: ds}, bridgeParams("USD", "monthly"))
			checkIdentity(t, c.name, rows)
			last := rows[len(rows)-1]
			if cents(t, last.NetFlow) != 0 || cents(t, last.Onboarded) != 0 || cents(t, last.Gain) != 0 {
				t.Errorf("flow %s, onboarded %s, gain %s", *last.NetFlow, *last.Onboarded, *last.Gain)
			}
			if !hasTag(last, "tracking_netted") {
				t.Errorf("quality %v", last.Quality)
			}
		})
	}
}

func TestBridgeMoveIntoANewAccount(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name            string
		out             int64
		flow, onboarded int64
		tagged          bool
	}{
		{"the whole debut", 3000, 0, 0, true},
		{"part of the debut", 1000, 0, 2000, true},
		{"nothing beside it", 0, 0, 3000, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db, ctx := openMigrated(t)
			seedReturnsSource(t, db, ctx, "bank", "schwab")
			e := monthEnds()
			var txns []txn
			if c.out != 0 {
				txns = []txn{{dy(2024, time.March, 5), canonical.TxKindWithdrawal, -c.out}}
			}
			seedCashOnlyAcct(t, db, ctx, "bank", "CHK", canonical.AccountKindCash,
				snapsOf(e, 9000, 9000, 9000, 9000-c.out, 9000-c.out, 9000-c.out, 9000-c.out), txns)
			seedAcct(t, db, ctx, "bank", "NEW", canonical.AccountKindBrokerage, nil, []snap{
				{dy(2024, time.March, 20), 3000}, {e[3], 3000}, {e[4], 3000}, {e[5], 3000}, {e[6], 3000}}, nil)
			rows := runBridge(t, db, ctx, bridgeParams("USD", "monthly"))
			checkIdentity(t, c.name, rows)
			for _, r := range rows {
				if r.Period != "2024-03" {
					continue
				}
				if cents(t, r.NetFlow) != c.flow*100 || cents(t, r.Onboarded) != c.onboarded*100 || cents(t, r.Gain) != 0 {
					t.Errorf("flow %s, onboarded %s, gain %s", *r.NetFlow, *r.Onboarded, *r.Gain)
				}
				if hasTag(r, "tracking_netted") != c.tagged {
					t.Errorf("quality %v", r.Quality)
				}
			}
		})
	}
}

// An account returns leaves out opens a gap: the identity still holds, gain
// carries the difference, and the bucket says by how much.
func TestBridgeGapIsTagged(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedBridgeHousehold(t, db, ctx, "USD")
	p := bridgeParams("USD", "monthly")
	p.ReturnsExclude = &ReturnsExclude{Accounts: map[string]map[string]bool{"bank": {"BROK": true}}}
	rows := runBridge(t, db, ctx, p)
	checkIdentity(t, "gap", rows)
	first := rows[0]
	if !hasTag(first, "bridge_gap") {
		t.Errorf("quality %v lacks bridge_gap", first.Quality)
	}
	if cents(t, first.Gap) != 1020000 {
		t.Errorf("gap %s, want the excluded account's 10200", *first.Gap)
	}
}

// The materialized months are the CLI's, and a window's months sum in SQL to
// the CLI's total.
func TestMaterializeBridgeIsTheCLIsRows(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedBridgeHousehold(t, db, ctx, "CHF")
	to := dy(2024, time.June, 30) + 43199
	if _, err := MaterializeReturns(ctx, db, MaterializeParams{ToEpoch: to, ComputedAt: 1}); err != nil {
		t.Fatalf("MaterializeReturns: %v", err)
	}
	for _, ccy := range materializeCurrencies {
		for _, want := range runBridge(t, db, ctx, bridgeParams(ccy, "monthly")) {
			var saved, gain, quality string
			err := db.QueryRowContext(ctx, `SELECT CAST(net_saved AS VARCHAR), CAST(gain AS VARCHAR), quality
				FROM report_bridge WHERE currency = ? AND period = ?`, ccy, want.Period).Scan(&saved, &gain, &quality)
			if err != nil {
				t.Fatalf("%s %s: %v", ccy, want.Period, err)
			}
			if saved != *want.NetSaved || gain != *want.Gain || quality != strings.Join(want.Quality, ";") {
				t.Errorf("%s %s: table %s %s %q, CLI %s %s %v", ccy, want.Period, saved, gain, quality,
					*want.NetSaved, *want.Gain, want.Quality)
			}
		}
	}
}

// With no account there are no months.
func TestBridgeEmptyGold(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	if rows := runBridge(t, db, ctx, bridgeParams("USD", "monthly")); len(rows) != 0 {
		t.Fatalf("rows = %+v", rows)
	}
}
