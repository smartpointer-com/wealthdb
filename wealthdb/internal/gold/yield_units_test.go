package gold

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// A whole factor is n new units for each old one, or one for n old ones,
// exactly or short of a fraction paid in cash; anything else is none, and
// so is a quantity several factors round to.
func TestAWholeFactor(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		before, after, want float64
	}{
		{33, 660, 20},
		{100, 1000, 10},
		{10.5, 31, 3},
		{105, 10, 0.1},
		{1000, 50, 0.05},
		{100, 150, 0},
		{10, 19, 0},
		{0.05, 0.08, 0},
		{100, 100, 0},
		{7, 1, 0},
		{0.5, 1, 0},
	} {
		k, ok := wholeFactor(c.before, c.after/c.before)
		if ok != (c.want != 0) || (ok && math.Abs(k-c.want) > 1e-12) {
			t.Errorf("%v to %v: %v %v, want %v", c.before, c.after, k, ok, c.want)
		}
	}
}

// A step's trades explain its quantities, or its change is one of units:
// a corporate action's leg names it, or the price per unit moved by its
// inverse. A change neither names is a trade no transaction records, and
// so is a small delivery. The quantities give the factor; the legs count
// once each and must state it. A change of units no whole factor
// explains, or one the legs state otherwise, is not ok.
func TestAChangeOfUnits(t *testing.T) {
	t.Parallel()
	v := func(x float64) *float64 { return &x }
	for _, c := range []struct {
		what  string
		s     unitStep
		k     float64
		units bool
	}{
		{"a buy", unitStep{before: 100, after: 120, trades: []unitTrade{{at: 1, qty: 20}}}, 1, false},
		{"a 20-for-1 change with its leg", unitStep{before: 100, after: 2000,
			trades: []unitTrade{{at: 1, qty: 1900, corp: true}}}, 20, false},
		{"a leg that states the new total", unitStep{before: 100, after: 2000,
			trades: []unitTrade{{at: 1, qty: 2000, corp: true}}}, 20, false},
		{"a 2-for-1 leg stated twice", unitStep{before: 100, after: 200,
			trades: []unitTrade{{at: 1, qty: 100, corp: true}, {at: 1, qty: 100, corp: true}}}, 2, false},
		{"a buy at the leg's second, sorted first", unitStep{before: 100, after: 250,
			trades: []unitTrade{{at: 1, qty: 50}, {at: 1, qty: 100, corp: true}}}, 2, false},
		{"a 2-for-1 leg, then a buy", unitStep{before: 100, after: 250,
			trades: []unitTrade{{at: 1, qty: 100, corp: true}, {at: 2, qty: 50}}}, 2, false},
		{"a 1-for-10 change as one net leg", unitStep{before: 100, after: 10,
			trades: []unitTrade{{at: 1, qty: -90, corp: true}}}, 0.1, false},
		{"a leg that states another change", unitStep{before: 100, after: 200,
			trades: []unitTrade{{at: 1, qty: 300, corp: true}}}, 1, true},
		{"a scrip dividend", unitStep{before: 100, after: 102, trades: []unitTrade{{at: 1, qty: 2, corp: true}}}, 1,
			false},
		{"a reverse split of a small position", unitStep{before: 7, after: 1,
			trades: []unitTrade{{at: 1, qty: -7, corp: true}, {at: 1, qty: 1, corp: true}}}, 1, true},
		{"a reverse split as two legs", unitStep{before: 105, after: 10,
			trades: []unitTrade{{at: 1, qty: -105, corp: true}, {at: 1, qty: 10, corp: true}}}, 0.1, false},
		{"a buy before a 2-for-1 leg", unitStep{before: 100, after: 240,
			trades: []unitTrade{{at: 1, qty: 20}, {at: 2, qty: 120, corp: true}}}, 2, false},
		{"a 3-for-2 split with its leg", unitStep{before: 100, after: 150,
			trades: []unitTrade{{at: 1, qty: 50, corp: true}}}, 1, true},
		{"a 10-for-1 change the price shows", unitStep{before: 200, after: 2000, valueBefore: v(10000),
			valueAfter: v(10460)}, 10, false},
		{"a buy, then a 10-for-1 change", unitStep{before: 200, after: 2200, valueBefore: v(10000),
			valueAfter: v(10460), trades: []unitTrade{{at: 1, qty: 20}}}, 10, false},
		{"a 10-for-1 change, then a buy", unitStep{before: 200, after: 2020, valueBefore: v(10000),
			valueAfter: v(10460), trades: []unitTrade{{at: 1, qty: 20}}}, 10, false},
		{"a 1-for-10 change the price shows", unitStep{before: 2000, after: 200, valueBefore: v(10000),
			valueAfter: v(9800)}, 0.1, false},
		{"a buy no transaction records", unitStep{before: 100, after: 200, valueBefore: v(5000),
			valueAfter: v(10100)}, 1, false},
		{"a doubling with no price", unitStep{before: 100, after: 200}, 1, false},
		{"a 3-for-2 change the price shows", unitStep{before: 100, after: 150, valueBefore: v(3000),
			valueAfter: v(3100)}, 1, true},
		{"a sale no transaction records", unitStep{before: 100, after: 90, valueBefore: v(3000),
			valueAfter: v(2600)}, 1, false},
		{"a position opened", unitStep{before: 0, after: 100}, 1, false},
	} {
		k, ok := c.s.change()
		if math.Abs(k-c.k) > 1e-12 || ok == c.units {
			t.Errorf("%s: %v %v, want %v units %v", c.what, k, ok, c.k, c.units)
		}
	}
}

// A transaction moves its key's quantity as the lot policy reads it: a
// trade by its quantity, cash in lieu that names one as a sale, a
// corporate action's leg as a delivery.
func TestAUnitMove(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		c    lots.Classified
		qty  float64
		move float64
		corp bool
	}{
		{lots.Classified{Action: lots.Buy}, 10, 10, false},
		{lots.Classified{Action: lots.Sell}, -10, -10, false},
		{lots.Classified{Action: lots.Corporate, Rule: lots.CorpReorg}, 90, 90, true},
		{lots.Classified{Action: lots.Corporate, Rule: lots.CorpCashInLieu}, -0.5, -0.5, false},
		{lots.Classified{Action: lots.Corporate, Rule: lots.CorpReturnOfCapital}, 5, 0, false},
		{lots.Classified{Action: lots.Ignore}, 5, 0, false},
	} {
		if move, corp := unitMove(c.c, c.qty); move != c.move || corp != c.corp {
			t.Errorf("%+v %v: %v %v", c.c, c.qty, move, corp)
		}
	}
}

