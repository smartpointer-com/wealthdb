package gold

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
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
		got, units := u.quantity(in, "src", c.acct, "QSTK", c.end)
		if (got == nil) != (c.want == 0) || (got != nil && *got != c.want) || units != c.units {
			t.Errorf("%s at %d: %v %v, want %v %v", c.acct, c.end, fmtFloatPtr(got), units, c.want, c.units)
		}
	}
	u.addChange(ik, unitChange{at: 50, k: 5, ok: true})
	if _, units := u.quantity(in, "src", "DEPOT", "QSTK", 35); !units {
		t.Error("two accounts that disagree at one snapshot")
	}
	// DEPOT states no quantity at 40: from 20 to 50 no account held it
	// with its units read.
	u.changes[ik] = nil
	u.points[unitKey{"src", "DEPOT", "QSTK"}] = []unitPoint{{at: 10, qty: q(100)}, {at: 40, prev: 30, qty: nil},
		{at: 50, prev: 40, qty: q(1000)}}
	u.read = map[yield.InstrumentKey][][2]int64{}
	u.cover()
	if _, units := u.quantity(in, "src", "DEPOT", "QSTK", 15); !units {
		t.Error("a span no account held through")
	}
	if _, units := u.quantity(in, "src", "DEPOT", "QSTK", 55); units {
		t.Error("a payment after the span")
	}
	// A cash account of another portfolio reads the one account of its
	// source that held the instrument; with two such accounts, none.
	in.Accounts[yield.AccountKey{Source: "src", Account: "OTHER"}] = yield.Account{Portfolio: "Q"}
	u.points[unitKey{"src", "DEPOT", "QONE"}] = []unitPoint{{at: 10, qty: q(40)}}
	u.accounts[yield.InstrumentKey{Source: "src", Instrument: "QONE"}] = []string{"DEPOT"}
	if got, _ := u.quantity(in, "src", "OTHER", "QONE", 15); got == nil || *got != 40 {
		t.Errorf("the one account: %v", fmtFloatPtr(got))
	}
	if got, _ := u.quantity(in, "src", "OTHER", "QSTK", 15); got != nil {
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
//     dividends stay its sold position's, its June one paid on nothing it
//     held, and B2's holding shows one row a pay day. Its realized yield
//     pools both accounts. The early June snapshot leaves SOLD out, and
//     is not its last, so SOLD's May amounts hold to its last snapshot.
//   - QBNK is held in DEPOT, its dividends booked to CASH beside it.
//   - QLATE's source took its first snapshot after its first dividend.
//   - QALS is held throughout, but the snapshots of 31 May and 5 June name
//     it by its CUSIP, an instrument its source folds into it
//     (instrument_aliases): its June dividend is paid on what both held.
//   - QSTMT is bought up at the instant of the March snapshot, which leaves
//     the buy out: its April dividend is paid on what it held after it.
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
	for i, d := range quarters(7, 15) {
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
	if _, err := SyncExposure(ctx, db, canonical.ExposureRows{
		Settings: canonical.ExposureSettings{MaxGapDays: canonical.DefaultMaxGapDays, AgedDays: 35, PartialPct: 3},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
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
