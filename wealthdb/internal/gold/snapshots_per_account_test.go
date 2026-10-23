package gold

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// The point-in-time readers take each key's active snapshot at their
// instant by the history's rule (migration 0140): a snapshot speaks
// only for the accounts it names. These tests hold every point-in-time
// report to the daily history on a book of partial runs, pin the
// history's own answer, and check the readers that pick in Go. All
// fixtures are invented.

// pitAt is the epoch second `hour` hours into the day `off` days from
// today.
func pitAt(off, hour int64) int64 { return (histToday()+off)*86400 + hour*3600 }

// pitPosition writes one holding of an account at an instant.
func pitPosition(t *testing.T, db *sql.DB, ctx context.Context, src, acct, key string, at int64, value float64) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, vehicle, currency, quantity, market_value)
        VALUES (?, ?, ?, ?, ?, 'public_equity', 'stock', 'USD', 1, ?)`,
		src, at, acct, key, key, value); err != nil {
		t.Fatalf("seed position %s/%s/%s@%d: %v", src, acct, key, at, err)
	}
}

// pitCash writes one current balance of an account at an instant.
func pitCash(t *testing.T, db *sql.DB, ctx context.Context, src, acct, ccy string, at int64, amount float64) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id, currency, balance_kind, amount)
        VALUES (?, ?, ?, ?, 'current', ?)`, src, at, acct, ccy, amount); err != nil {
		t.Fatalf("seed balance %s/%s/%s@%d: %v", src, acct, ccy, at, err)
	}
}

