package gold

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/returns"
)

// privateLegSeed is one transaction of the private-markets fixture.
type privateLegSeed struct {
	id, inst string
	at       int64
	kind     canonical.TxKind
	amount   int64
}

// seedPrivateFixture writes one investor account of a private-markets
// source and its legs and lines:
//
//   - FUND: two calls and a refund, a distribution, then a mark that
//     postdates every leg;
//   - LATE: a call after its only mark;
//   - SPV: a subscription, then an exit in kind, after which the
//     account's snapshot no longer lists it;
//   - VOID: an exit in kind that states no value;
//   - GHOST: a call and never a line;
//   - REFUND, a call refunded whole, and ZERO, a call of no amount marked
//     at nothing: no money in, out or marked, so no investment;
//   - DIRECT, a buy of a listed share, and a contribution that names no
//     instrument, which the view lists neither of.
//
// A CHF costs 0.90 USD in the first half of 2024 and 0.80 after.
func seedPrivateFixture(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	seedReturnsSource(t, db, ctx, "pm", "carta")
	usd := "USD"
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{SilverSourceID: "pm", AccountExternalID: "INV",
			AccountKind: canonical.AccountKindCustody, BaseCurrency: &usd, FirstSeenAt: 1, LastSeenAt: 1}})
	})
	var txs []canonical.TransactionChange
	for _, l := range []privateLegSeed{
		{"f1", "FUND", dy(2024, time.January, 10), canonical.TxKindContribution, -1000},
		{"f2", "FUND", dy(2024, time.June, 10), canonical.TxKindContribution, -1000},
		{"f3", "FUND", dy(2024, time.June, 20), canonical.TxKindContribution, 100},
		{"f4", "FUND", dy(2024, time.September, 10), canonical.TxKindDistribution, 300},
		{"l1", "LATE", dy(2024, time.January, 15), canonical.TxKindContribution, -500},
		{"l2", "LATE", dy(2024, time.November, 1), canonical.TxKindContribution, -500},
		{"s1", "SPV", dy(2024, time.February, 1), canonical.TxKindContribution, -200},
		{"s2", "SPV", dy(2024, time.July, 1), canonical.TxKindTransferOut, -600},
		{"v1", "VOID", dy(2024, time.February, 1), canonical.TxKindContribution, -50},
		{"v2", "VOID", dy(2024, time.July, 1), canonical.TxKindTransferOut, 0},
		{"g1", "GHOST", dy(2024, time.April, 1), canonical.TxKindContribution, -100},
		{"r1", "REFUND", dy(2024, time.April, 1), canonical.TxKindContribution, -300},
		{"r2", "REFUND", dy(2024, time.April, 9), canonical.TxKindContribution, 300},
		{"z1", "ZERO", dy(2024, time.April, 1), canonical.TxKindContribution, 0},
		{"d1", "DIRECT", dy(2024, time.February, 1), canonical.TxKindBuy, -700},
	} {
		inst := l.inst
		txs = append(txs, canonical.TransactionChange{SilverSourceID: "pm", TransactionExternalID: l.id,
			OccurredAt: l.at, AccountExternalID: "INV", InstrumentExternalID: &inst, Kind: l.kind,
			Currency: "USD", NetAmount: decp(l.amount)})
	}
	txs = append(txs, canonical.TransactionChange{SilverSourceID: "pm", TransactionExternalID: "n1",
		OccurredAt: dy(2024, time.March, 1), AccountExternalID: "INV", Kind: canonical.TxKindContribution,
		Currency: "USD", NetAmount: decp(-80)})
	inTx(t, db, ctx, func(w *Writer) error { return w.InsertTransactions(ctx, txs) })

	mar31, dec31 := dy(2024, time.March, 31), dy(2024, time.December, 31)
	line := func(inst string, at, valued int64, value int64) canonical.PositionChange {
		i := inst
		c := canonical.PositionChange{SilverSourceID: "pm", AccountExternalID: "INV", SnapshotAt: at,
			PositionKey: inst, InstrumentExternalID: &i, AssetClass: canonical.AssetClassPrivateEquity,
			Vehicle: canonical.VehicleFund, Currency: "USD", ValuedAt: valued, MarketValue: decp(value)}
		if inst == "DIRECT" {
			c.AssetClass, c.Vehicle = canonical.AssetClassPublicEquity, canonical.VehicleStock
		}
		return c
	}
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertPositions(ctx, []canonical.PositionChange{
			line("FUND", mar31, mar31, 1000), line("LATE", mar31, mar31, 500), line("SPV", mar31, mar31, 200),
			line("VOID", mar31, mar31, 50), line("DIRECT", mar31, mar31, 700), line("ZERO", mar31, mar31, 0),
			line("FUND", dec31, dy(2024, time.September, 30), 2100), line("LATE", dec31, mar31, 500),
			line("DIRECT", dec31, dec31, 900), line("ZERO", dec31, dec31, 0),
		})
	})
	seedFX(t, db, dy(2024, time.January, 1), "USD", "CHF", "0.90")
	seedFX(t, db, dy(2024, time.July, 1), "USD", "CHF", "0.80")
}

// privateRows is RunPrivate in ccy, by holding; the total under "".
func privateRows(t *testing.T, db *sql.DB, ctx context.Context, ccy string) (map[string]PrivateRow, []string) {
	t.Helper()
	rows, err := RunPrivate(ctx, db, PrivateLatest, ccy)
	if err != nil {
		t.Fatalf("RunPrivate: %v", err)
	}
	out := map[string]PrivateRow{}
	var order []string
	for _, r := range rows {
		out[r.InstrumentID] = r
		order = append(order, r.InstrumentID)
	}
	return out, order
}

func isNear(got *float64, want float64) bool { return got != nil && math.Abs(*got-want) < 1e-9 }

func TestPrivateMarkets(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedPrivateFixture(t, db, ctx)
	rows, order := privateRows(t, db, ctx, "USD")
	mar31 := dy(2024, time.March, 31)

	// One row per holding a call names, by first call, then the total.
	if want := []string{"FUND", "LATE", "SPV", "VOID", "GHOST", ""}; !slices.Equal(order, want) {
		t.Fatalf("rows %v, want %v: a listed share, an unnamed call and one with no money are no holding", order, want)
	}
	fund := rows["FUND"]
	if cents(t, fund.PaidIn) != 190000 || cents(t, fund.Distributed) != 30000 || cents(t, fund.NAV) != 210000 {
		t.Errorf("FUND money %v %v %v: the refund nets the calls", *fund.PaidIn, *fund.Distributed, *fund.NAV)
	}
	if !isNear(fund.DPI, 300.0/1900) || !isNear(fund.TVPI, 2400.0/1900) || len(fund.Quality) != 0 {
		t.Errorf("FUND dpi %v tvpi %v quality %v", fund.DPI, fund.TVPI, fund.Quality)
	}
	if fund.MarkedAt == nil || *fund.MarkedAt != dy(2024, time.September, 30) || fund.EndAt != dy(2024, time.December, 31) {
		t.Errorf("FUND marked %v, ends %d: the mark's day, the snapshot's", fund.MarkedAt, fund.EndAt)
	}
	// irr_pct is the returns engine's XIRR of the legs and the NAV.
	flows := []returns.Flow{{Day: EpochDay(dy(2024, time.January, 10)), Amount: 1000},
		{Day: EpochDay(dy(2024, time.June, 10)), Amount: 1000}, {Day: EpochDay(dy(2024, time.June, 20)), Amount: -100},
		{Day: EpochDay(dy(2024, time.September, 10)), Amount: -300}}
	want, err := returns.XIRR(0, 2100, EpochDay(dy(2024, time.January, 10))-1, EpochDay(dy(2024, time.December, 31)), flows)
	if err != nil || !isNear(fund.IRR, want) {
		t.Errorf("FUND irr %v, want %v (%v)", fund.IRR, want, err)
	}

	if late := rows["LATE"]; !slices.Contains(late.Quality, "call_after_mark") || !isNear(late.TVPI, 0.5) {
		t.Errorf("LATE %+v: a call after the mark reads low and says so", late)
	}
	spv := rows["SPV"]
	if cents(t, spv.Distributed) != 60000 || cents(t, spv.NAV) != 0 || !isNear(spv.TVPI, 3) ||
		!slices.Equal(spv.Quality, []string{"exited"}) || spv.MarkedAt == nil || *spv.MarkedAt != mar31 {
		t.Errorf("SPV %+v: an exit in kind is distributed, and the holding left after its last mark", spv)
	}
	if spv.IRR == nil || EpochDay(spv.EndAt) != EpochDay(dy(2024, time.July, 1)) {
		t.Errorf("SPV irr %v ends %d: an exit's return ends on its last leg", spv.IRR, spv.EndAt)
	}
	if v := rows["VOID"]; !slices.Equal(v.Quality[:2], []string{"exited", "exit_unvalued=1"}) || cents(t, v.Distributed) != 0 {
		t.Errorf("VOID %+v: an exit without a value says so", v)
	}
	ghost := rows["GHOST"]
	if ghost.NAV != nil || ghost.TVPI != nil || ghost.IRR != nil || !slices.Equal(ghost.Quality, []string{"no_mark"}) {
		t.Errorf("GHOST %+v: no mark, no NAV, TVPI or IRR", ghost)
	}

	total := rows[""]
	if !total.IsTotal || total.Holding != "Total" || cents(t, total.PaidIn) != 190000+100000+20000+5000+10000 {
		t.Errorf("total %+v", total)
	}
	// GHOST has no value: the total's NAV adds the others', and its TVPI
	// pools the holdings that have one.
	if cents(t, total.NAV) != 260000 || !isNear(total.TVPI, (900.0+2600)/3150) || !isNear(total.DPI, 900.0/3250) ||
		total.IRR == nil {
		t.Errorf("total nav %v tvpi %v dpi %v irr %v: a holding with no value leaves the pools, not the total",
			total.NAV, total.TVPI, total.DPI, total.IRR)
	}
	if !slices.Equal(total.Quality[:4], []string{"no_mark=1", "exited=2", "call_after_mark=1", "exit_unvalued=1"}) {
		t.Errorf("total quality %v", total.Quality)
	}
	if total.MarkedAt == nil || *total.MarkedAt != dy(2024, time.March, 31) {
		t.Errorf("total marked %v: the oldest mark", total.MarkedAt)
	}
}

// A holding that has paid back more than it took and still holds a value
// has the unique IRR of its legs, the shape of a successful fund.
func TestPrivateMarketsIRRPastPayback(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedPrivateFixture(t, db, ctx)
	inst, dec31 := "WIN", dy(2024, time.December, 31)
	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.InsertTransactions(ctx, []canonical.TransactionChange{
			{SilverSourceID: "pm", TransactionExternalID: "w1", OccurredAt: dy(2024, time.February, 1), AccountExternalID: "INV",
				InstrumentExternalID: &inst, Kind: canonical.TxKindContribution, Currency: "USD", NetAmount: decp(-1000)},
			{SilverSourceID: "pm", TransactionExternalID: "w2", OccurredAt: dy(2024, time.August, 1), AccountExternalID: "INV",
				InstrumentExternalID: &inst, Kind: canonical.TxKindDistribution, Currency: "USD", NetAmount: decp(1500)},
		}); err != nil {
			return err
		}
		return w.InsertPositions(ctx, []canonical.PositionChange{{SilverSourceID: "pm", AccountExternalID: "INV",
			SnapshotAt: dec31, PositionKey: inst, InstrumentExternalID: &inst,
			AssetClass: canonical.AssetClassPrivateEquity, Vehicle: canonical.VehicleFund, Currency: "USD",
			ValuedAt: dec31, MarketValue: decp(500)}})
	})
	rows, _ := privateRows(t, db, ctx, "USD")
	win := rows["WIN"]
	want, err := returns.XIRR(0, 500, EpochDay(dy(2024, time.February, 1))-1, EpochDay(dec31), []returns.Flow{
		{Day: EpochDay(dy(2024, time.February, 1)), Amount: 1000}, {Day: EpochDay(dy(2024, time.August, 1)), Amount: -1500}})
	if err != nil || !isNear(win.IRR, want) || slices.Contains(win.Quality, "mwr_negative_net_capital") {
		t.Errorf("WIN irr %v quality %v, want %v (%v)", win.IRR, win.Quality, want, err)
	}
}