// A payment's quantity is in today's units: the changes since its day
// compound. Two accounts that show a change at one snapshot differently,
// or a span no account held through with its units read, leave it in its
// day's units. A cash account that never held the instrument reads its
// portfolio's accounts, one that sold among them, one first seen later
// not.
func TestAPaymentsUnits(t *testing.T) {
	t.Parallel()
	q := func(x float64) *float64 { return &x }
	ik := yield.InstrumentKey{Source: "src", Instrument: "QSTK"}
	in := &yield.Inputs{Accounts: map[yield.AccountKey]yield.Account{
		{Source: "src", Account: "CASH"}: {Portfolio: "P"}, {Source: "src", Account: "DEPOT"}: {Portfolio: "P"},
		{Source: "src", Account: "GONE"}: {Portfolio: "P"}, {Source: "src", Account: "NEW"}: {Portfolio: "P"}}}
	u := &yieldUnits{
		points: map[unitKey][]unitPoint{
			{"src", "DEPOT", "QSTK"}: {{at: 10, qty: q(100)}, {at: 30, prev: 20, qty: q(200)},
				{at: 50, prev: 40, qty: q(2000)}},
			{"src", "GONE", "QSTK"}: {{at: 10, qty: q(50)}, {at: 30, prev: 20, qty: q(0)}},
			{"src", "NEW", "QSTK"}:  {{at: 50, qty: q(0)}},
		},
		snaps: map[accountKey][]int64{{"src", "DEPOT"}: {10, 20, 30, 40, 50, 60},
			{"src", "GONE"}: {10, 20, 30, 40, 50, 60}, {"src", "NEW"}: {50, 60}},
		accounts: map[yield.InstrumentKey][]string{ik: {"DEPOT", "GONE", "NEW"}},
		changes:  map[yield.InstrumentKey][]unitChange{},
		read:     map[yield.InstrumentKey][][2]int64{},
		last:     map[string]int64{"src": 60},
	}
	u.addChange(ik, unitChange{at: 30, k: 2, ok: true})
	u.addChange(ik, unitChange{at: 50, k: 10, ok: true})
	u.cover()
	for _, c := range []struct {
		acct  string
		end   int64
		want  float64
		units bool
	}{
		{"DEPOT", 15, 100 * 20, false},
		{"DEPOT", 35, 200 * 10, false},
		{"DEPOT", 55, 2000, false},
		{"CASH", 15, 150 * 20, false},
		{"GONE", 35, 0, false},
		{"DEPOT", 5, 0, false},
	} {
		got, units := u.quantity(u.keysOf(in, "src", c.acct, "QSTK"), c.end)
		if (got == nil) != (c.want == 0) || (got != nil && *got != c.want) || units != c.units {
			t.Errorf("%s at %d: %v %v, want %v %v", c.acct, c.end, fmtFloatPtr(got), units, c.want, c.units)
		}
	}
	u.addChange(ik, unitChange{at: 50, k: 5, ok: true})
	if _, units := u.quantity(u.keysOf(in, "src", "DEPOT", "QSTK"), 35); !units {
		t.Error("two accounts that disagree at one snapshot")
	}
	// DEPOT states no quantity at 40: from 20 to 50 no account held it
	// with its units read.
	u.changes[ik] = nil
	u.points[unitKey{"src", "DEPOT", "QSTK"}] = []unitPoint{{at: 10, qty: q(100)}, {at: 40, prev: 30, qty: nil},
		{at: 50, prev: 40, qty: q(1000)}}
	u.read = map[yield.InstrumentKey][][2]int64{}
	u.cover()
	if _, units := u.quantity(u.keysOf(in, "src", "DEPOT", "QSTK"), 15); !units {
		t.Error("a span no account held through")
	}
	if _, units := u.quantity(u.keysOf(in, "src", "DEPOT", "QSTK"), 55); units {
		t.Error("a payment after the span")
	}
	// A cash account of another portfolio reads the one account of its
	// source that held the instrument; with two such accounts, none.
	in.Accounts[yield.AccountKey{Source: "src", Account: "OTHER"}] = yield.Account{Portfolio: "Q"}
	u.points[unitKey{"src", "DEPOT", "QONE"}] = []unitPoint{{at: 10, qty: q(40)}}
	u.accounts[yield.InstrumentKey{Source: "src", Instrument: "QONE"}] = []string{"DEPOT"}
	if got, _ := u.quantity(u.keysOf(in, "src", "OTHER", "QONE"), 15); got == nil || *got != 40 {
		t.Errorf("the one account: %v", fmtFloatPtr(got))
	}
	if got, _ := u.quantity(u.keysOf(in, "src", "OTHER", "QSTK"), 15); got != nil {
		t.Errorf("two accounts: %v", fmtFloatPtr(got))
	}
}

// A day's quantity between snapshots is the newest snapshot's, moved by
// the account's trades since, before the day: a stray trade inside a run
// of equal snapshots moves only the days after it before the next
// snapshot. A trade stamped at a snapshot's instant counts in it, as the
// lot engine reads it, unless the snapshot leaves it out, as a statement
// that leaves out its last day's trades does. A corporate action's leg is
// a change of units, no trade. A snapshot that holds none of the
// instrument moves by no trade: it may hold it by another id.
func TestAQuantityBetweenSnapshots(t *testing.T) {
	t.Parallel()
	q := func(x float64) *float64 { return &x }
	out, in, stray := unitKey{"src", "B1", "QOUT"}, unitKey{"src", "B1", "QIN"}, unitKey{"src", "B1", "QSTRAY"}
	corp, gone := unitKey{"src", "B2", "QCORP"}, unitKey{"src", "B2", "QGONE"}
	u := &yieldUnits{
		snaps: map[accountKey][]int64{{"src", "B1"}: {10, 20, 30, 40}, {"src", "B2"}: {10, 50, 60}},
		points: map[unitKey][]unitPoint{
			out:   {{at: 10, qty: q(100)}, {at: 30, prev: 20, qty: q(150)}},
			in:    {{at: 10, qty: q(100)}, {at: 20, prev: 10, qty: q(150)}},
			stray: {{at: 10, qty: q(100)}},
			corp:  {{at: 10, qty: q(100)}, {at: 50, prev: 10, qty: q(1030)}},
			gone:  {{at: 10, qty: q(100)}, {at: 60, prev: 50, qty: q(0)}},
		},
		trades: map[unitKey][]unitTrade{
			out:   {{at: 20, qty: 50}},
			in:    {{at: 20, qty: 50}},
			stray: {{at: 15, qty: 5}},
			corp:  {{at: 20, qty: 30}, {at: 40, qty: 900, corp: true}},
			gone:  {{at: 62, qty: 1}},
		},
		late: map[unitKey]map[int64]bool{},
	}
	for _, c := range []struct {
		k    unitKey
		at   int64
		want float64
	}{{out, 25, 150}, {in, 25, 150}, {stray, 15, 100}, {stray, 17, 105}, {stray, 35, 100}, {corp, 45, 130},
		{gone, 65, 0}} {
		if got := u.heldOn(c.k, c.at); got == nil || *got != c.want {
			t.Errorf("%s at %d: %v, want %v", c.k.iid, c.at, fmtFloatPtr(got), c.want)
		}
	}
	if got := u.heldOn(corp, 5); got != nil {
		t.Errorf("before the first snapshot: %v", *got)
	}
}

