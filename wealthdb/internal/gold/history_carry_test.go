package gold

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// The daily-history macros resolve the active snapshot per key — (source,
// account) for positions, (source, account, currency) for cash — so every
// key contributes its own most recent observation and a run that covers
// part of a source cannot drop what it did not touch. These tests pin that
// rule, the zero-is-an-observation rule that keeps carry-forward honest,
// and the two endings that bound the carry: a later run of the same source
// that re-covers the key's company, and the source-clock horizon behind it.
// All fixtures are synthetic.

// histDay0 is the UTC day the fixtures anchor on, read ONCE per test binary
// so that seeding and asserting cannot straddle midnight and resolve against
// two different days.
var histDay0 = time.Now().UTC().Unix() / 86400

func histToday() int64 { return histDay0 }

// snapAt is the epoch second a snapshot `off` days from today carries —
// noon, so no day boundary is ambiguous. dayAt is the UTC-midnight
// epoch the history macros emit as as_of_day for that same day.
func snapAt(off int64) int64 { return (histToday()+off)*86400 + 43200 }
func dayAt(off int64) int64  { return (histToday() + off) * 86400 }

// seedHistAccount registers one USD account in the accounts dimension.
func seedHistAccount(t *testing.T, db *sql.DB, ctx context.Context, src, acct, kind, name string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              display_name, base_currency, first_seen_at, last_seen_at)
        VALUES (?, ?, ?, ?, 'USD', 0, 0)`, src, acct, kind, name); err != nil {
		t.Fatalf("seed account %s/%s: %v", src, acct, err)
	}
}

// seedHistCashIn writes one balance in `ccy` under the snapshot `off` days
// from today; seedHistCash is its USD case.
func seedHistCashIn(t *testing.T, db *sql.DB, ctx context.Context, src, acct, ccy string, off int64, amount string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO cash_balances (silver_source_id, snapshot_at, account_external_id,
                                   currency, balance_kind, amount)
        VALUES (?, ?, ?, ?, 'current', CAST(? AS DECIMAL(28,4)))`,
		src, snapAt(off), acct, ccy, amount); err != nil {
		t.Fatalf("seed balance %s/%s@%+d: %v", src, acct, off, err)
	}
}

func seedHistCash(t *testing.T, db *sql.DB, ctx context.Context, src, acct string, off int64, amount string) {
	t.Helper()
	seedHistCashIn(t, db, ctx, src, acct, "USD", off, amount)
}

// seedHistPosition writes one holding under the snapshot `off` days from today.
func seedHistPosition(t *testing.T, db *sql.DB, ctx context.Context, src, acct, key string, off int64, value string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO positions (silver_source_id, snapshot_at, account_external_id,
                               position_key, asset_class, vehicle, currency, market_value)
        VALUES (?, ?, ?, ?, 'public_equity', 'stock', 'USD', CAST(? AS DECIMAL(28,4)))`,
		src, snapAt(off), acct, key, value); err != nil {
		t.Fatalf("seed position %s/%s/%s@%+d: %v", src, acct, key, off, err)
	}
}

// histAccountsOn maps account -> (positions, cash, total) for one
// source on the day `off` days from today, as report_accounts_history
// reports it. An account missing from the map contributed nothing to
// that day at all.
type histValues struct{ pos, cash, total float64 }

func histAccountsOn(t *testing.T, db *sql.DB, ctx context.Context, src string, off int64) map[string]histValues {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
        SELECT account_external_id,
               CAST(positions_value_outccy AS DOUBLE),
               CAST(cash_balance_outccy AS DOUBLE),
               CAST(total_value_outccy AS DOUBLE)
          FROM report_accounts_history('USD')
         WHERE silver_source_id = ? AND as_of_day = ?`, src, dayAt(off))
	if err != nil {
		t.Fatalf("report_accounts_history: %v", err)
	}
	defer rows.Close()
	out := map[string]histValues{}
	for rows.Next() {
		var acct string
		var v histValues
		if err := rows.Scan(&acct, &v.pos, &v.cash, &v.total); err != nil {
			t.Fatalf("scan account history: %v", err)
		}
		out[acct] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate account history: %v", err)
	}
	return out
}

// histSourceTotalOn reads report_sources_history's daily total for one
// source; ok is false when the source has no row on that day.
func histSourceTotalOn(t *testing.T, db *sql.DB, ctx context.Context, src string, off int64) (float64, bool) {
	t.Helper()
	var tot sql.NullFloat64
	err := db.QueryRowContext(ctx, `
        SELECT CAST(total_value_outccy AS DOUBLE) FROM report_sources_history('USD')
         WHERE silver_source_id = ? AND as_of_day = ?`, src, dayAt(off)).Scan(&tot)
	if err == sql.ErrNoRows {
		return 0, false
	}
	if err != nil {
		t.Fatalf("report_sources_history: %v", err)
	}
	return tot.Float64, tot.Valid
}