// The total's IRR pools the legs of the holdings it lists: a call
// refunded whole is no holding, and its legs move no rate.
func TestPrivateMarketsTotalIRR(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedPrivateFixture(t, db, ctx)
	inst, dec31 := "GHOST", dy(2024, time.December, 31)
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertPositions(ctx, []canonical.PositionChange{{SilverSourceID: "pm", AccountExternalID: "INV",
			SnapshotAt: dec31, PositionKey: inst, InstrumentExternalID: &inst,
			AssetClass: canonical.AssetClassPrivateEquity, Vehicle: canonical.VehicleFund, Currency: "USD",
			ValuedAt: dec31, MarketValue: decp(100)}})
	})
	rows, order := privateRows(t, db, ctx, "USD")
	if slices.Contains(order, "REFUND") {
		t.Fatalf("rows %v: REFUND moved no money", order)
	}
	total := rows[""]
	if cents(t, total.NAV) != 210000+50000+10000 {
		t.Fatalf("total nav %v, want FUND, LATE and GHOST", total.NAV)
	}
	at := func(y int, m time.Month, d int, paid float64) returns.Flow {
		return returns.Flow{Day: EpochDay(dy(y, m, d)), Amount: paid}
	}
	// FUND, LATE, SPV, VOID and GHOST.
	flows := []returns.Flow{
		at(2024, time.January, 10, 1000), at(2024, time.June, 10, 1000), at(2024, time.June, 20, -100),
		at(2024, time.September, 10, -300),
		at(2024, time.January, 15, 500), at(2024, time.November, 1, 500),
		at(2024, time.February, 1, 200), at(2024, time.July, 1, -600),
		at(2024, time.February, 1, 50), at(2024, time.July, 1, 0),
		at(2024, time.April, 1, 100),
	}
	want, err := returns.XIRR(0, 2700, EpochDay(dy(2024, time.January, 10))-1, EpochDay(dec31), flows)
	if err != nil || !nearPtr(total.IRR, &want, 1e-9) {
		t.Errorf("total irr %v, want %v (%v): the listed holdings' legs alone", total.IRR, want, err)
	}
}

// Each leg converts at its own day's rate and the NAV at its snapshot's;
// the multiples move with the currency.
func TestPrivateMarketsHistoricFX(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedPrivateFixture(t, db, ctx)
	rows, _ := privateRows(t, db, ctx, "CHF")
	fund := rows["FUND"]
	if cents(t, fund.PaidIn) != 211111 || cents(t, fund.Distributed) != 37500 || cents(t, fund.NAV) != 262500 {
		t.Errorf("FUND CHF %v %v %v", *fund.PaidIn, *fund.Distributed, *fund.NAV)
	}
	// Each holding's money rounds to the cent before the multiples.
	if !isNear(fund.TVPI, (375.0+2625)/2111.11) {
		t.Errorf("FUND CHF tvpi %v", fund.TVPI)
	}
}

// report_private_markets holds the CLI's rows in every currency at
// every quarter end since the first call and at today, and web_private
// serves them: the dashboard and `returns private -d` agree. COSTED
// states its cost from the first quarter and is bought in the third, so
// the quarter ends before the buy read its stated cost, as -d does.
func TestPrivateMarketsTableIsTheCLIsRows(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedPrivateFixture(t, db, ctx)
	seedCapitalAccounts(t, db, ctx)
	costed := "COSTED"
	acquired := time.Date(2024, time.January, 15, 0, 0, 0, 0, time.UTC)
	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.InsertTransactions(ctx, []canonical.TransactionChange{{SilverSourceID: "pm",
			TransactionExternalID: "cb1", OccurredAt: dy(2024, time.August, 1), AccountExternalID: "INV",
			InstrumentExternalID: &costed, Kind: canonical.TxKindBuy, Currency: "USD", NetAmount: decp(-200)}}); err != nil {
			return err
		}
		var lines []canonical.PositionChange
		for _, at := range []int64{dy(2024, time.March, 31), dy(2024, time.December, 31)} {
			c := canonical.PositionChange{SilverSourceID: "pm", AccountExternalID: "INV", SnapshotAt: at,
				PositionKey: costed, InstrumentExternalID: &costed, AssetClass: canonical.AssetClassPrivateEquity,
				Vehicle: canonical.VehicleFund, Currency: "USD", ValuedAt: at, MarketValue: decp(250),
				AcquisitionDate: &acquired}
			c.SetBookValue(decp(200), canonical.Basis{Origin: canonical.BasisStated,
				Method: canonical.BasisMethodPaidIn, Fees: canonical.BasisFeesIncluded})
			lines = append(lines, c)
		}
		return w.InsertPositions(ctx, lines)
	})
	today := dy(2025, time.February, 15) + 43199
	m, err := computeMaterialPrivate(ctx, db, today)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.write(ctx, tx, 1); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// The first call falls in 2024's first quarter.
	quarterEnd := func(y int, m time.Month, d int) int64 { return dy(y, m, d) + 43199 }
	instants := []int64{quarterEnd(2024, time.March, 31), quarterEnd(2024, time.June, 30),
		quarterEnd(2024, time.September, 30), quarterEnd(2024, time.December, 31), today}
	stored, err := scanRows(ctx, db, "as_of", `SELECT DISTINCT as_of FROM report_private_markets ORDER BY 1`, nil,
		func(v *int64) []any { return []any{v} })
	if err != nil || !slices.Equal(stored, instants) {
		t.Fatalf("stored instants %v (%v), want every quarter end and today %v", stored, err, instants)
	}
	type cut struct {
		holding, paid, back, nav, quality, status string
		commitment, unfunded                      string
		marked, statedAt                          *int64
		dpi, tvpi, irr                            *float64
		statedTVPI, statedDPI                     *float64
	}
	for _, ccy := range MaterializedCurrencies() {
		for _, at := range instants {
			want, err := RunPrivate(ctx, db, at, ccy)
			if err != nil {
				t.Fatal(err)
			}
			got, err := scanRows(ctx, db, "web_private", `
				SELECT holding, COALESCE(CAST(paid_in AS VARCHAR), ''), CAST(distributed AS VARCHAR),
				       COALESCE(CAST(nav AS VARCHAR), ''), quality, COALESCE(status, ''),
				       CAST(epoch(marked_at) AS BIGINT), dpi, tvpi, irr_pct / 100,
				       COALESCE(CAST(commitment AS VARCHAR), ''), COALESCE(CAST(unfunded AS VARCHAR), ''),
				       stated_tvpi, stated_dpi, CAST(epoch(stated_as_of) AS BIGINT)
				  FROM web_private WHERE currency = ? AND epoch(as_of) = ?
				 ORDER BY is_total, first_call, instrument_external_id`,
				[]any{ccy, at}, func(c *cut) []any {
					return []any{&c.holding, &c.paid, &c.back, &c.nav, &c.quality, &c.status, i64(&c.marked),
						flt(&c.dpi), flt(&c.tvpi), flt(&c.irr), &c.commitment, &c.unfunded,
						flt(&c.statedTVPI), flt(&c.statedDPI), i64(&c.statedAt)}
				})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) || len(got) == 0 {
				t.Fatalf("%s at %d: %d rows in the table, %d from the CLI", ccy, at, len(got), len(want))
			}
			same := func(cell string, money *string) bool {
				if money == nil {
					return cell == ""
				}
				return cell != "" && cents(t, &cell) == cents(t, money)
			}
			for i, w := range want {
				g := got[i]
				if g.holding != w.Holding || !same(g.paid, w.PaidIn) || cents(t, &g.back) != cents(t, w.Distributed) ||
					!same(g.nav, w.NAV) || g.quality != strings.Join(w.Quality, ";") ||
					g.status != strOrBlank(w.Status) || (g.marked == nil) != (w.MarkedAt == nil) ||
					!nearPtr(g.dpi, w.DPI, 1e-12) || !nearPtr(g.tvpi, w.TVPI, 1e-12) || !nearPtr(g.irr, w.IRR, 1e-12) ||
					!same(g.commitment, w.Commitment) || !same(g.unfunded, w.Unfunded) ||
					!nearPtr(g.statedTVPI, w.StatedTVPI, 1e-12) || !nearPtr(g.statedDPI, w.StatedDPI, 1e-12) ||
					!slices.Equal(ptrSlice(g.statedAt), ptrSlice(w.StatedAsOf)) {
					t.Errorf("%s at %d row %d: table %+v, CLI %+v", ccy, at, i, g, w)
				}
				if g.holding == "FUND" && (ccy == "USD" || ccy == "CHF") &&
					(g.commitment == "") != (at < dy(2024, time.March, 31)) {
					t.Errorf("%s at %d: FUND's commitment %q, stated from its first account on", ccy, at, g.commitment)
				}
				if g.holding == costed && strings.Contains(g.quality, "paid_in_from_cost") != (at < dy(2024, time.August, 1)) {
					t.Errorf("%s at %d: COSTED %+v, from its stated cost only before its buy", ccy, at, g)
				}
			}
		}
	}
}

// ptrSlice is *p as a one-element slice, empty for nil.
func ptrSlice[T any](p *T) []T {
	if p == nil {
		return nil
	}
	return []T{*p}
}

// strOrBlank is *s, or "" for nil.
func strOrBlank(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// The stored instants are the quarter ends from the first call's
// quarter, then today, once where today ends a quarter.
func TestPrivateMaterialInstants(t *testing.T) {
	t.Parallel()
	end := func(y int, m time.Month, d int) int64 { return time.Date(y, m, d, 23, 59, 59, 0, time.UTC).Unix() }
	for _, c := range []struct {
		first, today int64
		want         []int64
	}{
		{dy(2024, time.February, 10), end(2024, time.August, 3),
			[]int64{end(2024, time.March, 31), end(2024, time.June, 30), end(2024, time.August, 3)}},
		{dy(2024, time.January, 1), end(2024, time.June, 30),
			[]int64{end(2024, time.March, 31), end(2024, time.June, 30)}},
		{dy(2024, time.May, 2), end(2024, time.May, 20), []int64{end(2024, time.May, 20)}},
	} {
		if got := privateMaterialInstants(c.first, c.today); !slices.Equal(got, c.want) {
			t.Errorf("first %d today %d: %v, want %v", c.first, c.today, got, c.want)
		}
	}
}

// The stored quarter ends start at the first line of a holding that has
// no leg, as its first_call does.
func TestPrivateMaterialStartsAtTheFirstLine(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "pl", "manual")
	usd, inst := "USD", "LINES"
	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.UpsertAccounts(ctx, []canonical.AccountChange{{SilverSourceID: "pl", AccountExternalID: "L1",
			AccountKind: canonical.AccountKindCustody, BaseCurrency: &usd, FirstSeenAt: 1, LastSeenAt: 1}}); err != nil {
			return err
		}
		return w.InsertPositions(ctx, []canonical.PositionChange{{SilverSourceID: "pl", AccountExternalID: "L1",
			SnapshotAt: dy(2023, time.August, 31), PositionKey: inst, InstrumentExternalID: &inst,
			AssetClass: canonical.AssetClassPrivateEquity, Vehicle: canonical.VehicleFund, Currency: "USD",
			MarketValue: decp(100)}})
	})
	today := dy(2024, time.February, 15) + 43199
	m, err := computeMaterialPrivate(ctx, db, today)
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for _, r := range m["USD"] {
		if r.IsTotal {
			got = append(got, r.AsOf)
		}
	}
	end := func(y int, mo time.Month, d int) int64 { return dy(y, mo, d) + 43199 }
	if want := []int64{end(2023, time.September, 30), end(2023, time.December, 31), today}; !slices.Equal(got, want) {
		t.Errorf("instants %v, want %v", got, want)
	}
}

