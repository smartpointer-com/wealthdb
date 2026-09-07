package amex

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
// collector stores posted_at / period_end / doc_date.
func day(y int, m time.Month, d int) int64 {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix()
}

// The two load clocks (bronze dump slug times) the fixture is seeded with. A
// second load is what makes the CURRENT stamp observable: it is the SOURCE's
// latest roster snapshot, which a card that stopped changing no longer carries
// one of its own.
var (
	loadUnix      = time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC).Unix()
	laterLoadUnix = time.Date(2026, 3, 22, 12, 0, 0, 0, time.UTC).Unix()
)

const (
	acct      = "0123456789ABCDEF0123456789ABCDEF"
	quietAcct = "FEDCBA9876543210FEDCBA9876543210"
)

func newFixture(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := t.TempDir() + "/amex.db"
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

// The seeds name their columns rather than relying on the fixture's order.
// A column added, dropped or moved in the schema then fails as a named-column
// error that says which column, instead of an arity mismatch or, worse, a row
// written into the neighbouring column.
const insertDumpRun = `INSERT INTO dump_runs
    (snapshot_at, silver_schema_version, run_dir)
    VALUES (?,?,?)`

const insertAccount = `INSERT INTO accounts
    (snapshot_at, account_external_id, account_token, display_name, mask,
     currency, balance, pending_charges, payment_due_at, account_status,
     line_of_business, user_type, payload)
    VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`

const insertTxn = `INSERT INTO transactions
    (txn_id, posted_at, account_external_id, amount, kind, description, merchant,
     category, category_code, txn_date, statement_end_at, currency, is_pending,
     source, payload)
    VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

const insertStmtBalance = `INSERT INTO statement_balances
    (account_external_id, period_start, period_end, opening, closing,
     transactions_covered, source, snapshot_at)
    VALUES (?,?,?,?,?,?,?,?)`

const insertDocument = `INSERT INTO documents
    (sha256, snapshot_at, account_external_id, doc_date, doc_kind, file_format,
     filename, size_bytes, payload)
    VALUES (?,?,?,?,?,?,?,?,?)`

// seed builds two cards over two loads: one with a small ledger of every kind
// the projection can produce, two statement periods (the only historic balance
// a card has) and a statement document, and a second whose roster row the
// later load content-deduped away, so its own latest snapshot stays a load
// behind the source's. Silver holds the PROVIDER's signs: the balance is the
// positive amount owed, spend is negative.
func seed(t *testing.T, db *sql.DB) {
	t.Helper()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	exec(insertDumpRun, loadUnix, 1, "/run")
	exec(insertDumpRun, laterLoadUnix, 1, "/run")

	account := func(snap int64, id, token string, name any, mask string,
		balance float64) {
		exec(insertAccount,
			snap, id, token, name, mask, "USD", balance, 25.00,
			day(2026, 4, 5), "Active", "CONSUMER", "ACCOUNT_HOLDER",
			`{"product":"card"}`)
	}
	// The first card's balance moved between the loads, so it has a row at
	// each and the projection reads the later one.
	account(loadUnix, acct, "AAAA1B2C3D4E5F6", "Example Card", "-01234", 380.00)
	account(laterLoadUnix, acct, "AAAA1B2C3D4E5F6", "Example Card", "-01234", 420.00)
	// The second card's did not move, so the content-dedup left it with no row
	// at the later load. It carries no product name either, so its display
	// falls back to the mask.
	account(loadUnix, quietAcct, "BBBB2C3D4E5F6A7", nil, "-56789", 250.00)

	tx := func(id string, d int64, amt float64, kind, category, merchant string,
		txnDate any, pending int) {
		exec(insertTxn,
			id, d, acct, amt, kind, merchant, merchant, category, "C1",
			txnDate, day(2026, 3, 12), nil, pending, "activity", "{}")
	}
	// A purchase, a fee, a merchant refund (categorised), the monthly bill
	// (uncategorised — the tell that separates it from a refund), the
	// reversal of the fee, and a pending purchase.
	tx("100000000000000001", day(2026, 2, 10), -60.00, "DEBIT",
		"Merchandise & Supplies", "EXAMPLE STORE", day(2026, 2, 9), 0)
	tx("100000000000000002", day(2026, 2, 20), -12.00, "DEBIT",
		"Fees & Adjustments", "ANNUAL MEMBERSHIP FEE", nil, 0)
	tx("100000000000000003", day(2026, 3, 2), 15.00, "CREDIT",
		"Merchandise & Supplies", "EXAMPLE STORE", nil, 0)
	tx("100000000000000004", day(2026, 3, 5), 400.00, "CREDIT",
		"", "PAYMENT RECEIVED THANK YOU", nil, 0)
	tx("100000000000000005", day(2026, 3, 6), 12.00, "CREDIT",
		"Fees & Adjustments", "ANNUAL FEE REVERSAL", nil, 0)
	tx("P0001ABCDEF0000000", day(2026, 3, 14), -25.00, "DEBIT",
		"Restaurants", "EXAMPLE CAFE", nil, 1)

	period := func(start, end int64, opening, closing float64) {
		exec(insertStmtBalance,
			acct, start, end, opening, closing, 1, "activity", loadUnix)
	}
	// The statement archive reaches back further than the ledger does (the
	// real shape: ~7 years of statements over ~24 months of activity), so the
	// oldest period end predates every transaction.
	period(day(2025, 12, 13), day(2026, 1, 12), 100.00, 160.00)
	period(day(2026, 2, 13), day(2026, 3, 12), 160.00, 420.00)

	exec(insertDocument,
		"sha-1", loadUnix, acct, day(2026, 3, 12), "statement", "pdf",
		"2026-03-12.pdf", 100, "{}")
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

// snapshots drains the snapshot stream into one batch.
func snapshots(t *testing.T, conn silver.Connection, w canonical.Window) canonical.SnapshotBatch {
	t.Helper()
	ctx := context.Background()
	stream, err := conn.Snapshots(ctx, w)
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	defer stream.Close()
	var all canonical.SnapshotBatch
	for {
		// Next yields a batch and says whether another FOLLOWS, so the
		// batch is accumulated before the loop decides to stop.
		b, more, err := stream.Next(ctx)
		if err != nil {
			t.Fatalf("Snapshots Next: %v", err)
		}
		all.Accounts = append(all.Accounts, b.Accounts...)
		all.CashBalances = append(all.CashBalances, b.CashBalances...)
		all.Positions = append(all.Positions, b.Positions...)
		if !more {
			return all
		}
	}
}

// transactions drains the transaction stream.
func transactions(t *testing.T, conn silver.Connection, w canonical.Window) []canonical.TransactionChange {
	t.Helper()
	ctx := context.Background()
	stream, err := conn.Transactions(ctx, w)
	if err != nil {
		t.Fatalf("Transactions: %v", err)
	}
	defer stream.Close()
	var out []canonical.TransactionChange
	for {
		b, more, err := stream.Next(ctx)
		if err != nil {
			t.Fatalf("Transactions Next: %v", err)
		}
		out = append(out, b.Transactions...)
		if !more {
			return out
		}
	}
}

func TestKind(t *testing.T) {
	if (&Adapter{}).Kind() != "amex" {
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
	if st.LatestChangeNumber != laterLoadUnix {
		t.Errorf("LatestChangeNumber = %d, want %d", st.LatestChangeNumber, laterLoadUnix)
	}
	// The oldest thing the projection touches is the FIRST STATEMENT PERIOD's
	// end, which predates every transaction: a card's balance history comes
	// only from the periods, so a window derived from the ledger alone would
	// leave the oldest anchors outside the re-emit range.
	if st.OldestSnapshotAt != day(2026, 1, 12) {
		t.Errorf("OldestSnapshotAt = %d, want %d", st.OldestSnapshotAt, day(2026, 1, 12))
	}
	if st.LatestSnapshotAt != laterLoadUnix {
		t.Errorf("LatestSnapshotAt = %d, want %d", st.LatestSnapshotAt, laterLoadUnix)
	}

	w := fullWindow(t, conn)
	if w.Start != day(2026, 1, 12) || w.End != laterLoadUnix {
		t.Errorf("window = [%d,%d]", w.Start, w.End)
	}

	// The watermark is the load clock, so a dump loaded past it re-triggers
	// and carries the watermark forward to that load.
	again, err := conn.ChangeWindow(ctx, loadUnix)
	if err != nil {
		t.Fatalf("ChangeWindow after the first load: %v", err)
	}
	if !again.HasChanges || again.NewChangeNumber != laterLoadUnix {
		t.Errorf("a second load must re-trigger: %+v", again)
	}

	idle, err := conn.ChangeWindow(ctx, laterLoadUnix)
	if err != nil {
		t.Fatalf("ChangeWindow idle: %v", err)
	}
	if idle.HasChanges {
		t.Error("an idle reload should yield no changes")
	}
}

func TestAccountIsACardLiability(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	conn := openConn(t, path)
	batch := snapshots(t, conn, fullWindow(t, conn))

	if len(batch.Accounts) != 2 {
		t.Fatalf("accounts = %d, want one per card", len(batch.Accounts))
	}
	for _, a := range batch.Accounts {
		if a.AccountKind != canonical.AccountKindCard {
			t.Errorf("%s: AccountKind = %q, want card", a.AccountExternalID, a.AccountKind)
		}
		if a.TaxWrapper == nil || *a.TaxWrapper != canonical.TaxWrapperTaxablePersonal {
			t.Errorf("%s: TaxWrapper = %v", a.AccountExternalID, a.TaxWrapper)
		}
	}
	// In id order: the card with a product name, then the one without, whose
	// display falls back to the mask.
	if a := batch.Accounts[0]; a.DisplayName == nil || *a.DisplayName != "Example Card" {
		t.Errorf("DisplayName = %v", a.DisplayName)
	}
	if a := batch.Accounts[1]; a.DisplayName == nil || *a.DisplayName != "-56789" {
		t.Errorf("an unnamed card should display its mask, got %v", a.DisplayName)
	}
	// A card carries no instrument, so nothing is ever a position.
	if len(batch.Positions) != 0 {
		t.Errorf("positions = %d, want 0", len(batch.Positions))
	}
}

func TestBalancesAreNegatedExactlyOnce(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	conn := openConn(t, path)
	batch := snapshots(t, conn, fullWindow(t, conn))

	type mark struct {
		id   string
		at   int64
		kind canonical.BalanceKind
		amt  string
	}
	var got []mark
	for _, b := range batch.CashBalances {
		got = append(got, mark{b.AccountExternalID, b.SnapshotAt, b.BalanceKind,
			b.Amount.String()})
	}
	want := []mark{
		// Each card's roster figure, as of the source's latest load — the
		// quiet card's too, though its own row is a load older.
		{acct, laterLoadUnix, canonical.BalanceKindCurrent, "-420"},
		{quietAcct, laterLoadUnix, canonical.BalanceKindCurrent, "-250"},
		// One closing mark per statement period — the only historic balance
		// truth a card has.
		{acct, day(2026, 1, 12), canonical.BalanceKindClosing, "-160"},
		{acct, day(2026, 3, 12), canonical.BalanceKindClosing, "-420"},
	}
	if len(got) != len(want) {
		t.Fatalf("cash balances = %+v, want %d marks", got, len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("mark %d = %+v, want %+v", i, got[i], w)
		}
	}
}

// TestCurrentIsStampedAtTheSourcesLatestSnapshot pins the stamp that keeps a
// quiet card in the latest net-worth view. The second card's roster row was
// content-deduped by the later load, so its own MAX(snapshot_at) is a load
// behind the source's; gold's cash_chosen keeps only the rows carrying the
// source's single MAX(snapshot_at), so a card stamped at its own would drop
// out the moment it stopped changing — taking its debt with it.
func TestCurrentIsStampedAtTheSourcesLatestSnapshot(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)

	// The premise the stamp exists for, asserted so the fixture cannot lose
	// it silently: the quiet card has no row at the later load.
	var own int64
	if err := db.QueryRow(`SELECT MAX(snapshot_at) FROM accounts
	                        WHERE account_external_id = ?`, quietAcct).
		Scan(&own); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if own != loadUnix {
		t.Fatalf("fixture: the quiet card's own latest snapshot = %d, want the "+
			"earlier load %d", own, loadUnix)
	}

	conn := openConn(t, path)
	current := map[string]int64{}
	for _, b := range snapshots(t, conn, fullWindow(t, conn)).CashBalances {
		if b.BalanceKind == canonical.BalanceKindCurrent {
			current[b.AccountExternalID] = b.SnapshotAt
		}
	}
	if len(current) != 2 {
		t.Fatalf("CURRENT marks = %d, want one per card", len(current))
	}
	for id, at := range current {
		if at != laterLoadUnix {
			t.Errorf("%s: CURRENT stamped at %d, want the source's latest %d",
				id, at, laterLoadUnix)
		}
	}
}

func TestTransactionKinds(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	conn := openConn(t, path)
	txs := transactions(t, conn, fullWindow(t, conn))

	byID := map[string]canonical.TransactionChange{}
	for _, tx := range txs {
		byID[tx.TransactionExternalID] = tx
	}
	for _, tc := range []struct {
		id     string
		kind   canonical.TxKind
		amount string
	}{
		{"100000000000000001", canonical.TxKindPurchase, "-60"},
		{"100000000000000002", canonical.TxKindFee, "-12"},
		// A CATEGORISED credit is a merchant refund.
		{"100000000000000003", canonical.TxKindRefund, "15"},
		// An UNCATEGORISED credit is the monthly bill — the tell that lets
		// the internal-transfer matcher pair it with the cash account's
		// withdrawal and retire the `card_spend` placeholder.
		{"100000000000000004", canonical.TxKindCardPayment, "400"},
		// A fee REVERSAL is a categorised credit like any other, so the
		// direction is read before the category: it nets against the fee
		// inside the spending base, where kinding it `fee` would force
		// the canonical sign negative and count the charge twice.
		{"100000000000000005", canonical.TxKindRefund, "12"},
		{"P0001ABCDEF0000000", canonical.TxKindPurchase, "-25"},
	} {
		tx, ok := byID[tc.id]
		if !ok {
			t.Errorf("%s missing", tc.id)
			continue
		}
		if tx.Kind != tc.kind {
			t.Errorf("%s kind = %q, want %q", tc.id, tx.Kind, tc.kind)
		}
		if tx.NetAmount == nil || tx.NetAmount.String() != tc.amount {
			t.Errorf("%s amount = %v, want %s", tc.id, tx.NetAmount, tc.amount)
		}
	}
}

// TestStatementEraKindsComeFromTheSection pins the DEEP era's mapping. Those
// rows carry no spend category at all — the section they were printed under is
// the only thing separating a bill payment from a statement credit from a
// purchase — and the statement states interest as its own section, which the
// modern era cannot separate from fees.
func TestStatementEraKindsComeFromTheSection(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	rows := []struct {
		id, section string
		amount      float64
		want        canonical.TxKind
	}{
		{"stmt:a:2020-02-10:0000", "STMT_PURCHASE", -30, canonical.TxKindPurchase},
		{"stmt:a:2020-02-10:0001", "STMT_PAYMENT", 200, canonical.TxKindCardPayment},
		{"stmt:a:2020-02-10:0002", "STMT_CREDIT", 10, canonical.TxKindRefund},
		{"stmt:a:2020-02-10:0003", "STMT_FEE", -12, canonical.TxKindFee},
		{"stmt:a:2020-02-10:0004", "STMT_INTEREST", -3, canonical.TxKindInterest},
	}
	for _, r := range rows {
		if _, err := db.Exec(insertTxn,
			r.id, day(2020, 2, 5), acct, r.amount, r.section, "OLD ROW",
			"OLD ROW", nil, nil, day(2020, 2, 5), nil, nil, 0, "statement",
			"{}"); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
	conn := openConn(t, path)
	got := map[string]canonical.TxKind{}
	for _, tx := range transactions(t, conn, fullWindow(t, conn)) {
		got[tx.TransactionExternalID] = tx.Kind
	}
	for _, r := range rows {
		if got[r.id] != r.want {
			t.Errorf("%s (%s) = %q, want %q", r.id, r.section, got[r.id], r.want)
		}
	}
}

func TestProviderCategoryAndMerchantAreCarriedVerbatim(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	conn := openConn(t, path)
	txs := transactions(t, conn, fullWindow(t, conn))

	for _, tx := range txs {
		if tx.TransactionExternalID != "100000000000000001" {
			continue
		}
		if tx.ProviderCategory == nil || *tx.ProviderCategory != "Merchandise & Supplies" {
			t.Errorf("ProviderCategory = %v, want the provider's own words",
				tx.ProviderCategory)
		}
		if tx.Counterparty == nil || *tx.Counterparty != "EXAMPLE STORE" {
			t.Errorf("Counterparty = %v", tx.Counterparty)
		}
		return
	}
	t.Fatal("purchase row missing")
}

func TestPendingAndChargeDateRideInThePayload(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	conn := openConn(t, path)
	txs := transactions(t, conn, fullWindow(t, conn))

	payloadOf := func(id string) map[string]any {
		t.Helper()
		for _, tx := range txs {
			if tx.TransactionExternalID == id {
				var m map[string]any
				if err := json.Unmarshal(tx.Payload, &m); err != nil {
					t.Fatalf("payload %s: %v", id, err)
				}
				return m
			}
		}
		t.Fatalf("%s missing", id)
		return nil
	}
	// The charge date is carried ALONGSIDE the post date, never substituted:
	// OccurredAt is the post date, which is what the balance series keys on.
	if got := payloadOf("100000000000000001")["txn_date"]; got != "2026-02-09" {
		t.Errorf("txn_date = %v, want 2026-02-09", got)
	}
	if got := payloadOf("P0001ABCDEF0000000")["pending"]; got != true {
		t.Errorf("pending = %v, want true", got)
	}
	if _, ok := payloadOf("100000000000000002")["txn_date"]; ok {
		t.Error("a row with no charge date should carry no txn_date")
	}
}

// TestAnUnknownDirectionFallsBackWithoutLosingTheRow pins BOTH signs of the
// fallback. A direction this build does not know keeps the row, kinded from
// the sign silver already normalised — purchase when it reduced the balance,
// card_payment when it increased it — and never as `other`: that is the
// deliberate departure from docs/DESIGN.md §6.8, and `other` would drop the
// row out of both the spending base and the matcher pool. The raw value is
// kept in the payload, which is what the drift counter reads.
func TestAnUnknownDirectionFallsBackWithoutLosingTheRow(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	odd := func(id string, amt float64) {
		t.Helper()
		if _, err := db.Exec(insertTxn,
			id, day(2026, 3, 10), acct, amt, "SOMETHING NEW", "ODD ROW",
			"ODD ROW", "Travel", "C9", nil, nil, nil, 0, "activity",
			"{}"); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	odd("100000000000000009", -5.00)
	odd("100000000000000008", 7.50)

	conn := openConn(t, path)
	byID := map[string]canonical.TransactionChange{}
	for _, tx := range transactions(t, conn, fullWindow(t, conn)) {
		byID[tx.TransactionExternalID] = tx
	}
	for _, tc := range []struct {
		id     string
		kind   canonical.TxKind
		amount string
	}{
		{"100000000000000009", canonical.TxKindPurchase, "-5"},
		{"100000000000000008", canonical.TxKindCardPayment, "7.5"},
	} {
		tx, ok := byID[tc.id]
		if !ok {
			t.Errorf("%s was dropped", tc.id)
			continue
		}
		if tx.Kind != tc.kind {
			t.Errorf("%s kind = %q, want %q", tc.id, tx.Kind, tc.kind)
		}
		if tx.NetAmount == nil || tx.NetAmount.String() != tc.amount {
			t.Errorf("%s amount = %v, want %s", tc.id, tx.NetAmount, tc.amount)
		}
		var m map[string]any
		if err := json.Unmarshal(tx.Payload, &m); err != nil {
			t.Fatalf("%s payload: %v", tc.id, err)
		}
		if m["source_kind"] != "SOMETHING NEW" {
			t.Errorf("%s source_kind = %v, want the raw value", tc.id, m["source_kind"])
		}
	}
}

func TestEmptyWindowEmitsNothing(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	conn := openConn(t, path)
	empty := canonical.Window{}
	if b := snapshots(t, conn, empty); len(b.Accounts) != 0 || len(b.CashBalances) != 0 {
		t.Errorf("snapshots on an empty window = %+v", b)
	}
	if txs := transactions(t, conn, empty); len(txs) != 0 {
		t.Errorf("transactions on an empty window = %d", len(txs))
	}
}

func TestEmptySilverIsNotAnError(t *testing.T) {
	path, _ := newFixture(t)
	conn := openConn(t, path)
	st, err := conn.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.LatestChangeNumber != -1 {
		t.Errorf("LatestChangeNumber = %d, want -1", st.LatestChangeNumber)
	}
	w, err := conn.ChangeWindow(context.Background(), 0)
	if err != nil {
		t.Fatalf("ChangeWindow: %v", err)
	}
	if w.HasChanges {
		t.Error("empty silver should report no changes")
	}
}
