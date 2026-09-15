package chase

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"testing"
	"time"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"

	_ "modernc.org/sqlite"
)

//go:embed testdata/silver_schema.sql
var silverSchemaSQL string

// day is a calendar date at UTC midnight (unix seconds) — matching how the
// collector stores posted_at / txn_date / period_end / doc_date.
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

const insertAccount = `INSERT INTO accounts
    (snapshot_at, account_external_id, account_type, nickname, mask, currency,
     balance, payload, product, pending_charges)
    VALUES (?,?,?,?,?,?,?,?,?,?)`

const insertTxn = `INSERT INTO transactions
    (fitid, posted_at, account_external_id, amount, kind, description,
     check_number, balance, source, payload, txn_date, merchant, category, currency)
    VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

const insertStmtBalance = `INSERT INTO statement_balances
    (account_external_id, period_start, period_end, opening, closing,
     snapshot_at, transactions_covered)
    VALUES (?,?,?,?,?,?,?)`

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// seed builds one checking account with four transactions carrying a running
// balance, plus statements (which the projection ignores — the cash series
// comes from the transaction running balances, not the statement dates).
func seed(t *testing.T, db *sql.DB) {
	t.Helper()
	exec(t, db, `INSERT INTO dump_runs VALUES (?,3,'/run')`, loadUnix)
	// Roster: current balance 1500.00 (deliberately later than the last
	// statement, to show the CURRENT balance is the live roster value).
	exec(t, db, insertAccount,
		loadUnix, "acct-1", "CHK", "Example Checking", "…9999", "USD",
		1500.00, "{}", "dda", nil)

	tx := func(fitid string, d int64, amt float64, kind string, bal any) {
		exec(t, db, insertTxn, fitid, d, "acct-1", amt, kind, "desc "+fitid,
			nil, bal, "csv", "{}", nil, nil, nil, nil)
	}
	tx("t1", day(2026, 1, 10), 1000.00, "CREDIT", 1000.00)
	tx("t2", day(2026, 1, 20), -300.00, "DEBIT", 700.00)
	tx("t3", day(2026, 2, 5), -50.00, "FEE", 650.00)
	tx("t4", day(2026, 2, 15), 2.50, "INT", 652.50)

	stmt := func(sha string, d int64) {
		exec(t, db, `INSERT INTO documents VALUES (?,?,?,?,'statement','pdf',?,100,'{}')`,
			sha, loadUnix, "acct-1", d, sha+".pdf")
	}
	stmt("s0", day(2025, 12, 31)) // before any transaction → skipped
	stmt("s1", day(2026, 1, 31))  // last tx on/before = t2 → 700.00
	stmt("s2", day(2026, 2, 28))  // last tx on/before = t4 → 652.50
}

// The card's live figure, as the roster reports it: POSITIVE, the amount owed.
// 500.00 posted (the last derived running balance) + 25.00 not yet posted.
const cardRosterBalance = 525.00

// seedCard adds a credit card beside the deposit account, exactly as the
// collector writes it: product 'card', a POSITIVE balance owed, spend negative
// and anything paying the balance down positive.
//
// It spans both card eras. Below the export seam the rows come from statement
// PDFs, carry a STMT_* section kind and NO per-row balance; the periods'
// printed figures are the only balance truth there. At and above the seam the
// rows come from the CSV/QFX exports, carry the provider's own `Type` and the
// loader's reconstructed running balance — and their periods' printed figures
// are what that reconstruction was anchored to, so they must not be emitted a
// second time.
func seedCard(t *testing.T, db *sql.DB) {
	t.Helper()
	exec(t, db, insertAccount,
		loadUnix, "card-1", "CARD", "Example Card", "…1111", "USD",
		cardRosterBalance, "{}", "card", 25.00)

	stmtBal := func(start, end int64, opening, closing float64, covered int) {
		exec(t, db, insertStmtBalance,
			"card-1", start, end, opening, closing, loadUnix, covered)
	}
	// A quiet period whose transactions never reached silver. Its printed
	// closing figure is still the balance truth for that month — and its
	// period_end is the earliest date the whole projection touches.
	stmtBal(day(2025, 10, 1), day(2025, 10, 31), 0.00, 100.00, 0)
	// Statement era: rows landed, no reconstructed balance anywhere in the
	// period, so the closing figure is emitted.
	stmtBal(day(2025, 11, 1), day(2025, 11, 30), 100.00, 300.00, 1)
	stmtBal(day(2025, 12, 1), day(2025, 12, 31), 300.00, 400.00, 1)
	// Export era: the reconstruction covers these periods, so their closing
	// figures stay out of the projection.
	stmtBal(day(2026, 1, 1), day(2026, 1, 31), 400.00, 580.00, 1)
	stmtBal(day(2026, 2, 1), day(2026, 2, 28), 580.00, 500.00, 1)

	tx := func(fitid string, d int64, amt float64, kind string, bal any,
		merchant, category any, source string) {
		exec(t, db, insertTxn, fitid, d, "card-1", amt, kind, "desc "+fitid,
			nil, bal, source, `{"fitid":"F-`+fitid+`"}`, d, merchant, category, nil)
	}
	// Statement era — no per-row balance, section kinds.
	tx("stmt_a", day(2025, 11, 15), -25.00, "STMT_PURCHASE", nil,
		"Example Merchant", nil, "statement")
	tx("stmt_b", day(2025, 12, 10), 25.00, "STMT_PAYMENT", nil,
		nil, nil, "statement")
	tx("stmt_c", day(2025, 12, 12), -3.00, "STMT_FEE", nil,
		nil, nil, "statement")
	tx("stmt_d", day(2025, 12, 20), -2.00, "STMT_INTEREST", nil,
		nil, nil, "statement")
	// Export era — the reconstructed running balance rolls the amount OFF the
	// balance owed (spend negative raises it).
	tx("card:card-1:c1:0", day(2026, 1, 15), -120.00, "Sale", 520.00,
		"EXAMPLE GROCER", "Groceries", "csv")
	tx("card:card-1:c2:0", day(2026, 1, 20), -60.00, "Sale", 580.00,
		"EXAMPLE CAFE", "Food & Drink", "csv")
	tx("card:card-1:c3:0", day(2026, 2, 20), 200.00, "Payment", 380.00,
		nil, nil, "csv")
	tx("card:card-1:c4:0", day(2026, 2, 22), 15.00, "Return", 365.00,
		"EXAMPLE GROCER", "Groceries", "csv")
	tx("card:card-1:c5:0", day(2026, 2, 24), 10.00, "Adjustment", 355.00,
		nil, nil, "csv")
	tx("card:card-1:c6:0", day(2026, 2, 25), -95.00, "Fee", 450.00,
		nil, "Fees & Adjustments", "csv")
	// A type this adapter version does not know.
	tx("card:card-1:c7:0", day(2026, 2, 26), -50.00, "Cash Advance", 500.00,
		"EXAMPLE ATM", nil, "csv")
	// An Adjustment the other way round: a debit that re-bills prior
	// spend, raising the balance owed like any purchase.
	tx("card:card-1:c10:0", day(2026, 2, 28), -20.00, "Adjustment", 520.00,
		nil, nil, "csv")
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

// project runs the whole projection over the full window.
func project(t *testing.T, path string) (canonical.SnapshotBatch, canonical.TransactionBatch) {
	t.Helper()
	ctx := context.Background()
	conn := openConn(t, path)
	w := fullWindow(t, conn)

	snaps, err := conn.Snapshots(ctx, w)
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	defer snaps.Close()
	sb, _, err := snaps.Next(ctx)
	if err != nil {
		t.Fatalf("Snapshots Next: %v", err)
	}

	txns, err := conn.Transactions(ctx, w)
	if err != nil {
		t.Fatalf("Transactions: %v", err)
	}
	defer txns.Close()
	tb, _, err := txns.Next(ctx)
	if err != nil {
		t.Fatalf("Transactions Next: %v", err)
	}
	return sb, tb
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

// A statement period whose transactions never reached silver still emits a
// closing balance at its period_end, so the change window has to reach back to
// it: gold deletes over [Start,End] before re-applying, and a record outside
// that window is re-inserted without its predecessor being removed — it would
// duplicate on every load.
func TestChangeWindowCoversStatementPeriodEnds(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	seedCard(t, db)
	conn := openConn(t, path)

	quiet := day(2025, 10, 31) // earlier than any transaction or roster row
	w := fullWindow(t, conn)
	if w.Start != quiet {
		t.Errorf("window Start = %d, want %d (the earliest statement period_end)",
			w.Start, quiet)
	}
	st, err := conn.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.OldestSnapshotAt != quiet {
		t.Errorf("OldestSnapshotAt = %d, want %d", st.OldestSnapshotAt, quiet)
	}

	sb, _ := project(t, path)
	for _, cb := range sb.CashBalances {
		if cb.SnapshotAt < w.Start || cb.SnapshotAt > w.End {
			t.Errorf("cash balance at %d outside the window [%d,%d]",
				cb.SnapshotAt, w.Start, w.End)
		}
	}
}

func TestSnapshots(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	sb, _ := project(t, path)

	// One cash account.
	if len(sb.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(sb.Accounts))
	}
	a := sb.Accounts[0]
	if a.AccountKind != canonical.AccountKindCash {
		t.Errorf("AccountKind = %q, want cash", a.AccountKind)
	}
	if a.DisplayName == nil || *a.DisplayName != "Example Checking" {
		t.Errorf("DisplayName = %v", a.DisplayName)
	}
	if a.BaseCurrency == nil || *a.BaseCurrency != "USD" {
		t.Errorf("BaseCurrency = %v", a.BaseCurrency)
	}
	if len(sb.Positions) != 0 || len(sb.Instruments) != 0 {
		t.Error("deposit account must not emit positions/instruments")
	}

	// Cash balances: one CURRENT (roster) + one CLOSING per transaction day,
	// valued at that day's running balance. Statements contribute nothing.
	assertBalances(t, sb.CashBalances, "acct-1", map[int64]wantBalance{
		loadUnix:         {canonical.BalanceKindCurrent, "1500"},
		day(2026, 1, 10): {canonical.BalanceKindClosing, "1000"},
		day(2026, 1, 20): {canonical.BalanceKindClosing, "700"},
		day(2026, 2, 5):  {canonical.BalanceKindClosing, "650"},
		day(2026, 2, 15): {canonical.BalanceKindClosing, "652.5"},
	})
}

// A card projects as a liability: AccountKind 'card', and every balance
// NEGATED out of silver's provider-verbatim owed-positive convention.
// The statement-balance source is product-guarded to cards, and nothing
// else enforces that: a deposit account's historic marks come from its
// export's own running-balance column, at day density, so a statement
// closing on top of them would be a second mark for the same day from a
// coarser source.
//
// The period is chosen to leave the guard as the only thing suppressing
// the row: acct-1's ledger runs 2026-01-10..2026-02-15, so a period in
// 2025-10 contains no balance-carrying deposit transaction and the
// NOT EXISTS clause admits it. Delete the product filter and this fails.
func TestStatementBalancesAreCardOnly(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	seedCard(t, db)
	exec(t, db, insertStmtBalance,
		"acct-1", day(2025, 10, 1), day(2025, 10, 31), 0.00, 999.00, loadUnix, 1)

	sb, _ := project(t, path)

	for _, cb := range sb.CashBalances {
		if cb.AccountExternalID == "acct-1" && cb.SnapshotAt == day(2025, 10, 31) {
			t.Errorf("a deposit account's statement closing was emitted (%s); "+
				"the statement-balance source is card-only", cb.Amount.String())
		}
	}
	// The card's own row over the same period still lands, so the test
	// fails for the guard rather than for an empty projection.
	var sawCard bool
	for _, cb := range sb.CashBalances {
		if cb.AccountExternalID == "card-1" && cb.SnapshotAt == day(2025, 10, 31) {
			sawCard = true
		}
	}
	if !sawCard {
		t.Error("the card's 2025-10 closing is missing; the fixture no longer bites")
	}
}

func TestCardSnapshots(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	seedCard(t, db)
	sb, _ := project(t, path)

	if len(sb.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(sb.Accounts))
	}
	byID := map[string]canonical.AccountChange{}
	for _, a := range sb.Accounts {
		byID[a.AccountExternalID] = a
	}
	if k := byID["card-1"].AccountKind; k != canonical.AccountKindCard {
		t.Errorf("card AccountKind = %q, want card", k)
	}
	if k := byID["acct-1"].AccountKind; k != canonical.AccountKindCash {
		t.Errorf("deposit AccountKind = %q, want cash", k)
	}
	if len(sb.Positions) != 0 {
		t.Error("a card carries no position — its balance is negative cash")
	}

	assertBalances(t, sb.CashBalances, "card-1", map[int64]wantBalance{
		// CURRENT: the live amount owed, negated.
		loadUnix: {canonical.BalanceKindCurrent, "-525"},
		// Statement era — the periods the reconstruction cannot reach.
		day(2025, 10, 31): {canonical.BalanceKindClosing, "-100"},
		day(2025, 11, 30): {canonical.BalanceKindClosing, "-300"},
		day(2025, 12, 31): {canonical.BalanceKindClosing, "-400"},
		// Export era — the reconstructed running balance, one per move day.
		day(2026, 1, 15): {canonical.BalanceKindClosing, "-520"},
		day(2026, 1, 20): {canonical.BalanceKindClosing, "-580"},
		day(2026, 2, 20): {canonical.BalanceKindClosing, "-380"},
		day(2026, 2, 22): {canonical.BalanceKindClosing, "-365"},
		day(2026, 2, 24): {canonical.BalanceKindClosing, "-355"},
		day(2026, 2, 25): {canonical.BalanceKindClosing, "-450"},
		day(2026, 2, 26): {canonical.BalanceKindClosing, "-500"},
		day(2026, 2, 28): {canonical.BalanceKindClosing, "-520"},
	})

	// The 2026-01 and 2026-02 statement closings (580.00 / 500.00) are the
	// anchors the reconstruction was built on; emitting them too would put a
	// second mark on a covered period. Nothing lands at those period ends
	// beyond what the ledger already carries.
	for _, cb := range sb.CashBalances {
		if cb.AccountExternalID == "card-1" && cb.SnapshotAt == day(2026, 1, 31) {
			t.Errorf("covered period 2026-01 emitted a statement closing (%s)",
				cb.Amount.String())
		}
	}

	// The deposit account's own marks are untouched by the card's arrival.
	assertBalances(t, sb.CashBalances, "acct-1", map[int64]wantBalance{
		loadUnix:         {canonical.BalanceKindCurrent, "1500"},
		day(2026, 1, 10): {canonical.BalanceKindClosing, "1000"},
		day(2026, 1, 20): {canonical.BalanceKindClosing, "700"},
		day(2026, 2, 5):  {canonical.BalanceKindClosing, "650"},
		day(2026, 2, 15): {canonical.BalanceKindClosing, "652.5"},
	})
}

// No day may carry two closing marks for one account: the running-balance
// reconstruction and the statement anchors must not overlap.
func TestNoDuplicateBalanceDays(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	seedCard(t, db)
	sb, _ := project(t, path)

	type key struct {
		acct string
		day  int64
		kind canonical.BalanceKind
	}
	seen := map[key]bool{}
	for _, cb := range sb.CashBalances {
		k := key{cb.AccountExternalID, cb.SnapshotAt, cb.BalanceKind}
		if seen[k] {
			t.Errorf("two %s balances for %s at %d", k.kind, k.acct, k.day)
		}
		seen[k] = true
	}
}

// Silver content-dedups an unchanged roster row, so a quiet account's own
// MAX(snapshot_at) falls behind the source's. Gold's cash_chosen keeps only
// the rows at the source's single MAX(snapshot_at), so every CURRENT balance
// has to be stamped there — otherwise a card whose debt did not move would
// vanish from the latest net-worth view.
func TestCurrentBalancesStampedAtSourceLatestSnapshot(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	seedCard(t, db)

	// A later load re-observes the deposit account only; the card's roster row
	// was unchanged and content-deduped away, so it keeps the older stamp.
	later := loadUnix + 86400
	exec(t, db, `INSERT INTO dump_runs VALUES (?,3,'/run2')`, later)
	exec(t, db, insertAccount,
		later, "acct-1", "CHK", "Example Checking", "…9999", "USD",
		1600.00, "{}", "dda", nil)

	sb, _ := project(t, path)

	current := map[string]canonical.CashBalanceChange{}
	for _, cb := range sb.CashBalances {
		if cb.BalanceKind == canonical.BalanceKindCurrent {
			if prev, dup := current[cb.AccountExternalID]; dup {
				t.Fatalf("two CURRENT balances for %s (at %d and %d)",
					cb.AccountExternalID, prev.SnapshotAt, cb.SnapshotAt)
			}
			current[cb.AccountExternalID] = cb
		}
	}
	if len(current) != 2 {
		t.Fatalf("CURRENT balances = %d, want 2 (every account)", len(current))
	}
	for id, cb := range current {
		if cb.SnapshotAt != later {
			t.Errorf("%s CURRENT stamped at %d, want %d (the source's latest "+
				"roster snapshot)", id, cb.SnapshotAt, later)
		}
	}
	// Each account still carries its own latest KNOWN figure — the stamp moves,
	// the value does not.
	if got := current["acct-1"].Amount.String(); got != "1600" {
		t.Errorf("deposit CURRENT = %s, want 1600", got)
	}
	if got := current["card-1"].Amount.String(); got != "-525" {
		t.Errorf("card CURRENT = %s, want -525 (the unchanged debt, negated)", got)
	}
}

func TestTransactions(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	_, tb := project(t, path)
	if len(tb.Transactions) != 4 {
		t.Fatalf("transactions = %d, want 4", len(tb.Transactions))
	}

	byID := map[string]canonical.TransactionChange{}
	var net canonical.Decimal
	for _, tx := range tb.Transactions {
		byID[tx.TransactionExternalID] = tx
		if tx.NetAmount != nil {
			net = net.Add(*tx.NetAmount)
		}
		if tx.Currency != "USD" {
			t.Errorf("%s currency = %q", tx.TransactionExternalID, tx.Currency)
		}
		if tx.Counterparty != nil || tx.ProviderCategory != nil {
			t.Errorf("%s: a deposit row carries no merchant/category",
				tx.TransactionExternalID)
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

func TestCardTransactions(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	seedCard(t, db)
	_, tb := project(t, path)

	byID := map[string]canonical.TransactionChange{}
	for _, tx := range tb.Transactions {
		byID[tx.TransactionExternalID] = tx
	}
	if len(byID) != 16 {
		t.Fatalf("transactions = %d, want 16 (4 deposit + 12 card)", len(byID))
	}

	cases := []struct {
		id   string
		kind canonical.TxKind
		amt  string
	}{
		// Statement section kinds.
		{"stmt_a", canonical.TxKindPurchase, "-25"},
		{"stmt_b", canonical.TxKindCardPayment, "25"},
		{"stmt_c", canonical.TxKindFee, "-3"},
		{"stmt_d", canonical.TxKindInterest, "-2"},
		// Export types.
		{"card:card-1:c1:0", canonical.TxKindPurchase, "-120"},
		{"card:card-1:c2:0", canonical.TxKindPurchase, "-60"},
		{"card:card-1:c3:0", canonical.TxKindCardPayment, "200"},
		{"card:card-1:c4:0", canonical.TxKindRefund, "15"},
		// An Adjustment follows the issuer's sign: a credit against prior
		// spend is a `refund`, a debit re-billing it a `purchase`. Both
		// net inside the spending base instead of dropping out of it, and
		// neither is flipped by the canonical-sign rule.
		{"card:card-1:c5:0", canonical.TxKindRefund, "10"},
		{"card:card-1:c10:0", canonical.TxKindPurchase, "-20"},
		{"card:card-1:c6:0", canonical.TxKindFee, "-95"},
		// Unrecognised type → other, source sign preserved.
		{"card:card-1:c7:0", canonical.TxKindOther, "-50"},
	}
	for _, exp := range cases {
		tx, ok := byID[exp.id]
		if !ok {
			t.Errorf("missing tx %s", exp.id)
			continue
		}
		if tx.Kind != exp.kind {
			t.Errorf("%s kind = %q, want %q", exp.id, tx.Kind, exp.kind)
		}
		if tx.NetAmount == nil || tx.NetAmount.String() != exp.amt {
			t.Errorf("%s net = %v, want %s", exp.id, tx.NetAmount, exp.amt)
		}
		if tx.GrossAmount == nil || tx.GrossAmount.String() != exp.amt {
			t.Errorf("%s gross = %v, want %s", exp.id, tx.GrossAmount, exp.amt)
		}
	}

	// No reward is produced: chase issues no reward transaction (a redemption
	// surfaces as an Adjustment), and nothing is guessed from descriptors.
	for id, tx := range byID {
		if tx.Kind == canonical.TxKindReward {
			t.Errorf("%s mapped to reward, which has no producer here", id)
		}
	}

	// Merchant → Counterparty, provider category → ProviderCategory, both
	// verbatim. An uncategorised payment carries neither.
	spend := byID["card:card-1:c1:0"]
	if spend.Counterparty == nil || *spend.Counterparty != "EXAMPLE GROCER" {
		t.Errorf("Counterparty = %v, want EXAMPLE GROCER", spend.Counterparty)
	}
	if spend.ProviderCategory == nil || *spend.ProviderCategory != "Groceries" {
		t.Errorf("ProviderCategory = %v, want Groceries", spend.ProviderCategory)
	}
	pay := byID["card:card-1:c3:0"]
	if pay.Counterparty != nil || pay.ProviderCategory != nil {
		t.Errorf("payment carries merchant/category: %v / %v",
			pay.Counterparty, pay.ProviderCategory)
	}

	// OccurredAt is the post date; the transaction date rides in the payload.
	if spend.OccurredAt != day(2026, 1, 15) {
		t.Errorf("OccurredAt = %d, want the post date %d",
			spend.OccurredAt, day(2026, 1, 15))
	}
	p := decodePayload(t, spend.Payload)
	if p["txn_date"] != "2026-01-15" {
		t.Errorf("payload txn_date = %v, want 2026-01-15", p["txn_date"])
	}
	if p["fitid"] != "F-card:card-1:c1:0" {
		t.Errorf("payload lost the collector's keys: %v", p)
	}
	if _, ok := p["source_kind"]; ok {
		t.Errorf("a recognised type must not annotate source_kind: %v", p)
	}

	// The unmapped type keeps its raw string in the payload.
	unknown := decodePayload(t, byID["card:card-1:c7:0"].Payload)
	if unknown["source_kind"] != "Cash Advance" {
		t.Errorf("payload source_kind = %v, want \"Cash Advance\"", unknown["source_kind"])
	}
}

// Sign handling is per-kind, not per-row: a card row whose provider sign
// contradicts its kind is corrected rather than trusted.
func TestCardSignsAreForcedByKind(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	seedCard(t, db)
	// A purchase booked positive and a payment booked negative — neither
	// direction the provider actually writes, both fixed on the way through.
	exec(t, db, insertTxn, "card:card-1:c8:0", day(2026, 2, 27), "card-1", 77.00,
		"Sale", "desc", nil, nil, "csv", "{}", day(2026, 2, 27), "EXAMPLE SHOP", nil, nil)
	exec(t, db, insertTxn, "card:card-1:c9:0", day(2026, 2, 27), "card-1", -88.00,
		"Payment", "desc", nil, nil, "csv", "{}", day(2026, 2, 27), nil, nil, nil)

	_, tb := project(t, path)
	got := map[string]string{}
	for _, tx := range tb.Transactions {
		if tx.NetAmount != nil {
			got[tx.TransactionExternalID] = tx.NetAmount.String()
		}
	}
	if got["card:card-1:c8:0"] != "-77" {
		t.Errorf("purchase net = %s, want -77", got["card:card-1:c8:0"])
	}
	if got["card:card-1:c9:0"] != "88" {
		t.Errorf("card payment net = %s, want 88", got["card:card-1:c9:0"])
	}
}

// A per-row currency overrides the account's; NULL means the account's own.
func TestTransactionRowCurrency(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	seedCard(t, db)
	exec(t, db, insertTxn, "card:card-1:fx:0", day(2026, 2, 27), "card-1", -40.00,
		"Sale", "desc", nil, nil, "csv", "{}", day(2026, 2, 27), "EXAMPLE SHOP", nil, "EUR")

	_, tb := project(t, path)
	for _, tx := range tb.Transactions {
		want := "USD"
		if tx.TransactionExternalID == "card:card-1:fx:0" {
			want = "EUR"
		}
		if tx.Currency != want {
			t.Errorf("%s currency = %q, want %q", tx.TransactionExternalID, tx.Currency, want)
		}
	}
}

type wantBalance struct {
	kind canonical.BalanceKind
	amt  string
}

// assertBalances checks one account's cash-balance marks exactly: every
// expected day present with the right kind and amount, and nothing else.
func assertBalances(t *testing.T, got []canonical.CashBalanceChange,
	accountID string, want map[int64]wantBalance) {
	t.Helper()
	seen := map[int64]bool{}
	for _, cb := range got {
		if cb.AccountExternalID != accountID {
			continue
		}
		exp, ok := want[cb.SnapshotAt]
		if !ok {
			t.Errorf("%s: unexpected balance at %s (%s %s)", accountID,
				isoDay(cb.SnapshotAt), cb.BalanceKind, cb.Amount.String())
			continue
		}
		seen[cb.SnapshotAt] = true
		if cb.BalanceKind != exp.kind {
			t.Errorf("%s @%s kind = %q, want %q", accountID,
				isoDay(cb.SnapshotAt), cb.BalanceKind, exp.kind)
		}
		if cb.Amount.String() != exp.amt {
			t.Errorf("%s @%s amount = %s, want %s", accountID,
				isoDay(cb.SnapshotAt), cb.Amount.String(), exp.amt)
		}
		if cb.Currency != "USD" {
			t.Errorf("%s @%s currency = %q", accountID, isoDay(cb.SnapshotAt), cb.Currency)
		}
	}
	for at := range want {
		if !seen[at] {
			t.Errorf("%s: missing balance at %s", accountID, isoDay(at))
		}
	}
}

func decodePayload(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("payload %s: %v", raw, err)
	}
	return m
}

func isoDay(u int64) string { return time.Unix(u, 0).UTC().Format(time.RFC3339) }

// TestCardTxKindQFXFallback pins the third vocabulary the card kind column
// carries: a row that only ever landed in the QFX export falls back to its
// OFX TRNTYPE, which says nothing but the direction of the money. Both values
// key off the sign — spend is a `purchase`, a credit reducing the balance owed
// is a `card_payment`, which pairs with its deposit leg and nets out. Mapping
// either to `other` would drop the row out of the spending base entirely.
func TestCardTxKindQFXFallback(t *testing.T) {
	cases := []struct {
		raw  string
		amt  int64
		want canonical.TxKind
	}{
		{"DEBIT", -40, canonical.TxKindPurchase},
		{"CREDIT", 250, canonical.TxKindCardPayment},
		// The sign wins over the name in both directions, exactly as it
		// does for an Adjustment.
		{"debit", 40, canonical.TxKindCardPayment},
		{"credit", -250, canonical.TxKindPurchase},
	}
	for _, c := range cases {
		got, known := cardTxKind(c.raw, canonical.NewDecimalFromInt(c.amt))
		if !known {
			t.Errorf("cardTxKind(%q, %d): not recognised, want %q", c.raw, c.amt, c.want)
		}
		if got != c.want {
			t.Errorf("cardTxKind(%q, %d) = %q, want %q", c.raw, c.amt, got, c.want)
		}
	}
}

// TestCheckNumberOnlyOnAnOutflow pins gold's contract for the cheque
// number (migration 0075): it names an OUTGOING payment, so the sign
// decides whether silver's column reaches gold at all.
//
// The gate is not pedantry. Silver fills `check_number` from two places
// with different meanings — the QFX CHECKNUM field, which is a cheque,
// and a deposit export whose column is headed "Check or Slip #", where
// an inflow carries a deposit SLIP number. Only the sign tells them
// apart, and a slip number surfaced as a cheque number would send the
// holder looking for a cheque they never wrote.
func TestCheckNumberOnlyOnAnOutflow(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)

	row := func(fitid string, amt float64, checkNo any) {
		exec(t, db, insertTxn, fitid, day(2026, 3, 2), "acct-1", amt, "DEBIT",
			"desc "+fitid, checkNo, nil, "csv", "{}", nil, nil, nil, nil)
	}
	row("chk-out", -250.00, "9042") // a cheque the holder wrote
	row("chk-in", 250.00, "980321") // a deposit SLIP riding in on a credit
	row("plain-out", -75.00, nil)   // an ordinary outflow, no cheque
	row("zero", 0.00, "9043")       // not an outflow: no money left

	_, tb := project(t, path)
	got := map[string]*string{}
	for _, tx := range tb.Transactions {
		got[tx.TransactionExternalID] = tx.CheckNumber
	}

	if v := got["chk-out"]; v == nil || *v != "9042" {
		t.Errorf("outgoing cheque: CheckNumber = %v, want 9042", v)
	}
	for _, id := range []string{"chk-in", "plain-out", "zero"} {
		if v := got[id]; v != nil {
			t.Errorf("%s: CheckNumber = %q, want nil — only an outflow carries one", id, *v)
		}
	}
	// Every other row the fixture seeds carries no cheque number, so a
	// gate that leaked would show up here rather than only on the four
	// rows above.
	for _, tx := range tb.Transactions {
		switch tx.TransactionExternalID {
		case "chk-out", "chk-in", "plain-out", "zero":
		default:
			if tx.CheckNumber != nil {
				t.Errorf("%s: unexpected CheckNumber %q", tx.TransactionExternalID, *tx.CheckNumber)
			}
		}
	}
}