// -d reads a day: the legs up to its end, the NAV from the last snapshot
// on or before it, and the status then.
func TestPrivateMarketsAsOf(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedPrivateFixture(t, db, ctx)
	rows := func(at int64) (map[string]PrivateRow, []string) {
		got, err := RunPrivate(ctx, db, at, "USD")
		if err != nil {
			t.Fatal(err)
		}
		out, order := map[string]PrivateRow{}, []string{}
		for _, r := range got {
			if r.AsOf != at {
				t.Fatalf("row as of %d, want %d", r.AsOf, at)
			}
			out[r.InstrumentID] = r
			order = append(order, r.InstrumentID)
		}
		return out, order
	}
	jun30 := dy(2024, time.June, 30) + 43199
	at, order := rows(jun30)
	if want := []string{"FUND", "LATE", "SPV", "VOID", "GHOST", ""}; !slices.Equal(order, want) {
		t.Fatalf("rows %v, want %v", order, want)
	}
	fund := at["FUND"]
	if cents(t, fund.PaidIn) != 190000 || cents(t, fund.Distributed) != 0 || cents(t, fund.NAV) != 100000 ||
		*fund.MarkedAt != dy(2024, time.March, 31) || *fund.Status != "held" ||
		EpochDay(fund.EndAt) != EpochDay(dy(2024, time.June, 20)) {
		t.Errorf("FUND on June 30 %s, marked %v, %v: the September distribution and the December mark are later",
			privateRowText(fund), *fund.MarkedAt, *fund.Status)
	}
	want, err := returns.XIRR(0, 1000, EpochDay(dy(2024, time.January, 10))-1, EpochDay(dy(2024, time.June, 20)),
		[]returns.Flow{{Day: EpochDay(dy(2024, time.January, 10)), Amount: 1000},
			{Day: EpochDay(dy(2024, time.June, 10)), Amount: 1000}, {Day: EpochDay(dy(2024, time.June, 20)), Amount: -100}})
	if err != nil || !isNear(fund.IRR, want) {
		t.Errorf("FUND irr %v, want %v (%v)", fund.IRR, want, err)
	}
	if spv := at["SPV"]; cents(t, spv.Distributed) != 0 || cents(t, spv.NAV) != 20000 || *spv.Status != "held" ||
		slices.Contains(spv.Quality, "exited") {
		t.Errorf("SPV on June 30 %s: its exit is in July", privateRowText(spv))
	}
	if total := at[""]; cents(t, total.PaidIn) != 190000+50000+20000+5000+10000 || total.Status != nil {
		t.Errorf("total on June 30 %s", privateRowText(total))
	}
	// Before the first call nothing is listed.
	if _, order := rows(dy(2024, time.January, 5)); len(order) != 0 {
		t.Errorf("rows before the first call %v", order)
	}
	// At the latest snapshot the fixture reads as TestPrivateMarkets says.
	latest, _ := rows(PrivateLatest)
	if *latest["FUND"].Status != "partially_exited" || *latest["SPV"].Status != "exited" ||
		*latest["LATE"].Status != "held" {
		t.Errorf("statuses %v %v %v", *latest["FUND"].Status, *latest["SPV"].Status, *latest["LATE"].Status)
	}
}

// A leg with no rate to the output currency leaves the sums and the
// return, and says so.
func TestPrivateMarketsUnpriced(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedPrivateFixture(t, db, ctx)
	rows, _ := privateRows(t, db, ctx, "EUR")
	if f := rows["FUND"]; !slices.Contains(f.Quality, "unpriced=5") || f.IRR != nil {
		t.Errorf("FUND in EUR %+v: four legs and a line have no rate", f)
	}
	// A zero is zero in every currency: VOID's exit of no value is priced.
	if v := rows["VOID"]; !slices.Contains(v.Quality, "unpriced=1") {
		t.Errorf("VOID in EUR %+v: its call alone has no rate", v)
	}
}

// A NAV a source carries at cost, before any mark of its own, says so:
// the multiples read 1 and the IRR 0 by construction, not by a valuation.
// AngelList states it as valuation_basis cost, Carta's fund carry as
// called_capital. A mark plus what moved in since is a mark.
func TestPrivateMarketsAtCost(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedPrivateFixture(t, db, ctx)
	dec31 := dy(2024, time.December, 31)
	for _, c := range []struct{ inst, basis string }{
		{"CARRY", "cost"}, {"CALLED", "called_capital"}, {"MARKED", "mark_plus_cost"},
	} {
		inst := c.inst
		inTx(t, db, ctx, func(w *Writer) error {
			if err := w.InsertTransactions(ctx, []canonical.TransactionChange{{SilverSourceID: "pm",
				TransactionExternalID: "c-" + inst, OccurredAt: dy(2024, time.October, 1), AccountExternalID: "INV",
				InstrumentExternalID: &inst, Kind: canonical.TxKindContribution, Currency: "USD", NetAmount: decp(-400)}}); err != nil {
				return err
			}
			return w.InsertPositions(ctx, []canonical.PositionChange{{SilverSourceID: "pm", AccountExternalID: "INV",
				SnapshotAt: dec31, PositionKey: inst, InstrumentExternalID: &inst,
				AssetClass: canonical.AssetClassPrivateEquity, Vehicle: canonical.VehicleFund, Currency: "USD",
				ValuedAt: dy(2024, time.October, 1), MarketValue: decp(400),
				Payload: json.RawMessage(`{"valuation_basis": "` + c.basis + `"}`)}})
		})
	}
	rows, _ := privateRows(t, db, ctx, "USD")
	for inst, want := range map[string]bool{"CARRY": true, "CALLED": true, "MARKED": false, "FUND": false} {
		if got := slices.Contains(rows[inst].Quality, "at_cost"); got != want {
			t.Errorf("%s quality %v, at_cost %v", inst, rows[inst].Quality, want)
		}
	}
	if !slices.Contains(rows[""].Quality, "at_cost=2") {
		t.Errorf("total quality %v: it counts the holdings at cost", rows[""].Quality)
	}
}

// seedSPVExit adds SPVX to the private-markets fixture: a subscription,
// one mark, and an exit in kind out of the account that delivers PUB,
// the listed shares the SPV received, at a stated value. payload is the
// exit's, as the equity-transfer ledger writes it; an empty one seeds no
// exit. GHOST gets a value, so the total has a NAV and an IRR.
func seedSPVExit(t *testing.T, db *sql.DB, ctx context.Context, payload string, exitAt int64) {
	t.Helper()
	seedPrivateFixture(t, db, ctx)
	spv, pub, ghost := "SPVX", "PUB", "GHOST"
	mar31, dec31 := dy(2024, time.March, 31), dy(2024, time.December, 31)
	txs := []canonical.TransactionChange{{SilverSourceID: "pm", TransactionExternalID: "x1",
		OccurredAt: dy(2024, time.March, 1), AccountExternalID: "INV", InstrumentExternalID: &spv,
		Kind: canonical.TxKindContribution, Currency: "USD", NetAmount: decp(-400)}}
	if payload != "" {
		txs = append(txs, canonical.TransactionChange{SilverSourceID: "pm",
			TransactionExternalID: canonical.LedgerTransferIDPrefix + "x2", OccurredAt: exitAt,
			AccountExternalID: "INV", InstrumentExternalID: &pub, Kind: canonical.TxKindTransferOut,
			Currency: "USD", Quantity: decp(-10), NetAmount: decp(-1000), Payload: json.RawMessage(payload)})
	}
	// An exit in kind out of an account that holds no private holding is
	// no private leg.
	txs = append(txs, canonical.TransactionChange{SilverSourceID: "pm",
		TransactionExternalID: canonical.LedgerTransferIDPrefix + "x3", OccurredAt: exitAt,
		AccountExternalID: "BRK", InstrumentExternalID: &pub, Kind: canonical.TxKindTransferOut,
		Currency: "USD", NetAmount: decp(-700)})
	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.InsertTransactions(ctx, txs); err != nil {
			return err
		}
		line := func(inst string, at int64, value int64) canonical.PositionChange {
			i := inst
			return canonical.PositionChange{SilverSourceID: "pm", AccountExternalID: "INV", SnapshotAt: at,
				PositionKey: inst, InstrumentExternalID: &i, AssetClass: canonical.AssetClassPrivateEquity,
				Vehicle: canonical.VehicleFund, Currency: "USD", ValuedAt: at, MarketValue: decp(value)}
		}
		return w.InsertPositions(ctx, []canonical.PositionChange{line(spv, mar31, 400), line(ghost, dec31, 100)})
	})
}

// privateRowText is a row's figures, for comparing two rows.
func privateRowText(r PrivateRow) string {
	s := func(p *string) string {
		if p == nil {
			return "-"
		}
		return *p
	}
	f := func(p *float64) string {
		if p == nil {
			return "-"
		}
		return strconv.FormatFloat(*p, 'g', -1, 64)
	}
	return strings.Join([]string{s(r.PaidIn), s(r.Distributed), s(r.NAV), strconv.FormatInt(r.EndAt, 10),
		f(r.DPI), f(r.TVPI), f(r.IRR), s(r.Commitment), s(r.Unfunded), f(r.StatedTVPI), f(r.StatedDPI),
		strings.Join(r.Quality, ";")}, " ")
}

// The flows of TestPrivateMarketsTotalIRR's total: FUND, LATE, SPV,
// VOID and GHOST, then SPVX's subscription.
func privateFixtureFlows() []returns.Flow {
	at := func(y int, m time.Month, d int, paid float64) returns.Flow {
		return returns.Flow{Day: EpochDay(dy(y, m, d)), Amount: paid}
	}
	return []returns.Flow{
		at(2024, time.January, 10, 1000), at(2024, time.June, 10, 1000), at(2024, time.June, 20, -100),
		at(2024, time.September, 10, -300),
		at(2024, time.January, 15, 500), at(2024, time.November, 1, 500),
		at(2024, time.February, 1, 200), at(2024, time.July, 1, -600),
		at(2024, time.February, 1, 50), at(2024, time.July, 1, 0),
		at(2024, time.April, 1, 100),
		at(2024, time.March, 1, 400),
	}
}

