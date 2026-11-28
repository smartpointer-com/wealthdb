package erstebankat

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
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

// Synthetic ids: a placeholder-letter IBAN with a numeric tail, and a card
// shadow account id of the same made-up shape the collector tests use.
const (
	iban = "ATkkBBBBBKKKKKKK1234"
	card = "SHDW0000000000S1"
)

// loadUnix is a full-timestamp load clock (a bronze dump slug time).
var loadUnix = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).Unix()

func newFixture(t *testing.T) (string, *sql.DB) {
	t.Helper()
	return silvertest.NewDB(t, silverSchemaSQL)
}

func account(t *testing.T, db *sql.DB, at int64, id, kind, name, mask string, balance float64) {
	t.Helper()
	silvertest.Exec(t, db, `INSERT INTO accounts VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		at, id, kind, "SRC-"+id, "PRODUCT", name, mask, "EUR", balance, nil, "{}")
}

// seed builds one checking account and one card, both listed by one run,
// with a ledger covering every kind rule and a daily-balance series. The
// card's figures are the amount owed, as silver holds them.
func seed(t *testing.T, db *sql.DB) {
	t.Helper()
	exec := func(q string, args ...any) { silvertest.Exec(t, db, q, args...) }
	exec(`INSERT INTO dump_runs VALUES (?,1,'/run')`, loadUnix)
	account(t, db, loadUnix, iban, "checking", "Girokonto", "…1234", 1500.00)
	account(t, db, loadUnix, card, "credit_card", "Kreditkarte", "…5678", 60.00)
	exec(`INSERT INTO run_accounts VALUES (?,?), (?,?)`, loadUnix, iban, loadUnix, card)

	tx := func(id, acct string, d int64, amt float64, relevant int, txType, desc, cp, ref, payload string) {
		// The loader derives the reference from the payload's free-text
		// lines, and the adapter reads the memo from the same place.
		if ref != "" {
			var p map[string]any
			if err := json.Unmarshal([]byte(payload), &p); err != nil {
				t.Fatal(err)
			}
			p["additionalTexts"] = map[string]any{"lineItems": []string{ref}}
			b, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			payload = string(b)
		}
		exec(`INSERT INTO transactions VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, acct, d, d, amt, "EUR", nil, relevant, txType, nil,
			nilIfEmpty(desc), nilIfEmpty(cp), nilIfEmpty(ref), loadUnix, payload)
	}
	cat := func(sub string) string {
		return `{"categories":[{"mainCategory":"X","subCategory":"` + sub + `"}]}`
	}
	jan := func(d int) int64 { return silvertest.Day(2026, 1, d) }
	tx("t1", iban, jan(5), 1000.00, 1, "OTHER", "Gehalt", "ACME GmbH", "Lohn Jänner", cat("SALARY"))
	tx("t2", iban, jan(6), -40.00, 1, "POSPAYMENT", "Testmarkt", "Testmarkt", "", cat("FOOD_SHOPPING"))
	tx("t3", iban, jan(31), -5.00, 1, "BANKFEE", "Abschluss", "", "Kontoführung", cat("FEES"))
	tx("t4", iban, jan(31), 2.00, 1, "BANKFEE", "Abschluss", "", "Habenzinsen", cat("SAVINGS_INCOME"))
	tx("t5", iban, jan(31), -0.50, 1, "BANKFEE", "Abschluss", "", "KESt", cat("TAXES"))
	tx("t6", iban, jan(31), 0, 0, "BANKINFO", "Information", "", "", cat("BANK_INFO"))
	tx("t7", iban, jan(31), -1.00, 1, "NEWTYPE", "Neu", "", "", "{}")
	tx("c1", card, jan(10), -80.00, 1, "CREDITCARD", "Shop", "Shop", "", `{"mcc":"5411"}`)
	tx("c2", card, jan(12), 20.00, 1, "CREDITCARD", "Shop", "Shop", "", `{"mcc":"5411"}`)
	tx("c3", card, jan(20), 60.00, 1, "CREDITCARD", "Zahlung", "", "", `{"mcc":""}`)

	bal := func(acct string, d int64, amt float64) {
		exec(`INSERT INTO daily_balances VALUES (?,?,?)`, acct, d, amt)
	}
	bal(iban, jan(5), 1000.00)
	bal(iban, jan(6), 960.00)
	bal(card, jan(10), 80.00)
	bal(card, jan(12), 60.00)
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func openConn(t *testing.T, path string) silver.Connection {
	t.Helper()
	return silvertest.Open(t, adapter, path)
}