// A dividend is paid on what its account held of its instrument before
// its day, in the snapshots that speak for the account, in the
// instrument's units at the instant. The book is invented: monthly
// snapshots and one early in June, each holding every position held then,
// and a statement's snapshot of another account on the morning of 30
// April, which says nothing of the rest.
//
//   - QADR changes its ratio 20 for 1 after its last dividend, a
//     corporate action delivering the new units; QTEN 10 for 1 with no
//     row, its value carrying on; QBUY doubles with no row and its price
//     steady, a buy no transaction records; Q32 splits 3 for 2 with a
//     row, which no whole factor explains: its rate stays and it carries
//     units.
//   - QTRIM is trimmed between its last record day and its pay day.
//   - QSTK is held in B2, and until June in SOLD of the same portfolio,
//     whose last snapshot lists it at no cent, no holding: SOLD's
//     dividends stay its sold position's, and B2's holding shows one row
//     a pay day. SOLD sells out on 1 June, after its record day: its June
//     one rates on the 300 it held then, the days it held none in its
//     record window no record day. Its realized yield pools both
//     accounts. The early June snapshot leaves SOLD out, and is not its
//     last, so SOLD's May amounts hold to its last snapshot.
//   - QBNK is held in DEPOT, its dividends booked to CASH beside it.
//   - QLATE's source took its first snapshot after its first dividend.
//   - QALS is held throughout, but the snapshots of 31 May and 5 June name
//     it by its CUSIP, an instrument its source folds into it
//     (instrument_aliases): its June dividend is paid on what both held.
//   - QSTMT is bought up at the instant of the March snapshot, which leaves
//     the buy out: its May dividend, its record day after the buy, is paid
//     on what it held after it.
func TestADividendIsPaidOnWhatItsAccountHeld(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	type seg struct {
		from       time.Time
		qty, value float64
	}
	type line struct {
		src, acct, iid string
		segs           []seg
	}
	early := ymd(2025, 1, 1)
	lines := []line{
		{"src", "B1", "QADR", []seg{{early, 100, 3000}, {ymd(2026, 6, 30), 2000, 3100}}},
		{"src", "B1", "QTEN", []seg{{early, 200, 10000}, {ymd(2026, 5, 31), 2000, 10460}}},
		{"src", "B1", "QBUY", []seg{{early, 100, 5000}, {ymd(2026, 1, 31), 200, 10100}}},
		{"src", "B1", "Q32", []seg{{early, 100, 3000}, {ymd(2026, 6, 30), 150, 3100}}},
		{"src", "B1", "QTRIM", []seg{{early, 400, 20000}, {ymd(2026, 6, 5), 100, 5000}}},
		{"src", "B2", "QSTK", []seg{{early, 100, 5000}}},
		{"src", "SOLD", "QSTK", []seg{{early, 300, 15000}, {ymd(2026, 6, 5), 0, 0}}},
		{"src", "DEPOT", "QBNK", []seg{{early, 50, 2500}}},
		{"src", "B2", "QBND", []seg{{early, 1000, 1000}}},
		{"src", "SOLD", "QBND", []seg{{early, 2000, 2000}, {ymd(2026, 6, 5), 0, 0}}},
		{"late", "L1", "QLATE", []seg{{early, 100, 5000}}},
		{"src", "B1", "QALS", []seg{{early, 100, 5000}}},
		{"src", "B1", "QSTMT", []seg{{early, 100, 5000}, {ymd(2026, 4, 30), 180, 9000}}},
	}
	var snaps []time.Time
	for m := 0; m <= 12; m++ {
		snaps = append(snaps, ymd(2025, 7+m, 1).AddDate(0, 0, -1))
	}
	snaps = append(snaps, ymd(2026, 6, 5))
	var positions []string
	for _, l := range lines {
		for _, d := range snaps {
			if l.src == "late" && d.Before(ymd(2026, 1, 31)) {
				continue
			}
			var s seg
			for _, x := range l.segs {
				if !x.from.After(d) {
					s = x
				}
			}
			if s.qty == 0 {
				continue
			}
			class := "'public_equity', 'stock'"
			if l.iid == "QBND" {
				class = "'fixed_income', 'bond'"
			}
			iid := l.iid
			if iid == "QALS" && (d.Equal(ymd(2026, 5, 31)) || d.Equal(ymd(2026, 6, 5))) {
				iid = "000000AA0"
			}
			positions = append(positions, "('"+l.src+"', "+itoa(d.Unix()+43200)+", '"+l.acct+"', '"+iid+"', '"+
				iid+"', "+class+", 'USD', "+ftoa(s.qty)+", "+ftoa(s.value)+")")
		}
	}
	positions = append(positions, "('src', "+itoa(ymd(2026, 4, 30).Unix()+21600)+
		", 'FUND', 'QFND', 'QFND', 'public_equity', 'stock', 'USD', 10, 100)",
		"('src', "+itoa(ymd(2026, 6, 30).Unix()+43200)+
			", 'SOLD', 'QSTK', 'QSTK', 'public_equity', 'stock', 'USD', 0.00001, 0.001)")
	var txns, kinds []string
	txn := func(src, acct, iid string, day time.Time, kind string, amount, qty float64) {
		id := "T" + itoa(int64(len(txns)))
		txns = append(txns, "('"+src+"', '"+id+"', "+itoa(day.Unix()+36000)+", '"+acct+"', '"+kind+"', 'USD', "+
			ftoa(amount)+", '"+iid+"', "+ftoa(qty)+")")
		income := map[string]string{"dividend": "INCOME_DIVIDENDS", "interest": "INCOME_INTEREST_EARNED"}[kind]
		if income != "" {
			kinds = append(kinds, "('"+src+"', '"+id+"', 'sig-"+iid+"', 1, '"+income+"', 'rule', 100)")
		}
	}
	quarters := func(m, d int) []time.Time {
		return []time.Time{ymd(2025, m, d), ymd(2025, m+3, d), ymd(2025, m+6, d), ymd(2025, m+9, d)}
	}
	for _, d := range quarters(8, 15) {
		txn("src", "B1", "QADR", d, "dividend", 50, 0)
	}
	txn("src", "B1", "QADR", ymd(2026, 6, 10), "corporate_action", 0, 1900)
	for _, d := range quarters(7, 20) {
		txn("src", "B1", "QTEN", d, "dividend", 100, 0)
	}
	for i, d := range quarters(9, 10) {
		txn("src", "B1", "QBUY", d, "dividend", []float64{50, 50, 100, 100}[i], 0)
	}
	for _, d := range quarters(7, 25) {
		txn("src", "B1", "Q32", d, "dividend", 50, 0)
	}
	txn("src", "B1", "Q32", ymd(2026, 6, 12), "corporate_action", 0, 50)
	for _, d := range quarters(9, 10) {
		txn("src", "B1", "QTRIM", d, "dividend", 200, 0)
	}
	txn("src", "B1", "QTRIM", ymd(2026, 6, 3), "sell", 15000, -300)
	for _, d := range quarters(9, 5) {
		txn("src", "B2", "QSTK", d, "dividend", 50, 0)
		txn("src", "SOLD", "QSTK", d, "dividend", 150, 0)
		txn("src", "CASH", "QBNK", d, "dividend", 25, 0)
	}
	txn("src", "SOLD", "QSTK", ymd(2026, 6, 1), "sell", 15000, -300)
	for _, d := range []time.Time{ymd(2025, 9, 15), ymd(2026, 3, 15)} {
		txn("src", "B2", "QBND", d, "interest", 25, 0)
		txn("src", "SOLD", "QBND", d, "interest", 50, 0)
	}
	txn("src", "SOLD", "QBND", ymd(2026, 6, 1), "sell", 2000, -2000)
	txn("late", "L1", "QLATE", ymd(2025, 11, 10), "dividend", 50, 0)
	txn("late", "L1", "QLATE", ymd(2026, 2, 10), "dividend", 50, 0)
	for _, d := range quarters(9, 10) {
		txn("src", "B1", "QALS", d, "dividend", 50, 0)
	}
	for i, d := range []time.Time{ymd(2025, 7, 15), ymd(2025, 10, 15), ymd(2026, 1, 15), ymd(2026, 5, 15)} {
		txn("src", "B1", "QSTMT", d, "dividend", []float64{100, 100, 100, 180}[i], 0)
	}
	txns = append(txns, "('src', 'TSTMT', "+itoa(ymd(2026, 3, 31).Unix()+43200)+
		", 'B1', 'buy', 'USD', -4000, 'QSTMT', 80)")
	execEach(t, db,
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		    first_seen_at, last_seen_at, portfolio_external_id) VALUES
		    ('src', 'B1', 'brokerage', 'Brokerage', 'USD', 0, 0, NULL),
		    ('src', 'B2', 'brokerage', 'Second', 'USD', 0, 0, 'P'),
		    ('src', 'SOLD', 'brokerage', 'Sold', 'USD', 0, 0, 'P'),
		    ('src', 'CASH', 'cash', 'Cash', 'USD', 0, 0, 'BANK'),
		    ('src', 'DEPOT', 'brokerage', 'Custody', 'USD', 0, 0, 'BANK'),
		    ('src', 'FUND', 'brokerage', 'Statement', 'USD', 0, 0, NULL),
		    ('late', 'L1', 'brokerage', 'Late', 'USD', 0, 0, NULL)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, name, currency,
		    first_seen_at, last_seen_at) VALUES
		    ('src', 'QADR', 'public_equity', 'stock', 'QADR', 'Example Depositary', 'USD', 0, 0),
		    ('src', 'QTEN', 'public_equity', 'stock', 'QTEN', 'Example Ten', 'USD', 0, 0),
		    ('src', 'QBUY', 'public_equity', 'stock', 'QBUY', 'Example Buy', 'USD', 0, 0),
		    ('src', 'Q32', 'public_equity', 'stock', 'Q32', 'Example Three Two', 'USD', 0, 0),
		    ('src', 'QTRIM', 'public_equity', 'stock', 'QTRIM', 'Example Trim', 'USD', 0, 0),
		    ('src', 'QSTK', 'public_equity', 'stock', 'QSTK', 'Example Co', 'USD', 0, 0),
		    ('src', 'QBNK', 'public_equity', 'stock', 'QBNK', 'Example Bank', 'USD', 0, 0),
		    ('src', 'QBND', 'fixed_income', 'bond', NULL, 'EXAMPLE NOTE', 'USD', 0, 0),
		    ('src', 'QFND', 'public_equity', 'stock', 'QFND', 'Example Fund', 'USD', 0, 0),
		    ('late', 'QLATE', 'public_equity', 'stock', 'QLATE', 'Example Late', 'USD', 0, 0),
		    ('src', 'QALS', 'public_equity', 'stock', 'QALS', 'Example Alias', 'USD', 0, 0),
		    ('src', 'QSTMT', 'public_equity', 'stock', 'QSTMT', 'Example Statement', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, cusip, symbol, name,
		    currency, first_seen_at, last_seen_at) VALUES
		    ('src', '000000AA0', 'public_equity', 'stock', '000000AA0', 'QALS', 'Example Alias', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value) VALUES `+strings.Join(positions, ", "),
		`INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id, kind,
		    currency, net_amount, instrument_external_id, quantity) VALUES `+strings.Join(txns, ", "),
		`INSERT INTO income_txn_enrichment (silver_source_id, transaction_external_id, payer_signature,
		    signature_version, income_detailed, provenance, assigned_at) VALUES `+strings.Join(kinds, ", "))
	rebuildExposureWithDefaults(t, db, ctx)
	if _, err := RebuildYield(ctx, db); err != nil {
		t.Fatal(err)
	}
	in, err := YieldInputs(ctx, db, ymd(2026, 6, 30).Unix()+86399, "USD")
	if err != nil {
		t.Fatal(err)
	}
	b := yield.Compute(in)
	key := func(src, acct, iid string) yield.HoldingKey {
		return yield.HoldingKey{Source: src, Account: acct, PositionKey: iid}
	}
	for _, h := range in.Holdings {
		if h.Account == "SOLD" {
			t.Errorf("a position at no amount is a holding: %+v", h.HoldingKey)
		}
	}
	for _, c := range []struct {
		acct, iid string
		want      float64
	}{
		{"B1", "QADR", 50.0 / 2000 * 4 / (3100.0 / 2000) * 100},
		{"B1", "QTEN", 100.0 / 2000 * 4 / (10460.0 / 2000) * 100},
		{"B1", "QBUY", 0.5 * 4 / (10100.0 / 200) * 100},
		{"B1", "Q32", 0.5 * 4 / (3100.0 / 150) * 100},
		{"B1", "QTRIM", 0.5 * 4 / 50 * 100},
		{"B2", "QSTK", 0.5 * 4 / 50 * 100},
		{"DEPOT", "QBNK", 0.5 * 4 / 50 * 100},
		{"B1", "QALS", 0.5 * 4 / 50 * 100},
		{"B1", "QSTMT", 1.0 * 4 / 50 * 100},
	} {
		p := position(b, key("src", c.acct, c.iid))
		if p.DividendYield == nil || math.Abs(*p.DividendYield-c.want) > 1e-9 {
			t.Errorf("%s: dividend yield %s, want %v", c.iid, fmtFloatPtr(p.DividendYield), c.want)
		}
		if p.Tags[yield.TagUnits] != (c.iid == "Q32") {
			t.Errorf("%s: tags %v", c.iid, p.Tags)
		}
	}
	if ps := in.Payments[key("src", "B1", "QSTMT")]; len(ps) != 4 || ps[3].Record == nil || *ps[3].Record != 180 {
		t.Errorf("QSTMT: payments %+v", ps)
	}
	if ps := in.Payments[key("src", "SOLD", "QSTK")]; len(ps) != 4 || ps[3].Record == nil || *ps[3].Record != 300 ||
		ps[3].Least == nil || *ps[3].Least != 300 || *ps[3].Most != 300 {
		t.Errorf("SOLD: payments %+v", ps)
	}
	late := in.Payments[key("late", "L1", "QLATE")]
	if len(late) != 2 || late[0].Quantity != nil || late[1].Quantity == nil || *late[1].Quantity != 100 {
		t.Errorf("before the first snapshot: %+v", late)
	}
	// SOLD's amounts to its last snapshot, and its last day's, pool with
	// B2's.
	sold := ymd(2026, 6, 30).Sub(ymd(2025, 7, 1)).Hours() / 24
	former := in.FormerDaily[yield.InstrumentKey{Source: "src", Instrument: "QSTK"}]
	if len(former) != 1 || former[0].Days != 365 || math.Abs(former[0].Mean()-(15000*sold+0.001)/365) > 1e-9 {
		t.Errorf("former %+v", former)
	}
	b2, gone := key("src", "B2", "QSTK"), key("src", "SOLD", "QSTK")
	if len(in.Payments[b2]) != 4 || len(in.Payments[gone]) != 4 || in.Income[b2].Total() != 4*50 ||
		in.Income[gone].Total() != 4*150 {
		t.Errorf("B2's %v %v; SOLD's %v %v", in.Payments[b2], in.Income[b2], in.Payments[gone], in.Income[gone])
	}
	received := 4*50 + 4*150.0
	mean := (5000*365 + 15000*sold + 0.001) / 365
	if p := position(b, key("src", "B2", "QSTK")); p.Realized == nil || math.Abs(*p.Realized-received/mean*100) > 1e-9 {
		t.Errorf("pooled realized %s, want %v", fmtFloatPtr(p.Realized), received/mean*100)
	}
	// SOLD's coupons stay its sold bond's, which pools them with its
	// amounts.
	coupons := in.Income[key("src", "SOLD", "QBND")][yield.TypeCoupons]
	mean = (1000*365 + 2000*sold) / 365
	if p := position(b, key("src", "B2", "QBND")); coupons != 100 || p.Realized == nil ||
		math.Abs(*p.Realized-150/mean*100) > 1e-9 {
		t.Errorf("the bond: SOLD's coupons %v, pooled realized %s, want %v", coupons, fmtFloatPtr(p.Realized),
			150/mean*100)
	}
	// The income views name SOLD's lines by its sold positions.
	got, err := IncomeReceived(ctx, db, in, ymd(2025, 7, 1).Unix(), ymd(2026, 6, 30).Unix()+86399)
	if err != nil {
		t.Fatal(err)
	}
	named := map[string]int{}
	for _, l := range got {
		if l.Holding.Account == "SOLD" || l.Holding.Account == "B2" {
			named[l.Holding.Account+"/"+l.Holding.PositionKey]++
		}
	}
	if named["SOLD/QSTK"] != 4 || named["SOLD/QBND"] != 2 || named["B2/QSTK"] != 4 || named["B2/QBND"] != 2 {
		t.Errorf("received lines %v", named)
	}
}

func ftoa(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// The record window reads what the account held as each of its days
// began, from its furthest to its nearest, on the days it held some. Each
// share of the United States pays 0.6 a share on 11 June, its window 7 May
// to 8 June, and is priced at 50. The book is invented:
//
//   - QFAR1 sells a quarter of its 400 on 7 May, the window's first day:
//     it held 400 and 300 across it, and rates nothing. QFAR2 sells it on
//     6 May, the day before: it rates on the 300 it held.
//   - QDAY, in an account whose snapshots held none of it before, shows
//     400 from 10 May, inside the window, before its record day: the days
//     it held none are no record day, and it rates on 400.
//   - QSEEN's account shows its first snapshot on 10 May, inside the
//     window: it rates on its pay day's 400, guarded by the first quantity
//     it shows after its record day, as QAFTER does, first seen on 13 May,
//     after its record day.
//   - QEND, a share of Italy quoted in euros, pays on Wednesday 3 June,
//     its record instant Monday 1 June, the window's nearest day. A
//     snapshot stamped at that instant counts a sale of 120 of its 340:
//     the window reads it too, and QEND rates nothing.
//   - QTWO is held in two accounts, one snapshot valuing it in dollars and
//     one in francs at the same instant, and no listing currency: the two
//     markets disagree, and it reads no lag.
func TestARecordWindowReadsWhatTheAccountHeld(t *testing.T) {
	t.Parallel()
	pay, early := ymd(2026, 6, 11), ymd(2025, 1, 1)
	row := func(acct, iid string, at time.Time, qty float64) string {
		return "('day', " + itoa(at.Unix()+43200) + ", '" + acct + "', '" + iid + "', '" + iid +
			"', 'public_equity', 'stock', 'USD', " + ftoa(qty) + ", " + ftoa(qty*50) + ")"
	}
	rows := []string{row("D1", "QPAD", ymd(2026, 5, 10), 100), row("D3", "QAFTER", ymd(2026, 5, 13), 400)}
	for _, d := range []time.Time{ymd(2026, 5, 10), ymd(2026, 5, 31), ymd(2026, 6, 30)} {
		rows = append(rows, row("D1", "QDAY", d, 400), row("D2", "QSEEN", d, 400))
	}
	for _, d := range []time.Time{ymd(2026, 5, 31), ymd(2026, 6, 30)} {
		rows = append(rows, row("D3", "QAFTER", d, 400))
	}
	end := ymd(2026, 6, 1).Unix()
	in, _, _ := dividendBook{instruments: []bookInstrument{
		{"src", "QFAR1", "", "", "QFAR1"}, {"src", "QFAR2", "", "", "QFAR2"}, {"day", "QPAD", "", "", "QPAD"},
		{"day", "QDAY", "", "", "QDAY"}, {"day", "QSEEN", "", "", "QSEEN"}, {"day", "QAFTER", "", "", "QAFTER"},
		{"src", "QEND", "IT0000000008", "", "QEND"}, {"src", "QTWO", "", "", "QTWO"},
	}, lines: []bookLine{
		{"src", "B1", "QFAR1", []bookSeg{{early, 400, 20000}, {ymd(2026, 5, 31), 300, 15000}}},
		{"src", "B1", "QFAR2", []bookSeg{{early, 400, 20000}, {ymd(2026, 5, 31), 300, 15000}}},
		{"day", "D1", "QPAD", []bookSeg{{early, 100, 5000}}},
		{"src", "B3", "QEND", []bookSeg{{early, 340, 17000}, {ymd(2026, 6, 30), 220, 11000}}},
		{"src", "B1", "QTWO", []bookSeg{{early, 400, 20000}, {ymd(2026, 6, 30), 200, 10000}}},
		{"src", "B4", "QTWO", []bookSeg{{early, 100, 5000}}},
	}, txns: []bookTxn{
		{"src", "B1", "QFAR1", ymd(2026, 5, 7), "sell", 5000, -100}, {"src", "B1", "QFAR1", pay, "dividend", 180, 0},
		{"src", "B1", "QFAR2", ymd(2026, 5, 6), "sell", 5000, -100}, {"src", "B1", "QFAR2", pay, "dividend", 180, 0},
		{"day", "D1", "QDAY", pay, "dividend", 240, 0}, {"day", "D2", "QSEEN", pay, "dividend", 240, 0},
		{"day", "D3", "QAFTER", pay, "dividend", 240, 0}, {"src", "B3", "QEND", ymd(2026, 6, 3), "dividend", 170, 0},
		{"src", "B1", "QTWO", ymd(2026, 6, 1), "sell", 10000, -200}, {"src", "B1", "QTWO", pay, "dividend", 240, 0},
	}, currencies: map[string]string{"QEND": "EUR"}, after: []string{
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
		    instrument_external_id, asset_class, vehicle, currency, quantity, market_value) VALUES ` +
			strings.Join(rows, ", ") + `, ('src', ` + itoa(end) + `, 'B3', 'QEND', 'QEND', 'public_equity', 'stock',
		    'EUR', 220, 11000)`,
		`INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at, account_external_id, kind,
		    currency, net_amount, instrument_external_id, quantity)
		    VALUES ('src', 'TEND', ` + itoa(end) + `, 'B3', 'sell', 'EUR', 6000, 'QEND', -120)`,
		`INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
		    VALUES ('fx', 0, 'USD', 'EUR', 1.1)`,
		`UPDATE positions SET currency = 'CHF' WHERE position_key = 'QTWO' AND account_external_id = 'B4'`,
		`UPDATE instruments SET currency = NULL WHERE instrument_external_id = 'QTWO'`,
	}}.inputs(t)
	b := yield.Compute(in)
	for _, c := range []struct {
		src, acct, iid      string
		lag                 int
		record, least, most float64
		pct                 float64
	}{
		{"src", "B1", "QFAR1", 30, 300, 300, 400, 0}, {"src", "B1", "QFAR2", 30, 300, 300, 300, 1.2},
		{"day", "D1", "QDAY", 30, 400, 400, 400, 1.2}, {"day", "D2", "QSEEN", 30, 0, 0, 0, 1.2},
		{"day", "D3", "QAFTER", 30, 0, 0, 0, 1.2}, {"src", "B3", "QEND", 2, 220, 220, 340, 0},
	} {
		k := yield.HoldingKey{Source: c.src, Account: c.acct, PositionKey: c.iid}
		ps := in.Payments[k]
		if len(ps) != 1 || ps[0].LagDays != c.lag || (ps[0].Record == nil) != (c.record == 0) ||
			(c.record != 0 && (*ps[0].Record != c.record || ps[0].Least == nil || *ps[0].Least != c.least ||
				*ps[0].Most != c.most)) || (c.record == 0 && (ps[0].Before == nil || *ps[0].Before != 400)) {
			t.Errorf("%s: payments %+v", c.iid, ps)
			continue
		}
		p := position(b, k)
		if (p.DividendYield == nil) != (c.pct == 0) || (c.pct != 0 && math.Abs(*p.DividendYield-c.pct) > 1e-9) {
			t.Errorf("%s: dividend yield %s, want %v", c.iid, fmtFloatPtr(p.DividendYield), c.pct)
		}
	}
	if ps := in.Payments[yield.HoldingKey{Source: "src", Account: "B1", PositionKey: "QTWO"}]; len(ps) != 1 ||
		ps[0].LagDays != 0 || ps[0].Record != nil {
		t.Errorf("QTWO: payments %+v", ps)
	}
}