// seedExitsOfNoPrivateHolding adds exits in kind, on day at, that are no
// private holding's. The equity-transfer ledger writes four: of DIRECT,
// which INV bought, and of XIN, which it took in; a transfer of cash out
// of INV; and of PUB2 out of INV2, whose one holding is a call refunded
// whole, so the view lists none of it. Two more are a source's own
// deliveries out of INV: of LISTED, a share known from its line only,
// and of PUB3, which no row of INV names.
func seedExitsOfNoPrivateHolding(t *testing.T, db *sql.DB, ctx context.Context, at int64) {
	t.Helper()
	tx := func(id, acct, inst string, on int64, kind canonical.TxKind, amount int64) canonical.TransactionChange {
		c := canonical.TransactionChange{SilverSourceID: "pm", TransactionExternalID: id, OccurredAt: on,
			AccountExternalID: acct, Kind: kind, Currency: "USD", NetAmount: decp(amount),
			Payload: json.RawMessage(`{"equity_transfer_ledger":true}`)}
		if inst != "" {
			c.InstrumentExternalID = &inst
		}
		return c
	}
	own := func(c canonical.TransactionChange) canonical.TransactionChange {
		c.Payload = nil
		return c
	}
	jan := dy(2024, time.January, 20)
	listed := "LISTED"
	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.InsertTransactions(ctx, []canonical.TransactionChange{
			tx("o1", "INV", "DIRECT", at, canonical.TxKindTransferOut, -900),
			tx("o2", "INV", "XIN", jan, canonical.TxKindTransferIn, 250),
			tx("o3", "INV", "XIN", at, canonical.TxKindTransferOut, -250),
			tx("o4", "INV", "", at, canonical.TxKindTransferOut, -300),
			tx("o5", "INV2", "REF2", jan, canonical.TxKindContribution, -300),
			tx("o6", "INV2", "REF2", dy(2024, time.February, 20), canonical.TxKindContribution, 300),
			tx("o7", "INV2", "PUB2", at, canonical.TxKindTransferOut, -800),
			own(tx("o8", "INV", listed, at, canonical.TxKindTransferOut, -400)),
			own(tx("o9", "INV", "PUB3", at, canonical.TxKindTransferOut, -500)),
		}); err != nil {
			return err
		}
		return w.InsertPositions(ctx, []canonical.PositionChange{{SilverSourceID: "pm", AccountExternalID: "INV",
			SnapshotAt: dy(2024, time.March, 31), PositionKey: listed, InstrumentExternalID: &listed,
			AssetClass: canonical.AssetClassPublicEquity, Vehicle: canonical.VehicleStock, Currency: "USD",
			MarketValue: decp(400)}})
	})
}

// None of those exits moves a figure: a row or the total.
func TestPrivateMarketsExitsOfNoPrivateHolding(t *testing.T) {
	t.Parallel()
	exit := dy(2025, time.January, 15)
	db, ctx := openMigrated(t)
	seedSPVExit(t, db, ctx, "", exit)
	seedExitsOfNoPrivateHolding(t, db, ctx, exit)
	bare, bctx := openMigrated(t)
	seedSPVExit(t, bare, bctx, "", exit)
	rows, order := privateRows(t, db, ctx, "USD")
	base, baseOrder := privateRows(t, bare, bctx, "USD")
	if !slices.Equal(order, baseOrder) {
		t.Fatalf("rows %v, without the exits %v", order, baseOrder)
	}
	for _, k := range order {
		if got, want := privateRowText(rows[k]), privateRowText(base[k]); got != want {
			t.Errorf("%q: %s, without the exits %s", k, got, want)
		}
	}
}

// On the total, exit_unvalued and unpriced count the holdings and the
// unattributed exits: an exit that states no value, and one with no
// rate to the output currency, which also blanks the IRR.
func TestPrivateMarketsUnattributedExitQuality(t *testing.T) {
	t.Parallel()
	exit := dy(2025, time.January, 15)
	db, ctx := openMigrated(t)
	seedSPVExit(t, db, ctx, "", exit)
	pub, pub2 := "PUB", "PUB2"
	ledger := json.RawMessage(`{"equity_transfer_ledger":true}`)
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertTransactions(ctx, []canonical.TransactionChange{
			{SilverSourceID: "pm", TransactionExternalID: "u1", OccurredAt: exit, AccountExternalID: "INV",
				InstrumentExternalID: &pub, Kind: canonical.TxKindTransferOut, Currency: "JPY", NetAmount: decp(-1000),
				Payload: ledger},
			{SilverSourceID: "pm", TransactionExternalID: "u2", OccurredAt: exit, AccountExternalID: "INV",
				InstrumentExternalID: &pub2, Kind: canonical.TxKindTransferOut, Currency: "USD", NetAmount: decp(0),
				Payload: ledger},
		})
	})
	bare, bctx := openMigrated(t)
	seedSPVExit(t, bare, bctx, "", exit)
	rows, _ := privateRows(t, db, ctx, "USD")
	base, _ := privateRows(t, bare, bctx, "USD")
	total, want := rows[""], base[""]
	if !slices.Contains(total.Quality, "unattributed_exit=2") || !slices.Contains(total.Quality, "unpriced=1") ||
		slices.ContainsFunc(want.Quality, func(q string) bool { return strings.HasPrefix(q, "unpriced=") }) {
		t.Errorf("total quality %v, without the exits %v: the exit with no rate is unpriced", total.Quality, want.Quality)
	}
	unvalued := func(q []string) int {
		for _, tag := range q {
			if n, ok := strings.CutPrefix(tag, "exit_unvalued="); ok {
				v, _ := strconv.Atoi(n)
				return v
			}
		}
		return 0
	}
	if unvalued(total.Quality) != unvalued(want.Quality)+1 {
		t.Errorf("total quality %v, without the exits %v: the exit of no value is unvalued", total.Quality, want.Quality)
	}
	if cents(t, total.Distributed) != cents(t, want.Distributed) || total.IRR == nil {
		t.Errorf("total %s, without the exits %s: the sums and the IRR leave the unpriced exit out",
			privateRowText(total), privateRowText(want))
	}
}

// An SPV that exits in the listed shares it received: the out-leg names
// the shares, and its holding the SPV. The exit is the SPV's
// distribution, at the value the leg states, on its day.
func TestPrivateMarketsExitInKindNamesItsHolding(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	exit := dy(2024, time.August, 1)
	seedSPVExit(t, db, ctx, `{"equity_transfer_ledger":true,"cost_basis":400,"instrument":"PUB","holding":"SPVX"}`, exit)
	rows, order := privateRows(t, db, ctx, "USD")
	if slices.Contains(order, "PUB") {
		t.Fatalf("rows %v: the delivered shares are no holding", order)
	}
	spv := rows["SPVX"]
	if cents(t, spv.Distributed) != 100000 || cents(t, spv.NAV) != 0 || !isNear(spv.DPI, 2.5) || !isNear(spv.TVPI, 2.5) ||
		!slices.Equal(spv.Quality, []string{"exited"}) || EpochDay(spv.EndAt) != EpochDay(exit) {
		t.Errorf("SPVX %s, ends %d: the exit is its distribution, on its day", privateRowText(spv), spv.EndAt)
	}
	want, err := returns.XIRR(0, 0, EpochDay(dy(2024, time.March, 1))-1, EpochDay(exit), []returns.Flow{
		{Day: EpochDay(dy(2024, time.March, 1)), Amount: 400}, {Day: EpochDay(exit), Amount: -1000}})
	if err != nil || !isNear(spv.IRR, want) {
		t.Errorf("SPVX irr %v, want %v (%v)", spv.IRR, want, err)
	}
	total := rows[""]
	if cents(t, total.Distributed) != 90000+100000 || slices.ContainsFunc(total.Quality, isUnattributedTag) {
		t.Errorf("total %s: it adds the holding's exit, which is attributed", privateRowText(total))
	}
	flows := append(privateFixtureFlows(), returns.Flow{Day: EpochDay(exit), Amount: -1000})
	want, err = returns.XIRR(0, 2100+500+100, EpochDay(dy(2024, time.January, 10))-1,
		EpochDay(dy(2024, time.December, 31)), flows)
	if err != nil || !nearPtr(total.IRR, &want, 1e-9) {
		t.Errorf("total irr %v, want %v (%v)", total.IRR, want, err)
	}
}

func isUnattributedTag(q string) bool { return strings.HasPrefix(q, "unattributed_exit=") }

// The same exit with no holding named is no holding's: the SPV reads as
// it does with no exit at all. The total counts the exit, out of the
// account of a listed holding, and says so.
func TestPrivateMarketsUnattributedExit(t *testing.T) {
	t.Parallel()
	exit := dy(2025, time.January, 15) // after every holding's last day
	db, ctx := openMigrated(t)
	seedSPVExit(t, db, ctx, `{"equity_transfer_ledger":true,"cost_basis":400,"instrument":"PUB"}`, exit)
	bare, bctx := openMigrated(t)
	seedSPVExit(t, bare, bctx, "", exit)
	rows, order := privateRows(t, db, ctx, "USD")
	base, baseOrder := privateRows(t, bare, bctx, "USD")
	if !slices.Equal(order, baseOrder) {
		t.Fatalf("rows %v, without the exit %v", order, baseOrder)
	}
	if got, want := privateRowText(rows["SPVX"]), privateRowText(base["SPVX"]); got != want {
		t.Errorf("SPVX %s, without the exit %s", got, want)
	}
	total := rows[""]
	if cents(t, total.Distributed) != cents(t, base[""].Distributed)+100000 || EpochDay(total.EndAt) != EpochDay(exit) ||
		!slices.Contains(total.Quality, "unattributed_exit=1") || slices.ContainsFunc(base[""].Quality, isUnattributedTag) {
		t.Errorf("total %s ends %d, without the exit %s", privateRowText(total), total.EndAt, privateRowText(base[""]))
	}
	flows := append(privateFixtureFlows(), returns.Flow{Day: EpochDay(exit), Amount: -1000})
	want, err := returns.XIRR(0, 2100+500+100, EpochDay(dy(2024, time.January, 10))-1, EpochDay(exit), flows)
	if err != nil || !nearPtr(total.IRR, &want, 1e-9) {
		t.Errorf("total irr %v, want %v (%v): the exit is one of its flows, on its day", total.IRR, want, err)
	}
}

