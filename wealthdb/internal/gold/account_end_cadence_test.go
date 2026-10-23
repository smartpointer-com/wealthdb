package gold

import (
	"context"
	"database/sql"
	"testing"
)

// A key's last observation ends at its re-cover snapshot only where its
// source's newest snapshot lies more than one and a half of the key's
// cadences past it (migration 0144). These tests pin that second
// condition on invented books, in the history and at an instant alike.

// cadenceEnd is a key's ending snapshot by pos_key_ends (no ccy) or
// cash_key_ends, absent where it has none.
func cadenceEnd(t *testing.T, db *sql.DB, ctx context.Context, src, acct, ccy string) (int64, bool) {
	t.Helper()
	q := `SELECT end_snap FROM pos_key_ends() WHERE src = ? AND acct = ?`
	args := []any{src, acct}
	if ccy != "" {
		q = `SELECT end_snap FROM cash_key_ends() WHERE src = ? AND acct = ? AND ccy = ?`
		args = append(args, ccy)
	}
	var end int64
	switch err := db.QueryRowContext(ctx, q, args...).Scan(&end); err {
	case nil:
		return end, true
	case sql.ErrNoRows:
		return 0, false
	default:
		t.Fatalf("%s: %v", q, err)
		return 0, false
	}
}

// accountOn is an account's total in the history on a day and in the
// point-in-time report at that day's last second; ok says whether the
// history has a row for it.
func accountOn(t *testing.T, db *sql.DB, ctx context.Context, src, acct string, off int64) (hist, pit float64, ok bool) {
	t.Helper()
	day := histToday() + off
	var h sql.NullFloat64
	err := db.QueryRowContext(ctx, `
        SELECT CAST(total_value_outccy AS DOUBLE) FROM report_accounts_history('USD')
         WHERE silver_source_id = ? AND account_external_id = ? AND as_of_day = ?`,
		src, acct, day*86400).Scan(&h)
	if err != nil && err != sql.ErrNoRows {
		t.Fatalf("history of %s on day %+d: %v", acct, off, err)
	}
	if err := db.QueryRowContext(ctx, `
        SELECT COALESCE(CAST(total_value_outccy AS DOUBLE), 0) FROM report_accounts(?, 'USD')
         WHERE silver_source_id = ? AND account_external_id = ?`,
		day*86400+86399, src, acct).Scan(&pit); err != nil {
		t.Fatalf("report_accounts of %s on day %+d: %v", acct, off, err)
	}
	return h.Float64, pit, err == nil
}