// seedPartialRuns writes a book of every shape a partial run takes,
// from 420 days ago to a week ago:
//
//   - stmt-src's month-end statements name M-0000 every month and
//     Q-0000 at quarter ends only; C-0000 sits in the first three
//     quarter ends and is left out of the fourth, which names the rest:
//     it is closed. Q-0000's last quarter end it shares with M-0000,
//     whose next month re-covers it, but the source's newest statement
//     lies inside one and a half of Q-0000's cadences: it stands.
//   - bank-src's feed writes the custody account CUST-0000 and the cash
//     of DEP-0000 (two legs, the EUR one drained to zero and still
//     reported at zero) and DEP-0001 weekly, at 20:00; DEP-0001 is
//     closed, left out of a feed that names DEP-0000's legs. Its card
//     run writes CARD-0000 monthly at 08:00, sometimes on a feed's day.
//   - lone-src observes H-0000's positions and L-0000's cash once, each
//     alone, and keeps observing other accounts for more than
//     hist_carry_days() after: the clock ends them. G-0000 is observed
//     alone twice, 390 days apart: the clock never ends a key between
//     two of its own observations.
//   - pair-src's runs every ten days write E-0002 and, until a run that
//     leaves both out, E-0000 and E-0001: the two leave together, and
//     the next run ends both. Z-0000 closes with an explicit $0 row.
//   - c365-src and c366-src each observe an X key alone and run again
//     365 and 366 days after it: the clock spares the first, ends the
//     second.
func seedPartialRuns(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	for _, r := range []struct{ quote, mid string }{{"EUR", "1.2"}, {"CHF", "0.9"}, {"GBP", "0.8"}} {
		seedFX(t, db, pitAt(-500, 12), "USD", r.quote, r.mid)
	}
	for _, a := range []struct{ src, acct, kind string }{
		{"stmt-src", "M-0000", "brokerage"}, {"stmt-src", "Q-0000", "brokerage"}, {"stmt-src", "C-0000", "brokerage"},
		{"bank-src", "CUST-0000", "custody"}, {"bank-src", "DEP-0000", "cash"}, {"bank-src", "DEP-0001", "cash"},
		{"bank-src", "CARD-0000", "card"},
		{"lone-src", "H-0000", "brokerage"}, {"lone-src", "K-0000", "brokerage"},
		{"lone-src", "L-0000", "cash"}, {"lone-src", "N-0000", "cash"}, {"lone-src", "G-0000", "brokerage"},
		{"pair-src", "E-0000", "brokerage"}, {"pair-src", "E-0001", "brokerage"}, {"pair-src", "E-0002", "brokerage"},
		{"pair-src", "Z-0000", "brokerage"},
		{"c365-src", "X-0365", "brokerage"}, {"c365-src", "Y-0365", "brokerage"},
		{"c366-src", "X-0366", "brokerage"}, {"c366-src", "Y-0366", "brokerage"},
	} {
		seedHistAccount(t, db, ctx, a.src, a.acct, a.kind, "Account EXAMPLE")
	}
	for i := int64(0); i <= 13; i++ {
		at := pitAt(-420+30*i, 18)
		pitPosition(t, db, ctx, "stmt-src", "M-0000", "MX", at, float64(1000+10*i))
		if i%3 == 0 {
			pitPosition(t, db, ctx, "stmt-src", "Q-0000", "QX", at, float64(5000+100*i))
		}
		if i%3 == 0 && i <= 6 {
			pitPosition(t, db, ctx, "stmt-src", "C-0000", "CX", at, 700)
		}
	}
	for j := int64(0); j <= 58; j++ {
		at := pitAt(-413+7*j, 20)
		pitPosition(t, db, ctx, "bank-src", "CUST-0000", "BX", at, float64(2000+j))
		pitCash(t, db, ctx, "bank-src", "DEP-0000", "USD", at, float64(3000+j))
		eur := float64(100 + j)
		if j >= 50 {
			eur = 0
		}
		pitCash(t, db, ctx, "bank-src", "DEP-0000", "EUR", at, eur)
		if j <= 40 {
			pitCash(t, db, ctx, "bank-src", "DEP-0001", "USD", at, float64(400+j))
		}
	}
	for k := int64(0); k <= 13; k++ {
		pitCash(t, db, ctx, "bank-src", "CARD-0000", "USD", pitAt(-410+30*k, 8), float64(-500-10*k))
	}
	pitPosition(t, db, ctx, "lone-src", "H-0000", "HX", pitAt(-400, 10), 900)
	for _, off := range []int64{-399, -200, -30} {
		pitPosition(t, db, ctx, "lone-src", "K-0000", "KX", pitAt(off, 10), 300)
	}
	pitCash(t, db, ctx, "lone-src", "L-0000", "USD", pitAt(-410, 9), 800)
	for _, off := range []int64{-405, -300, -40} {
		pitCash(t, db, ctx, "lone-src", "N-0000", "USD", pitAt(off, 9), 50)
	}
	pitPosition(t, db, ctx, "lone-src", "G-0000", "GX", pitAt(-415, 10), 77)
	pitPosition(t, db, ctx, "lone-src", "G-0000", "GX", pitAt(-25, 10), 88)
	for j := int64(0); j <= 20; j++ {
		at := pitAt(-300+10*j, 14)
		pitPosition(t, db, ctx, "pair-src", "E-0002", "EX", at, float64(100+j))
		if j <= 10 {
			pitPosition(t, db, ctx, "pair-src", "E-0000", "EX", at, 500)
			pitPosition(t, db, ctx, "pair-src", "E-0001", "EX", at, 600)
		}
		switch {
		case j <= 14:
			pitPosition(t, db, ctx, "pair-src", "Z-0000", "ZX", at, 300)
		case j == 15:
			pitPosition(t, db, ctx, "pair-src", "Z-0000", "ZX", at, 0)
		}
	}
	for _, c := range []struct {
		src, suffix string
		after       int64
	}{{"c365-src", "0365", 365}, {"c366-src", "0366", 366}} {
		pitPosition(t, db, ctx, c.src, "X-"+c.suffix, "XX", pitAt(-400, 10), 365)
		pitPosition(t, db, ctx, c.src, "Y-"+c.suffix, "YX", pitAt(-399, 10), 1)
		pitPosition(t, db, ctx, c.src, "Y-"+c.suffix, "YX", pitAt(-400+c.after, 10), 1)
	}
}

// pitValues are one row's values, NULL kept.
type pitValues []sql.NullFloat64

func (v pitValues) String() string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = "null"
		if x.Valid {
			parts[i] = fmt.Sprintf("%.4f", x.Float64)
		}
	}
	return strings.Join(parts, "/")
}