// report_private_flows_multi holds the legs the total counts, per
// holding and quarter: the listed holdings' and the exit out of their
// account that names no holding, but no leg of a holding the view leaves
// out, of a listed share or of another account, and none of the exits of
// no private holding (seedExitsOfNoPrivateHolding). What it adds up to
// is the total's paid in and distributed.
func TestPrivateFlowsAreTheTotalsLegs(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedSPVExit(t, db, ctx, `{"equity_transfer_ledger":true,"cost_basis":400,"instrument":"PUB"}`,
		dy(2025, time.January, 15))
	seedExitsOfNoPrivateHolding(t, db, ctx, dy(2025, time.January, 15))
	// A call refunded whole across the CHF's move: no investment, though
	// its legs differ in CHF.
	back := "BACK"
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertTransactions(ctx, []canonical.TransactionChange{
			{SilverSourceID: "pm", TransactionExternalID: "b1", OccurredAt: dy(2024, time.June, 20),
				AccountExternalID: "INV", InstrumentExternalID: &back, Kind: canonical.TxKindContribution,
				Currency: "USD", NetAmount: decp(-300)},
			{SilverSourceID: "pm", TransactionExternalID: "b2", OccurredAt: dy(2024, time.July, 10),
				AccountExternalID: "INV", InstrumentExternalID: &back, Kind: canonical.TxKindContribution,
				Currency: "USD", NetAmount: decp(300)}})
	})
	type flow struct {
		quarter      int64
		called, back string
		holdings     int64
		unattributed bool
		quality      string
	}
	read := func(ccy string) []flow {
		got, err := scanRows(ctx, db, "report_private_flows_multi", `
			SELECT quarter, CAST(sum(called) AS VARCHAR), CAST(sum(distributed) AS VARCHAR),
			       count(holding), bool_or(unattributed AND holding IS NULL AND asset_class IS NULL),
			       string_agg(NULLIF(quality, ''), ';')
			  FROM report_private_flows_multi(?) WHERE currency = ? GROUP BY quarter ORDER BY quarter`,
			[]any{PrivateLatest, ccy}, func(f *flow) []any {
				return []any{&f.quarter, &f.called, &f.back, &f.holdings, &f.unattributed, str(new(*string))}
			})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := read("USD")
	type want struct {
		quarter      int64
		called, back int64
		holdings     int64
		unattributed bool
	}
	// A quarter opens at midnight, UTC.
	q := func(y int, m time.Month, d int) int64 { return EpochDay(dy(y, m, d)) * 86400 }
	wants := []want{
		{q(2024, time.January, 1), 2150, 0, 5, false},
		{q(2024, time.April, 1), 1000, 0, 2, false},
		{q(2024, time.July, 1), 0, 900, 3, false},
		{q(2024, time.October, 1), 500, 0, 1, false},
		{q(2025, time.January, 1), 0, 1000, 0, true},
	}
	if len(got) != len(wants) {
		t.Fatalf("%d quarters %+v, want %d", len(got), got, len(wants))
	}
	var calledTo, backTo int64
	for i, w := range wants {
		g := got[i]
		calledTo, backTo = calledTo+w.called, backTo+w.back
		if g.quarter != w.quarter || cents(t, &g.called) != 100*w.called || cents(t, &g.back) != 100*w.back ||
			g.holdings != w.holdings || g.unattributed != w.unattributed {
			t.Errorf("quarter %d: %+v, want %+v", i, g, w)
		}
	}
	rows, _ := privateRows(t, db, ctx, "USD")
	if 100*calledTo != cents(t, rows[""].PaidIn) || 100*backTo != cents(t, rows[""].Distributed) {
		t.Errorf("to date %d and %d, the total %s", calledTo, backTo, privateRowText(rows[""]))
	}
	// Each row names its holding with the holding's class and vehicle,
	// and VOID's exit of no value says so.
	type named struct{ holding, class, vehicle, quality string }
	void, err := scanRows(ctx, db, "report_private_flows_multi", `
		SELECT holding, asset_class, vehicle, quality FROM report_private_flows_multi(?)
		 WHERE currency = 'USD' AND instrument_external_id = 'VOID' AND distributed = 0 AND called = 0`,
		[]any{PrivateLatest}, func(n *named) []any { return []any{&n.holding, &n.class, &n.vehicle, &n.quality} })
	if err != nil || len(void) != 1 || void[0] != (named{"VOID", "private_equity", "fund", "exit_unvalued=1"}) {
		t.Errorf("VOID's exit %+v (%v)", void, err)
	}
	// Each currency converts at the legs' own days: the first quarter's
	// calls at 0.90 USD to the CHF. The refunded call stays out of the
	// third quarter's.
	chf := read("CHF")
	if len(chf) != len(wants) || cents(t, &chf[0].called) != 238889 || cents(t, &chf[2].called) != 0 {
		t.Errorf("CHF %+v, want 2388.89 called first and 0 third", chf)
	}
}

// seedPrivateListing writes source pl's account L1, whose holdings each
// show one rule of which holdings the view lists:
//
//   - BUYF, a fund bought with buys, a fee on it, and a sale;
//   - STK, a private company's stock bought directly;
//   - NOTE, a convertible note, of the instrument's class private_debt;
//   - LOAN, a private loan, repaid in part;
//   - POS, a fund only a source's positions state, with its cost and
//     the day it was acquired;
//   - BARE, a stock the positions state with no cost;
//   - PEETF, a private-equity ETF: the instrument's vehicle is etf,
//     though its line says fund;
//   - PUBX, a listed share.
func seedPrivateListing(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	seedReturnsSource(t, db, ctx, "pl", "manual")
	usd := "USD"
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{SilverSourceID: "pl", AccountExternalID: "L1",
			AccountKind: canonical.AccountKindCustody, BaseCurrency: &usd, FirstSeenAt: 1, LastSeenAt: 1}})
	})
	var txs []canonical.TransactionChange
	for _, l := range []privateLegSeed{
		{"b1", "BUYF", dy(2024, time.February, 1), canonical.TxKindBuy, -1000},
		{"b2", "BUYF", dy(2024, time.February, 1), canonical.TxKindFee, -20},
		{"b3", "BUYF", dy(2024, time.September, 1), canonical.TxKindSell, 300},
		{"k1", "STK", dy(2024, time.March, 1), canonical.TxKindBuy, -500},
		{"n1", "NOTE", dy(2024, time.April, 1), canonical.TxKindBuy, -250},
		{"o1", "LOAN", dy(2024, time.May, 1), canonical.TxKindBuy, -800},
		{"o2", "LOAN", dy(2024, time.November, 1), canonical.TxKindSell, 200},
		{"e1", "PEETF", dy(2024, time.March, 1), canonical.TxKindBuy, -100},
		{"x1", "PUBX", dy(2024, time.March, 1), canonical.TxKindBuy, -100},
	} {
		inst := l.inst
		txs = append(txs, canonical.TransactionChange{SilverSourceID: "pl", TransactionExternalID: l.id,
			OccurredAt: l.at, AccountExternalID: "L1", InstrumentExternalID: &inst, Kind: l.kind,
			Currency: "USD", NetAmount: decp(l.amount)})
	}
	inTx(t, db, ctx, func(w *Writer) error { return w.InsertTransactions(ctx, txs) })
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertInstruments(ctx, []canonical.InstrumentChange{
			{SilverSourceID: "pl", InstrumentExternalID: "NOTE", AssetClass: canonical.AssetClassPrivateDebt,
				Vehicle: canonical.VehicleConvertibleNote, Currency: &usd},
			{SilverSourceID: "pl", InstrumentExternalID: "PEETF", AssetClass: canonical.AssetClassPrivateEquity,
				Vehicle: canonical.VehicleETF, Currency: &usd},
		})
	})
	jun30, dec31 := dy(2024, time.June, 30), dy(2024, time.December, 31)
	acquired := time.Date(2024, time.March, 15, 0, 0, 0, 0, time.UTC)
	line := func(inst string, at int64, class canonical.AssetClass, vehicle canonical.Vehicle, value, cost int64) canonical.PositionChange {
		i := inst
		c := canonical.PositionChange{SilverSourceID: "pl", AccountExternalID: "L1", SnapshotAt: at,
			PositionKey: inst, InstrumentExternalID: &i, AssetClass: class, Vehicle: vehicle, Currency: "USD",
			ValuedAt: at, MarketValue: decp(value)}
		if cost > 0 {
			c.SetBookValue(decp(cost), canonical.Basis{Origin: canonical.BasisStated,
				Method: canonical.BasisMethodPaidIn, Fees: canonical.BasisFeesIncluded})
			c.AcquisitionDate = &acquired
		}
		return c
	}
	pe, pd := canonical.AssetClassPrivateEquity, canonical.AssetClassPrivateDebt
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertPositions(ctx, []canonical.PositionChange{
			line("POS", jun30, pe, canonical.VehicleFund, 450, 400),
			line("BARE", jun30, pe, canonical.VehicleStock, 300, 0),
			line("BUYF", dec31, pe, canonical.VehicleFund, 900, 0),
			line("STK", dec31, pe, canonical.VehicleStock, 2000, 0),
			line("NOTE", dec31, pd, canonical.VehicleConvertibleNote, 260, 0),
			line("LOAN", dec31, pd, canonical.VehicleLoan, 600, 0),
			line("POS", dec31, pe, canonical.VehicleFund, 600, 400),
			line("BARE", dec31, pe, canonical.VehicleStock, 300, 0),
			line("PEETF", dec31, pe, canonical.VehicleFund, 120, 0),
			line("PUBX", dec31, canonical.AssetClassPublicEquity, canonical.VehicleStock, 130, 0),
		})
	})
}

// Every private-equity and private-debt holding is listed whatever
// funds it, a listed fund and a listed share are not, and a holding with
// no cash leg reads its stated cost, or none.
func TestPrivateMarketsListing(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedPrivateListing(t, db, ctx)
	rows, order := privateRows(t, db, ctx, "USD")
	if want := []string{"BUYF", "STK", "POS", "NOTE", "LOAN", "BARE", ""}; !slices.Equal(order, want) {
		t.Fatalf("rows %v, want %v: by first call, no ETF and no listed share", order, want)
	}
	kind := func(r PrivateRow) string { return *r.AssetClass + "/" + *r.Vehicle + "/" + *r.Status }
	for inst, want := range map[string]string{
		"BUYF": "private_equity/fund/partially_exited", "STK": "private_equity/stock/held",
		"NOTE": "private_debt/convertible_note/held", "LOAN": "private_debt/loan/partially_exited",
		"POS": "private_equity/fund/held", "BARE": "private_equity/stock/held",
	} {
		if got := kind(rows[inst]); got != want {
			t.Errorf("%s reads %s, want %s", inst, got, want)
		}
	}
	buyf := rows["BUYF"]
	if cents(t, buyf.PaidIn) != 102000 || cents(t, buyf.Distributed) != 30000 || !isNear(buyf.TVPI, 1200.0/1020) {
		t.Errorf("BUYF %s: the buy and the fee are paid in, the sale came back", privateRowText(buyf))
	}
	// POS reads its cost on the day it was acquired, a leg the IRR reads.
	pos := rows["POS"]
	acquired := time.Date(2024, time.March, 15, 0, 0, 0, 0, time.UTC).Unix()
	if cents(t, pos.PaidIn) != 40000 || cents(t, pos.NAV) != 60000 || pos.FirstCall != acquired ||
		!slices.Equal(pos.Quality, []string{"paid_in_from_cost"}) {
		t.Errorf("POS %s, first %d", privateRowText(pos), pos.FirstCall)
	}
	want, err := returns.XIRR(0, 600, EpochDay(acquired)-1, EpochDay(dy(2024, time.December, 31)),
		[]returns.Flow{{Day: EpochDay(acquired), Amount: 400}})
	if err != nil || !isNear(pos.IRR, want) {
		t.Errorf("POS irr %v, want %v (%v)", pos.IRR, want, err)
	}
	// BARE states no cost: no paid in, multiple or IRR, and it opens on
	// its first line.
	bare := rows["BARE"]
	if bare.PaidIn != nil || bare.DPI != nil || bare.TVPI != nil || bare.IRR != nil || cents(t, bare.NAV) != 30000 ||
		!slices.Equal(bare.Quality, []string{"no_cost"}) || EpochDay(bare.FirstCall) != EpochDay(dy(2024, time.June, 30)) {
		t.Errorf("BARE %s, first %d", privateRowText(bare), bare.FirstCall)
	}
	// A holding with no cost adds its NAV to the total, not to its
	// pooled multiples or its IRR, and the total says so.
	total := rows[""]
	if cents(t, total.PaidIn) != 297000 || cents(t, total.NAV) != 466000 || cents(t, total.Distributed) != 50000 ||
		!isNear(total.TVPI, (500.0+4360)/2970) || !isNear(total.DPI, 500.0/2970) || total.IRR == nil ||
		!slices.Contains(total.Quality, "no_cost=1") || !slices.Contains(total.Quality, "paid_in_from_cost=1") {
		t.Errorf("total %s", privateRowText(total))
	}
	// At June 30 POS reads the cost its first line states; before it
	// nothing.
	early, err := RunPrivate(ctx, db, dy(2024, time.June, 30)+43199, "USD")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range early {
		if r.InstrumentID == "POS" && (cents(t, r.PaidIn) != 40000 || cents(t, r.NAV) != 45000) {
			t.Errorf("POS on June 30 %s", privateRowText(r))
		}
	}
}