// TestASlowAccountStandsWithinItsCadence is the quarterly account beside
// monthly ones. A monthly statement names M every month and Q at quarter
// ends; M's next month re-covers Q's company without it. Where Q's last
// quarter end is recent, inside one and a half quarters of the source's
// newest statement, Q stands: through the month ends after it, in the
// history and at an instant alike. Where the source has run past that,
// Q is closed, and it ends at the month after its last quarter end, at
// every instant. A new quarterly account, seen at one quarter end only,
// takes a month as its cadence: it ends at its second month end. A cash
// leg reported monthly in the runs of a leg reported nightly stands as a
// quarterly account does.
func TestASlowAccountStandsWithinItsCadence(t *testing.T) {
	db, ctx := openMigrated(t)
	for _, a := range []struct{ src, acct, kind string }{
		{"recent-src", "M-0000", "brokerage"}, {"recent-src", "Q-0000", "brokerage"},
		{"closed-src", "M-0001", "brokerage"}, {"closed-src", "Q-0001", "brokerage"},
		{"new-src", "M-0002", "brokerage"}, {"new-src", "Q-0002", "brokerage"},
		{"legs-src", "L-0000", "cash"},
	} {
		seedHistAccount(t, db, ctx, a.src, a.acct, a.kind, "Account EXAMPLE")
	}
	// recent-src: Q's last quarter end is 60 days old, the newest
	// statement 30 days past it.
	for off := int64(-240); off <= -30; off += 30 {
		pitPosition(t, db, ctx, "recent-src", "M-0000", "MX", pitAt(off, 18), 100)
		if off%90 == -60%90 {
			pitPosition(t, db, ctx, "recent-src", "Q-0000", "QX", pitAt(off, 18), 500)
		}
	}
	// closed-src: Q's last quarter end is 240 days old, the newest
	// statement 210 days past it.
	for off := int64(-420); off <= -30; off += 30 {
		pitPosition(t, db, ctx, "closed-src", "M-0001", "MX", pitAt(off, 18), 100)
		if off <= -240 && off%90 == -240%90 {
			pitPosition(t, db, ctx, "closed-src", "Q-0001", "QX", pitAt(off, 18), 500)
		}
	}
	// new-src: Q's one quarter end is 90 days old, the newest statement
	// 60 days past it.
	for off := int64(-150); off <= -30; off += 30 {
		pitPosition(t, db, ctx, "new-src", "M-0002", "MX", pitAt(off, 18), 100)
	}
	pitPosition(t, db, ctx, "new-src", "Q-0002", "QX", pitAt(-90, 18), 500)
	// legs-src: the USD leg nightly, the EUR leg in its run once a month;
	// the EUR leg's last run is 20 days old.
	seedFX(t, db, pitAt(-200, 12), "USD", "EUR", "1.0")
	for off := int64(-80); off <= 0; off++ {
		pitCash(t, db, ctx, "legs-src", "L-0000", "USD", pitAt(off, 2), 10)
		if off%30 == -20%30 {
			pitCash(t, db, ctx, "legs-src", "L-0000", "EUR", pitAt(off, 2), 300)
		}
	}

	if end, ok := cadenceEnd(t, db, ctx, "recent-src", "Q-0000", ""); ok {
		t.Errorf("the recent quarterly account ends at day %+d, want no ending", end/86400-histToday())
	}
	if end, ok := cadenceEnd(t, db, ctx, "closed-src", "Q-0001", ""); !ok || end != pitAt(-210, 18) {
		t.Errorf("the closed quarterly account ends at %d (%v), want its re-cover statement %d",
			end, ok, pitAt(-210, 18))
	}
	if end, ok := cadenceEnd(t, db, ctx, "new-src", "Q-0002", ""); !ok || end != pitAt(-60, 18) {
		t.Errorf("the new quarterly account ends at %d (%v), want its second month end %d", end, ok, pitAt(-60, 18))
	}
	if end, ok := cadenceEnd(t, db, ctx, "legs-src", "L-0000", "EUR"); ok {
		t.Errorf("the monthly leg ends at day %+d, want no ending", end/86400-histToday())
	}
	for _, c := range []struct {
		src, acct string
		off       int64
		want      float64
	}{
		{"recent-src", "Q-0000", -31, 500}, {"recent-src", "Q-0000", -30, 500}, {"recent-src", "Q-0000", 0, 500},
		{"closed-src", "Q-0001", -211, 500}, {"closed-src", "Q-0001", -210, 0}, {"closed-src", "Q-0001", 0, 0},
		{"new-src", "Q-0002", -61, 500}, {"new-src", "Q-0002", -60, 0},
		{"legs-src", "L-0000", -21, 310}, {"legs-src", "L-0000", -19, 310}, {"legs-src", "L-0000", 0, 310},
	} {
		hist, pit, _ := accountOn(t, db, ctx, c.src, c.acct, c.off)
		if !near(hist, c.want) || !near(pit, c.want) {
			t.Errorf("%s on day %+d: history %v, at the day's end %v, want %v", c.acct, c.off, hist, pit, c.want)
		}
	}
}

// TestBurstAccountsStandBetweenBursts is why the cadence alone ends
// nothing. Two cash accounts are reported only on the days a second run
// of their source refreshes them, beside a nightly run of another
// account. Their cadence is a day, inside each burst, and their source
// runs on nightly. Each burst is a run of its own, so no nightly run
// covers their company, and nothing ends them between bursts.
func TestBurstAccountsStandBetweenBursts(t *testing.T) {
	db, ctx := openMigrated(t)
	for _, acct := range []string{"F-0000", "B-0000", "B-0001"} {
		seedHistAccount(t, db, ctx, "burst-src", acct, "cash", "Account EXAMPLE")
	}
	for off := int64(-90); off <= 0; off++ {
		pitCash(t, db, ctx, "burst-src", "F-0000", "USD", pitAt(off, 2), 1000)
	}
	for _, off := range []int64{-60, -59, -58, -20, -19} {
		pitCash(t, db, ctx, "burst-src", "B-0000", "USD", pitAt(off, 10), 200)
		pitCash(t, db, ctx, "burst-src", "B-0001", "USD", pitAt(off, 10), 300)
	}
	for _, acct := range []string{"B-0000", "B-0001"} {
		if end, ok := cadenceEnd(t, db, ctx, "burst-src", acct, "USD"); ok {
			t.Errorf("%s ends at day %+d, want no ending", acct, end/86400-histToday())
		}
		for _, off := range []int64{-40, -10, 0} {
			if hist, pit, ok := accountOn(t, db, ctx, "burst-src", acct, off); !ok || hist == 0 || pit == 0 {
				t.Errorf("%s on day %+d: history %v (%v), at the day's end %v, want it standing",
					acct, off, hist, ok, pit)
			}
		}
	}
}

