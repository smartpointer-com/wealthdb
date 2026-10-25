package firstcitizens

import (
	"context"
	"database/sql"
	_ "embed"
	"reflect"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"

	_ "modernc.org/sqlite"
)

//go:embed testdata/silver_schema.sql
var silverSchemaSQL string

// loadUnix is a full-timestamp load clock (a bronze dump slug time).
var loadUnix = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).Unix()

func newFixture(t *testing.T) (string, *sql.DB) {
	t.Helper()
	return silvertest.NewDB(t, silverSchemaSQL)
}

// seed builds one checking account with four transactions, each carrying a
// running balance (every history row has one), plus statement documents (which
// the projection ignores — the cash series comes from the running balances).
func seed(t *testing.T, db *sql.DB) {
	t.Helper()
	exec := func(q string, args ...any) {
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	exec(`INSERT INTO dump_runs VALUES (?,1,'/run')`, loadUnix)
	// Roster: current balance 1500.00 (deliberately later than the last
	// transaction, to show the CURRENT balance is the live roster value).
	exec(`INSERT INTO accounts VALUES (?,?,?,?,?,?,?,?)`,
		loadUnix, "acct-1", "Checking", "Example Checking", "…9999", "USD", 1500.00, "{}")
	exec(`INSERT INTO run_accounts VALUES (?,?)`, loadUnix, "acct-1")

	// The silver `kind` is only DEBIT/CREDIT; the semantic type (interest, fee)
	// is classified from the description.
	tx := func(fitid string, d int64, amt float64, kind, desc string, bal float64) {
		exec(`INSERT INTO transactions VALUES (?,?,?,?,?,?,?,?,?,?)`,
			fitid, d, "acct-1", amt, kind, desc, nil, bal, "history", "{}")
	}
	tx("t1", silvertest.Day(2026, 1, 10), 1000.00, "CREDIT", "PAYROLL DEPOSIT", 1000.00)
	tx("t2", silvertest.Day(2026, 1, 20), -300.00, "DEBIT", "COFFEE BAR", 700.00)
	tx("t3", silvertest.Day(2026, 2, 5), -50.00, "DEBIT", "MONTHLY SERVICE CHARGE", 650.00)
	tx("t4", silvertest.Day(2026, 2, 15), 2.50, "CREDIT", "INTEREST PAYMENT", 652.50)

	stmt := func(sha string, d int64) {
		exec(`INSERT INTO documents VALUES (?,?,?,?,'statement','pdf',?,100,'{}')`,
			sha, loadUnix, "acct-1", d, sha+".pdf")
	}
	stmt("s1", silvertest.Day(2026, 1, 31))
	stmt("s2", silvertest.Day(2026, 2, 28))
}

func openConn(t *testing.T, path string) silver.Connection {
	t.Helper()
	return silvertest.Open(t, &Adapter{}, path)
}

func TestKind(t *testing.T) {
	if (&Adapter{}).Kind() != "firstcitizens" {
		t.Fatalf("Kind = %q", (&Adapter{}).Kind())
	}
}

// TestStatusAndChangeWindow pins spanExtrema: the snapshot range and the
// re-emit window run from the first transaction (the first cash mark) to the
// load. The load clock itself is silver's (TestLoadClock).
func TestStatusAndChangeWindow(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	conn := openConn(t, path)

	st, err := conn.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	first := silvertest.Day(2026, 1, 10)
	if st.OldestSnapshotAt != first || st.LatestSnapshotAt != loadUnix {
		t.Errorf("snapshot range = [%d,%d], want [%d,%d]",
			st.OldestSnapshotAt, st.LatestSnapshotAt, first, loadUnix)
	}
	if w := silvertest.FullWindow(t, conn); w.Start != first || w.End != loadUnix {
		t.Errorf("window = [%d,%d], want [%d,%d]", w.Start, w.End, first, loadUnix)
	}
}

// A closing mark is stamped at its day's last second, so the span and the
// re-emit window reach that stamp when the ledger's last day is later than
// the last run.
func TestSpanReachesTheLastClosingStamp(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	day := silvertest.Day(2026, 3, 10)
	silvertest.Exec(t, db, `INSERT INTO transactions VALUES (?,?,?,?,?,?,?,?,?,?)`,
		"t5", day, "acct-1", -10.00, "DEBIT", "COFFEE BAR", nil, 642.50, "history", "{}")
	conn := openConn(t, path)

	st, err := conn.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if want := day + silver.DayEndOffset; st.LatestSnapshotAt != want {
		t.Errorf("LatestSnapshotAt = %d, want the day's last second %d", st.LatestSnapshotAt, want)
	}
	if w := silvertest.FullWindow(t, conn); w.End != day+silver.DayEndOffset {
		t.Errorf("window end = %d, want %d", w.End, day+silver.DayEndOffset)
	}
}

// The window bounds the closing stamps, as it bounds every other record:
// a day whose last second lies past the window's end is not emitted.
func TestClosingMarksStayInsideTheWindow(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	conn := openConn(t, path)
	w := silvertest.FullWindow(t, conn)
	w.End = silvertest.Day(2026, 1, 20) + 3600
	var closes []int64
	for _, cb := range silvertest.Merged(t, conn, w).CashBalances {
		if cb.BalanceKind == canonical.BalanceKindClosing {
			closes = append(closes, cb.SnapshotAt)
		}
	}
	if want := []int64{silvertest.Day(2026, 1, 10) + silver.DayEndOffset}; !reflect.DeepEqual(closes, want) {
		t.Errorf("closing stamps = %v, want %v", closes, want)
	}
}

func TestSnapshots(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	conn := openConn(t, path)
	ctx := context.Background()
	w := silvertest.FullWindow(t, conn)

	stream, err := conn.Snapshots(ctx, w)
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	defer stream.Close()
	batch, _, err := stream.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}

	// One cash account.
	if len(batch.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(batch.Accounts))
	}
	a := batch.Accounts[0]
	if a.AccountKind != canonical.AccountKindCash {
		t.Errorf("AccountKind = %q, want cash", a.AccountKind)
	}
	if a.DisplayName == nil || *a.DisplayName != "Example Checking" {
		t.Errorf("DisplayName = %v", a.DisplayName)
	}
	if a.BaseCurrency == nil || *a.BaseCurrency != "USD" {
		t.Errorf("BaseCurrency = %v", a.BaseCurrency)
	}
	if len(batch.Positions) != 0 || len(batch.Instruments) != 0 {
		t.Error("deposit account must not emit positions/instruments")
	}

	// Cash balances: one CURRENT (roster) + one CLOSING per transaction day,
	// valued at that day's running balance. Statements contribute nothing.
	want := map[string]struct {
		kind canonical.BalanceKind
		amt  string
	}{
		unixKey(loadUnix): {canonical.BalanceKindCurrent, "1500"},
		unixKey(silvertest.Day(2026, 1, 10) + 86399): {canonical.BalanceKindClosing, "1000"},
		unixKey(silvertest.Day(2026, 1, 20) + 86399): {canonical.BalanceKindClosing, "700"},
		unixKey(silvertest.Day(2026, 2, 5) + 86399):  {canonical.BalanceKindClosing, "650"},
		unixKey(silvertest.Day(2026, 2, 15) + 86399): {canonical.BalanceKindClosing, "652.5"},
	}
	if len(batch.CashBalances) != len(want) {
		t.Fatalf("cash balances = %d, want %d", len(batch.CashBalances), len(want))
	}
	for _, cb := range batch.CashBalances {
		exp, ok := want[unixKey(cb.SnapshotAt)]
		if !ok {
			t.Errorf("unexpected cash balance at %d", cb.SnapshotAt)
			continue
		}
		if cb.BalanceKind != exp.kind {
			t.Errorf("balance @%d kind = %q, want %q", cb.SnapshotAt, cb.BalanceKind, exp.kind)
		}
		if cb.Amount.String() != exp.amt {
			t.Errorf("balance @%d amount = %s, want %s", cb.SnapshotAt, cb.Amount.String(), exp.amt)
		}
		if cb.Currency != "USD" {
			t.Errorf("balance @%d currency = %q", cb.SnapshotAt, cb.Currency)
		}
	}
}