// status reads what came back and what is still marked, and the exited
// tag says the same: a NAV of 0 after a distribution is an exit, as is a
// holding that left the holdings; a NAV of 0 with nothing back is held.
// LINEONLY, marked on a line with no cost and then gone with no money
// either way, is not listed; LINEBACK, the same with a distribution, is
// exited.
func TestPrivateMarketsStatus(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedPrivateFixture(t, db, ctx)
	dec31 := dy(2024, time.December, 31)
	for _, h := range []struct {
		inst string
		back int64
	}{{"ZEROED", 150}, {"WRITTEN", 0}} {
		inst := h.inst
		txs := []canonical.TransactionChange{{SilverSourceID: "pm", TransactionExternalID: inst + "-c",
			OccurredAt: dy(2024, time.March, 5), AccountExternalID: "INV", InstrumentExternalID: &inst,
			Kind: canonical.TxKindContribution, Currency: "USD", NetAmount: decp(-100)}}
		if h.back > 0 {
			txs = append(txs, canonical.TransactionChange{SilverSourceID: "pm", TransactionExternalID: inst + "-d",
				OccurredAt: dy(2024, time.October, 5), AccountExternalID: "INV", InstrumentExternalID: &inst,
				Kind: canonical.TxKindDistribution, Currency: "USD", NetAmount: decp(h.back)})
		}
		inTx(t, db, ctx, func(w *Writer) error {
			if err := w.InsertTransactions(ctx, txs); err != nil {
				return err
			}
			return w.InsertPositions(ctx, []canonical.PositionChange{{SilverSourceID: "pm", AccountExternalID: "INV",
				SnapshotAt: dec31, PositionKey: inst, InstrumentExternalID: &inst,
				AssetClass: canonical.AssetClassPrivateEquity, Vehicle: canonical.VehicleFund, Currency: "USD",
				ValuedAt: dec31, MarketValue: decp(0)}})
		})
	}
	lineOnly, lineBack := "LINEONLY", "LINEBACK"
	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.InsertTransactions(ctx, []canonical.TransactionChange{{SilverSourceID: "pm",
			TransactionExternalID: "lb-d", OccurredAt: dy(2024, time.October, 5), AccountExternalID: "INV",
			InstrumentExternalID: &lineBack, Kind: canonical.TxKindDistribution, Currency: "USD",
			NetAmount: decp(120)}}); err != nil {
			return err
		}
		var lines []canonical.PositionChange
		for _, inst := range []string{lineOnly, lineBack} {
			i := inst
			lines = append(lines, canonical.PositionChange{SilverSourceID: "pm", AccountExternalID: "INV",
				SnapshotAt: dy(2024, time.March, 31), PositionKey: i, InstrumentExternalID: &i,
				AssetClass: canonical.AssetClassPrivateEquity, Vehicle: canonical.VehicleFund, Currency: "USD",
				MarketValue: decp(100)})
		}
		return w.InsertPositions(ctx, lines)
	})
	rows, _ := privateRows(t, db, ctx, "USD")
	for inst, want := range map[string]string{"FUND": "partially_exited", "LATE": "held", "SPV": "exited",
		"VOID": "exited", "GHOST": "held", "ZEROED": "exited", "WRITTEN": "held", "LINEBACK": "exited"} {
		r := rows[inst]
		if r.Status == nil || *r.Status != want || slices.Contains(r.Quality, "exited") != (want == "exited") {
			t.Errorf("%s status %v quality %v, want %s", inst, r.Status, r.Quality, want)
		}
	}
	if l, ok := rows[lineOnly]; ok {
		t.Errorf("LINEONLY %s: gone with no money either way, it is no investment", privateRowText(l))
	}
	if l := rows[lineBack]; cents(t, l.NAV) != 0 || cents(t, l.Distributed) != 12000 ||
		!slices.Equal(l.Quality, []string{"no_cost", "exited"}) {
		t.Errorf("LINEBACK %s", privateRowText(l))
	}
	if total := rows[""]; total.Status != nil || !slices.Contains(total.Quality, "exited=4") {
		t.Errorf("total status %v quality %v", total.Status, total.Quality)
	}
}

// A fund a custody account holds and the cash account of its portfolio
// pays for is one holding, though the calls name it by its ISIN and the
// position by another id that states the ISIN. It is named by the
// custody account and its position's instrument. The same fund in
// another portfolio is another holding, and one with no position takes
// the account of its first call.
func TestPrivateMarketsPortfolioHolding(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "pb", "ubs")
	usd, isin := "USD", "XX0000000001"
	var accts []canonical.AccountChange
	for _, a := range []struct{ id, portfolio string }{{"CASH1", "P1"}, {"CUST1", "P1"}, {"CASH2", "P2"}, {"CUST2", "P2"}} {
		pf := a.portfolio
		accts = append(accts, canonical.AccountChange{SilverSourceID: "pb", AccountExternalID: a.id,
			AccountKind: canonical.AccountKindCustody, BaseCurrency: &usd, PortfolioExternalID: &pf,
			FirstSeenAt: 1, LastSeenAt: 1})
	}
	inTx(t, db, ctx, func(w *Writer) error { return w.UpsertAccounts(ctx, accts) })
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertInstruments(ctx, []canonical.InstrumentChange{
			{SilverSourceID: "pb", InstrumentExternalID: "VAL1", ISIN: &isin, AssetClass: canonical.AssetClassPrivateEquity,
				Vehicle: canonical.VehicleFund, Currency: &usd},
			{SilverSourceID: "pb", InstrumentExternalID: isin, ISIN: &isin, AssetClass: canonical.AssetClassPrivateEquity,
				Vehicle: canonical.VehicleFund, Currency: &usd},
			{SilverSourceID: "pb", InstrumentExternalID: "ONLY", AssetClass: canonical.AssetClassPrivateEquity,
				Vehicle: canonical.VehicleFund, Currency: &usd},
		})
	})
	var txs []canonical.TransactionChange
	for _, l := range []struct {
		id, acct, inst string
		at             int64
		amount         int64
	}{
		{"c1", "CASH1", isin, dy(2024, time.February, 1), -1000}, {"c2", "CASH1", isin, dy(2024, time.May, 1), -500},
		{"c3", "CASH2", isin, dy(2024, time.March, 1), -200}, {"c4", "CASH1", "ONLY", dy(2024, time.April, 1), -300},
	} {
		inst := l.inst
		txs = append(txs, canonical.TransactionChange{SilverSourceID: "pb", TransactionExternalID: l.id,
			OccurredAt: l.at, AccountExternalID: l.acct, InstrumentExternalID: &inst, Kind: canonical.TxKindBuy,
			Currency: "USD", NetAmount: decp(l.amount)})
	}
	inTx(t, db, ctx, func(w *Writer) error { return w.InsertTransactions(ctx, txs) })
	dec31 := dy(2024, time.December, 31)
	val := "VAL1"
	inTx(t, db, ctx, func(w *Writer) error {
		var lines []canonical.PositionChange
		for _, l := range []struct {
			acct  string
			value int64
		}{{"CUST1", 1800}, {"CUST2", 260}} {
			lines = append(lines, canonical.PositionChange{SilverSourceID: "pb", AccountExternalID: l.acct, SnapshotAt: dec31,
				PositionKey: val, InstrumentExternalID: &val, AssetClass: canonical.AssetClassPrivateEquity,
				Vehicle: canonical.VehicleFund, Currency: "USD", MarketValue: decp(l.value)})
		}
		return w.InsertPositions(ctx, lines)
	})
	got, err := RunPrivate(ctx, db, PrivateLatest, "USD")
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ acct, inst, paid, nav string }
	var rows []row
	for _, r := range got {
		if !r.IsTotal {
			rows = append(rows, row{r.AccountExternalID, r.InstrumentID, strOrBlank(r.PaidIn), strOrBlank(r.NAV)})
		}
	}
	want := []row{{"CASH1", "ONLY", "300", ""}, {"CUST1", "VAL1", "1500", "1800"}, {"CUST2", "VAL1", "200", "260"}}
	if !slices.Equal(rows, want) {
		t.Errorf("rows %+v, want %+v", rows, want)
	}
	for _, r := range got {
		if r.InstrumentID == "VAL1" && (r.IRR == nil || slices.Contains(r.Quality, "no_mark")) {
			t.Errorf("%s %s: its calls and its mark read as one holding", r.AccountExternalID, privateRowText(r))
		}
	}
}

// Inside one portfolio, a fund that moves between two custody accounts
// is still one holding: the transfer out that a transfer in of the same
// quantity answers on its day is no exit. One the transfer in does not
// answer, of another quantity, is an exit. A fund held in two accounts
// with no call reads the cost of the lines each account holds, though
// their snapshots differ.
func TestPrivateMarketsPortfolioMoves(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedReturnsSource(t, db, ctx, "pb", "ubs")
	usd, pf := "USD", "P1"
	var accts []canonical.AccountChange
	for _, id := range []string{"CASH1", "CUSTA", "CUSTB"} {
		accts = append(accts, canonical.AccountChange{SilverSourceID: "pb", AccountExternalID: id,
			AccountKind: canonical.AccountKindCustody, BaseCurrency: &usd, PortfolioExternalID: &pf,
			FirstSeenAt: 1, LastSeenAt: 1})
	}
	inTx(t, db, ctx, func(w *Writer) error { return w.UpsertAccounts(ctx, accts) })
	moved := dy(2024, time.September, 1)
	tx := func(id, acct, inst string, at int64, kind canonical.TxKind, qty, amount int64) canonical.TransactionChange {
		return canonical.TransactionChange{SilverSourceID: "pb", TransactionExternalID: id, OccurredAt: at,
			AccountExternalID: acct, InstrumentExternalID: &inst, Kind: kind, Currency: "USD",
			Quantity: decp(qty), NetAmount: decp(amount)}
	}
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertTransactions(ctx, []canonical.TransactionChange{
			tx("m1", "CASH1", "MOVE", dy(2024, time.February, 1), canonical.TxKindBuy, 10, -1000),
			tx("m2", "CUSTA", "MOVE", moved, canonical.TxKindTransferOut, -10, -1600),
			tx("m3", "CUSTB", "MOVE", moved, canonical.TxKindTransferIn, 10, 1600),
			tx("s1", "CASH1", "SPLIT", dy(2024, time.February, 1), canonical.TxKindBuy, 10, -1000),
			tx("s2", "CUSTA", "SPLIT", moved, canonical.TxKindTransferOut, -10, -1600),
			tx("s3", "CUSTB", "SPLIT", moved, canonical.TxKindTransferIn, 4, 640),
		})
	})
	acquired := time.Date(2024, time.March, 15, 0, 0, 0, 0, time.UTC)
	line := func(acct, inst string, at, value, cost int64) canonical.PositionChange {
		i := inst
		c := canonical.PositionChange{SilverSourceID: "pb", AccountExternalID: acct, SnapshotAt: at,
			PositionKey: inst, InstrumentExternalID: &i, AssetClass: canonical.AssetClassPrivateEquity,
			Vehicle: canonical.VehicleFund, Currency: "USD", ValuedAt: at, MarketValue: decp(value)}
		if cost > 0 {
			c.SetBookValue(decp(cost), canonical.Basis{Origin: canonical.BasisStated,
				Method: canonical.BasisMethodPaidIn, Fees: canonical.BasisFeesIncluded})
			c.AcquisitionDate = &acquired
		}
		return c
	}
	jun30, dec30, dec31 := dy(2024, time.June, 30), dy(2024, time.December, 30), dy(2024, time.December, 31)
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertPositions(ctx, []canonical.PositionChange{
			line("CUSTA", "MOVE", jun30, 1500, 0), line("CUSTA", "SPLIT", jun30, 1500, 0),
			line("CUSTA", "TWO", dec30, 450, 400),
			line("CUSTB", "MOVE", dec31, 1800, 0), line("CUSTB", "SPLIT", dec31, 700, 0),
			line("CUSTB", "TWO", dec31, 350, 300),
		})
	})
	rows, _ := privateRows(t, db, ctx, "USD")
	if m := rows["MOVE"]; m.AccountExternalID != "CUSTB" || cents(t, m.PaidIn) != 100000 || cents(t, m.Distributed) != 0 ||
		cents(t, m.NAV) != 180000 || *m.Status != "held" || len(m.Quality) != 0 {
		t.Errorf("MOVE %s at %s: a move inside the portfolio is no exit", privateRowText(m), m.AccountExternalID)
	}
	if s := rows["SPLIT"]; cents(t, s.Distributed) != 160000 || *s.Status != "partially_exited" {
		t.Errorf("SPLIT %s: a transfer out no transfer in answers is an exit", privateRowText(s))
	}
	if two := rows["TWO"]; cents(t, two.PaidIn) != 70000 || cents(t, two.NAV) != 80000 ||
		!slices.Equal(two.Quality, []string{"paid_in_from_cost"}) {
		t.Errorf("TWO %s: its cost is both accounts' lines", privateRowText(two))
	}
}