// pitRead maps each row's key, its leading `keys` columns joined, to the
// values of the columns after them.
func pitRead(t *testing.T, db *sql.DB, ctx context.Context, keys int, q string, args ...any) map[string]pitValues {
	t.Helper()
	out := map[string]pitValues{}
	if err := scanAll(ctx, db, q, func(rows *sql.Rows) error {
		cols, err := rows.Columns()
		if err != nil {
			return err
		}
		k, v := make([]string, keys), make(pitValues, len(cols)-keys)
		dst := make([]any, 0, len(cols))
		for i := range k {
			dst = append(dst, &k[i])
		}
		for i := range v {
			dst = append(dst, &v[i])
		}
		if err := rows.Scan(dst...); err != nil {
			return err
		}
		out[strings.Join(k, "|")] = v
		return nil
	}, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return out
}

// pitSame says a point-in-time row equals the history's: a key the
// history has no row for on the day holds nothing, so every value of the
// point-in-time row is zero.
func pitSame(pit pitValues, hist pitValues, inHistory bool) bool {
	for i := range pit {
		h := sql.NullFloat64{Valid: true}
		if inHistory {
			h = hist[i]
		}
		if pit[i].Valid != h.Valid || (pit[i].Valid && !near(pit[i].Float64, h.Float64)) {
			return false
		}
	}
	return true
}

// pitColumns are the value columns of a report, in a single currency
// (outccy, and base) or in every reporting currency.
func pitColumns(suffixes ...string) string {
	var out []string
	for _, s := range suffixes {
		out = append(out, "positions_value_"+s, "cash_balance_"+s, "total_value_"+s)
	}
	return strings.Join(out, ", ")
}

// TestPointInTimeIsTheHistoryEveryDay is the rule's invariant: read at a
// day's last second, every point-in-time report equals the history's
// day, in value, for every source of a book of partial runs, in a
// single currency and in every reporting currency. Each day's
// point-in-time read is a LATERAL call of the report at that day's end,
// which equals a direct call (checked on a sample of days).
func TestPointInTimeIsTheHistoryEveryDay(t *testing.T) {
	db, ctx := openMigrated(t)
	seedPartialRuns(t, db, ctx)
	single, base := pitColumns("outccy"), pitColumns("base")
	usd, all := pitColumns("usd"), pitColumns("usd", "chf", "eur", "gbp")
	const account, portfolio, source = "silver_source_id, account_external_id", "silver_source_id, portfolio_external_id",
		"silver_source_id"
	for _, c := range []struct {
		name, keys, history, histCols, pit, pitCols string
	}{
		{"accounts", account, "report_accounts_history('USD')", single + ", " + base,
			"report_accounts(%s, 'USD')", single + ", " + base},
		{"accounts multi", account, "report_accounts_history_multi()", all + ", " + base,
			"report_accounts_multi(%s)", all + ", " + base},
		{"portfolios", portfolio, "report_portfolios_history_multi()", usd + ", " + base,
			"report_portfolios(%s, 'USD')", single + ", " + base},
		{"portfolios multi", portfolio, "report_portfolios_history_multi()", all + ", " + base,
			"report_portfolios_multi(%s)", all + ", " + base},
		{"sources", source, "report_sources_history_multi()", usd + ", " + base,
			"report_sources(%s, 'USD')", single + ", " + base},
		{"sources multi", source, "report_sources_history_multi()", all + ", " + base,
			"report_sources_multi(%s)", all + ", " + base},
		{"global", "'all'", "report_global_history_multi()", usd, "report_global(%s, 'USD')", single},
		{"global multi", "'all'", "report_global_history_multi()", all, "report_global_multi(%s)", all},
	} {
		nkeys := strings.Count(c.keys, ",") + 1
		hist := pitRead(t, db, ctx, nkeys+1, "SELECT CAST(as_of_day // 86400 AS VARCHAR), "+c.keys+", "+c.histCols+
			" FROM "+c.history+" WHERE as_of_day <= ?", histToday()*86400)
		pit := pitRead(t, db, ctx, nkeys+1, "SELECT CAST(d.day AS VARCHAR), "+c.keys+", "+c.pitCols+
			" FROM (SELECT day FROM hist_days() WHERE day <= ?) d, LATERAL "+fmt.Sprintf(c.pit, "d.day * 86400 + 86399")+
			" r", histToday())
		if len(pit) == 0 || len(hist) == 0 {
			t.Fatalf("%s: %d point-in-time rows, %d history rows", c.name, len(pit), len(hist))
		}
		for _, off := range []int64{-420, -301, -150, -64, -30, 0} {
			d := histToday() + off
			for k, v := range pitRead(t, db, ctx, nkeys, "SELECT "+c.keys+", "+c.pitCols+" FROM "+fmt.Sprintf(c.pit, "?"),
				d*86400+86399) {
				if got := pit[fmt.Sprint(d)+"|"+k]; got.String() != v.String() {
					t.Errorf("%s, day %+d: %s reads %s laterally, %s directly", c.name, off, k, got, v)
				}
			}
		}
		failed := 0
		for k, v := range hist {
			if _, ok := pit[k]; !ok {
				t.Errorf("%s: %s in the history only (%s)", c.name, k, v)
				failed++
			}
		}
		for k, v := range pit {
			if h, ok := hist[k]; !pitSame(v, h, ok) {
				t.Errorf("%s: %s = %s, the history %s (present %v)", c.name, k, v, h, ok)
				failed++
			}
			if failed > 10 {
				t.Fatalf("%s: stopping after %d differences", c.name, failed)
			}
		}
	}
}

// pitDiff counts the rows two queries of the same columns disagree on,
// either way.
func pitDiff(t *testing.T, db *sql.DB, ctx context.Context, a, b string) (diff, rows int) {
	t.Helper()
	if err := db.QueryRowContext(ctx, `
        WITH a AS (`+a+`), b AS (`+b+`)
        SELECT (SELECT COUNT(*) FROM (SELECT * FROM a EXCEPT ALL SELECT * FROM b))
             + (SELECT COUNT(*) FROM (SELECT * FROM b EXCEPT ALL SELECT * FROM a)),
               (SELECT COUNT(*) FROM b)`).Scan(&diff, &rows); err != nil {
		t.Fatalf("compare %s with %s: %v", a, b, err)
	}
	return diff, rows
}

// TestActiveAtADaysEndIsTheHistorysDay proves the rule at its own
// grain: the snapshot each key holds at a day's last second is the one
// the history gives it on that day, for every key and day of the book.
func TestActiveAtADaysEndIsTheHistorysDay(t *testing.T) {
	db, ctx := openMigrated(t)
	seedPartialRuns(t, db, ctx)
	const ends = `(SELECT list(day * 86400 + 86399) FROM hist_days())`
	for _, c := range []struct{ at, history string }{
		{`SELECT instant // 86400 AS day, src, acct, snap FROM pos_active_at(` + ends + `)`,
			`SELECT day, src, acct, snap FROM hist_active_pos()`},
		{`SELECT instant // 86400 AS day, src, acct, ccy, snap FROM cash_active_at(` + ends + `)`,
			`SELECT day, src, acct, ccy, snap FROM hist_active_cash()`},
	} {
		if diff, rows := pitDiff(t, db, ctx, c.at, c.history); diff != 0 || rows == 0 {
			t.Errorf("%s: %d rows differ from the history's %d", c.at, diff, rows)
		}
	}
}

// TestKeySpansEndEveryShape pins the endings on the book of every ending
// shape: each key's last observation ends at its own instant
// (pos_key_spans, cash_key_spans), and an observation followed by a
// later one of its key holds to it.
func TestKeySpansEndEveryShape(t *testing.T) {
	db, ctx := openMigrated(t)
	seedPartialRuns(t, db, ctx)
	// The book has every ending, each at its instant: C-0000 and DEP-0001
	// at the full run after their last, E-0000 and E-0001 together at the
	// run after theirs, Z-0000 after its $0 row, H-0000, L-0000 and X-0366
	// at the clock. Q-0000 stands inside its cadence, X-0365 sits exactly
	// on the horizon, and neither ends, as CARD-0000 and the rest.
	// G-0000's first observation holds to its second, 390 days on.
	want := map[string]int64{
		"C-0000": pitAt(-150, 18), "DEP-0001": pitAt(-126, 20),
		"E-0000": pitAt(-190, 14), "E-0001": pitAt(-190, 14), "Z-0000": pitAt(-140, 14),
		"H-0000": pitAt(-30, 10), "L-0000": pitAt(-40, 9), "X-0366": pitAt(-34, 10),
		"G-0000@" + fmt.Sprint(pitAt(-415, 10)): pitAt(-25, 10),
	}
	got := map[string]int64{}
	if err := scanAll(ctx, db, `
        SELECT acct, snap, until, snap = MAX(snap) OVER (PARTITION BY src, acct) FROM pos_key_spans()
        UNION ALL
        SELECT acct, snap, until, snap = MAX(snap) OVER (PARTITION BY src, acct, ccy) FROM cash_key_spans()`,
		func(rows *sql.Rows) error {
			var a string
			var snap int64
			var until sql.NullInt64
			var last bool
			if err := rows.Scan(&a, &snap, &until, &last); err != nil {
				return err
			}
			switch {
			case last && until.Valid:
				got[a] = until.Int64
			case a == "G-0000" && !last:
				got[a+"@"+fmt.Sprint(snap)] = until.Int64
			}
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("endings %v, want %v", got, want)
	}
}

// TestAnInstantBetweenRunsKeepsEveryAccount reads a source on the
// morning a partial run lands between two full ones: every point-in-time
// reader keeps the account the partial run left out, at the full run
// before it, and dates each account by its own snapshot and the source
// by the newest of them.
func TestAnInstantBetweenRunsKeepsEveryAccount(t *testing.T) {
	db, ctx := openMigrated(t)
	const src = "day-src"
	seedHistAccount(t, db, ctx, src, "A-0000", "brokerage", "Brokerage EXAMPLE")
	seedHistAccount(t, db, ctx, src, "B-0000", "brokerage", "Custody EXAMPLE")
	full, partial, next := pitAt(-1, 6), pitAt(-1, 9), pitAt(-1, 18)
	pitPosition(t, db, ctx, src, "A-0000", "AX", full, 100)
	pitPosition(t, db, ctx, src, "B-0000", "BX", full, 200)
	pitCash(t, db, ctx, src, "A-0000", "USD", full, 10)
	pitCash(t, db, ctx, src, "B-0000", "USD", full, 20)
	pitPosition(t, db, ctx, src, "A-0000", "AX", partial, 110)
	pitCash(t, db, ctx, src, "A-0000", "USD", partial, 11)
	pitPosition(t, db, ctx, src, "A-0000", "AX", next, 120)
	pitPosition(t, db, ctx, src, "B-0000", "BX", next, 210)
	pitCash(t, db, ctx, src, "A-0000", "USD", next, 12)
	pitCash(t, db, ctx, src, "B-0000", "USD", next, 21)
	noon := pitAt(-1, 12)

	accounts, err := AccountsAsOf(ctx, db, noon, "USD")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		total float64
		at    int64
	}{"A-0000": {121, partial}, "B-0000": {220, full}}
	if len(accounts) != 2 {
		t.Fatalf("accounts %+v", accounts)
	}
	for _, a := range accounts {
		if w := want[a.AccountExternalID]; !near(num(t, a.TotalValueOutCcy), w.total) || a.SnapshotAt != w.at {
			t.Errorf("%s: total %s at %d, want %v at %d", a.AccountExternalID, *a.TotalValueOutCcy, a.SnapshotAt,
				w.total, w.at)
		}
	}
	sources, err := SourcesAsOf(ctx, db, noon, "USD")
	if err != nil || len(sources) != 1 || !near(num(t, sources[0].TotalValueOutCcy), 341) ||
		sources[0].SnapshotAt != partial {
		t.Errorf("sources %+v %v", sources, err)
	}
	global, err := GlobalAsOf(ctx, db, noon, "USD")
	if err != nil || !near(num(t, global.TotalValueOutCcy), 341) || global.MinSnapshotAt != full ||
		global.MaxSnapshotAt != partial {
		t.Errorf("global %+v %v", global, err)
	}
	positions, err := PositionsAsOf(ctx, db, noon, "USD", lots.MissingIgnore)
	if err != nil {
		t.Fatal(err)
	}
	cash, err := CashAsOf(ctx, db, noon, "USD")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, p := range append(positions, cash...) {
		lines = append(lines, fmt.Sprintf("%s/%s=%s@%d", p.AccountExternalID, p.PositionKey, *p.MarketValue,
			p.SnapshotAt-full))
	}
	sort.Strings(lines)
	if got := strings.Join(lines, " "); got != "A-0000/AX=110@10800 A-0000/cash:USD=11@10800 B-0000/BX=200@0 B-0000/cash:USD=20@0" {
		t.Errorf("positions and cash: %s", got)
	}
	for _, c := range []struct{ q, want string }{
		{`SELECT string_agg(account_external_id || '=' || CAST(value_usd AS DOUBLE), ' ' ORDER BY account_external_id)
		    FROM report_positions_multi(?)`, "A-0000=110.0 B-0000=200.0"},
		{`SELECT string_agg(acct || '/' || position_key || '=' || value, ' ' ORDER BY acct, position_key)
		    FROM exposure_holdings(?, 'USD')`, "A-0000/AX=110.0 A-0000/cash:USD=11.0 B-0000/BX=200.0 B-0000/cash:USD=20.0"},
	} {
		var got string
		if err := db.QueryRowContext(ctx, c.q, noon).Scan(&got); err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.q, got, err, c.want)
		}
	}
}