func TestKind(t *testing.T) {
	if got := adapter.Source().Kind; got != "erstebank_at" {
		t.Fatalf("Kind = %q", got)
	}
	registered, err := silver.Get("erstebank_at")
	if err != nil || len(registered.Collectors) != 1 || registered.Collectors[0].Name != "erstebank_at" {
		t.Fatalf("registered = %+v, %v; want one collector named for it", registered, err)
	}
}

func TestEmptyStatus(t *testing.T) {
	silvertest.AssertEmptyStatus(t, adapter, silverSchemaSQL)
}

// The span reaches from the earliest record (the first ledger day) to the
// load, and an idle reload yields nothing.
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
	if st.OldestTransactionAt != silvertest.Day(2026, 1, 5) || st.LatestTransactionAt != silvertest.Day(2026, 1, 31) {
		t.Errorf("tx range = [%d,%d]", st.OldestTransactionAt, st.LatestTransactionAt)
	}
	w := silvertest.WindowSince0(t, conn)
	if !w.Full || w.NewChangeNumber != loadUnix {
		t.Errorf("window = %+v, want the whole history up to the load %d", w, loadUnix)
	}
	idle, err := conn.ChangeWindow(ctx, loadUnix)
	if err != nil {
		t.Fatalf("ChangeWindow idle: %v", err)
	}
	if idle.HasChanges {
		t.Error("idle reload reported changes")
	}
}

// A checking account is cash, a card is a card, and every card figure turns
// from the amount owed into negative cash.
func TestSnapshots(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	conn := openConn(t, path)
	batch := silvertest.Merged(t, conn, silvertest.WindowSince0(t, conn))

	kinds := map[string]string{}
	for _, a := range batch.Accounts {
		kinds[a.AccountExternalID] = string(a.AccountKind) + " " + *a.DisplayName
		if a.BaseCurrency == nil || *a.BaseCurrency != "EUR" {
			t.Errorf("%s BaseCurrency = %v", a.AccountExternalID, a.BaseCurrency)
		}
	}
	if want := map[string]string{iban: "cash Girokonto …1234", card: "card Kreditkarte …5678"}; !reflect.DeepEqual(kinds, want) {
		t.Errorf("accounts = %v, want %v", kinds, want)
	}
	if len(batch.Positions) != 0 || len(batch.Instruments) != 0 {
		t.Error("a bank source must not emit positions or instruments")
	}

	end := func(d int) int64 { return silvertest.Day(2026, 1, d) + silver.DayEndOffset }
	type key struct {
		id   string
		at   int64
		kind canonical.BalanceKind
	}
	got := map[key]string{}
	for _, cb := range batch.CashBalances {
		got[key{cb.AccountExternalID, cb.SnapshotAt, cb.BalanceKind}] = cb.Amount.String() + " " + cb.Currency
	}
	want := map[key]string{
		{iban, loadUnix, canonical.BalanceKindCurrent}: "1500 EUR",
		{card, loadUnix, canonical.BalanceKindCurrent}: "-60 EUR",
		{iban, end(5), canonical.BalanceKindClosing}:   "1000 EUR",
		{iban, end(6), canonical.BalanceKindClosing}:   "960 EUR",
		{card, end(10), canonical.BalanceKindClosing}:  "-80 EUR",
		{card, end(12), canonical.BalanceKindClosing}:  "-60 EUR",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cash balances =\n %v\nwant\n %v", got, want)
	}
}