func TestTransactions(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	conn := openConn(t, path)
	ctx := context.Background()
	w := silvertest.FullWindow(t, conn)

	stream, err := conn.Transactions(ctx, w)
	if err != nil {
		t.Fatalf("Transactions: %v", err)
	}
	defer stream.Close()
	batch, _, err := stream.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(batch.Transactions) != 4 {
		t.Fatalf("transactions = %d, want 4", len(batch.Transactions))
	}

	byID := map[string]canonical.TransactionChange{}
	var net canonical.Decimal
	for _, tx := range batch.Transactions {
		byID[tx.TransactionExternalID] = tx
		if tx.NetAmount != nil {
			net = net.Add(*tx.NetAmount)
		}
		if tx.Currency != "USD" {
			t.Errorf("%s currency = %q", tx.TransactionExternalID, tx.Currency)
		}
	}
	// Kinds: credit→deposit, debit→withdrawal, service charge→fee (from the
	// description), interest→interest (from the description).
	cases := map[string]struct {
		kind canonical.TxKind
		amt  string
	}{
		"t1": {canonical.TxKindDeposit, "1000"},
		"t2": {canonical.TxKindWithdrawal, "-300"},
		"t3": {canonical.TxKindFee, "-50"},
		"t4": {canonical.TxKindInterest, "2.5"},
	}
	for id, exp := range cases {
		tx, ok := byID[id]
		if !ok {
			t.Errorf("missing tx %s", id)
			continue
		}
		if tx.Kind != exp.kind {
			t.Errorf("%s kind = %q, want %q", id, tx.Kind, exp.kind)
		}
		if tx.NetAmount == nil || tx.NetAmount.String() != exp.amt {
			t.Errorf("%s net = %v, want %s", id, tx.NetAmount, exp.amt)
		}
	}
	// Net flow reconciles: 1000 - 300 - 50 + 2.50 = 652.50 (the last running
	// balance, from a zero start — the fixture's flows net to it).
	if net.String() != "652.5" {
		t.Errorf("net flow = %s, want 652.5", net.String())
	}
}