// TestYieldUnitsTakeTheHistorysEnding reads yield's account grid on the
// same rule. X-0000's last observation shares a daily run with Y-0000: a
// run of Z-0000 alone says nothing of it, Y-0000's next run ends it.
// Q-0000 is observed alone before the window: its source's newest
// snapshot at the window's start leaves it out, yet it holds there.
func TestYieldUnitsTakeTheHistorysEnding(t *testing.T) {
	db, ctx := openMigrated(t)
	const src = "units-src"
	day := func(d int64) int64 { return d*86400 + 43200 }
	pitPosition(t, db, ctx, src, "Q-0000", "QQ", day(10), 100)
	for _, d := range []int64{18, 19, 20} {
		pitPosition(t, db, ctx, src, "X-0000", "QX", day(d), 100)
		pitPosition(t, db, ctx, src, "Y-0000", "QY", day(d), 100)
	}
	pitPosition(t, db, ctx, src, "Z-0000", "QZ", day(30), 100)
	pitPosition(t, db, ctx, src, "Y-0000", "QY", day(40), 100)
	paid := map[yield.InstrumentKey]bool{{Source: src, Instrument: "QX"}: true, {Source: src, Instrument: "QQ"}: true}
	u, err := readYieldUnits(ctx, db, nil, paid, day(25), day(50))
	if err != nil {
		t.Fatal(err)
	}
	for acct, want := range map[string][]int64{
		"X-0000": {day(18), day(19), day(20), day(40)}, "Q-0000": {day(10)},
		"Y-0000": {day(18), day(19), day(20), day(40)}, "Z-0000": {day(30)},
	} {
		if got := u.snaps[accountKey{src, acct}]; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s speaks at %v, want %v", acct, got, want)
		}
	}
	for _, c := range []struct {
		k    unitKey
		at   int64
		want float64
	}{
		{unitKey{src, "X-0000", "QX"}, day(35), 1},
		{unitKey{src, "X-0000", "QX"}, day(45), 0},
		{unitKey{src, "Q-0000", "QQ"}, day(30), 1},
	} {
		if got := u.heldOn(c.k, c.at); got == nil || *got != c.want {
			t.Errorf("%s/%s at day %d: %v, want %v", c.k.acct, c.k.iid, c.at/86400, fmtFloatPtr(got), c.want)
		}
	}
}