// latestTotals maps account -> total for what the POINT-IN-TIME report
// (the per-source rule migration 0021 keeps) says about a source today. It
// carries a row for every account in the dimension, 0 when the source's
// latest snapshot gave it no lines — so the history, which emits rows only
// for account-days that have lines, agrees with it on VALUES, not on rows.
func latestTotals(t *testing.T, db *sql.DB, ctx context.Context, src string) map[string]float64 {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
        SELECT account_external_id, CAST(total_value_outccy AS DOUBLE)
          FROM report_accounts(?, 'USD') WHERE silver_source_id = ?`, dayAt(0)+86399, src)
	if err != nil {
		t.Fatalf("report_accounts: %v", err)
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var a string
		var v sql.NullFloat64
		if err := rows.Scan(&a, &v); err != nil {
			t.Fatalf("scan report_accounts: %v", err)
		}
		if !v.Valid {
			t.Errorf("report_accounts total for %s is NULL", a)
		}
		out[a] = v.Float64
	}
	return out
}

// wantAccounts asserts the exact per-account picture of a source-day.
func wantAccounts(t *testing.T, got map[string]histValues, off int64, want map[string]histValues) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("day %+d: accounts = %v, want %v", off, got, want)
		return
	}
	for acct, w := range want {
		g, ok := got[acct]
		if !ok {
			t.Errorf("day %+d: account %s missing, want %v", off, acct, w)
			continue
		}
		if !nearly(g.pos, w.pos) || !nearly(g.cash, w.cash) || !nearly(g.total, w.total) {
			t.Errorf("day %+d: account %s = %v, want %v", off, acct, g, w)
		}
	}
}

// TestHistoryCarriesAccountsAcrossPartialRuns is the defect this rule
// exists for: a source whose accounts arrive under DIFFERENT snapshots
// — a card's statement closing on one day, a deposit re-dump on the
// next — must still report every account on every day, and the day's
// total must be the sum of both accounts' last known balances. Under
// one active snapshot per source each day showed whichever account
// happened to snapshot last, so a card-only day read as the card's
// negative balance alone.
func TestHistoryCarriesAccountsAcrossPartialRuns(t *testing.T) {
	db, ctx := openMigrated(t)
	const src = "partial-src"
	seedHistAccount(t, db, ctx, src, "DEP-0000", "cash", "Deposit EXAMPLE")
	seedHistAccount(t, db, ctx, src, "CARD-0000", "card", "Card EXAMPLE")

	seedHistCash(t, db, ctx, src, "DEP-0000", -5, "900")   // deposit run
	seedHistCash(t, db, ctx, src, "CARD-0000", -4, "-600") // card run, its own snapshot
	seedHistCash(t, db, ctx, src, "DEP-0000", -3, "1000")  // deposit run again

	// The card-only day carries the deposit forward instead of dropping it.
	wantAccounts(t, histAccountsOn(t, db, ctx, src, -4), -4, map[string]histValues{
		"DEP-0000":  {cash: 900, total: 900},
		"CARD-0000": {cash: -600, total: -600},
	})
	// ...and the deposit-only day carries the card, negative balance and all.
	wantAccounts(t, histAccountsOn(t, db, ctx, src, -3), -3, map[string]histValues{
		"DEP-0000":  {cash: 1000, total: 1000},
		"CARD-0000": {cash: -600, total: -600},
	})

	for _, c := range []struct {
		off  int64
		want float64
	}{{-5, 900}, {-4, 300}, {-3, 400}, {0, 400}} {
		got, ok := histSourceTotalOn(t, db, ctx, src, c.off)
		if !ok {
			t.Errorf("day %+d: no source total", c.off)
			continue
		}
		if !nearly(got, c.want) {
			t.Errorf("day %+d: source total = %v, want %v (every account's last balance)",
				c.off, got, c.want)
		}
	}
}

// TestHistoryZeroObservationWins pins the rule carry-forward depends
// on: a balance of zero is an observation, not an absence. The line
// bases used to drop `amount <> 0` rows as noise, which was harmless
// while a day resolved to one snapshot per source and is a bug under
// carry-forward — an account paid down to zero would keep contributing
// its last non-zero balance for ever.
func TestHistoryZeroObservationWins(t *testing.T) {
	db, ctx := openMigrated(t)
	const src = "zero-src"
	seedHistAccount(t, db, ctx, src, "DEP-0000", "cash", "Drained EXAMPLE")
	seedHistAccount(t, db, ctx, src, "DEP-0001", "cash", "Everyday EXAMPLE")

	seedHistCash(t, db, ctx, src, "DEP-0000", -6, "500") // its own run
	seedHistCash(t, db, ctx, src, "DEP-0001", -5, "100")
	seedHistCash(t, db, ctx, src, "DEP-0000", -4, "0")   // drained to zero
	seedHistCash(t, db, ctx, src, "DEP-0001", -2, "100") // a run that ignores DEP-0000

	wantAccounts(t, histAccountsOn(t, db, ctx, src, -5), -5, map[string]histValues{
		"DEP-0000": {cash: 500, total: 500},
		"DEP-0001": {cash: 100, total: 100},
	})
	for _, off := range []int64{-4, -2, 0} {
		wantAccounts(t, histAccountsOn(t, db, ctx, src, off), off, map[string]histValues{
			"DEP-0000": {cash: 0, total: 0},
			"DEP-0001": {cash: 100, total: 100},
		})
	}
	if got, ok := histSourceTotalOn(t, db, ctx, src, 0); !ok || !nearly(got, 100) {
		t.Errorf("today's source total = %v (ok=%v), want 100 — the zero must not "+
			"carry the drained account's old 500", got, ok)
	}
}

// TestHistoryZeroLineNeedsNoFxRate: keeping zero rows means they reach
// the FX conversion, where a currency nobody quotes has no path at all.
// A zero is worth zero in every currency, so it must convert to 0 rather
// than to NULL — a NULL would propagate through the day's SUM and blank
// the whole account (and with it the source and global totals), not just
// that line.
func TestHistoryZeroLineNeedsNoFxRate(t *testing.T) {
	db, ctx := openMigrated(t)
	const src = "nofx-src"
	seedHistAccount(t, db, ctx, src, "ACC-0000", "brokerage", "Exotic EXAMPLE")
	seedHistPosition(t, db, ctx, src, "ACC-0000", "X", -3, "1000")
	seedHistCashIn(t, db, ctx, src, "ACC-0000", "XTS", -3, "0") // no rate for XTS

	for _, off := range []int64{-3, 0} {
		wantAccounts(t, histAccountsOn(t, db, ctx, src, off), off, map[string]histValues{
			"ACC-0000": {pos: 1000, cash: 0, total: 1000},
		})
	}
	var global sql.NullFloat64
	if err := db.QueryRowContext(ctx, `
        SELECT CAST(total_value_outccy AS DOUBLE) FROM report_global_history('USD')
         WHERE as_of_day = ?`, dayAt(0)).Scan(&global); err != nil {
		t.Fatalf("report_global_history: %v", err)
	}
	if !global.Valid || !nearly(global.Float64, 1000) {
		t.Errorf("global total = %v (valid=%v), want 1000 — an unconvertible zero "+
			"must contribute 0, not blank the account", global.Float64, global.Valid)
	}
}

// TestHistoryFullDumpSupersedesADroppedAccount pins the first of the two
// endings. A run that re-covers everything an account was last seen with,
// without the account itself, was in a position to report it and did not:
// the account is closed, not merely uncovered, and leaves the series on
// that day — as it did under the per-source rule, and as the point-in-time
// report still says.
func TestHistoryFullDumpSupersedesADroppedAccount(t *testing.T) {
	db, ctx := openMigrated(t)
	const src = "dump-src"
	seedHistAccount(t, db, ctx, src, "A-0000", "cash", "Closed EXAMPLE")
	seedHistAccount(t, db, ctx, src, "B-0000", "cash", "Live EXAMPLE")

	seedHistCash(t, db, ctx, src, "A-0000", -4, "500") // one run covers both
	seedHistCash(t, db, ctx, src, "B-0000", -4, "100")
	seedHistCash(t, db, ctx, src, "B-0000", -2, "100") // the next covers B alone

	wantAccounts(t, histAccountsOn(t, db, ctx, src, -3), -3, map[string]histValues{
		"A-0000": {cash: 500, total: 500},
		"B-0000": {cash: 100, total: 100},
	})
	for _, off := range []int64{-2, 0} {
		wantAccounts(t, histAccountsOn(t, db, ctx, src, off), off, map[string]histValues{
			"B-0000": {cash: 100, total: 100},
		})
	}
	if got, ok := histSourceTotalOn(t, db, ctx, src, 0); !ok || !nearly(got, 100) {
		t.Errorf("today's source total = %v (ok=%v), want 100 — a re-dump without "+
			"A-0000 ends it", got, ok)
	}
	if got := latestTotals(t, db, ctx, src); !nearly(got["A-0000"], 0) || !nearly(got["B-0000"], 100) {
		t.Errorf("report_accounts today = %v, want A-0000 at 0 and B-0000 at 100 — "+
			"history and the point-in-time report must agree that A-0000 is gone", got)
	}
}

// TestHistoryFullDumpSupersedesSeveralDroppedKeys pins the same ending
// when MORE THAN ONE key leaves in the same run: an account whose every
// currency leg closes at once, two accounts closed together, and the
// positions side of both. Each departing key is company for the others,
// so a rule that waited for the WHOLE peer set to reappear would leave
// every one of them waiting on a key that is gone too, and none would
// ever end — the horizon alone would carry them, and the source's daily
// total would stay inflated for a year. Only peers still reported at the
// candidate snapshot are required, so the next dump ends them all and
// the history reconciles with the point-in-time report again.
func TestHistoryFullDumpSupersedesSeveralDroppedKeys(t *testing.T) {
	db, ctx := openMigrated(t)
	seedFX(t, db, snapAt(-30), "USD", "EUR", "1.2") // 1 EUR = 1.2 USD

	// A multi-currency account closing: its own legs are each other's
	// peers, and only the live sibling re-covers them.
	const multi = "multidrop-src"
	seedHistAccount(t, db, ctx, multi, "M-0000", "cash", "Closed EXAMPLE")
	seedHistAccount(t, db, ctx, multi, "M-0001", "cash", "Live EXAMPLE")
	seedHistCashIn(t, db, ctx, multi, "M-0000", "USD", -4, "300") // one run covers all legs
	seedHistCashIn(t, db, ctx, multi, "M-0000", "EUR", -4, "200")
	seedHistCashIn(t, db, ctx, multi, "M-0001", "USD", -4, "100")
	seedHistCashIn(t, db, ctx, multi, "M-0001", "USD", -2, "100") // the next covers M-0001 alone

	wantAccounts(t, histAccountsOn(t, db, ctx, multi, -3), -3, map[string]histValues{
		"M-0000": {cash: 540, total: 540}, // 300 USD + 200 EUR
		"M-0001": {cash: 100, total: 100},
	})
	for _, off := range []int64{-2, 0} {
		wantAccounts(t, histAccountsOn(t, db, ctx, multi, off), off, map[string]histValues{
			"M-0001": {cash: 100, total: 100},
		})
	}

	// Two single-currency accounts closing in the same nightly dump.
	const pair = "pairdrop-src"
	seedHistAccount(t, db, ctx, pair, "C-0000", "cash", "Closed EXAMPLE")
	seedHistAccount(t, db, ctx, pair, "C-0001", "card", "Closed EXAMPLE")
	seedHistAccount(t, db, ctx, pair, "C-0002", "cash", "Live EXAMPLE")
	seedHistCash(t, db, ctx, pair, "C-0000", -4, "500")
	seedHistCash(t, db, ctx, pair, "C-0001", -4, "250")
	seedHistCash(t, db, ctx, pair, "C-0002", -4, "100")
	seedHistCash(t, db, ctx, pair, "C-0002", -2, "100")

	wantAccounts(t, histAccountsOn(t, db, ctx, pair, -3), -3, map[string]histValues{
		"C-0000": {cash: 500, total: 500},
		"C-0001": {cash: 250, total: 250},
		"C-0002": {cash: 100, total: 100},
	})
	for _, off := range []int64{-2, 0} {
		wantAccounts(t, histAccountsOn(t, db, ctx, pair, off), off, map[string]histValues{
			"C-0002": {cash: 100, total: 100},
		})
	}

	// The same shape on the positions series, whose key is the account.
	const pos = "posdrop-src"
	seedHistAccount(t, db, ctx, pos, "D-0000", "brokerage", "Closed EXAMPLE")
	seedHistAccount(t, db, ctx, pos, "D-0001", "brokerage", "Closed EXAMPLE")
	seedHistAccount(t, db, ctx, pos, "D-0002", "brokerage", "Live EXAMPLE")
	seedHistPosition(t, db, ctx, pos, "D-0000", "X", -4, "1000")
	seedHistPosition(t, db, ctx, pos, "D-0001", "Y", -4, "400")
	seedHistPosition(t, db, ctx, pos, "D-0002", "Z", -4, "100")
	seedHistPosition(t, db, ctx, pos, "D-0002", "Z", -2, "100")

	wantAccounts(t, histAccountsOn(t, db, ctx, pos, -3), -3, map[string]histValues{
		"D-0000": {pos: 1000, total: 1000},
		"D-0001": {pos: 400, total: 400},
		"D-0002": {pos: 100, total: 100},
	})
	for _, off := range []int64{-2, 0} {
		wantAccounts(t, histAccountsOn(t, db, ctx, pos, off), off, map[string]histValues{
			"D-0002": {pos: 100, total: 100},
		})
	}

	// Today's history total is the live key's alone in each case, which
	// is what the point-in-time report says about the whole source.
	for _, src := range []string{multi, pair, pos} {
		var want float64
		for _, v := range latestTotals(t, db, ctx, src) {
			want += v
		}
		got, ok := histSourceTotalOn(t, db, ctx, src, 0)
		if !ok || !nearly(got, want) || !nearly(want, 100) {
			t.Errorf("%s: history total today = %v (ok=%v), report_accounts total = %v, "+
				"want both at 100 — every key of the closing run must end on the next dump",
				src, got, ok, want)
		}
	}
}

// TestHistoryCarryIsBoundedBySourceClock pins the second ending, the one
// that catches a key nothing ever re-covers because it was observed alone.
// It stops contributing once its OWN source has produced a snapshot more
// than hist_carry_days() days after its last observation: the source
// demonstrably kept running and never mentioned it again. The evidence is
// the source's own clock, never the calendar — a source that stops running
// supersedes nothing, so its accounts keep their last values (unchanged
// from the per-source carry this replaced).
func TestHistoryCarryIsBoundedBySourceClock(t *testing.T) {
	db, ctx := openMigrated(t)
	const src = "bound-src"
	seedHistAccount(t, db, ctx, src, "H-0000", "cash", "Forgotten EXAMPLE")
	seedHistAccount(t, db, ctx, src, "K-0000", "cash", "Live EXAMPLE")

	seedHistCash(t, db, ctx, src, "H-0000", -400, "400") // its own run, never repeated
	seedHistCash(t, db, ctx, src, "K-0000", -399, "300")
	seedHistCash(t, db, ctx, src, "K-0000", -30, "300") // 370 days after H's

	// The source's clock is still within the horizon of H's observation.
	for _, off := range []int64{-380, -31} {
		wantAccounts(t, histAccountsOn(t, db, ctx, src, off), off, map[string]histValues{
			"H-0000": {cash: 400, total: 400},
			"K-0000": {cash: 300, total: 300},
		})
	}
	// The run 370 days later carries the source past the horizon: H is gone.
	for _, off := range []int64{-30, 0} {
		wantAccounts(t, histAccountsOn(t, db, ctx, src, off), off, map[string]histValues{
			"K-0000": {cash: 300, total: 300},
		})
	}

	// A source that goes quiet supersedes nothing: its accounts keep
	// their last values to the end of the spine, however old.
	const dark = "dark-src"
	seedHistAccount(t, db, ctx, dark, "D-0000", "cash", "Quiet EXAMPLE")
	seedHistCash(t, db, ctx, dark, "D-0000", -400, "700")
	wantAccounts(t, histAccountsOn(t, db, ctx, dark, 0), 0, map[string]histValues{
		"D-0000": {cash: 700, total: 700},
	})
}

// TestHistorySlowCadenceAccountKeepsEveryDay: both endings apply to a
// key's LAST observation only, so neither can open a hole in the middle of
// a series. An account valued less often than the horizon is long — a
// private holding marked every other year, say — sits inside a source that
// snapshots far more often without ever falling out between its own marks.
func TestHistorySlowCadenceAccountKeepsEveryDay(t *testing.T) {
	db, ctx := openMigrated(t)
	const src = "slow-src"
	seedHistAccount(t, db, ctx, src, "M-0000", "brokerage", "Marked EXAMPLE")
	seedHistAccount(t, db, ctx, src, "N-0000", "brokerage", "Frequent EXAMPLE")

	// M is re-valued every ~380 days; N keeps the source busy in between.
	seedHistPosition(t, db, ctx, src, "M-0000", "P", -800, "5000")
	seedHistPosition(t, db, ctx, src, "M-0000", "P", -420, "6000")
	seedHistPosition(t, db, ctx, src, "M-0000", "P", -40, "7000")
	for _, off := range []int64{-800, -500, -200, -1} {
		seedHistPosition(t, db, ctx, src, "N-0000", "Q", off, "10")
	}

	var gaps int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM hist_days() d
         WHERE NOT EXISTS (
            SELECT 1 FROM report_accounts_history('USD') h
             WHERE h.silver_source_id = ? AND h.account_external_id = 'M-0000'
               AND h.as_of_day = d.day * 86400)`, src).Scan(&gaps); err != nil {
		t.Fatalf("gap count: %v", err)
	}
	if gaps != 0 {
		t.Errorf("M-0000 is absent on %d days of the spine, want 0 — an observation "+
			"with a later one behind it must never expire", gaps)
	}
	for _, c := range []struct {
		off  int64
		want float64
	}{{-800, 5000}, {-421, 5000}, {-420, 6000}, {-41, 6000}, {-40, 7000}, {0, 7000}} {
		got := histAccountsOn(t, db, ctx, src, c.off)
		if v, ok := got["M-0000"]; !ok || !nearly(v.pos, c.want) {
			t.Errorf("day %+d: M-0000 = %v (present=%v), want positions %v",
				c.off, v, ok, c.want)
		}
	}
}