// A dividend is paid on what its account held on its record day. Where
// the market of the security paid fixes the record day, and the account
// held some then, the payment is per that quantity, where the account held
// within a tenth of it across the record window. Else it is per the
// quantity on its pay day, and the guard stands: an account paid that held
// none on the record day the fixed lag gives held some on the true one.
// These stocks pay once, 0.6 a share of what they held on their record
// day, and are priced at 50. The book is invented:
//
//   - QUS, a share of the United States, sells half of its 400 on 9 June,
//     two days before it pays on 11 June, after its record window: its
//     record day, a month before, read 400. QWIN sells half on 1 June,
//     inside its window: the quantity it was paid on is unknown, and it
//     rates nothing.
//   - QJP, a depositary receipt of a Japanese share as the exposure pass
//     reads one (the share's situs, basis underlying), sells half on 12
//     April, 60 days before: its record day, three months before, read
//     400, where a month before would read 200. QDJP, a receipt by its
//     name whose situs is declared Japanese, reads the same.
//   - QUNK, a share quoted in yen, of no market that fixes a record day,
//     and QETF, a fund of the United States, sell half on 1 June: paid on
//     under two thirds of what they held 100 days before, they rate
//     nothing.
//   - QNEW, a share of the United States bought on 20 May, after its
//     record day, rates on its pay day's quantity, the first it shows
//     held after its record day.
//   - QBUY, a share of the United States bought on 5 May, before its
//     record day, shows none on it: the snapshot before holds none, and
//     its trades move no snapshot that holds none. It rates on its pay
//     day's quantity, which a sale of half on 1 June leaves under two
//     thirds of the 400 the 31 May snapshot shows: it rates nothing.
//     QFRESH, at a source whose first snapshot falls between its record
//     day and its pay day, reads the same. QEDGE, at a source whose first
//     snapshot is stamped as its pay day begins, is guarded by that one.
//   - QEU1 and QEU2, receipts of a German and a French share, first show
//     in the snapshot nine days before their pay day, with no trade on
//     record, after their record day two weeks before: they rate on that
//     quantity. QDER, a German receipt, bought 100 more three weeks before
//     its pay day, inside its record window: its record day read 300, but
//     it rates nothing.
//   - QSPL, a share of the United States, splits two for one on 20 May,
//     between its record day and its pay day: its record day's 100 read
//     as 200 of today's units, its pay day's 200 as they stand.
//   - QIT, a share of Italy quoted in euros, pays on a Monday in November
//     and in May, its ex-day the Thursday before. A sale of 120 of its
//     340 on the ex-day still earns the May payment: it rates on 340, and
//     the year pays both halves. QIT2 pays on a Wednesday, its ex-day the
//     Monday, when it sells a third: it rates on what it held before.
//     QIT3 pays on Monday 4 May, after the holiday of 1 May, so its lag
//     steps over the holiday to its true ex-day, the Wednesday before. A
//     sale of 120 on that day still earns the payment: it rates on 340.
//   - QADU, a receipt by its name whose share is unknown, quoted in
//     dollars, has no record day: trimmed to half 60 days before its pay
//     day and paid on all, it rates nothing under the guard.
//   - QLST, listed in dollars, is valued in francs by its snapshots: it
//     reads the lag of a share quoted in dollars.
func TestADividendIsPaidOnItsRecordDaysQuantity(t *testing.T) {
	t.Parallel()
	early, pay := ymd(2025, 1, 1), ymd(2026, 6, 11)
	half := []bookSeg{{early, 400, 20000}, {ymd(2026, 6, 30), 200, 10000}}
	shown := []bookSeg{{ymd(2026, 5, 31), 100, 5000}}
	situs := func(iid, country, basis string) []string {
		return []string{`DELETE FROM exposure_situs WHERE position_key = '` + iid + `'`,
			`INSERT INTO exposure_situs (silver_source_id, account_external_id, position_key, as_of, situs, basis)
			    VALUES ('src', 'B1', '` + iid + `', NULL, '` + country + `', '` + basis + `')`}
	}
	after := []string{
		`INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
		    VALUES ('fx', 0, 'USD', 'EUR', 1.1), ('fx', 0, 'USD', 'JPY', 0.007)`,
		`UPDATE positions SET vehicle = 'etf' WHERE position_key = 'QETF'`,
		`UPDATE instruments SET vehicle = 'etf' WHERE instrument_external_id = 'QETF'`,
		`UPDATE instruments SET name = 'Example Japan ADR' WHERE instrument_external_id = 'QDJP'`,
	}
	after = append(append(append(append(after, situs("QJP", "JP", situsUnderlying)...),
		situs("QDJP", "JP", situsDeclared)...), situs("QEU1", "DE", situsUnderlying)...),
		situs("QEU2", "FR", situsUnderlying)...)
	after = append(after, situs("QDER", "DE", situsUnderlying)...)
	after = append(after, `UPDATE instruments SET name = 'Example Unknown ADR' WHERE instrument_external_id = 'QADU'`,
		`UPDATE positions SET currency = 'CHF' WHERE position_key = 'QLST'`)
	after = append(after,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		    asset_class, vehicle, currency, quantity, market_value)
		    VALUES ('edge', `+itoa(pay.Unix())+`, 'E1', 'QEDGE', 'QEDGE', 'public_equity', 'stock', 'USD', 400, 20000)`,
		`INSERT INTO exposure_situs (silver_source_id, account_external_id, position_key, as_of, situs, basis)
		    VALUES ('edge', 'E1', 'QEDGE', NULL, 'US', '`+situsCountry+`')`)
	in, _, _ := dividendBook{instruments: []bookInstrument{
		{"src", "QUS", "US0000000001", "", "QUS"}, {"src", "QWIN", "US0000000010", "", "QWIN"},
		{"src", "QJP", "", "", "QJP"}, {"src", "QDJP", "", "", "QDJP"},
		{"src", "QUNK", "", "", "QUNK"}, {"src", "QETF", "US0000000003", "", "QETF"},
		{"src", "QNEW", "US0000000002", "", "QNEW"}, {"src", "QEU1", "", "", "QEU1"}, {"src", "QEU2", "", "", "QEU2"},
		{"src", "QSPL", "US0000000004", "", "QSPL"}, {"src", "QBUY", "US0000000006", "", "QBUY"},
		{"fresh", "QFRESH", "US0000000007", "", "QFRESH"}, {"src", "QIT", "IT0000000005", "", "QIT"},
		{"src", "QIT2", "IT0000000006", "", "QIT2"}, {"src", "QDER", "", "", "QDER"},
		{"src", "QIT3", "IT0000000007", "", "QIT3"}, {"src", "QADU", "", "", "QADU"},
		{"src", "QLST", "", "", "QLST"},
		{"edge", "QEDGE", "US0000000009", "", "QEDGE"},
	}, lines: []bookLine{
		{"src", "B1", "QUS", half}, {"src", "B1", "QWIN", half},
		{"src", "B1", "QJP", []bookSeg{{early, 400, 20000}, {ymd(2026, 4, 30), 200, 10000}}},
		{"src", "B1", "QDJP", []bookSeg{{early, 400, 20000}, {ymd(2026, 4, 30), 200, 10000}}},
		{"src", "B1", "QUNK", half}, {"src", "B1", "QETF", half},
		{"src", "B1", "QNEW", shown}, {"src", "B1", "QEU1", shown}, {"src", "B1", "QEU2", shown},
		{"src", "B1", "QSPL", []bookSeg{{early, 100, 10000}, {ymd(2026, 5, 31), 200, 10000}}},
		{"src", "B1", "QBUY", []bookSeg{{ymd(2026, 5, 31), 400, 20000}, {ymd(2026, 6, 30), 200, 10000}}},
		{"fresh", "F1", "QFRESH", []bookSeg{{ymd(2026, 5, 31), 400, 20000}, {ymd(2026, 6, 30), 200, 10000}}},
		{"src", "B1", "QIT", []bookSeg{{early, 340, 17000}, {ymd(2026, 5, 31), 220, 11000}}},
		{"src", "B1", "QIT2", []bookSeg{{early, 300, 15000}, {ymd(2026, 5, 31), 200, 10000}}},
		{"src", "B1", "QDER", []bookSeg{{early, 200, 10000}, {ymd(2026, 4, 30), 300, 15000}}},
		{"src", "B1", "QIT3", []bookSeg{{early, 340, 17000}, {ymd(2026, 4, 30), 220, 11000}}},
		{"src", "B1", "QADU", []bookSeg{{early, 400, 20000}, {ymd(2026, 4, 30), 200, 10000}}},
		{"src", "B1", "QLST", half},
	}, txns: []bookTxn{
		{"src", "B1", "QUS", ymd(2026, 6, 9), "sell", 10000, -200}, {"src", "B1", "QUS", pay, "dividend", 0.6 * 400, 0},
		{"src", "B1", "QWIN", ymd(2026, 6, 1), "sell", 10000, -200}, {"src", "B1", "QWIN", pay, "dividend", 240, 0},
		{"src", "B1", "QJP", ymd(2026, 4, 12), "sell", 10000, -200}, {"src", "B1", "QJP", pay, "dividend", 0.6 * 400, 0},
		{"src", "B1", "QDJP", ymd(2026, 4, 12), "sell", 10000, -200}, {"src", "B1", "QDJP", pay, "dividend", 240, 0},
		{"src", "B1", "QUNK", ymd(2026, 6, 1), "sell", 10000, -200}, {"src", "B1", "QUNK", pay, "dividend", 240, 0},
		{"src", "B1", "QETF", ymd(2026, 6, 1), "sell", 10000, -200}, {"src", "B1", "QETF", pay, "dividend", 240, 0},
		{"src", "B1", "QNEW", ymd(2026, 5, 20), "buy", -5000, 100}, {"src", "B1", "QNEW", pay, "dividend", 60, 0},
		{"src", "B1", "QEU1", ymd(2026, 6, 9), "dividend", 60, 0}, {"src", "B1", "QEU2", ymd(2026, 6, 9), "dividend", 60, 0},
		{"src", "B1", "QSPL", ymd(2026, 5, 20), "corporate_action", 0, 100},
		{"src", "B1", "QSPL", pay, "dividend", 0.6 * 100, 0},
		{"src", "B1", "QBUY", ymd(2026, 5, 5), "buy", -20000, 400},
		{"src", "B1", "QBUY", ymd(2026, 6, 1), "sell", 10000, -200}, {"src", "B1", "QBUY", pay, "dividend", 240, 0},
		{"fresh", "F1", "QFRESH", ymd(2026, 6, 1), "sell", 10000, -200},
		{"fresh", "F1", "QFRESH", pay, "dividend", 240, 0},
		{"src", "B1", "QIT", ymd(2025, 11, 10), "dividend", 0.5 * 340, 0},
		{"src", "B1", "QIT", ymd(2026, 5, 7), "sell", 6000, -120},
		{"src", "B1", "QIT", ymd(2026, 5, 11), "dividend", 0.5 * 340, 0},
		{"src", "B1", "QIT2", ymd(2026, 5, 11), "sell", 5000, -100},
		{"src", "B1", "QIT2", ymd(2026, 5, 13), "dividend", 0.5 * 300, 0},
		{"src", "B1", "QDER", ymd(2026, 4, 28), "buy", -5000, 100},
		{"src", "B1", "QDER", ymd(2026, 5, 20), "dividend", 0.6 * 300, 0},
		{"edge", "E1", "QEDGE", pay, "dividend", 240, 0},
		{"src", "B1", "QIT3", ymd(2026, 4, 29), "sell", 6000, -120},
		{"src", "B1", "QIT3", ymd(2026, 5, 4), "dividend", 0.5 * 340, 0},
		{"src", "B1", "QADU", ymd(2026, 4, 12), "sell", 10000, -200}, {"src", "B1", "QADU", pay, "dividend", 240, 0},
		{"src", "B1", "QLST", ymd(2026, 6, 9), "sell", 10000, -200}, {"src", "B1", "QLST", pay, "dividend", 240, 0},
	}, currencies: map[string]string{"QIT": "EUR", "QIT2": "EUR", "QIT3": "EUR", "QUNK": "JPY"},
		after: after}.inputs(t)
	b := yield.Compute(in)
	for _, c := range []struct {
		iid           string
		lag           int
		qty, rec, pct float64
	}{
		{"QUS", yield.RecordLagUSShare.Days, 200, 400, 1.2}, {"QWIN", yield.RecordLagUSShare.Days, 200, 400, 0},
		{"QJP", yield.RecordLagJapaneseReceipt.Days, 200, 400, 1.2},
		{"QDJP", yield.RecordLagJapaneseReceipt.Days, 200, 400, 1.2}, {"QUNK", 0, 200, 0, 0}, {"QETF", 0, 200, 0, 0},
		{"QNEW", yield.RecordLagUSShare.Days, 100, 0, 1.2}, {"QEU1", yield.RecordLagEuropeanReceipt.Days, 100, 0, 1.2},
		{"QEU2", yield.RecordLagEuropeanReceipt.Days, 100, 0, 1.2},
		{"QSPL", yield.RecordLagUSShare.Days, 200, 200, 0.6}, {"QBUY", yield.RecordLagUSShare.Days, 200, 0, 0},
		{"QFRESH", yield.RecordLagUSShare.Days, 200, 0, 0},
		{"QDER", yield.RecordLagEuropeanReceipt.Days, 300, 300, 0},
		{"QIT2", 2, 200, 300, 1.0}, {"QIT3", 5, 220, 340, 1.0}, {"QADU", 0, 200, 0, 0},
	} {
		k := yield.HoldingKey{Source: "src", Account: "B1", PositionKey: c.iid}
		if c.iid == "QFRESH" {
			k.Source, k.Account = "fresh", "F1"
		}
		ps := in.Payments[k]
		if len(ps) != 1 || ps[0].LagDays != c.lag || ps[0].Quantity == nil || *ps[0].Quantity != c.qty ||
			(ps[0].Record == nil) != (c.rec == 0) || (c.rec != 0 && *ps[0].Record != c.rec) {
			t.Errorf("%s: payments %+v", c.iid, ps)
		}
		p := position(b, k)
		switch {
		case c.pct == 0:
			if p.DividendYield != nil || !p.Tags[yield.TagUnknown] {
				t.Errorf("%s: dividend yield %s, tags %v", c.iid, fmtFloatPtr(p.DividendYield), p.Tags)
			}
		case p.DividendYield == nil || math.Abs(*p.DividendYield-c.pct) > 1e-9 || !p.Tags[yield.TagIndicated] ||
			p.Tags[yield.TagUnits]:
			t.Errorf("%s: dividend yield %s, want %v, tags %v", c.iid, fmtFloatPtr(p.DividendYield), c.pct, p.Tags)
		}
		if (c.iid == "QBUY" || c.iid == "QFRESH") && (ps[0].Before == nil || *ps[0].Before != 400) {
			t.Errorf("%s: guarded by %s, want 400", c.iid, fmtFloatPtr(ps[0].Before))
		}
	}
	if ps := in.Payments[yield.HoldingKey{Source: "src", Account: "B1", PositionKey: "QLST"}]; len(ps) != 1 ||
		ps[0].LagDays != yield.RecordLagUSShare.Days || ps[0].Record == nil || *ps[0].Record != 400 {
		t.Errorf("QLST: payments %+v", ps)
	}
	edge := in.Payments[yield.HoldingKey{Source: "edge", Account: "E1", PositionKey: "QEDGE"}]
	if len(edge) != 1 || edge[0].LagDays != yield.RecordLagUSShare.Days || edge[0].Record != nil ||
		edge[0].Quantity == nil || *edge[0].Quantity != 400 || edge[0].Before == nil || *edge[0].Before != 400 {
		t.Errorf("QEDGE: payments %+v", edge)
	}
	// A Monday's ex-day is the Thursday before: four days.
	it := yield.HoldingKey{Source: "src", Account: "B1", PositionKey: "QIT"}
	ps, p := in.Payments[it], position(b, it)
	if len(ps) != 2 || ps[1].LagDays != 4 || ps[1].Record == nil || *ps[1].Record != 340 ||
		p.DividendYield == nil || math.Abs(*p.DividendYield-1.0/50*100) > 1e-9 || p.Tags[yield.TagPartial] {
		t.Errorf("QIT: payments %+v, dividend yield %s, tags %v", ps, fmtFloatPtr(p.DividendYield), p.Tags)
	}
}

// TestATradeOnTheCashAccountMovesItsPortfoliosHolding: a bank books a
// trade, and its dividend, on a cash account beside the custody account
// that holds the share. The book is invented, at a bank whose portfolio
// P1 has the custody accounts SAFE and SAFE2, the cash account CASH and
// the brokerage account BRK, each shown at month ends:
//   - QCHS, a Swiss share: SAFE held 50 to February; CASH booked a sale of
//     10 on 12 March and the dividend of 31 March, 0.5 a share. The record
//     day, two business days before, reads February's 50 less the sale.
//   - QOWN: CASH holds some of it itself, so its sale of 10 moves its own
//     holding, and SAFE's own sale of 5 moves SAFE's.
//   - QTWO: SAFE and SAFE2 both hold it as CASH sells: the sale moves
//     neither, and the payment reads both unmoved.
//   - QSEQ: SAFE held it to January, SAFE2 from February: CASH's sale in
//     March moves SAFE2's, the one that held it then.
//   - QHLD: CASH names it QCSH, an id only CASH's own alias folds into
//     QHLD; its sale still moves SAFE's QHLD.
//   - QALS: CASH names it QALT, an id only SAFE's alias folds into QALS;
//     its sale moves SAFE's QALS.
//   - QBRK: BRK, a brokerage account, sells it without holding it: the
//     sale moves no other account's holding.
//   - QOTH: CASH2, of portfolio P2, sells it: SAFE keeps February's 50.
//   - QFND, a fund with no record day: CASH's buy of 50 on 2 March moves
//     SAFE's 100, read as the pay day begins.
func TestATradeOnTheCashAccountMovesItsPortfoliosHolding(t *testing.T) {
	t.Parallel()
	early, pay := ymd(2026-1, 1, 1), ymd(2026, 3, 31)
	sold := func(n float64) []bookSeg { return []bookSeg{{early, 50, 2500}, {pay, 50 - n, 2500 - 50*n}} }
	shares := []string{"QCHS", "QOWN", "QTWO", "QSEQ", "QHLD", "QALS", "QBRK", "QOTH"}
	var after []string
	for _, iid := range shares {
		for _, acct := range []string{"SAFE", "SAFE2", "CASH"} {
			after = append(after, `INSERT INTO exposure_situs (silver_source_id, account_external_id, position_key, as_of,
			    situs, basis) VALUES ('bank', '`+acct+`', '`+iid+`', NULL, 'CH', '`+situsCountry+`')`)
		}
	}
	after = append(after,
		`INSERT INTO fx_rates (silver_source_id, snapshot_at, base_currency, quote_currency, mid_rate)
		    VALUES ('fx', 0, 'USD', 'CHF', 0.9)`,
		`UPDATE accounts SET account_kind = 'cash' WHERE account_external_id IN ('CASH', 'CASH2')`,
		`INSERT INTO holding_aliases (silver_source_id, account_external_id, from_key, to_key)
		    VALUES ('bank', 'CASH', 'QCSH', 'QHLD'), ('bank', 'SAFE', 'QALT', 'QALS')`,
		`UPDATE positions SET vehicle = 'etf' WHERE position_key = 'QFND'`,
		`UPDATE instruments SET vehicle = 'etf' WHERE instrument_external_id = 'QFND'`)
	ccys := map[string]string{"QCSH": "CHF", "QALT": "CHF"}
	insts := []bookInstrument{{"bank", "QCSH", "", "", ""}, {"bank", "QALT", "", "", ""},
		{"bank", "QFND", "US0000000011", "", "QFND"}}
	for i, iid := range shares {
		ccys[iid] = "CHF"
		insts = append(insts, bookInstrument{"bank", iid, "CH000000000" + strconv.Itoa(i+1), "", iid})
	}
	var txns []bookTxn
	for _, iid := range shares {
		txns = append(txns, bookTxn{"bank", "CASH", iid, pay, "dividend", 20, 0})
	}
	march := ymd(2026, 3, 12)
	txns = append(txns,
		bookTxn{"bank", "CASH", "QCHS", march, "sell", 500, -10},
		bookTxn{"bank", "SAFE", "QOWN", march, "sell", 250, -5},
		bookTxn{"bank", "CASH", "QOWN", march, "sell", 500, -10},
		bookTxn{"bank", "CASH", "QTWO", march, "sell", 500, -10},
		bookTxn{"bank", "CASH", "QSEQ", march, "sell", 500, -10},
		bookTxn{"bank", "CASH", "QCSH", march, "sell", 500, -10},
		bookTxn{"bank", "CASH", "QALT", march, "sell", 500, -10},
		bookTxn{"bank", "BRK", "QBRK", march, "sell", 500, -10},
		bookTxn{"bank", "CASH2", "QOTH", march, "sell", 500, -10},
		bookTxn{"bank", "CASH", "QFND", ymd(2026, 3, 2), "buy", -5000, 50},
		bookTxn{"bank", "CASH", "QFND", pay, "dividend", 30, 0})
	jan := ymd(2026, 1, 31)
	in, _, _ := dividendBook{instruments: insts, lines: []bookLine{
		{"bank", "SAFE", "QCHS", sold(10)}, {"bank", "SAFE", "QOWN", sold(5)}, {"bank", "CASH", "QOWN", sold(10)},
		{"bank", "SAFE", "QTWO", sold(0)}, {"bank", "SAFE2", "QTWO", sold(0)},
		{"bank", "SAFE", "QSEQ", []bookSeg{{early, 50, 2500}, {jan, 0, 0}}},
		{"bank", "SAFE2", "QSEQ", []bookSeg{{jan, 50, 2500}, {pay, 40, 2000}}},
		{"bank", "SAFE", "QHLD", sold(10)}, {"bank", "SAFE", "QALS", sold(10)},
		{"bank", "SAFE", "QBRK", sold(0)}, {"bank", "SAFE", "QOTH", sold(0)},
		{"bank", "SAFE", "QFND", []bookSeg{{early, 100, 5000}, {pay, 150, 7500}}},
	}, txns: txns, currencies: ccys, after: after,
		portfolios: map[string]string{"SAFE": "P1", "SAFE2": "P1", "CASH": "P1", "BRK": "P1", "CASH2": "P2"}}.inputs(t)
	for _, c := range []struct {
		acct, iid string
		want      float64
	}{
		{"SAFE", "QCHS", 40}, {"CASH", "QOWN", 40}, {"SAFE", "QTWO", 100}, {"SAFE2", "QSEQ", 40},
		{"SAFE", "QHLD", 40}, {"SAFE", "QALS", 40}, {"SAFE", "QBRK", 50}, {"SAFE", "QOTH", 50},
	} {
		ps := in.Payments[yield.HoldingKey{Source: "bank", Account: c.acct, PositionKey: c.iid}]
		if len(ps) != 1 || ps[0].Record == nil || *ps[0].Record != c.want {
			t.Errorf("%s/%s: payments %+v, want one read on %v held on its record day", c.acct, c.iid, ps, c.want)
		}
	}
	ps := in.Payments[yield.HoldingKey{Source: "bank", Account: "SAFE", PositionKey: "QFND"}]
	if len(ps) != 1 || ps[0].Record != nil || ps[0].Quantity == nil || *ps[0].Quantity != 150 {
		t.Errorf("QFND: payments %+v, want one read on the 150 held as its pay day began", ps)
	}
}
