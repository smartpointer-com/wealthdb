package loader_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/loader"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"

	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/firstcitizens"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/raiffeisen_at"
)

// A deposit bank states each account's CURRENT balance at every run that
// listed it, and each ledger close at the last second of its day. Gold's
// own endings then decide when an account the bank stopped listing closes
// (wealthdb/docs/DESIGN.md §10.7): its last balance ends at the first
// later snapshot at which every peer observed with it, and still observed
// then, is observed again, once the bank's newest snapshot lies more than
// one and a half of its cadences past that balance. Its cadence is the
// median gap between the days it is observed on, ledger closes included.
//
// These tests load the firstcitizens and raiffeisen_at adapters into gold
// on an invented book of weekly runs:
//
//   - ACTIVE changes at every run, and its ledger closes the day of week 2
//     after that run.
//   - QUIET is listed by every run but never changes, so silver holds its
//     first roster row alone.
//   - GONE never changes either, and the bank stops listing it at week 4.
//     Its cadence is the weekly run.
//   - LEDGER never changes on the roster, closes in the ledger every few
//     days between the runs, and the bank stops listing it at week 4 too.
//     Its cadence is the ledger's.
//
// Week 4 lies a week past week 3, the last run that listed GONE and LEDGER.
// That is past LEDGER's grace but inside GONE's: LEDGER ends at week 4 and
// GONE stands. Once the bank runs a week more, GONE ends at week 4 too.
// QUIET never ends.

// depositBank is one bank's silver: its kind, its test schema, the currency
// its accounts keep, the INSERT that records that the run at ?1 listed the
// account ?2, and the INSERT of the close ?3 of account ?1 on the day at ?2.
type depositBank struct {
	kind, schema, ccy, listed, close string
}

var depositBanks = []depositBank{
	{kind: "firstcitizens", schema: "../silver/firstcitizens/testdata/silver_schema.sql", ccy: "USD",
		listed: `INSERT INTO run_accounts VALUES (?1, ?2)`,
		close:  `INSERT INTO transactions VALUES (?1 || ?2, ?2, ?1, 0, 'CREDIT', '', NULL, ?3, 'history', '{}')`},
	{kind: "raiffeisen_at", schema: "../silver/raiffeisen_at/testdata/silver_schema.sql", ccy: "EUR",
		listed: `INSERT INTO run_accounts VALUES (?1, ?2)`,
		close:  `INSERT INTO daily_balances VALUES (?1, ?2, ?3, ?2)`},
}

// depositDay is the UTC midnight of the bank's day d; its runs fall on days
// 0, 7, 14, ….
func depositDay(d int64) int64 { return silvertest.Day(2026, 1, 5) + d*86400 }

// depositWeek is the instant of the bank's run in week k.
func depositWeek(k int64) int64 { return depositDay(7*k) + 12*3600 }

// run writes the bank's run of week k, listing accts.
func (b depositBank) run(t *testing.T, s *sql.DB, k int64, accts ...string) {
	t.Helper()
	at := depositWeek(k)
	silvertest.Exec(t, s, `INSERT INTO dump_runs VALUES (?, 2, '/run')`, at)
	for _, a := range accts {
		silvertest.Exec(t, s, b.listed, at, a)
	}
}

// row writes a changed roster row of acct at week k.
func (b depositBank) row(t *testing.T, s *sql.DB, k int64, acct string, balance float64) {
	t.Helper()
	silvertest.Exec(t, s, `INSERT INTO accounts VALUES (?, ?, 'Savings', '', '…0000', ?, ?, '{}')`,
		depositWeek(k), acct, b.ccy, balance)
}

// closeOn writes acct's ledger close on day d.
func (b depositBank) closeOn(t *testing.T, s *sql.DB, d int64, acct string, balance float64) {
	t.Helper()
	silvertest.Exec(t, s, b.close, acct, depositDay(d), balance)
}

// balanceAt is an account's total in the point-in-time report at an instant.
func balanceAt(t *testing.T, g *sql.DB, src, acct, ccy string, at int64) float64 {
	t.Helper()
	var v float64
	if err := g.QueryRow(`
        SELECT COALESCE(CAST(total_value_outccy AS DOUBLE), 0) FROM report_accounts(?, ?)
         WHERE silver_source_id = ? AND account_external_id = ?`, at, ccy, src, acct).Scan(&v); err != nil {
		t.Fatalf("report_accounts of %s at %d: %v", acct, at, err)
	}
	return v
}

// historyOn is an account's total in the daily history on the day at
// midnight day.
func historyOn(t *testing.T, g *sql.DB, src, acct, ccy string, day int64) float64 {
	t.Helper()
	var v float64
	if err := g.QueryRow(`
        SELECT CAST(total_value_outccy AS DOUBLE) FROM report_accounts_history(?)
         WHERE silver_source_id = ? AND account_external_id = ? AND as_of_day = ?`,
		ccy, src, acct, day).Scan(&v); err != nil {
		t.Fatalf("history of %s on %d: %v", acct, day, err)
	}
	return v
}

