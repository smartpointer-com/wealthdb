package bank_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"

	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/firstcitizens"
	_ "github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/raiffeisen_at"
)

// listingBank is a bank source that states a Listing: its kind, its test
// schema, the currency its accounts keep, the schema version before the
// one that adds the listing table, and the INSERT of the close ?3 of
// account ?1 on the day at ?2.
type listingBank struct {
	kind, schema, ccy string
	beforeListing     int
	close             string
}

var listingBanks = []listingBank{
	{kind: "firstcitizens", schema: "../firstcitizens/testdata/silver_schema.sql", ccy: "USD", beforeListing: 1,
		close: `INSERT INTO transactions VALUES (?1 || ?2, ?2, ?1, 0, 'CREDIT', '', NULL, ?3, 'history', '{}')`},
	{kind: "raiffeisen_at", schema: "../raiffeisen_at/testdata/silver_schema.sql", ccy: "EUR", beforeListing: 2,
		close: `INSERT INTO daily_balances VALUES (?1, ?2, ?3, ?2)`},
}

// loadUnix is a full-timestamp load clock (a bronze dump slug time).
var loadUnix = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).Unix()

// seed builds the bank's silver with one account listed by the run at
// loadUnix, its roster balance 1500, and two closes. It returns the
// silver's path, a handle on it, and the source.
func (b listingBank) seed(t *testing.T) (string, *sql.DB, silver.Source) {
	t.Helper()
	ddl, err := os.ReadFile(b.schema)
	if err != nil {
		t.Fatal(err)
	}
	src, err := silver.Get(b.kind)
	if err != nil {
		t.Fatal(err)
	}
	path, db := silvertest.NewDB(t, string(ddl))
	silvertest.Exec(t, db, `INSERT INTO dump_runs VALUES (?,1,'/run')`, loadUnix)
	silvertest.Exec(t, db, `INSERT INTO accounts VALUES (?,?,?,?,?,?,?,?)`,
		loadUnix, "acct-1", "Checking", "", "…0001", b.ccy, 1500.00, "{}")
	silvertest.Exec(t, db, `INSERT INTO run_accounts VALUES (?,?)`, loadUnix, "acct-1")
	silvertest.Exec(t, db, b.close, "acct-1", silvertest.Day(2026, 1, 10), 1000.00)
	silvertest.Exec(t, db, b.close, "acct-1", silvertest.Day(2026, 1, 20), 700.00)
	return path, db, src
}

func open(t *testing.T, src silver.Source, path string) silver.Connection {
	t.Helper()
	conn, err := src.Open(context.Background(), silver.OpenSpec{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// TestCurrentBalanceAtEachRunThatListedTheAccount pins a CURRENT balance at
// each run whose roster listed the account (Source.Listing), valued at its
// roster row as of that run. Silver content-dedups an unchanged roster row,
// so the quiet account stands at every run that still lists it, the latest
// included, while its account record keeps its own row. The account the
// latest runs left out stops at its last listing run, so gold can end it
// (wealthdb/docs/DESIGN.md §10.7) rather than reading it as open at the
// source's latest run. A run that changed no roster row still bounds the
// window.
func TestCurrentBalanceAtEachRunThatListedTheAccount(t *testing.T) {
	for _, b := range listingBanks {
		t.Run(b.kind, func(t *testing.T) {
			path, db, src := b.seed(t)
			r0, r1, r2, r3 := silvertest.SeedRuns(t, db, loadUnix, "acct-1", "acct-2", "acct-3")
			conn := open(t, src, path)
			w := silvertest.WindowSince0(t, conn)
			if !w.Full {
				t.Errorf("window = %+v, want the whole history, the latest run %d in it", w, r3)
			}
			batch := silvertest.Merged(t, conn, w)
			want := map[string]map[int64]string{
				"acct-1": {r2: "1500 " + b.ccy, r3: "1500 " + b.ccy},
				"acct-2": {r0: "250 " + b.ccy, r1: "250 " + b.ccy, r2: "250 " + b.ccy, r3: "250 " + b.ccy},
				"acct-3": {r0: "75 " + b.ccy, r1: "80 " + b.ccy},
			}
			if got := silvertest.CurrentsByInstant(batch); !reflect.DeepEqual(got, want) {
				t.Errorf("CURRENT balances =\n %v\nwant\n %v", got, want)
			}
			for _, a := range batch.Accounts {
				if a.AccountExternalID == "acct-2" && a.LastSeenAt != r0 {
					t.Errorf("quiet account LastSeenAt = %d, want its own latest row %d", a.LastSeenAt, r0)
				}
			}
		})
	}
}

// TestSnapshotsNeedTheListing pins a silver loaded before the listing
// table: it is read as far as it goes. Its runs say nothing about a quiet
// account, so no CURRENT balance is stamped, the closing balances still
// are, and the load advises the collector's load.
func TestSnapshotsNeedTheListing(t *testing.T) {
	for _, b := range listingBanks {
		t.Run(b.kind, func(t *testing.T) {
			path, db, src := b.seed(t)
			silvertest.Exec(t, db, `DROP TABLE run_accounts;
        INSERT INTO schema_meta (silver_schema_version, applied_at) VALUES (?, 0)`, b.beforeListing)
			conn := open(t, src, path)
			sb := silvertest.Merged(t, conn, silvertest.WindowSince0(t, conn))
			closings := 0
			for _, cb := range sb.CashBalances {
				switch cb.BalanceKind {
				case canonical.BalanceKindCurrent:
					t.Errorf("CURRENT balance %+v from a silver without run_accounts", cb)
				case canonical.BalanceKindClosing:
					closings++
				}
			}
			if closings == 0 {
				t.Error("no closing balance: the silver should still read as far as it goes")
			}
			notes := conn.(silver.SchemaAdviser).SchemaAdvice()
			if len(notes) != 1 || !slices.Contains(notes[0].Missing, "run_accounts") ||
				!strings.HasSuffix(notes[0].String(), " load empty until its collector's load applies the migrations") ||
				notes[0].Silver != filepath.Base(path) {
				t.Errorf("advice = %v, want one note naming run_accounts and the load", notes)
			}
		})
	}
}

// TestClosingMarksStayInsideTheWindow pins a day-end series under a window
// that is not full: the window bounds the closing stamps, as it bounds
// every other record, so a day whose last second lies past the window's
// end is not emitted.
func TestClosingMarksStayInsideTheWindow(t *testing.T) {
	for _, b := range listingBanks {
		t.Run(b.kind, func(t *testing.T) {
			path, _, src := b.seed(t)
			conn := open(t, src, path)
			w := canonical.Window{HasChanges: true, Start: 0, End: silvertest.Day(2026, 1, 20) + 3600}
			var closes []int64
			for _, cb := range silvertest.Merged(t, conn, w).CashBalances {
				if cb.BalanceKind == canonical.BalanceKindClosing {
					closes = append(closes, cb.SnapshotAt)
				}
			}
			if want := []int64{silvertest.Day(2026, 1, 10) + silver.DayEndOffset}; !reflect.DeepEqual(closes, want) {
				t.Errorf("closing stamps = %v, want %v", closes, want)
			}
		})
	}
}