// A private debt holding's interest, dividends and coupons are cash it
// returns: they count as distributed.
func TestPrivateMarketsDebtIncome(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedPrivateListing(t, db, ctx)
	note, stk := "NOTE", "STK"
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertTransactions(ctx, []canonical.TransactionChange{
			{SilverSourceID: "pl", TransactionExternalID: "i1", OccurredAt: dy(2024, time.October, 1), AccountExternalID: "L1",
				InstrumentExternalID: &note, Kind: canonical.TxKindInterest, Currency: "USD", NetAmount: decp(15)},
			{SilverSourceID: "pl", TransactionExternalID: "i2", OccurredAt: dy(2024, time.November, 1), AccountExternalID: "L1",
				InstrumentExternalID: &note, Kind: canonical.TxKindDividend, Currency: "USD", NetAmount: decp(5)},
			{SilverSourceID: "pl", TransactionExternalID: "i3", OccurredAt: dy(2024, time.November, 1), AccountExternalID: "L1",
				InstrumentExternalID: &stk, Kind: canonical.TxKindDividend, Currency: "USD", NetAmount: decp(40)},
			{SilverSourceID: "pl", TransactionExternalID: "i4", OccurredAt: dy(2024, time.December, 1), AccountExternalID: "L1",
				InstrumentExternalID: &note, Kind: canonical.TxKindCoupon, Currency: "USD", NetAmount: decp(10)},
		})
	})
	rows, _ := privateRows(t, db, ctx, "USD")
	if n := rows["NOTE"]; cents(t, n.Distributed) != 3000 || *n.Status != "partially_exited" {
		t.Errorf("NOTE %s: its interest, dividend and coupon came back", privateRowText(n))
	}
	if k := rows["STK"]; cents(t, k.Distributed) != 0 {
		t.Errorf("STK %s: a private equity holding's dividend is no distribution", privateRowText(k))
	}
}

// A holding whose lines never stated a value but zero, and from which
// nothing came back, has no mark: a source that carries a fund at zero
// until it values it states no value. One marked before and at zero now
// is written off and keeps its zero; the total adds no NAV for the first
// and pools its TVPI without it. LEFT0, never valued and then gone, has
// no mark and has exited.
func TestPrivateMarketsNeverValued(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedPrivateFixture(t, db, ctx)
	jun30, dec31 := dy(2024, time.June, 30), dy(2024, time.December, 31)
	for _, h := range []struct {
		inst           string
		before, latest int64
	}{{"UNVALUED", 0, 0}, {"WRITEOFF", 300, 0}, {"LEFT0", 0, -1}} {
		inst := h.inst
		inTx(t, db, ctx, func(w *Writer) error {
			if err := w.InsertTransactions(ctx, []canonical.TransactionChange{{SilverSourceID: "pm",
				TransactionExternalID: inst + "-c", OccurredAt: dy(2024, time.March, 5), AccountExternalID: "INV",
				InstrumentExternalID: &inst, Kind: canonical.TxKindContribution, Currency: "USD",
				NetAmount: decp(-300)}}); err != nil {
				return err
			}
			var lines []canonical.PositionChange
			for _, l := range []struct{ at, value int64 }{{jun30, h.before}, {dec31, h.latest}} {
				if l.value < 0 {
					continue
				}
				lines = append(lines, canonical.PositionChange{SilverSourceID: "pm", AccountExternalID: "INV",
					SnapshotAt: l.at, PositionKey: inst, InstrumentExternalID: &inst,
					AssetClass: canonical.AssetClassPrivateEquity, Vehicle: canonical.VehicleFund, Currency: "USD",
					Quantity: decp(250000), MarketValue: decp(l.value)})
			}
			return w.InsertPositions(ctx, lines)
		})
	}
	rows, _ := privateRows(t, db, ctx, "USD")
	if u := rows["UNVALUED"]; u.NAV != nil || u.TVPI != nil || u.MarkedAt != nil || u.IRR != nil ||
		!slices.Equal(u.Quality, []string{"no_mark"}) || *u.Status != "held" {
		t.Errorf("UNVALUED %s, marked %v: a zero never anything else is no mark", privateRowText(u), u.MarkedAt)
	}
	if w := rows["WRITEOFF"]; cents(t, w.NAV) != 0 || !isNear(w.TVPI, 0) || w.MarkedAt == nil || *w.Status != "held" {
		t.Errorf("WRITEOFF %s: a write-off keeps its zero", privateRowText(w))
	}
	if l := rows["LEFT0"]; l.NAV != nil || l.MarkedAt != nil || !slices.Equal(l.Quality, []string{"no_mark", "exited"}) ||
		*l.Status != "exited" {
		t.Errorf("LEFT0 %s: a holding that left has exited, valued or not", privateRowText(l))
	}
	if total := rows[""]; !slices.Contains(total.Quality, "no_mark=3") || cents(t, total.NAV) != 260000 {
		t.Errorf("total %s: it leaves the unvalued NAV out", privateRowText(total))
	}
}

// A pool that has paid back more than it took, with calls after the
// money came back, still has the IRR the solver finds: the flows change
// sign more than once, which mwr_nonunique says.
func TestPrivateIRRPastPaybackPooled(t *testing.T) {
	t.Parallel()
	flows := []returns.Flow{{Day: 100, Amount: 1000}, {Day: 300, Amount: -1800}, {Day: 400, Amount: 200}}
	rate, q := solveIRR(returns.XIRR, 0, 500, 99, 700, flows, false, true, nil)
	if rate == nil || !slices.Contains(q, "mwr_nonunique") {
		t.Errorf("rate %v quality %v", rate, q)
	}
	if rate, q := solveIRR(returns.XIRR, 0, 500, 99, 700, flows, false, false, nil); rate != nil ||
		!slices.Contains(q, "mwr_negative_net_capital") {
		t.Errorf("an account's return: rate %v quality %v", rate, q)
	}
}

// seedCapitalAccounts states the fixture's capital accounts: FUND's on
// its account at its two marks, the later one with its own remainder and
// TVPI; LATE's in CHF, naming no account, in two documents of one day;
// GHOST's, calls and no commitment; VOID's, a commitment and no calls;
// REFUND's, a private holding the view does not list; and ELSE's, no
// private holding's.
func seedCapitalAccounts(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	inv := "INV"
	tvpi := 1.25
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertReference(ctx, &canonical.SnapshotBatch{CapitalAccounts: []canonical.CapitalAccountChange{
			{SilverSourceID: "pm", AccountExternalID: &inv, InstrumentExternalID: "FUND", AsOf: dy(2024, time.March, 31),
				Currency: "USD", Commitment: decp(5000), Called: decp(1000), Distributed: decp(0), NAV: decp(1000)},
			{SilverSourceID: "pm", AccountExternalID: &inv, InstrumentExternalID: "FUND",
				AsOf: dy(2024, time.December, 31), Currency: "USD", Commitment: decp(5000), Called: decp(1900),
				Unfunded: decp(3000), Distributed: decp(300), NAV: decp(2100), TVPI: &tvpi},
			{SilverSourceID: "pm", InstrumentExternalID: "LATE", AsOf: dy(2024, time.March, 31), Currency: "CHF",
				Commitment: decp(1000), Called: decp(500)},
			{SilverSourceID: "pm", InstrumentExternalID: "LATE", AsOf: dy(2024, time.March, 31), SourceRef: "z",
				Currency: "CHF", Commitment: decp(7000), Called: decp(500)},
			{SilverSourceID: "pm", InstrumentExternalID: "GHOST", AsOf: dy(2024, time.April, 1), Currency: "USD",
				Called: decp(100)},
			{SilverSourceID: "pm", InstrumentExternalID: "VOID", AsOf: dy(2024, time.April, 1), Currency: "USD",
				Commitment: decp(50)},
			{SilverSourceID: "pm", InstrumentExternalID: "REFUND", AsOf: dy(2024, time.April, 1), Currency: "USD",
				Commitment: decp(300)},
			{SilverSourceID: "pm", InstrumentExternalID: "ELSE", AsOf: dy(2024, time.April, 1), Currency: "USD",
				Commitment: decp(900)},
		}})
	})
}

// seedCapitalHoldings adds holdings to the fixture that test how a capital
// account finds its holding: PAIR, held in INV and in INV2 of another
// portfolio, each account stating its own on another day, and a statement
// naming no account that both would take; ISINF, stated by its ISIN and
// called beyond its commitment; and NORATE, committed in a currency with
// no rate.
func seedCapitalHoldings(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	usd, inv, inv2, p2, isin := "USD", "INV", "INV2", "P2", "XX0000000021"
	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.UpsertAccounts(ctx, []canonical.AccountChange{{SilverSourceID: "pm", AccountExternalID: inv2,
			AccountKind: canonical.AccountKindCustody, BaseCurrency: &usd, PortfolioExternalID: &p2,
			FirstSeenAt: 1, LastSeenAt: 1}}); err != nil {
			return err
		}
		return w.UpsertInstruments(ctx, []canonical.InstrumentChange{{SilverSourceID: "pm",
			InstrumentExternalID: "ISINF", AssetClass: canonical.AssetClassPrivateEquity,
			Vehicle: canonical.VehicleFund, ISIN: &isin, Currency: &usd}})
	})
	var txs []canonical.TransactionChange
	for _, l := range []struct{ id, acct, inst string }{
		{"p1", inv, "PAIR"}, {"p2", inv2, "PAIR"}, {"i1", inv, "ISINF"}, {"n1r", inv, "NORATE"},
	} {
		inst := l.inst
		txs = append(txs, canonical.TransactionChange{SilverSourceID: "pm", TransactionExternalID: l.id,
			OccurredAt: dy(2024, time.March, 1), AccountExternalID: l.acct, InstrumentExternalID: &inst,
			Kind: canonical.TxKindContribution, Currency: "USD", NetAmount: decp(-100)})
	}
	inTx(t, db, ctx, func(w *Writer) error { return w.InsertTransactions(ctx, txs) })
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertReference(ctx, &canonical.SnapshotBatch{CapitalAccounts: []canonical.CapitalAccountChange{
			{SilverSourceID: "pm", AccountExternalID: &inv, InstrumentExternalID: "PAIR", AsOf: dy(2024, time.March, 31),
				Currency: "USD", Commitment: decp(2000), Called: decp(500)},
			{SilverSourceID: "pm", AccountExternalID: &inv2, InstrumentExternalID: "PAIR", AsOf: dy(2024, time.June, 30),
				Currency: "USD", Commitment: decp(3000), Called: decp(1000)},
			{SilverSourceID: "pm", InstrumentExternalID: "PAIR", AsOf: dy(2024, time.April, 1), Currency: "USD",
				Commitment: decp(4000)},
			{SilverSourceID: "pm", AccountExternalID: &inv, InstrumentExternalID: isin, AsOf: dy(2024, time.March, 31),
				Currency: "USD", Commitment: decp(1000), Called: decp(1200)},
			{SilverSourceID: "pm", AccountExternalID: &inv, InstrumentExternalID: "NORATE", AsOf: dy(2024, time.March, 31),
				Currency: "QLZ", Commitment: decp(500), Called: decp(100)},
		}})
	})
}