func TestTransactions(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	conn := openConn(t, path)
	txs := silvertest.Transactions(t, conn, silvertest.WindowSince0(t, conn))

	byID := map[string]canonical.TransactionChange{}
	for _, tx := range txs {
		byID[tx.TransactionExternalID] = tx
	}
	if _, ok := byID["t6"]; ok {
		t.Error("an information row that books no amount must be left out")
	}
	want := map[string]struct {
		kind canonical.TxKind
		net  string
	}{
		"t1": {canonical.TxKindDeposit, "1000"},
		"t2": {canonical.TxKindWithdrawal, "-40"},
		"t3": {canonical.TxKindFee, "-5"},
		"t4": {canonical.TxKindInterest, "2"},
		"t5": {canonical.TxKindTax, "-0.5"},
		"t7": {canonical.TxKindWithdrawal, "-1"},
		"c1": {canonical.TxKindPurchase, "-80"},
		"c2": {canonical.TxKindRefund, "20"},
		"c3": {canonical.TxKindCardPayment, "60"},
	}
	if len(txs) != len(want) {
		t.Fatalf("transactions = %d, want %d", len(txs), len(want))
	}
	for id, exp := range want {
		tx := byID[id]
		if tx.Kind != exp.kind || tx.NetAmount.String() != exp.net || tx.GrossAmount.String() != exp.net {
			t.Errorf("%s = %s %s, want %s %s", id, tx.Kind, tx.NetAmount.String(), exp.kind, exp.net)
		}
		if tx.Currency != "EUR" {
			t.Errorf("%s currency = %q", id, tx.Currency)
		}
	}

	t1 := byID["t1"]
	if deref(t1.Description) != "Gehalt" || deref(t1.Memo) != "Lohn Jänner" ||
		deref(t1.Counterparty) != "ACME GmbH" || deref(t1.ProviderCategory) != "SALARY" {
		t.Errorf("t1 text = %q / %q / %q / %q", deref(t1.Description), deref(t1.Memo),
			deref(t1.Counterparty), deref(t1.ProviderCategory))
	}
	if t2 := byID["t2"]; t2.Memo != nil {
		t.Errorf("t2 memo = %q, want none", *t2.Memo)
	}

	var p map[string]any
	if err := json.Unmarshal(byID["t7"].Payload, &p); err != nil || p["source_kind"] != "NEWTYPE" {
		t.Errorf("unknown type payload = %s, want source_kind kept", byID["t7"].Payload)
	}
	var known map[string]any
	if err := json.Unmarshal(byID["t1"].Payload, &known); err != nil || known["source_kind"] != nil {
		t.Errorf("known type payload = %s, want no source_kind", byID["t1"].Payload)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// TestTxKinds pins the kind rules row by row.
func TestTxKinds(t *testing.T) {
	pos := canonical.NewDecimalFromFloat(10)
	neg := canonical.NewDecimalFromFloat(-10)
	checking := []struct {
		txType, text string
		amt          canonical.Decimal
		want         canonical.TxKind
		known        bool
	}{
		{"POSPAYMENT", "", neg, canonical.TxKindWithdrawal, true},
		{"POSPAYMENT", "", pos, canonical.TxKindDeposit, true},
		{"ATM", "", neg, canonical.TxKindWithdrawal, true},
		{"CASHCOLLECTION", "", neg, canonical.TxKindWithdrawal, true},
		{"STANDINGORDER", "", neg, canonical.TxKindWithdrawal, true},
		{"MANUAL", "", pos, canonical.TxKindDeposit, true},
		{"OTHER", "", pos, canonical.TxKindDeposit, true},
		{"BANKFEE", "Abschluss Kontoführung", neg, canonical.TxKindFee, true},
		{"BANKFEE", "Abschluss", neg, canonical.TxKindFee, true},
		{"BANKFEE", "Sollzinsen", neg, canonical.TxKindInterest, true},
		{"BANKFEE", "Habenzinsen", pos, canonical.TxKindInterest, true},
		{"BANKFEE", "KESt auf Zinsen", neg, canonical.TxKindTax, true},
		{"", "", neg, canonical.TxKindWithdrawal, false},
		{"SOMETHINGNEW", "", pos, canonical.TxKindDeposit, false},
	}
	for _, tc := range checking {
		if got, known := checkingTxKind(tc.txType, tc.text, tc.amt); got != tc.want || known != tc.known {
			t.Errorf("checkingTxKind(%q,%q,%s) = %s,%v; want %s,%v",
				tc.txType, tc.text, tc.amt.String(), got, known, tc.want, tc.known)
		}
	}
	cards := []struct {
		txType   string
		merchant bool
		amt      canonical.Decimal
		want     canonical.TxKind
		known    bool
	}{
		{"CREDITCARD", true, neg, canonical.TxKindPurchase, true},
		{"CREDITCARD", false, neg, canonical.TxKindPurchase, true},
		{"CREDITCARD", true, pos, canonical.TxKindRefund, true},
		{"CREDITCARD", false, pos, canonical.TxKindCardPayment, true},
		{"POSPAYMENT", true, neg, canonical.TxKindPurchase, false},
		{"POSPAYMENT", true, pos, canonical.TxKindCardPayment, false},
		{"", false, pos, canonical.TxKindCardPayment, false},
	}
	for _, tc := range cards {
		if got, known := cardTxKind(tc.txType, tc.merchant, tc.amt); got != tc.want || known != tc.known {
			t.Errorf("cardTxKind(%q,%v,%s) = %s,%v; want %s,%v",
				tc.txType, tc.merchant, tc.amt.String(), got, known, tc.want, tc.known)
		}
	}
}

// TestCurrentBalanceAtEachRunThatListedTheAccount pins a CURRENT balance at
// each run whose roster listed the account, valued at its roster row as of
// that run. Silver content-dedups an unchanged roster row, so a quiet
// account stands at every run that still lists it. A card the later run
// left out stops at its last listing run, so gold can end it
// (wealthdb/docs/DESIGN.md §10.7), and its figure stays negative.
func TestCurrentBalanceAtEachRunThatListedTheAccount(t *testing.T) {
	const left = "SHDW0000000000S2"
	path, db := newFixture(t)
	seed(t, db)
	r0, r1 := loadUnix-7*86400, loadUnix+86400
	silvertest.Exec(t, db, `INSERT INTO dump_runs VALUES (?,1,'/run'), (?,1,'/run')`, r0, r1)
	account(t, db, r0, left, "credit_card", "Kreditkarte", "…0002", 25.00)
	silvertest.Exec(t, db, `INSERT INTO run_accounts VALUES (?,?), (?,?), (?,?)`,
		r0, left, r1, iban, r1, card)
	conn := openConn(t, path)
	w := silvertest.WindowSince0(t, conn)
	if !w.Full || w.NewChangeNumber != r1 {
		t.Errorf("window = %+v, want the whole history up to the latest run %d", w, r1)
	}
	want := map[string]map[int64]string{
		iban: {loadUnix: "1500 EUR", r1: "1500 EUR"},
		card: {loadUnix: "-60 EUR", r1: "-60 EUR"},
		left: {r0: "-25 EUR"},
	}
	if got := silvertest.CurrentsByInstant(silvertest.Merged(t, conn, w)); !reflect.DeepEqual(got, want) {
		t.Errorf("CURRENT balances =\n %v\nwant\n %v", got, want)
	}
}

// A silver without run_accounts states no CURRENT balance and still emits
// its accounts and closing balances.
func TestSnapshotsWithoutRunAccountsStateNoCurrentBalance(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	silvertest.Exec(t, db, `DROP TABLE run_accounts`)
	conn := openConn(t, path)
	batch := silvertest.Merged(t, conn, silvertest.WindowSince0(t, conn))
	if len(batch.Accounts) != 2 {
		t.Errorf("accounts = %d, want 2", len(batch.Accounts))
	}
	for _, cb := range batch.CashBalances {
		if cb.BalanceKind == canonical.BalanceKindCurrent {
			t.Errorf("CURRENT balance %+v without a listing table", cb)
		}
	}
	if len(batch.CashBalances) != 4 {
		t.Errorf("closing balances = %d, want 4", len(batch.CashBalances))
	}
}
