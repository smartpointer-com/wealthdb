package raiffeisenat

import (
	"context"
	"database/sql"
	_ "embed"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"

	_ "modernc.org/sqlite"
)

//go:embed testdata/silver_schema.sql
var silverSchemaSQL string

// Synthetic IBAN: placeholder-letter body with a numeric tail (never real).
const iban = "ATkkBBBBBKKKKKKK1234"

// loadUnix is a full-timestamp load clock (a bronze dump slug time).
var loadUnix = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).Unix()

func newFixture(t *testing.T) (string, *sql.DB) {
	t.Helper()
	return silvertest.NewDB(t, silverSchemaSQL)
}

// seed builds one checking account with four transactions and a daily-balance
// series (which — unlike the US siblings — is the source of the closing cash
// marks, independent of the transactions). One balance day (2025-12-31)
// predates the first transaction, showing the series reaches back further.
func seed(t *testing.T, db *sql.DB) {
	t.Helper()
	exec := func(q string, args ...any) {
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	exec(`INSERT INTO dump_runs VALUES (?,1,'/run')`, loadUnix)
	// Roster: current balance 1500.00 (later than the last transaction, to show
	// the CURRENT balance is the live roster value). Nickname empty → the
	// display name comes from the account type + mask.
	exec(`INSERT INTO accounts VALUES (?,?,?,?,?,?,?,?)`,
		loadUnix, iban, "Testkonto", "", "…1234", "EUR", 1500.00, "{}")

	// transactions: 12 columns (no per-row balance). Kind is classified from
	// the category / description, not the coarse silver `kind`.
	tx := func(id string, d int64, amt float64, category, desc string) {
		exec(`INSERT INTO transactions VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, iban, d, d, amt, "EUR", "", category, desc, "ACME GmbH", "history", "{}")
	}
	tx("t1", silvertest.Day(2026, 1, 10), 1000.00, "income_other", "SALARY")
	tx("t2", silvertest.Day(2026, 1, 20), -300.00, "tv_phone_internet", "COFFEE")
	tx("t3", silvertest.Day(2026, 2, 5), -50.00, "fees_bank", "KONTOENTGELT")
	tx("t4", silvertest.Day(2026, 2, 15), 2.50, "interest_income", "ZINSEN")

	// daily_balances: the kontostaende series, keyed (account, day).
	bal := func(d int64, amt float64) {
		exec(`INSERT INTO daily_balances VALUES (?,?,?,?)`, iban, d, amt, loadUnix)
	}
	bal(silvertest.Day(2025, 12, 31), 900.00) // predates the first transaction
	bal(silvertest.Day(2026, 1, 10), 1000.00)
	bal(silvertest.Day(2026, 1, 20), 700.00)
	bal(silvertest.Day(2026, 2, 5), 650.00)
	bal(silvertest.Day(2026, 2, 15), 652.50)

	// A statement document (ignored by the projection).
	exec(`INSERT INTO documents VALUES ('s1',?,?,?,'statement','pdf','s1.pdf',100,'{}')`,
		loadUnix, iban, silvertest.Day(2026, 1, 31))
}

func openConn(t *testing.T, path string) silver.Connection {
	t.Helper()
	return silvertest.Open(t, &Adapter{}, path)
}

func TestKind(t *testing.T) {
	if (&Adapter{}).Kind() != "raiffeisen_at" {
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
	// The span reaches the earliest daily balance (before the first transaction).
	if st.OldestSnapshotAt != silvertest.Day(2025, 12, 31) {
		t.Errorf("OldestSnapshotAt = %d, want %d", st.OldestSnapshotAt, silvertest.Day(2025, 12, 31))
	}
	if st.LatestSnapshotAt != loadUnix {
		t.Errorf("LatestSnapshotAt = %d, want %d", st.LatestSnapshotAt, loadUnix)
	}
	if st.OldestTransactionAt != silvertest.Day(2026, 1, 10) || st.LatestTransactionAt != silvertest.Day(2026, 2, 15) {
		t.Errorf("tx range = [%d,%d]", st.OldestTransactionAt, st.LatestTransactionAt)
	}

	w := silvertest.FullWindow(t, conn)
	if w.NewChangeNumber != loadUnix {
		t.Errorf("NewChangeNumber = %d, want %d", w.NewChangeNumber, loadUnix)
	}
	if w.Start != silvertest.Day(2025, 12, 31) || w.End != loadUnix {
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

	// One cash account; display name from the account type + mask.
	if len(batch.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(batch.Accounts))
	}
	a := batch.Accounts[0]
	if a.AccountKind != canonical.AccountKindCash {
		t.Errorf("AccountKind = %q, want cash", a.AccountKind)
	}
	if a.DisplayName == nil || *a.DisplayName != "Testkonto …1234" {
		t.Errorf("DisplayName = %v", a.DisplayName)
	}
	if a.BaseCurrency == nil || *a.BaseCurrency != "EUR" {
		t.Errorf("BaseCurrency = %v", a.BaseCurrency)
	}
	if len(batch.Positions) != 0 || len(batch.Instruments) != 0 {
		t.Error("deposit account must not emit positions/instruments")
	}

	// Cash balances: one CURRENT (roster) + one CLOSING per daily-balance day
	// (including the pre-ledger day). Statements contribute nothing.
	want := map[string]struct {
		kind canonical.BalanceKind
		amt  string
	}{
		unixKey(loadUnix):                     {canonical.BalanceKindCurrent, "1500"},
		unixKey(silvertest.Day(2025, 12, 31)): {canonical.BalanceKindClosing, "900"},
		unixKey(silvertest.Day(2026, 1, 10)):  {canonical.BalanceKindClosing, "1000"},
		unixKey(silvertest.Day(2026, 1, 20)):  {canonical.BalanceKindClosing, "700"},
		unixKey(silvertest.Day(2026, 2, 5)):   {canonical.BalanceKindClosing, "650"},
		unixKey(silvertest.Day(2026, 2, 15)):  {canonical.BalanceKindClosing, "652.5"},
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
		if cb.Currency != "EUR" {
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
		if tx.Currency != "EUR" {
			t.Errorf("%s currency = %q", tx.TransactionExternalID, tx.Currency)
		}
	}
	// Kinds: credit→deposit, debit→withdrawal, fee category→fee, interest
	// category→interest.
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
	// Net flow: 1000 - 300 - 50 + 2.50 = 652.50.
	if net.String() != "652.5" {
		t.Errorf("net flow = %s, want 652.5", net.String())
	}
}

func unixKey(u int64) string { return time.Unix(u, 0).UTC().Format(time.RFC3339) }

// TestTxKind pins the category/description classifier, whose short-token
// substring matching once mistook "coffee" for a fee.
func TestTxKind(t *testing.T) {
	pos := canonical.NewDecimalFromFloat(10)
	neg := canonical.NewDecimalFromFloat(-10)
	cases := []struct {
		name           string
		category, desc string
		amt            canonical.Decimal
		want           canonical.TxKind
	}{
		{"plain credit", "income_other", "SALARY", pos, canonical.TxKindDeposit},
		{"plain debit", "shopping", "GROCERY", neg, canonical.TxKindWithdrawal},
		{"coffee is not a fee", "tv_phone_internet", "COFFEE BAR", neg, canonical.TxKindWithdrawal},
		{"internet is not interest", "tv_phone_internet", "INTERNET", neg, canonical.TxKindWithdrawal},
		{"fee slug token", "fees_bank", "", neg, canonical.TxKindFee},
		{"german compound fee", "other", "KONTOFÜHRUNGSENTGELT", neg, canonical.TxKindFee},
		{"listing closing entry, no category", "", "Kontoführung", neg, canonical.TxKindFee},
		{"german interest", "other", "HABENZINSEN", pos, canonical.TxKindInterest},
		{"interest category", "interest_income", "", pos, canonical.TxKindInterest},
	}
	for _, tc := range cases {
		if got := txKind(tc.category, tc.desc, tc.amt); got != tc.want {
			t.Errorf("%s: txKind(%q,%q) = %q, want %q", tc.name, tc.category, tc.desc, got, tc.want)
		}
	}
}

// TestQuietAccountCurrentAtSourceLatest pins the CURRENT balance of an
// account whose roster row did not change at the latest load to that load.
// Silver content-dedups an unchanged roster row, and gold's cash_chosen keeps
// only rows at the source's latest snapshot, so a stamp at the account's own
// older row would drop it from the latest net-worth view.
func TestQuietAccountCurrentAtSourceLatest(t *testing.T) {
	const quietIBAN = "ATkkBBBBBKKKKKKK0002"
	path, db := newFixture(t)
	seed(t, db)
	quietLoad := loadUnix - 7*86400
	if _, err := db.Exec(`INSERT INTO accounts VALUES (?,?,?,?,?,?,?,?)`,
		quietLoad, quietIBAN, "Sparkonto", "", "…0002", nil, 250.00, "{}"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	conn := openConn(t, path)
	ctx := context.Background()
	stream, err := conn.Snapshots(ctx, silvertest.FullWindow(t, conn))
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	defer stream.Close()
	batch, _, err := stream.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	var found bool
	for _, cb := range batch.CashBalances {
		if cb.AccountExternalID != quietIBAN {
			continue
		}
		found = true
		if cb.BalanceKind != canonical.BalanceKindCurrent || cb.SnapshotAt != loadUnix {
			t.Errorf("quiet account: %s balance at %d, want current at the source's latest %d",
				cb.BalanceKind, cb.SnapshotAt, loadUnix)
		}
		if cb.Amount.String() != "250" || cb.Currency != "EUR" {
			t.Errorf("quiet account: %s %s, want 250 EUR", cb.Amount.String(), cb.Currency)
		}
	}
	if !found {
		t.Fatal("quiet account: no CURRENT balance")
	}
	for _, a := range batch.Accounts {
		if a.AccountExternalID == quietIBAN && a.LastSeenAt != quietLoad {
			t.Errorf("quiet account LastSeenAt = %d, want its own latest row %d", a.LastSeenAt, quietLoad)
		}
	}
}