// A holding reads its latest capital account on or before the instant:
// the commitment and the unfunded at the account's day's rate, the
// stated remainder where there is one, else the commitment less the
// calls, never below 0, and 0 once the holding has exited; the source's
// TVPI, else the stated figures' multiples. A statement finds its holding
// by identity in its account's group, or in its source where it names no
// account, and each account of a source keeps its own. The total adds the
// commitments and counts the ones of a private holding no listed holding
// takes, and a commitment with no rate is unpriced.
func TestPrivateMarketsCapitalAccounts(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedPrivateFixture(t, db, ctx)
	seedCapitalAccounts(t, db, ctx)
	seedCapitalHoldings(t, db, ctx)
	// Rows by instrument, a holding of another account than INV under
	// instrument@account.
	at := func(asOf int64, ccy string) map[string]PrivateRow {
		got, err := RunPrivate(ctx, db, asOf, ccy)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]PrivateRow{}
		for _, r := range got {
			k := r.InstrumentID
			if r.AccountExternalID != "INV" && !r.IsTotal {
				k += "@" + r.AccountExternalID
			}
			out[k] = r
		}
		return out
	}
	latest := at(PrivateLatest, "USD")
	fund, late, total := latest["FUND"], latest["LATE"], latest[""]
	if cents(t, fund.Commitment) != 500000 || cents(t, fund.Unfunded) != 300000 || !isNear(fund.StatedTVPI, 1.25) ||
		!isNear(fund.StatedDPI, 300.0/1900) || fund.StatedAsOf == nil ||
		*fund.StatedAsOf != dy(2024, time.December, 31) {
		t.Errorf("FUND latest %s", privateRowText(fund))
	}
	if cents(t, late.Commitment) != 90000 || cents(t, late.Unfunded) != 45000 || late.StatedTVPI != nil ||
		late.StatedDPI != nil {
		t.Errorf("LATE latest %s: CHF at the account's day's rate, its first document of the day", privateRowText(late))
	}
	if spv := latest["SPV"]; spv.Commitment != nil || spv.Unfunded != nil || spv.StatedAsOf != nil {
		t.Errorf("SPV states no capital account: %s", privateRowText(spv))
	}
	// An account with no commitment states no unfunded. VOID has exited:
	// it owes no further calls, though its account states none.
	if ghost := latest["GHOST"]; ghost.Commitment != nil || ghost.Unfunded != nil || ghost.StatedAsOf == nil {
		t.Errorf("GHOST, called and no commitment: %s", privateRowText(ghost))
	}
	if void := latest["VOID"]; cents(t, void.Commitment) != 5000 || cents(t, void.Unfunded) != 0 ||
		*void.Status != "exited" {
		t.Errorf("VOID, exited with a commitment and no calls: %s", privateRowText(void))
	}
	// Each account of a source keeps its own statement of one instrument,
	// and the one naming no account, which both would take, takes neither.
	if p := latest["PAIR"]; cents(t, p.Commitment) != 200000 || cents(t, p.Unfunded) != 150000 {
		t.Errorf("PAIR in INV: %s", privateRowText(p))
	}
	if p := latest["PAIR@INV2"]; cents(t, p.Commitment) != 300000 || cents(t, p.Unfunded) != 200000 {
		t.Errorf("PAIR in INV2: %s", privateRowText(p))
	}
	// A statement by ISIN finds the holding of that identity; calls beyond
	// the commitment leave nothing unfunded.
	if f := latest["ISINF"]; cents(t, f.Commitment) != 100000 || cents(t, f.Unfunded) != 0 {
		t.Errorf("ISINF: %s", privateRowText(f))
	}
	if n := latest["NORATE"]; n.Commitment != nil || n.Unfunded != nil || !slices.Contains(n.Quality, "unpriced=1") {
		t.Errorf("NORATE, a commitment with no rate: %s %v", privateRowText(n), n.Quality)
	}
	if cents(t, total.Commitment) != 1195000 || cents(t, total.Unfunded) != 695000 || total.StatedTVPI != nil ||
		total.StatedAsOf != nil || !slices.Contains(total.Quality, "unmatched_commitment=2") ||
		!slices.Contains(total.Quality, "unpriced=1") {
		t.Errorf("total latest %s %v", privateRowText(total), total.Quality)
	}
	if chf := at(PrivateLatest, "CHF")["LATE"]; cents(t, chf.Commitment) != 100000 || cents(t, chf.Unfunded) != 50000 {
		t.Errorf("LATE in CHF %s", privateRowText(chf))
	}

	// Between FUND's two accounts the earlier stands: no remainder is
	// stated, so the commitment less the calls. VOID is still held, and
	// its account states no calls, so its own paid in, the whole
	// commitment, leaves nothing unfunded.
	apr := at(dy(2024, time.April, 30), "USD")
	if f := apr["FUND"]; cents(t, f.Commitment) != 500000 || cents(t, f.Unfunded) != 400000 ||
		!isNear(f.StatedTVPI, 1) || !isNear(f.StatedDPI, 0) || *f.StatedAsOf != dy(2024, time.March, 31) {
		t.Errorf("FUND on April 30 %s", privateRowText(f))
	}
	if void := apr["VOID"]; cents(t, void.Commitment) != 5000 || cents(t, void.Unfunded) != 0 {
		t.Errorf("VOID on April 30, a commitment, no calls and paid in whole: %s", privateRowText(void))
	}
	// Before any account, none.
	if f := at(dy(2024, time.February, 1), "USD")["FUND"]; f.Commitment != nil || f.StatedTVPI != nil {
		t.Errorf("FUND before its first account %s", privateRowText(f))
	}
	if q := at(dy(2024, time.February, 1), "USD")[""].Quality; slices.ContainsFunc(q, func(s string) bool {
		return strings.HasPrefix(s, "unmatched_commitment")
	}) {
		t.Errorf("total before REFUND's account %v", q)
	}
}

// TestPrivateMarketsUnfundedFromPaidIn: a capital account that states a
// commitment and no remainder, call or payment gives the holding's own
// calls to subtract, each in the commitment's currency at its own day's
// rate, the rest converted once at the account's day's rate. BELOW reads
// the difference, and BOUND, paid short by 1% exactly, its rest; NEAR,
// paid within 1% of it, and OVER, paid beyond it, read 0. FULL and PART
// are called in the first half of 2024 and stated at its end, after the
// CHF rate moved: in CHF, FULL still reads 0 and PART its USD rest at
// the account's day's rate. EXIT has left the holdings and owes nothing;
// NOPAY, a line with no call and no cost, reads none; NORATEC, called in
// a currency with no rate, reads none and is unpriced. The total adds
// what the holdings read.
func TestPrivateMarketsUnfundedFromPaidIn(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedPrivateFixture(t, db, ctx)
	inv, mar1, mar31, dec31 := "INV", dy(2024, time.March, 1), dy(2024, time.March, 31), dy(2024, time.December, 31)
	var txs []canonical.TransactionChange
	for _, l := range []struct {
		inst, ccy string
		amount    int64
	}{{"BELOW", "USD", -600}, {"BOUND", "USD", -990}, {"NEAR", "USD", -995}, {"OVER", "USD", -1100},
		{"FULL", "USD", -1000}, {"PART", "USD", -600}, {"EXIT", "USD", -400}, {"NORATEC", "QLZ", -500}} {
		inst := l.inst
		txs = append(txs, canonical.TransactionChange{SilverSourceID: "pm", TransactionExternalID: "u-" + inst,
			OccurredAt: mar1, AccountExternalID: inv, InstrumentExternalID: &inst,
			Kind: canonical.TxKindContribution, Currency: l.ccy, NetAmount: decp(l.amount)})
	}
	inTx(t, db, ctx, func(w *Writer) error { return w.InsertTransactions(ctx, txs) })
	nopay, exit := "NOPAY", "EXIT"
	line := func(inst *string, at int64) canonical.PositionChange {
		return canonical.PositionChange{SilverSourceID: "pm", AccountExternalID: inv,
			SnapshotAt: at, PositionKey: *inst, InstrumentExternalID: inst,
			AssetClass: canonical.AssetClassPrivateEquity, Vehicle: canonical.VehicleSPV, Currency: "USD",
			ValuedAt: at, MarketValue: decp(400)}
	}
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertPositions(ctx, []canonical.PositionChange{line(&nopay, mar31), line(&nopay, dec31),
			line(&exit, mar31)})
	})
	var caps []canonical.CapitalAccountChange
	for _, inst := range []string{"BELOW", "BOUND", "NEAR", "OVER", "FULL", "PART", "EXIT", "NOPAY", "NORATEC"} {
		asOf := mar31
		if inst == "FULL" || inst == "PART" {
			asOf = dec31
		}
		caps = append(caps, canonical.CapitalAccountChange{SilverSourceID: "pm", AccountExternalID: &inv,
			InstrumentExternalID: inst, AsOf: asOf, Currency: "USD", Commitment: decp(1000)})
	}
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertReference(ctx, &canonical.SnapshotBatch{CapitalAccounts: caps})
	})
	usd, _ := privateRows(t, db, ctx, "USD")
	for inst, want := range map[string]int64{"BELOW": 40000, "BOUND": 1000, "NEAR": 0, "OVER": 0, "FULL": 0,
		"PART": 40000, "EXIT": 0} {
		if r := usd[inst]; cents(t, r.Commitment) != 100000 || r.Unfunded == nil || cents(t, r.Unfunded) != want {
			t.Errorf("%s: %s, want unfunded %d cents", inst, privateRowText(r), want)
		}
	}
	if r := usd["EXIT"]; *r.Status != "exited" {
		t.Errorf("EXIT has left the holdings: %s", privateRowText(r))
	}
	if r := usd["NOPAY"]; cents(t, r.Commitment) != 100000 || r.PaidIn != nil || r.Unfunded != nil {
		t.Errorf("NOPAY, no paid in to subtract: %s", privateRowText(r))
	}
	if r := usd["NORATEC"]; cents(t, r.Commitment) != 100000 || r.Unfunded != nil ||
		!slices.Contains(r.Quality, "unpriced=2") {
		t.Errorf("NORATEC, a call with no rate and the unfunded it gives: %s %v", privateRowText(r), r.Quality)
	}
	if total := usd[""]; cents(t, total.Unfunded) != 81000 {
		t.Errorf("total unfunded %s, want the holdings' 810", privateRowText(total))
	}
	chf, _ := privateRows(t, db, ctx, "CHF")
	if r := chf["FULL"]; cents(t, r.Commitment) != 125000 || cents(t, r.Unfunded) != 0 {
		t.Errorf("FULL in CHF, paid in whole: %s", privateRowText(r))
	}
	if r := chf["PART"]; cents(t, r.Commitment) != 125000 || cents(t, r.Unfunded) != 50000 {
		t.Errorf("PART in CHF, 400 USD open at the account's day's rate: %s", privateRowText(r))
	}
}