// TestHistoryCashCarriesEachCurrencyLeg pins the cash grain. A balance row
// is per (account, CURRENCY), so a run that reports one leg of a
// multi-currency account carries the others rather than dropping them —
// and, symmetrically, a run that re-covers the account's other legs ends
// the one it left out.
func TestHistoryCashCarriesEachCurrencyLeg(t *testing.T) {
	db, ctx := openMigrated(t)
	seedFX(t, db, snapAt(-30), "USD", "EUR", "1.2") // 1 EUR = 1.2 USD

	// Legs arriving under their own runs: each carries independently.
	const legs = "legs-src"
	seedHistAccount(t, db, ctx, legs, "L-0000", "cash", "Legs EXAMPLE")
	seedHistCashIn(t, db, ctx, legs, "L-0000", "USD", -6, "100")
	seedHistCashIn(t, db, ctx, legs, "L-0000", "EUR", -5, "200")
	seedHistCashIn(t, db, ctx, legs, "L-0000", "USD", -2, "150")
	for _, c := range []struct {
		off  int64
		want float64
	}{{-6, 100}, {-5, 340}, {-3, 340}, {-2, 390}, {0, 390}} {
		wantAccounts(t, histAccountsOn(t, db, ctx, legs, c.off), c.off, map[string]histValues{
			"L-0000": {cash: c.want, total: c.want},
		})
	}

	// Both legs in one run, then a run that reports only the USD leg: the
	// EUR leg was re-coverable and was left out, so it ends there.
	const drop = "legdrop-src"
	seedHistAccount(t, db, ctx, drop, "L-0001", "cash", "Legs EXAMPLE")
	seedHistCashIn(t, db, ctx, drop, "L-0001", "USD", -6, "100")
	seedHistCashIn(t, db, ctx, drop, "L-0001", "EUR", -6, "200")
	seedHistCashIn(t, db, ctx, drop, "L-0001", "USD", -3, "150")
	for _, c := range []struct {
		off  int64
		want float64
	}{{-6, 340}, {-4, 340}, {-3, 150}, {0, 150}} {
		wantAccounts(t, histAccountsOn(t, db, ctx, drop, c.off), c.off, map[string]histValues{
			"L-0001": {cash: c.want, total: c.want},
		})
	}
}

