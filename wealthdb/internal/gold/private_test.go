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
//   - DIRECT, a buy, and a contribution that names no instrument, which
//     the view lists neither of.
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
		return canonical.PositionChange{SilverSourceID: "pm", AccountExternalID: "INV", SnapshotAt: at,
			PositionKey: inst, InstrumentExternalID: &i, AssetClass: canonical.AssetClassPrivateEquity,
			Vehicle: canonical.VehicleFund, Currency: "USD", ValuedAt: valued, MarketValue: decp(value)}
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
	rows, err := RunPrivate(ctx, db, ccy)
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
		t.Fatalf("rows %v, want %v: a buy, an unnamed call and one with no money are no holding", order, want)
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
	if total.NAV != nil || total.TVPI != nil || !isNear(total.DPI, 900.0/3250) {
		t.Errorf("total nav %v tvpi %v dpi %v: a holding with no value blanks the pooled tvpi",
			total.NAV, total.TVPI, total.DPI)
	}
	if !slices.Equal(total.Quality, []string{"no_mark=1", "exited=2", "call_after_mark=1", "exit_unvalued=1"}) {
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

// report_private_markets holds the CLI's rows in every currency, and
// web_private serves them: the dashboard and `returns private` agree.
func TestPrivateMarketsTableIsTheCLIsRows(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedPrivateFixture(t, db, ctx)
	m, err := computeMaterialPrivate(ctx, db)
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
	type cut struct {
		holding, paid, back, nav, quality string
		marked                            *int64
		dpi, tvpi, irr                    *float64
	}
	for _, ccy := range MaterializedCurrencies() {
		want, err := RunPrivate(ctx, db, ccy)
		if err != nil {
			t.Fatal(err)
		}
		got, err := scanRows(ctx, db, "web_private", `
			SELECT holding, CAST(paid_in AS VARCHAR), CAST(distributed AS VARCHAR), COALESCE(CAST(nav AS VARCHAR), ''),
			       quality, CAST(epoch(marked_at) AS BIGINT), dpi, tvpi, irr_pct / 100
			  FROM web_private WHERE currency = ? ORDER BY is_total, first_call, instrument_external_id`,
			[]any{ccy}, func(c *cut) []any {
				return []any{&c.holding, &c.paid, &c.back, &c.nav, &c.quality, i64(&c.marked),
					flt(&c.dpi), flt(&c.tvpi), flt(&c.irr)}
			})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: %d rows in the table, %d from the CLI", ccy, len(got), len(want))
		}
		for i, w := range want {
			g := got[i]
			nav := ""
			if w.NAV != nil {
				nav = *w.NAV
			}
			if g.holding != w.Holding || cents(t, &g.paid) != cents(t, w.PaidIn) || cents(t, &g.back) != cents(t, w.Distributed) ||
				(nav == "") != (g.nav == "") || nav != "" && cents(t, &g.nav) != cents(t, w.NAV) ||
				g.quality != strings.Join(w.Quality, ";") || (g.marked == nil) != (w.MarkedAt == nil) ||
				!nearPtr(g.dpi, w.DPI, 1e-12) || !nearPtr(g.tvpi, w.TVPI, 1e-12) || !nearPtr(g.irr, w.IRR, 1e-12) {
				t.Errorf("%s row %d: table %+v, CLI %+v", ccy, i, g, w)
			}
		}
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
		f(r.DPI), f(r.TVPI), f(r.IRR), strings.Join(r.Quality, ";")}, " ")
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
// private holding's: of DIRECT, which INV bought, and of XIN, which it
// took in; a transfer of cash out of INV; and of PUB2 out of INV2, whose
// one holding is a call refunded whole, so the view lists none of it.
func seedExitsOfNoPrivateHolding(t *testing.T, db *sql.DB, ctx context.Context, at int64) {
	t.Helper()
	tx := func(id, acct, inst string, on int64, kind canonical.TxKind, amount int64) canonical.TransactionChange {
		c := canonical.TransactionChange{SilverSourceID: "pm", TransactionExternalID: id, OccurredAt: on,
			AccountExternalID: acct, Kind: kind, Currency: "USD", NetAmount: decp(amount)}
		if inst != "" {
			c.InstrumentExternalID = &inst
		}
		return c
	}
	jan := dy(2024, time.January, 20)
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertTransactions(ctx, []canonical.TransactionChange{
			tx("o1", "INV", "DIRECT", at, canonical.TxKindTransferOut, -900),
			tx("o2", "INV", "XIN", jan, canonical.TxKindTransferIn, 250),
			tx("o3", "INV", "XIN", at, canonical.TxKindTransferOut, -250),
			tx("o4", "INV", "", at, canonical.TxKindTransferOut, -300),
			tx("o5", "INV2", "REF2", jan, canonical.TxKindContribution, -300),
			tx("o6", "INV2", "REF2", dy(2024, time.February, 20), canonical.TxKindContribution, 300),
			tx("o7", "INV2", "PUB2", at, canonical.TxKindTransferOut, -800),
		})
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
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertTransactions(ctx, []canonical.TransactionChange{
			{SilverSourceID: "pm", TransactionExternalID: "u1", OccurredAt: exit, AccountExternalID: "INV",
				InstrumentExternalID: &pub, Kind: canonical.TxKindTransferOut, Currency: "JPY", NetAmount: decp(-1000)},
			{SilverSourceID: "pm", TransactionExternalID: "u2", OccurredAt: exit, AccountExternalID: "INV",
				InstrumentExternalID: &pub2, Kind: canonical.TxKindTransferOut, Currency: "USD", NetAmount: decp(0)},
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
	if cents(t, total.Distributed) != cents(t, want.Distributed) || total.IRR != nil || want.IRR == nil {
		t.Errorf("total %s, without the exits %s: the sums leave the unpriced exit out, and the IRR is blank",
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
// quarter: the listed holdings' and the exit out of their account that
// names no holding, but no leg of a holding the view leaves out, of a
// buy or of another account, and none of the exits of no private
// holding (seedExitsOfNoPrivateHolding). What it adds up to is the
// total's paid in and distributed.
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
		quarter                    int64
		called, back, net, quality string
		calledTo, backTo, netTo    string
	}
	got, err := scanRows(ctx, db, "report_private_flows_multi", `
		SELECT quarter, CAST(called AS VARCHAR), CAST(distributed AS VARCHAR), CAST(net AS VARCHAR), quality,
		       CAST(called_to_date AS VARCHAR), CAST(distributed_to_date AS VARCHAR), CAST(net_to_date AS VARCHAR)
		  FROM report_private_flows_multi(?) WHERE currency = 'USD'`,
		[]any{privateAsOf}, func(f *flow) []any {
			return []any{&f.quarter, &f.called, &f.back, &f.net, &f.quality, &f.calledTo, &f.backTo, &f.netTo}
		})
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		quarter      int64
		called, back int64
		quality      string
	}
	// A quarter opens at midnight, UTC.
	q := func(y int, m time.Month, d int) int64 { return EpochDay(dy(y, m, d)) * 86400 }
	wants := []want{
		{q(2024, time.January, 1), 2150, 0, ""},
		{q(2024, time.April, 1), 1000, 0, ""},
		{q(2024, time.July, 1), 0, 900, "exit_unvalued=1"},
		{q(2024, time.October, 1), 500, 0, ""},
		{q(2025, time.January, 1), 0, 1000, ""},
	}
	if len(got) != len(wants) {
		t.Fatalf("%d quarters %+v, want %d", len(got), got, len(wants))
	}
	var calledTo, backTo int64
	for i, w := range wants {
		g := got[i]
		calledTo, backTo = calledTo+w.called, backTo+w.back
		if g.quarter != w.quarter || cents(t, &g.called) != 100*w.called || cents(t, &g.back) != 100*w.back ||
			cents(t, &g.net) != 100*(w.back-w.called) || g.quality != w.quality ||
			cents(t, &g.calledTo) != 100*calledTo || cents(t, &g.backTo) != 100*backTo ||
			cents(t, &g.netTo) != 100*(backTo-calledTo) {
			t.Errorf("quarter %d: %+v, want %+v", i, g, w)
		}
	}
	rows, _ := privateRows(t, db, ctx, "USD")
	if last := got[len(got)-1]; cents(t, &last.calledTo) != cents(t, rows[""].PaidIn) ||
		cents(t, &last.backTo) != cents(t, rows[""].Distributed) {
		t.Errorf("to date %s and %s, the total %s", last.calledTo, last.backTo, privateRowText(rows[""]))
	}
	// Each currency converts at the legs' own days: the first quarter's
	// calls at 0.90 USD to the CHF. The refunded call stays out of the
	// third quarter's.
	chf, err := scanRows(ctx, db, "report_private_flows_multi", `SELECT CAST(called AS VARCHAR)
		  FROM report_private_flows_multi(?) WHERE currency = 'CHF'`,
		[]any{privateAsOf}, func(c *string) []any { return []any{c} })
	if err != nil || len(chf) != len(wants) || cents(t, &chf[0]) != 238889 || cents(t, &chf[2]) != 0 {
		t.Errorf("CHF called %v (%v), want 2388.89 first and 0 third", chf, err)
	}
}
