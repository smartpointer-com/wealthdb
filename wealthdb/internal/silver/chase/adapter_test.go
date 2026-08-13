package chase

import (
	"context"
	"database/sql"
	_ "embed"
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"

	_ "modernc.org/sqlite"
)

//go:embed testdata/silver_schema.sql
var silverSchemaSQL string

// day is a calendar date at UTC midnight (unix seconds) — matching how the
// collector stores posted_at / doc_date.
func day(y int, m time.Month, d int) int64 {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix()
}

// loadUnix is a full-timestamp load clock (a bronze dump slug time).
var loadUnix = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).Unix()

func newFixture(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := t.TempDir() + "/chase.db"
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(silverSchemaSQL); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return path, db
}

// seed builds one checking account with four transactions carrying a running
// balance, plus statements (which the projection ignores — the cash series
// comes from the transaction running balances, not the statement dates).
func seed(t *testing.T, db *sql.DB) {
	t.Helper()
	exec := func(q string, args ...any) {
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	exec(`INSERT INTO dump_runs VALUES (?,1,'/run')`, loadUnix)
	// Roster: current balance 1500.00 (deliberately later than the last
	// statement, to show the CURRENT balance is the live roster value).
	exec(`INSERT INTO accounts VALUES (?,?,?,?,?,?,?,?)`,
		loadUnix, "acct-1", "CHK", "Example Checking", "…9999", "USD", 1500.00, "{}")

	tx := func(fitid string, d int64, amt float64, kind string, bal any) {
		exec(`INSERT INTO transactions VALUES (?,?,?,?,?,?,?,?,?,?)`,
			fitid, d, "acct-1", amt, kind, "desc "+fitid, nil, bal, "csv", "{}")
	}
	tx("t1", day(2026, 1, 10), 1000.00, "CREDIT", 1000.00)
	tx("t2", day(2026, 1, 20), -300.00, "DEBIT", 700.00)
	tx("t3", day(2026, 2, 5), -50.00, "FEE", 650.00)
	tx("t4", day(2026, 2, 15), 2.50, "INT", 652.50)

	stmt := func(sha string, d int64) {
		exec(`INSERT INTO documents VALUES (?,?,?,?,'statement','pdf',?,100,'{}')`,
			sha, loadUnix, "acct-1", d, sha+".pdf")
	}
	stmt("s0", day(2025, 12, 31)) // before any transaction → skipped
	stmt("s1", day(2026, 1, 31))  // last tx on/before = t2 → 700.00
	stmt("s2", day(2026, 2, 28))  // last tx on/before = t4 → 652.50
}

func openConn(t *testing.T, path string) silver.Connection {
	t.Helper()
	conn, err := (&Adapter{}).Open(context.Background(), silver.OpenSpec{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func fullWindow(t *testing.T, conn silver.Connection) canonical.Window {
	t.Helper()
	w, err := conn.ChangeWindow(context.Background(), 0)
	if err != nil {
		t.Fatalf("ChangeWindow: %v", err)
	}
	if !w.HasChanges {
		t.Fatal("ChangeWindow: expected changes")
	}
	return w
}

func TestKind(t *testing.T) {
	if (&Adapter{}).Kind() != "chase" {
		t.Fatalf("Kind = %q", (&Adapter{}).Kind())
	}
}

func TestStatusAndChangeWindow(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	conn := openConn(t, path)
	ctx := context.Background()

	st, err := conn.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.LatestChangeNumber != loadUnix {
		t.Errorf("LatestChangeNumber = %d, want %d", st.LatestChangeNumber, loadUnix)
	}
	// Snapshot extrema span the first transaction (first cash mark) to the load.
	if st.OldestSnapshotAt != day(2026, 1, 10) {
		t.Errorf("OldestSnapshotAt = %d, want %d", st.OldestSnapshotAt, day(2026, 1, 10))
	}
	if st.LatestSnapshotAt != loadUnix {
		t.Errorf("LatestSnapshotAt = %d, want %d", st.LatestSnapshotAt, loadUnix)
	}
	if st.OldestTransactionAt != day(2026, 1, 10) || st.LatestTransactionAt != day(2026, 2, 15) {
		t.Errorf("tx range = [%d,%d]", st.OldestTransactionAt, st.LatestTransactionAt)
	}

	w := fullWindow(t, conn)
	if w.NewChangeNumber != loadUnix {
		t.Errorf("NewChangeNumber = %d, want %d", w.NewChangeNumber, loadUnix)
	}
	if w.Start != day(2026, 1, 10) || w.End != loadUnix {
		t.Errorf("window = [%d,%d]", w.Start, w.End)
	}

	// An idle reload (watermark already at the latest load) yields nothing.
	idle, err := conn.ChangeWindow(ctx, loadUnix)
	if err != nil {
		t.Fatalf("ChangeWindow idle: %v", err)
	}
	if idle.HasChanges {
		t.Error("idle reload reported changes")
	}
}

func TestSnapshots(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	conn := openConn(t, path)
	ctx := context.Background()
	w := fullWindow(t, conn)

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
		unixKey(loadUnix):         {canonical.BalanceKindCurrent, "1500"},
		unixKey(day(2026, 1, 10)): {canonical.BalanceKindClosing, "1000"},
		unixKey(day(2026, 1, 20)): {canonical.BalanceKindClosing, "700"},
		unixKey(day(2026, 2, 5)):  {canonical.BalanceKindClosing, "650"},
		unixKey(day(2026, 2, 15)): {canonical.BalanceKindClosing, "652.5"},
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
	w := fullWindow(t, conn)

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
	// Kinds: credit→deposit, debit→withdrawal, FEE→fee, INT→interest.
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