func unixKey(u int64) string { return time.Unix(u, 0).UTC().Format(time.RFC3339) }

// The bank's own filing reaches gold as the provider category, as found;
// its catch-all says nothing and is dropped.
func TestTransactionsCarryTheBanksFiling(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	for id, payload := range map[string]string{
		"t2": `{"pfmData":{"classificationDesc":"Restaurants"}}`,
		"t3": `{"pfmData":{"classificationDesc":"Uncategorized"}}`,
	} {
		if _, err := db.Exec(`UPDATE transactions SET payload = ? WHERE fitid = ?`, payload, id); err != nil {
			t.Fatal(err)
		}
	}
	conn := openConn(t, path)
	ctx := context.Background()
	stream, err := conn.Transactions(ctx, silvertest.FullWindow(t, conn))
	if err != nil {
		t.Fatalf("Transactions: %v", err)
	}
	defer stream.Close()
	batch, _, err := stream.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	for _, tx := range batch.Transactions {
		var want string
		if tx.TransactionExternalID == "t2" {
			want = "Restaurants"
		}
		got := ""
		if tx.ProviderCategory != nil {
			got = *tx.ProviderCategory
		}
		if got != want {
			t.Errorf("%s provider category = %q, want %q", tx.TransactionExternalID, got, want)
		}
	}
}

// TestCurrentBalanceAtEachRunThatListedTheAccount pins a CURRENT balance at
// each run whose roster listed the account, valued at its roster row as of
// that run. Silver content-dedups an unchanged roster row, so the quiet
// account stands at every run that still lists it, the latest included,
// while its account record keeps its own row. The account the latest runs
// left out stops at its last listing run, so gold can end it
// (wealthdb/docs/DESIGN.md §10.7) rather than reading it as open at the
// source's latest run. A run that changed no roster row still bounds the
// window.
func TestCurrentBalanceAtEachRunThatListedTheAccount(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	r0, r1, r2, r3 := silvertest.SeedRuns(t, db, loadUnix, "acct-1", "acct-2", "acct-3")
	conn := openConn(t, path)
	w := silvertest.FullWindow(t, conn)
	if w.End != r3 {
		t.Errorf("window end = %d, want the latest run %d", w.End, r3)
	}
	batch := silvertest.Merged(t, conn, w)
	want := map[string]map[int64]string{
		"acct-1": {r2: "1500 USD", r3: "1500 USD"},
		"acct-2": {r0: "250 USD", r1: "250 USD", r2: "250 USD", r3: "250 USD"},
		"acct-3": {r0: "75 USD", r1: "80 USD"},
	}
	if got := silvertest.CurrentsByInstant(batch); !reflect.DeepEqual(got, want) {
		t.Errorf("CURRENT balances =\n %v\nwant\n %v", got, want)
	}
	for _, a := range batch.Accounts {
		if a.AccountExternalID == "acct-2" && a.LastSeenAt != r0 {
			t.Errorf("quiet account LastSeenAt = %d, want its own latest row %d", a.LastSeenAt, r0)
		}
	}
}

// TestSnapshotsNeedRunAccounts pins a silver loaded before run_accounts
// (silver migration 0002): its runs say nothing about a quiet account, so
// the snapshots fail and name the load that adds the table.
func TestSnapshotsNeedRunAccounts(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	silvertest.Exec(t, db, `DROP TABLE run_accounts`)
	conn := openConn(t, path)
	_, err := conn.Snapshots(context.Background(), silvertest.FullWindow(t, conn))
	if want := "firstcitizens silver has no run_accounts; run `firstcitizens load` first"; err == nil || err.Error() != want {
		t.Errorf("Snapshots = %v, want %q", err, want)
	}
}