// TestTheCadenceIsTheMedianOfTheKeysDays reads the cadence off a key's
// own observation days, in either table. K is read twice on its first
// day, then 5, 10 and 50 days apart, each time in a run with P, which runs
// on alone from the day after K's last. The median of its day gaps is 10,
// so K's last observation ends at P's next run once the source runs more
// than 15 days past it, and stands at exactly 15. Two readings of a day
// would make the shortest gap hours; the shortest day gap of 5 would end
// it at 8 days, the mean of 22 hold it open 33, the longest 75.
func TestTheCadenceIsTheMedianOfTheKeysDays(t *testing.T) {
	for _, table := range []string{"positions", "cash"} {
		for _, c := range []struct {
			name   string
			newest int64 // seconds past K's last observation
			ends   bool
		}{
			{"exactly one and a half cadences on", 15 * 86400, false},
			{"a second past", 15*86400 + 1, true},
		} {
			t.Run(table+", "+c.name, func(t *testing.T) {
				db, ctx := openMigrated(t)
				seed := func(acct string, at int64) {
					if table == "cash" {
						pitCash(t, db, ctx, "med-src", acct, "USD", at, 100)
					} else {
						pitPosition(t, db, ctx, "med-src", acct, acct, at, 100)
					}
				}
				last := pitAt(-35, 18)
				for _, at := range []int64{pitAt(-100, 6), pitAt(-100, 18), pitAt(-95, 18), pitAt(-85, 18), last} {
					seed("K-0000", at)
					seed("P-0000", at)
				}
				seed("P-0000", pitAt(-34, 18))
				seed("P-0000", last+c.newest)
				ccy := ""
				if table == "cash" {
					ccy = "USD"
				}
				end, ok := cadenceEnd(t, db, ctx, "med-src", "K-0000", ccy)
				if ok != c.ends || (ok && end != pitAt(-34, 18)) {
					t.Errorf("K-0000 ends at %d (%v), want an ending %v at its re-cover run %d",
						end, ok, c.ends, pitAt(-34, 18))
				}
			})
		}
	}
}

// TestASoldOutAccountEndsAtTheRunThatStatesItsCash: S sold every holding
// and keeps the proceeds as cash. The next run of its monthly statement
// states S's cash and none of its holdings: that run reported S, so its
// holdings end there with no grace, and S is its cash alone. Where the run
// states no cash of S, it says nothing of S itself, and S's holdings stand
// inside their grace.
func TestASoldOutAccountEndsAtTheRunThatStatesItsCash(t *testing.T) {
	db, ctx := openMigrated(t)
	for _, src := range []string{"cash-src", "nocash-src"} {
		for _, acct := range []string{"A-0000", "S-0000"} {
			seedHistAccount(t, db, ctx, src, acct, "brokerage", "Account EXAMPLE")
		}
		for off := int64(-150); off <= -30; off += 30 {
			pitPosition(t, db, ctx, src, "A-0000", "AX", pitAt(off, 18), 100)
			if off < -30 {
				pitPosition(t, db, ctx, src, "S-0000", "SX", pitAt(off, 18), 5000)
			}
		}
	}
	pitCash(t, db, ctx, "cash-src", "S-0000", "USD", pitAt(-30, 18), 4900)
	if end, ok := cadenceEnd(t, db, ctx, "cash-src", "S-0000", ""); !ok || end != pitAt(-30, 18) {
		t.Errorf("S's holdings end at %d (%v), want the run that states its cash %d", end, ok, pitAt(-30, 18))
	}
	if end, ok := cadenceEnd(t, db, ctx, "nocash-src", "S-0000", ""); ok {
		t.Errorf("S's holdings end at day %+d where no run states its cash, want them standing", end/86400-histToday())
	}
	for _, c := range []struct {
		src  string
		want float64
	}{{"cash-src", 4900}, {"nocash-src", 5000}} {
		for _, off := range []int64{-30, 0} {
			if hist, pit, _ := accountOn(t, db, ctx, c.src, "S-0000", off); !near(hist, c.want) || !near(pit, c.want) {
				t.Errorf("%s S on day %+d: history %v, at the day's end %v, want %v", c.src, off, hist, pit, c.want)
			}
		}
	}
}

// TestACashRunMovesNoPositionsGrace: the grace reads the clock of the
// key's own table, as the horizon does. A's holdings are reported nightly
// with B's to A's last day, B's re-cover them the next night, and then the
// source's positions stop while its cash runs on to today. A source that
// stops running ends nothing, so A stands.
func TestACashRunMovesNoPositionsGrace(t *testing.T) {
	db, ctx := openMigrated(t)
	for off := int64(-60); off <= -51; off++ {
		if off < -51 {
			pitPosition(t, db, ctx, "wind-src", "A-0000", "AX", pitAt(off, 2), 100)
		}
		pitPosition(t, db, ctx, "wind-src", "B-0000", "BX", pitAt(off, 2), 100)
	}
	for off := int64(-60); off <= 0; off++ {
		pitCash(t, db, ctx, "wind-src", "C-0000", "USD", pitAt(off, 2), 100)
	}
	if end, ok := cadenceEnd(t, db, ctx, "wind-src", "A-0000", ""); ok {
		t.Errorf("A ends at day %+d, want it standing: no positions run passed its grace", end/86400-histToday())
	}
}