// TestHistoryPositionsFollowTheAccountSnapshot pins the positions side
// of the same rule, and why positions need no zero filter of their own:
// the carry unit is the account's whole snapshot, so a holding that is
// absent from the account's next snapshot is sold, not carried, while
// an account another run passed over keeps every holding it had.
func TestHistoryPositionsFollowTheAccountSnapshot(t *testing.T) {
	db, ctx := openMigrated(t)
	const src = "pos-src"
	seedHistAccount(t, db, ctx, src, "P-0000", "brokerage", "Brokerage EXAMPLE")
	seedHistAccount(t, db, ctx, src, "Q-0000", "brokerage", "Custody EXAMPLE")

	seedHistPosition(t, db, ctx, src, "P-0000", "X", -5, "1000") // P's own run
	seedHistPosition(t, db, ctx, src, "P-0000", "Y", -5, "500")
	seedHistPosition(t, db, ctx, src, "Q-0000", "Z", -4, "700")
	seedHistPosition(t, db, ctx, src, "P-0000", "X", -3, "1200") // P-only run; Y sold
	seedHistPosition(t, db, ctx, src, "Q-0000", "Z", -1, "0")    // Q closed out at zero

	wantAccounts(t, histAccountsOn(t, db, ctx, src, -4), -4, map[string]histValues{
		"P-0000": {pos: 1500, total: 1500},
		"Q-0000": {pos: 700, total: 700},
	})
	wantAccounts(t, histAccountsOn(t, db, ctx, src, -3), -3, map[string]histValues{
		"P-0000": {pos: 1200, total: 1200}, // Y gone with P's own snapshot
		"Q-0000": {pos: 700, total: 700},   // untouched by that run, carried
	})
	wantAccounts(t, histAccountsOn(t, db, ctx, src, 0), 0, map[string]histValues{
		"P-0000": {pos: 1200, total: 1200},
		"Q-0000": {pos: 0, total: 0},
	})

	// The per-position series agrees: only the holdings of each
	// account's own active snapshot are on the day.
	rows, err := db.QueryContext(ctx, `
        SELECT account_external_id, position_key, CAST(value_outccy AS DOUBLE)
          FROM report_positions_history('USD')
         WHERE silver_source_id = ? AND as_of_day = ?
         ORDER BY 1, 2`, src, dayAt(-3))
	if err != nil {
		t.Fatalf("report_positions_history: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var acct, key string
		var val float64
		if err := rows.Scan(&acct, &key, &val); err != nil {
			t.Fatalf("scan position history: %v", err)
		}
		got = append(got, fmt.Sprintf("%s/%s=%g", acct, key, val))
	}
	want := "P-0000/X=1200 Q-0000/Z=700"
	if strings.Join(got, " ") != want {
		t.Errorf("positions on the P-only day = %q, want %q", strings.Join(got, " "), want)
	}
}

// dumpMacro renders a history macro's whole answer as sorted text.
// Every epoch column named here (as_of_day always, plus a macro's own
// snapshot_at) is normalised to a day offset from today, so the golden
// does not drift with the calendar; days past today (the spine follows
// the database's own clock, which can tick over mid-test) are excluded
// for the same reason. NULL is spelled out; every other column is its
// own rendering.
func dumpMacro(t *testing.T, db *sql.DB, ctx context.Context, macro, src string, epochCols ...string) string {
	t.Helper()
	epochs := append([]string{"as_of_day"}, epochCols...)
	proj := make([]string, len(epochs))
	for i, c := range epochs {
		proj[i] = fmt.Sprintf("(%s // 86400) - %d AS %s_off", c, histToday(), c)
	}
	q := fmt.Sprintf(`
        SELECT CAST(COLUMNS(*) AS VARCHAR) FROM (
            SELECT %s, * EXCLUDE (%s)
              FROM %s WHERE silver_source_id = '%s' AND as_of_day <= %d)`,
		strings.Join(proj, ", "), strings.Join(epochs, ", "), macro, src, dayAt(0))
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("%s: %v", macro, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("%s columns: %v", macro, err)
	}
	var lines []string
	for rows.Next() {
		cells := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("%s scan: %v", macro, err)
		}
		parts := make([]string, len(cells))
		for i, c := range cells {
			parts[i] = "<null>"
			if c.Valid {
				parts[i] = c.String
			}
		}
		lines = append(lines, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s iterate: %v", macro, err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// TestHistoryFullDumpSourceIsUnchanged holds the common case still: a
// source that writes every account in one run has one snapshot per day
// either way, so per-account resolution must reproduce the per-source
// answer byte for byte. The goldens were captured from the per-source
// macros before the rule changed. (The one place a full dump does move
// is an account observed at ZERO, which the per-source line bases
// dropped and these keep — TestHistoryZeroBalanceEmitsItsOwnRow.)
func TestHistoryFullDumpSourceIsUnchanged(t *testing.T) {
	db, ctx := openMigrated(t)
	const src = "full-src"
	seedHistAccount(t, db, ctx, src, "F-0000", "brokerage", "Brokerage EXAMPLE")
	seedHistAccount(t, db, ctx, src, "F-0001", "cash", "Deposit EXAMPLE")

	for _, r := range []struct {
		off       int64
		pos, cash string
	}{{-3, "1000", "250"}, {-1, "1100", "260"}} {
		seedHistPosition(t, db, ctx, src, "F-0000", "X", r.off, r.pos)
		seedHistCash(t, db, ctx, src, "F-0001", r.off, r.cash)
	}

	for _, c := range []struct {
		macro, want string
		epochCols   []string
	}{
		{"report_accounts_history('USD')", goldenAccountsHistory, nil},
		{"report_accounts_history_multi()", goldenAccountsHistoryMulti, nil},
		{"report_sources_history('USD')", goldenSourcesHistory, nil},
		{"report_portfolios_history('USD')", goldenPortfoliosHistory, nil},
		{"report_positions_history('USD')", goldenPositionsHistory, []string{"snapshot_at"}},
	} {
		if got := dumpMacro(t, db, ctx, c.macro, src, c.epochCols...); got != c.want {
			t.Errorf("%s over a full-dump source changed:\n--- got ---\n%s\n--- want ---\n%s",
				c.macro, got, c.want)
		}
	}
}

// TestHistoryZeroBalanceEmitsItsOwnRow is the one row-set difference a
// full dump does see. An account whose only line on a day is a zero
// balance produced NO history row while the line bases filtered
// `amount <> 0`; it now emits an explicit 0, which is what the
// point-in-time report has always said about it.
func TestHistoryZeroBalanceEmitsItsOwnRow(t *testing.T) {
	db, ctx := openMigrated(t)
	const src = "fullzero-src"
	seedHistAccount(t, db, ctx, src, "Z-0000", "cash", "Drained EXAMPLE")
	seedHistAccount(t, db, ctx, src, "Z-0001", "cash", "Live EXAMPLE")

	seedHistCash(t, db, ctx, src, "Z-0000", -3, "500") // one run writes both
	seedHistCash(t, db, ctx, src, "Z-0001", -3, "100")
	seedHistCash(t, db, ctx, src, "Z-0000", -1, "0") // the next drains Z-0000
	seedHistCash(t, db, ctx, src, "Z-0001", -1, "100")

	wantAccounts(t, histAccountsOn(t, db, ctx, src, -2), -2, map[string]histValues{
		"Z-0000": {cash: 500, total: 500},
		"Z-0001": {cash: 100, total: 100},
	})
	for _, off := range []int64{-1, 0} {
		wantAccounts(t, histAccountsOn(t, db, ctx, src, off), off, map[string]histValues{
			"Z-0000": {cash: 0, total: 0},
			"Z-0001": {cash: 100, total: 100},
		})
	}
	if got, ok := histSourceTotalOn(t, db, ctx, src, 0); !ok || !nearly(got, 100) {
		t.Errorf("today's source total = %v (ok=%v), want 100", got, ok)
	}
	if got := latestTotals(t, db, ctx, src); !nearly(got["Z-0000"], 0) || !nearly(got["Z-0001"], 100) {
		t.Errorf("report_accounts today = %v, want Z-0000 at 0 and Z-0001 at 100 — "+
			"the explicit zero row is what makes the two agree", got)
	}
}

// The goldens below are the per-source macros' verbatim answer to the
// full-dump fixture, captured before the active-snapshot rule changed.
// Column order is the macro's own; the leading field(s) are day offsets
// from today (as_of_day, then snapshot_at for the positions series).
const goldenAccountsHistory = `-1|full-src|F-0000|brokerage|Brokerage EXAMPLE|USD|<null>|<null>|<null>|<null>|<null>|<null>|1100.0000|0.0000|1100.0000|1100.0000|0.0000|1100.0000
-1|full-src|F-0001|cash|Deposit EXAMPLE|USD|<null>|<null>|<null>|<null>|<null>|<null>|0.0000|260.0000|260.0000|0.0000|260.0000|260.0000
-2|full-src|F-0000|brokerage|Brokerage EXAMPLE|USD|<null>|<null>|<null>|<null>|<null>|<null>|1000.0000|0.0000|1000.0000|1000.0000|0.0000|1000.0000
-2|full-src|F-0001|cash|Deposit EXAMPLE|USD|<null>|<null>|<null>|<null>|<null>|<null>|0.0000|250.0000|250.0000|0.0000|250.0000|250.0000
-3|full-src|F-0000|brokerage|Brokerage EXAMPLE|USD|<null>|<null>|<null>|<null>|<null>|<null>|1000.0000|0.0000|1000.0000|1000.0000|0.0000|1000.0000
-3|full-src|F-0001|cash|Deposit EXAMPLE|USD|<null>|<null>|<null>|<null>|<null>|<null>|0.0000|250.0000|250.0000|0.0000|250.0000|250.0000
0|full-src|F-0000|brokerage|Brokerage EXAMPLE|USD|<null>|<null>|<null>|<null>|<null>|<null>|1100.0000|0.0000|1100.0000|1100.0000|0.0000|1100.0000
0|full-src|F-0001|cash|Deposit EXAMPLE|USD|<null>|<null>|<null>|<null>|<null>|<null>|0.0000|260.0000|260.0000|0.0000|260.0000|260.0000`

const goldenAccountsHistoryMulti = `-1|full-src|F-0000|brokerage|Brokerage EXAMPLE|USD|<null>|<null>|<null>|<null>|taxable_personal|self_directed|1100.0000|0.0000|1100.0000|1100.0000|0.0000|1100.0000|<null>|0.0000|<null>|<null>|0.0000|<null>
-1|full-src|F-0001|cash|Deposit EXAMPLE|USD|<null>|<null>|<null>|<null>|taxable_personal|self_directed|0.0000|260.0000|260.0000|0.0000|260.0000|260.0000|0.0000|<null>|<null>|0.0000|<null>|<null>
-2|full-src|F-0000|brokerage|Brokerage EXAMPLE|USD|<null>|<null>|<null>|<null>|taxable_personal|self_directed|1000.0000|0.0000|1000.0000|1000.0000|0.0000|1000.0000|<null>|0.0000|<null>|<null>|0.0000|<null>
-2|full-src|F-0001|cash|Deposit EXAMPLE|USD|<null>|<null>|<null>|<null>|taxable_personal|self_directed|0.0000|250.0000|250.0000|0.0000|250.0000|250.0000|0.0000|<null>|<null>|0.0000|<null>|<null>
-3|full-src|F-0000|brokerage|Brokerage EXAMPLE|USD|<null>|<null>|<null>|<null>|taxable_personal|self_directed|1000.0000|0.0000|1000.0000|1000.0000|0.0000|1000.0000|<null>|0.0000|<null>|<null>|0.0000|<null>
-3|full-src|F-0001|cash|Deposit EXAMPLE|USD|<null>|<null>|<null>|<null>|taxable_personal|self_directed|0.0000|250.0000|250.0000|0.0000|250.0000|250.0000|0.0000|<null>|<null>|0.0000|<null>|<null>
0|full-src|F-0000|brokerage|Brokerage EXAMPLE|USD|<null>|<null>|<null>|<null>|taxable_personal|self_directed|1100.0000|0.0000|1100.0000|1100.0000|0.0000|1100.0000|<null>|0.0000|<null>|<null>|0.0000|<null>
0|full-src|F-0001|cash|Deposit EXAMPLE|USD|<null>|<null>|<null>|<null>|taxable_personal|self_directed|0.0000|260.0000|260.0000|0.0000|260.0000|260.0000|0.0000|<null>|<null>|0.0000|<null>|<null>`

const goldenSourcesHistory = `-1|full-src|USD|taxable_personal|self_directed|1100.0000|260.0000|1360.0000|1100.0000|260.0000|1360.0000
-2|full-src|USD|taxable_personal|self_directed|1000.0000|250.0000|1250.0000|1000.0000|250.0000|1250.0000
-3|full-src|USD|taxable_personal|self_directed|1000.0000|250.0000|1250.0000|1000.0000|250.0000|1250.0000
0|full-src|USD|taxable_personal|self_directed|1100.0000|260.0000|1360.0000|1100.0000|260.0000|1360.0000`

const goldenPortfoliosHistory = `-1|full-src||<null>|USD|<null>|<null>|taxable_personal|self_directed|1100.0000|260.0000|1360.0000|1100.0000|260.0000|1360.0000
-2|full-src||<null>|USD|<null>|<null>|taxable_personal|self_directed|1000.0000|250.0000|1250.0000|1000.0000|250.0000|1250.0000
-3|full-src||<null>|USD|<null>|<null>|taxable_personal|self_directed|1000.0000|250.0000|1250.0000|1000.0000|250.0000|1250.0000
0|full-src||<null>|USD|<null>|<null>|taxable_personal|self_directed|1100.0000|260.0000|1360.0000|1100.0000|260.0000|1360.0000`

const goldenPositionsHistory = `-1|-1|full-src|F-0000|Brokerage EXAMPLE|<null>|<null>|<null>|X|<null>|<null>|<null>|public_equity|USD|<null>|1100.0000|1100.0000
-2|-3|full-src|F-0000|Brokerage EXAMPLE|<null>|<null>|<null>|X|<null>|<null>|<null>|public_equity|USD|<null>|1000.0000|1000.0000
-3|-3|full-src|F-0000|Brokerage EXAMPLE|<null>|<null>|<null>|X|<null>|<null>|<null>|public_equity|USD|<null>|1000.0000|1000.0000
0|-1|full-src|F-0000|Brokerage EXAMPLE|<null>|<null>|<null>|X|<null>|<null>|<null>|public_equity|USD|<null>|1100.0000|1100.0000`

// historyMacroColumns is the documented shape of every macro that
// resolves the active snapshot — the column list its readers (the
// returns loader, the Metabase serving views, the CLI scans) are
// written against.
var historyMacroColumns = []struct {
	macro string
	cols  []string
}{
	{"report_accounts_history('USD')", []string{
		"as_of_day", "silver_source_id", "account_external_id", "account_kind", "display_name",
		"base_currency", "relationship_id", "nickname", "account_category", "portfolio_external_id",
		"tax_wrapper", "management_style", "positions_value_base", "cash_balance_base",
		"total_value_base", "positions_value_outccy", "cash_balance_outccy", "total_value_outccy"}},
	{"report_accounts_history_multi()", []string{
		"as_of_day", "silver_source_id", "account_external_id", "account_kind", "display_name",
		"base_currency", "relationship_id", "nickname", "account_category", "portfolio_external_id",
		"tax_wrapper", "management_style", "positions_value_base", "cash_balance_base",
		"total_value_base", "positions_value_usd", "cash_balance_usd", "total_value_usd",
		"positions_value_chf", "cash_balance_chf", "total_value_chf",
		"positions_value_eur", "cash_balance_eur", "total_value_eur"}},
	{"report_sources_history('USD')", []string{
		"as_of_day", "silver_source_id", "base_currency", "tax_wrapper", "management_style",
		"positions_value_base", "cash_balance_base", "total_value_base",
		"positions_value_outccy", "cash_balance_outccy", "total_value_outccy"}},
	{"report_sources_history_multi()", []string{
		"as_of_day", "silver_source_id", "base_currency", "tax_wrapper", "management_style",
		"positions_value_base", "cash_balance_base", "total_value_base",
		"positions_value_usd", "cash_balance_usd", "total_value_usd",
		"positions_value_chf", "cash_balance_chf", "total_value_chf",
		"positions_value_eur", "cash_balance_eur", "total_value_eur"}},
	{"report_portfolios_history('USD')", []string{
		"as_of_day", "silver_source_id", "portfolio_external_id", "display_name", "base_currency",
		"relationship_id", "nickname", "tax_wrapper", "management_style",
		"positions_value_base", "cash_balance_base", "total_value_base",
		"positions_value_outccy", "cash_balance_outccy", "total_value_outccy"}},
	{"report_portfolios_history_multi()", []string{
		"as_of_day", "silver_source_id", "portfolio_external_id", "display_name", "base_currency",
		"relationship_id", "nickname", "tax_wrapper", "management_style",
		"positions_value_base", "cash_balance_base", "total_value_base",
		"positions_value_usd", "cash_balance_usd", "total_value_usd",
		"positions_value_chf", "cash_balance_chf", "total_value_chf",
		"positions_value_eur", "cash_balance_eur", "total_value_eur"}},
	{"report_positions_history('USD')", []string{
		"as_of_day", "silver_source_id", "snapshot_at", "account_external_id", "display_name",
		"relationship_id", "nickname", "account_category", "position_key", "instrument_external_id",
		"symbol", "name", "asset_class", "currency", "quantity", "market_value", "value_outccy"}},
	{"report_positions_history_multi()", []string{
		"as_of_day", "silver_source_id", "snapshot_at", "account_external_id", "display_name",
		"relationship_id", "nickname", "account_category", "position_key", "instrument_external_id",
		"symbol", "name", "asset_class", "vehicle", "currency", "quantity", "market_value",
		"value_usd", "value_chf", "value_eur"}},
	{"report_global_history('USD')", []string{
		"as_of_day", "cash_balance_outccy", "positions_value_outccy", "total_value_outccy"}},
}

// TestMigration0051DDLIsRerunnable holds the re-issued history macros
// to the replay bar and confirms each still answers afterwards.
func TestMigration0051DDLIsRerunnable(t *testing.T) {
	db, ctx := openMigrated(t)
	const src = "replay-src"
	seedHistAccount(t, db, ctx, src, "R-0000", "brokerage", "Brokerage EXAMPLE")
	seedHistPosition(t, db, ctx, src, "R-0000", "X", -2, "1000")
	seedHistCash(t, db, ctx, src, "R-0000", -1, "250")

	rerunMigrationDDL(t, db, ctx, "0051_history_per_account_carry.sql")

	for _, m := range historyMacroColumns {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+m.macro).Scan(&n); err != nil {
			t.Errorf("%s after re-run: %v", m.macro, err)
			continue
		}
		if n == 0 {
			t.Errorf("%s after re-run: no rows over a seeded source", m.macro)
		}
	}
}

// TestHistoryMacroShapesUnchanged pins the blast radius: re-issuing the
// active-snapshot macros changes which rows land on a day, never the
// columns a row carries.
func TestHistoryMacroShapesUnchanged(t *testing.T) {
	db, ctx := openMigrated(t)
	for _, m := range historyMacroColumns {
		rows, err := db.QueryContext(ctx, "SELECT * FROM "+m.macro+" LIMIT 0")
		if err != nil {
			t.Errorf("%s: %v", m.macro, err)
			continue
		}
		cols, err := rows.Columns()
		rows.Close()
		if err != nil {
			t.Errorf("%s columns: %v", m.macro, err)
			continue
		}
		if strings.Join(cols, ",") != strings.Join(m.cols, ",") {
			t.Errorf("%s columns =\n  %v\nwant\n  %v", m.macro, cols, m.cols)
		}
	}
}