// seedCarriedFund writes a fund an account's own statement lists in
// January, and the source's monthly runs of its other account after.
func seedCarriedFund(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	execEach(t, db,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, symbol, name,
		    currency, first_seen_at, last_seen_at) VALUES
		    ('test-src', 'FUNDQ', 'public_equity', 'etf', 'FUNDQ', 'Fund Q ETF', 'USD', 0, 0),
		    ('test-src', 'STKM', 'public_equity', 'stock', 'STKM', 'Stock M', 'USD', 0, 0)`)
	at := func(m, d int) int64 { return ymd(2026, m, d).Unix() + 43200 }
	execEach(t, db, `INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
	    instrument_external_id, asset_class, vehicle, currency, quantity, market_value)
	    VALUES ('test-src', `+itoa(at(1, 15))+`, 'A2', 'FUNDQ', 'FUNDQ', 'public_equity', 'etf', 'USD', 10, 500)`)
	for _, m := range []int{1, 2, 3} {
		pitPosition(t, db, ctx, "test-src", "A1", "STKM", at(m+1, 1)-86400, 100)
	}
}

// wantedFund is the wanted list's fund of that reference, nil for none.
func wantedFund(w *Wanted, ref string) *WantedFund {
	for i := range w.Funds {
		if w.Funds[i].Ref == ref {
			return &w.Funds[i]
		}
	}
	return nil
}

// TestRefdataWantedHoldsWhatItsAccountCarries: a fund an account's own
// statement lists stays held while the source's newer runs name only its
// other account.
func TestRefdataWantedHoldsWhatItsAccountCarries(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	seedCarriedFund(t, db, ctx)
	w, err := RefdataWanted(ctx, db, ymd(2026, 4, 10))
	if err != nil {
		t.Fatal(err)
	}
	if f := wantedFund(w, "symbol:FUNDQ"); f == nil || f.HeldFrom == nil || *f.HeldFrom != "2026-01-15" ||
		f.HeldTo != nil {
		t.Errorf("FUNDQ %+v of %+v", f, w.Funds)
	}
}

// TestRefdataWantedBeforeMigration0140: a gold at migration 0139 (a
// read-only open of an older file) has no pos_active_at and reads its
// source's newest snapshot as every account's, so the fund ends at its
// own last snapshot.
func TestRefdataWantedBeforeMigration0140(t *testing.T) {
	t.Parallel()
	db, ctx := openAtVersion(t, 139)
	seedCarriedFund(t, db, ctx)
	w, err := RefdataWanted(ctx, db, ymd(2026, 4, 10))
	if err != nil {
		t.Fatal(err)
	}
	if f := wantedFund(w, "symbol:FUNDQ"); f == nil || f.HeldTo == nil || *f.HeldTo != "2026-01-15" {
		t.Errorf("FUNDQ %+v of %+v", f, w.Funds)
	}
}

// TestCheckReadsEachAccountsActiveSnapshot: B's own run lands after
// A's last snapshot in the window; the positions check still compares A
// at its own.
func TestCheckReadsEachAccountsActiveSnapshot(t *testing.T) {
	db, ctx := lotFixture(t, tradesSeed+`
        UPDATE positions SET book_value = 410 WHERE snapshot_at = 450*86400;
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key,
                               instrument_external_id, asset_class, vehicle, currency, quantity, market_value) VALUES
            ('lot-src', 480*86400, 'B', 'QRS', 'QRS', 'public_equity', 'stock', 'USD', 5, 50);`)
	shadow := lots.Shadow
	avg := lots.Average
	rebuild(t, db, ctx, lots.Config{Sources: map[string]lots.SourceConfig{"lot-src": {Mode: &shadow, Method: &avg}}}, false)
	rows, err := GainsCheck(ctx, db, 0, 500*lotDay, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Check != "positions" || rows[0].AccountExternalID != "A" ||
		!near(num(t, rows[0].EngineCost), 400) || !near(num(t, rows[0].StatedCost), 410) {
		t.Fatalf("check rows %+v", rows)
	}
}

// TestMigration0140DDLIsRerunnable holds the migration to the replay bar:
// a second application changes no answer.
func TestMigration0140DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	seedPartialRuns(t, db, ctx)
	const accounts = `SELECT * FROM report_accounts_multi(9223372036854775807)`
	const history = `SELECT * FROM hist_active_pos() UNION ALL BY NAME SELECT * FROM hist_active_cash()`
	snapshot := func(q string) string {
		t.Helper()
		var s string
		if err := db.QueryRowContext(ctx, `SELECT string_agg(CAST(x AS VARCHAR), ';' ORDER BY CAST(x AS VARCHAR))
		    FROM (`+q+`) x`).Scan(&s); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return s
	}
	before, beforeHistory := snapshot(accounts), snapshot(history)
	rerunMigrationDDL(t, db, ctx, "0140_snapshots_per_account.sql")
	if snapshot(accounts) != before || snapshot(history) != beforeHistory || before == "" {
		t.Error("the replay changed an answer")
	}
}