// cashEnd is an account's ending snapshot by cash_key_ends, absent where it
// has none.
func cashEnd(t *testing.T, g *sql.DB, src, acct string) (int64, bool) {
	t.Helper()
	var end int64
	switch err := g.QueryRow(`SELECT end_snap FROM cash_key_ends() WHERE src = ? AND acct = ?`,
		src, acct).Scan(&end); err {
	case nil:
		return end, true
	case sql.ErrNoRows:
		return 0, false
	default:
		t.Fatalf("cash_key_ends of %s: %v", acct, err)
		return 0, false
	}
}

func TestADepositAccountTheBankStopsListingEnds(t *testing.T) {
	for _, b := range depositBanks {
		t.Run(b.kind, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			silverPath, s := newSilver(t, b.schema)
			g, err := gold.OpenFresh(filepath.Join(t.TempDir(), "gold.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer g.Close()
			ld := loader.New(g)
			spec := loader.SourceSpec{ID: "bank", Kind: b.kind, Path: silverPath}
			load := func() {
				t.Helper()
				if _, err := ld.Load(ctx, spec); err != nil {
					t.Fatalf("Load: %v", err)
				}
			}

			b.row(t, s, 0, "QUIET", 75)
			b.row(t, s, 0, "GONE", 250)
			b.row(t, s, 0, "LEDGER", 40)
			for k := int64(0); k <= 4; k++ {
				b.row(t, s, k, "ACTIVE", float64(1000+k))
				if k <= 3 {
					b.run(t, s, k, "ACTIVE", "QUIET", "GONE", "LEDGER")
				} else {
					b.run(t, s, k, "ACTIVE", "QUIET")
				}
			}
			b.closeOn(t, s, 14, "ACTIVE", 1500)
			for _, d := range []int64{1, 3, 5, 8, 10, 12, 15, 17, 19} {
				b.closeOn(t, s, d, "LEDGER", 40)
			}
			load()

			// The ledger owns the day's end: on the day of week 2 the run's
			// balance holds until the close, and the day reads the close.
			if got := balanceAt(t, g, "bank", "ACTIVE", b.ccy, depositWeek(2)+3600); got != 1002 {
				t.Errorf("ACTIVE after the week 2 run = %v, want the run's 1002", got)
			}
			if got := balanceAt(t, g, "bank", "ACTIVE", b.ccy, depositDay(14)+86399); got != 1500 {
				t.Errorf("ACTIVE at the end of the week 2 day = %v, want the close 1500", got)
			}
			if got := historyOn(t, g, "bank", "ACTIVE", b.ccy, depositDay(14)); got != 1500 {
				t.Errorf("ACTIVE's history on the week 2 day = %v, want the close 1500", got)
			}

			// Week 4 left GONE and LEDGER out. It lies inside GONE's grace,
			// a week and a half, but past LEDGER's, three days.
			var last int64
			if err := g.QueryRow(`SELECT MAX(snapshot_at) FROM cash_balances
			     WHERE account_external_id = 'GONE'`).Scan(&last); err != nil {
				t.Fatal(err)
			}
			if last != depositWeek(3) {
				t.Errorf("GONE's last balance at %d, want its last listing run %d", last, depositWeek(3))
			}
			if end, ok := cashEnd(t, g, "bank", "GONE"); ok {
				t.Errorf("GONE ends at %d inside its grace, want no ending", end)
			}
			if end, ok := cashEnd(t, g, "bank", "LEDGER"); !ok || end != depositWeek(4) {
				t.Errorf("LEDGER ends at %d (%v), want the run that left it out %d", end, ok, depositWeek(4))
			}
			for acct, want := range map[string]float64{"ACTIVE": 1004, "QUIET": 75, "GONE": 250, "LEDGER": 0} {
				if got := balanceAt(t, g, "bank", acct, b.ccy, depositWeek(4)+3600); got != want {
					t.Errorf("%s after week 4 = %v, want %v", acct, got, want)
				}
			}

			// Week 5 lies two weeks past GONE's last listing run, past its
			// grace: it ends at week 4.
			b.row(t, s, 5, "ACTIVE", 1005)
			b.run(t, s, 5, "ACTIVE", "QUIET")
			load()
			if end, ok := cashEnd(t, g, "bank", "GONE"); !ok || end != depositWeek(4) {
				t.Errorf("GONE ends at %d (%v), want the run that left it out %d", end, ok, depositWeek(4))
			}
			if end, ok := cashEnd(t, g, "bank", "QUIET"); ok {
				t.Errorf("QUIET ends at %d, want no ending", end)
			}
			for _, c := range []struct {
				acct string
				at   int64
				want float64
			}{
				{"GONE", depositWeek(4) - 1, 250}, {"GONE", depositWeek(4), 0}, {"GONE", depositWeek(5), 0},
				{"LEDGER", depositWeek(4) - 1, 40}, {"LEDGER", depositWeek(5), 0},
				{"QUIET", depositWeek(5), 75}, {"ACTIVE", depositWeek(5), 1005},
			} {
				if got := balanceAt(t, g, "bank", c.acct, b.ccy, c.at); got != c.want {
					t.Errorf("%s at %d = %v, want %v", c.acct, c.at, got, c.want)
				}
			}
		})
	}
}
