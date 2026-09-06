package ubs

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// newCardFixture builds an in-memory ubs-web silver holding just the
// card tables the projection reads (collector migration 0007). Values
// are invented throughout.
func newCardFixture(t *testing.T) *webReader {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/ubs-web-cards.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	const schema = `
CREATE TABLE dump_runs (snapshot_at INTEGER PRIMARY KEY, run_dir TEXT);
CREATE TABLE card_accounts (
    snapshot_at INTEGER, account_external_id TEXT, account_number TEXT,
    currency_iso TEXT, balance REAL, available REAL, credit_limit REAL,
    reserved_amount REAL, reserved_count INTEGER, product_name TEXT,
    card_type TEXT, account_status TEXT, structure_type TEXT, payload TEXT);
CREATE TABLE card_transactions (
    transaction_external_id TEXT PRIMARY KEY, account_external_id TEXT,
    snapshot_at INTEGER, transaction_date INTEGER, value_date INTEGER,
    amount REAL, currency_iso TEXT, original_amount REAL,
    original_currency_iso TEXT, exchange_rate REAL, merchant TEXT,
    merchant_category TEXT, merchant_group_code TEXT, card_number TEXT,
    settled_in_invoice INTEGER, payload TEXT);
CREATE TABLE card_invoices (
    account_external_id TEXT, period_end INTEGER, period_start INTEGER,
    invoice_external_id TEXT, snapshot_at INTEGER, invoicing_date INTEGER,
    debiting_date INTEGER, due_on INTEGER, due_amount REAL,
    minimal_due_amount REAL, currency_iso TEXT, balance_forward REAL,
    total_debit REAL, total_credit REAL, reconciles INTEGER,
    statement_type TEXT, payment_method TEXT, invoice_status TEXT,
    transactions_covered INTEGER, payload TEXT);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return &webReader{db: db}
}

func fullWindow() canonical.Window {
	return canonical.Window{HasChanges: true, Start: 0, End: 4102444800}
}

// --------------------------------------------------------------------
// The account and its balance
// --------------------------------------------------------------------

func TestCardAccountProjectsAsACardKind(t *testing.T) {
	r := newCardFixture(t)
	if _, err := r.db.Exec(`
INSERT INTO card_accounts VALUES
 (1000, 'ACCT-1', '0000 0000', 'CHF', -250.0, 4750.0, 5000.0, NULL, NULL,
  'Example Card', 'EXCA', 'ACTIVE', 'COMPLEX_TLA', '{}')`); err != nil {
		t.Fatal(err)
	}
	byTime := map[int64]*canonical.SnapshotBatch{1000: {}}
	if err := r.appendWebCards(context.Background(), fullWindow(), byTime); err != nil {
		t.Fatal(err)
	}
	accs := byTime[1000].Accounts
	if len(accs) != 1 || accs[0].AccountKind != canonical.AccountKindCard {
		t.Fatalf("want one card account, got %+v", accs)
	}
	if accs[0].DisplayName == nil || *accs[0].DisplayName != "Example Card" {
		t.Errorf("display name = %v, want the product line", accs[0].DisplayName)
	}
	bals := byTime[1000].CashBalances
	if len(bals) != 1 {
		t.Fatalf("want one balance, got %d", len(bals))
	}
	// UBS already signs a card negative-when-owed; nothing is flipped.
	if got := bals[0].Amount.String(); got != "-250" {
		t.Errorf("balance = %s, want -250 (the source sign, unnegated)", got)
	}
	if bals[0].BalanceKind != canonical.BalanceKindCurrent {
		t.Errorf("balance kind = %q, want current", bals[0].BalanceKind)
	}
}

func TestReservedIsNotAddedToTheBalance(t *testing.T) {
	// The roster balance ALREADY includes authorised-but-unposted spend:
	// an account's balance equals the sum of its cards'
	// balanceIncludingReserved. Adding reserved_amount back would count
	// that spend twice.
	r := newCardFixture(t)
	if _, err := r.db.Exec(`
INSERT INTO card_accounts VALUES
 (1000, 'ACCT-1', NULL, 'CHF', -250.0, NULL, NULL, -30.0, 2, NULL, NULL,
  NULL, NULL, '{}')`); err != nil {
		t.Fatal(err)
	}
	byTime := map[int64]*canonical.SnapshotBatch{1000: {}}
	if err := r.appendWebCards(context.Background(), fullWindow(), byTime); err != nil {
		t.Fatal(err)
	}
	if got := byTime[1000].CashBalances[0].Amount.String(); got != "-250" {
		t.Errorf("balance = %s, want -250 (the roster figure as reported; "+
			"reserved is already inside it)", got)
	}
}

func TestACardWithNoBalanceStillProjectsTheAccount(t *testing.T) {
	r := newCardFixture(t)
	if _, err := r.db.Exec(`
INSERT INTO card_accounts VALUES
 (1000, 'ACCT-1', '0000 1111', 'CHF', NULL, NULL, NULL, NULL, NULL, NULL,
  NULL, NULL, NULL, '{}')`); err != nil {
		t.Fatal(err)
	}
	byTime := map[int64]*canonical.SnapshotBatch{1000: {}}
	if err := r.appendWebCards(context.Background(), fullWindow(), byTime); err != nil {
		t.Fatal(err)
	}
	if len(byTime[1000].Accounts) != 1 || len(byTime[1000].CashBalances) != 0 {
		t.Errorf("want the account without a balance, got %d/%d",
			len(byTime[1000].Accounts), len(byTime[1000].CashBalances))
	}
	// The printed account number stands in when there is no product line.
	if n := byTime[1000].Accounts[0].DisplayName; n == nil || *n != "0000 1111" {
		t.Errorf("display name = %v, want the account number fallback", n)
	}
}

func TestASilverWithoutCardTablesProjectsNothing(t *testing.T) {
	// A ubs-web DB written before collector migration 0007.
	r := newWebFixture(t)
	byTime := map[int64]*canonical.SnapshotBatch{1000: {}}
	if err := r.appendWebCards(context.Background(), fullWindow(), byTime); err != nil {
		t.Fatalf("a pre-0007 silver must load, not fail: %v", err)
	}
	if _, err := r.cardTransactions(context.Background(), fullWindow()); err != nil {
		t.Fatalf("a pre-0007 silver must load, not fail: %v", err)
	}
}

// --------------------------------------------------------------------
// Statement balances
// --------------------------------------------------------------------

func TestStatementClosingBalancesLandAtThePeriodEnd(t *testing.T) {
	r := newCardFixture(t)
	if _, err := r.db.Exec(`
INSERT INTO card_invoices VALUES
 ('ACCT-1', 5000, 4000, 'INV-1', 1000, 5000, 5500, 5600, -250.0, -25.0,
  'CHF', -100.0, -200.0, 50.0, 1, 'INVOICE', 'LSV', 'OPEN', 1, '{}')`); err != nil {
		t.Fatal(err)
	}
	byTime := map[int64]*canonical.SnapshotBatch{}
	if err := r.appendCardStatementBalances(context.Background(), fullWindow(), byTime); err != nil {
		t.Fatal(err)
	}
	// The balance belongs at the billing date, not at a fetch time — so
	// the projection creates the batch the period needs.
	batch, ok := byTime[5000]
	if !ok {
		t.Fatal("no batch at the period end")
	}
	if len(batch.CashBalances) != 1 {
		t.Fatalf("want one closing balance, got %d", len(batch.CashBalances))
	}
	b := batch.CashBalances[0]
	if b.BalanceKind != canonical.BalanceKindClosing || b.Amount.String() != "-250" {
		t.Errorf("got %s %s, want closing -250", b.BalanceKind, b.Amount.String())
	}
}

func TestAPeriodThatDoesNotReconcileEmitsNoBalance(t *testing.T) {
	// A wrong balance is worse than a missing one: the carry-forward
	// rule fills a gap, but nothing corrects a figure that is present.
	r := newCardFixture(t)
	if _, err := r.db.Exec(`
INSERT INTO card_invoices VALUES
 ('ACCT-1', 5000, 4000, 'INV-1', 1000, 5000, 5500, 5600, -250.0, -25.0,
  'CHF', -100.0, -200.0, 999.0, 0, 'INVOICE', 'LSV', 'OPEN', 1, '{}')`); err != nil {
		t.Fatal(err)
	}
	byTime := map[int64]*canonical.SnapshotBatch{}
	if err := r.appendCardStatementBalances(context.Background(), fullWindow(), byTime); err != nil {
		t.Fatal(err)
	}
	if len(byTime) != 0 {
		t.Errorf("a period that fails its own identity must emit nothing, got %+v", byTime)
	}
}

// --------------------------------------------------------------------
// The ledger
// --------------------------------------------------------------------

func insertCardTxn(t *testing.T, r *webReader, id string, valueDate int64,
	amount float64, merchant, category string) {
	t.Helper()
	if _, err := r.db.Exec(`
INSERT INTO card_transactions VALUES
 (?, 'ACCT-1', 1000, ?, ?, ?, 'CHF', ?, 'CHF', NULL, ?, ?, '0000', NULL, 0, '{}')`,
		id, valueDate, valueDate, amount, amount, merchant, category); err != nil {
		t.Fatal(err)
	}
}

func TestCardLedgerKindsFollowSignAndDescriptor(t *testing.T) {
	r := newCardFixture(t)
	insertCardTxn(t, r, "T-SPEND", 2000, -42.5, "EXAMPLE SHOP EXAMPLETOWN CHE", "Grocery stores")
	insertCardTxn(t, r, "T-REFUND", 2100, 12.0, "EXAMPLE SHOP EXAMPLETOWN CHE", "Grocery stores")
	insertCardTxn(t, r, "T-BILL", 2200, 250.0, "DIRECT DEBIT", "")
	insertCardTxn(t, r, "T-BILL2", 2300, 100.0, "TRANSFER FROM ACCOUNT", "")

	batch, err := r.cardTransactions(context.Background(), fullWindow())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]canonical.TxKind{}
	for _, tx := range batch.Transactions {
		got[tx.TransactionExternalID] = tx.Kind
	}
	want := map[string]canonical.TxKind{
		"T-SPEND":  canonical.TxKindPurchase,
		"T-REFUND": canonical.TxKindRefund,
		"T-BILL":   canonical.TxKindCardPayment,
		"T-BILL2":  canonical.TxKindCardPayment,
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s kind = %q, want %q", id, got[id], w)
		}
	}
}

func TestAMerchantThatMerelyContainsASettlementPhraseIsNotOne(t *testing.T) {
	// The descriptors match the whole descriptor, so a shop whose name
	// carries the words is still a refund when it credits the card.
	r := newCardFixture(t)
	insertCardTxn(t, r, "T-1", 2000, 20.0, "DIRECT DEBIT SUPPLIES AG ZURICH", "Retail business")
	batch, err := r.cardTransactions(context.Background(), fullWindow())
	if err != nil {
		t.Fatal(err)
	}
	if k := batch.Transactions[0].Kind; k != canonical.TxKindRefund {
		t.Errorf("kind = %q, want refund", k)
	}
}

func TestCardAmountsAreOrientedByKind(t *testing.T) {
	r := newCardFixture(t)
	insertCardTxn(t, r, "T-SPEND", 2000, -42.5, "EXAMPLE SHOP", "Grocery stores")
	insertCardTxn(t, r, "T-BILL", 2200, 250.0, "DIRECT DEBIT", "")
	batch, err := r.cardTransactions(context.Background(), fullWindow())
	if err != nil {
		t.Fatal(err)
	}
	for _, tx := range batch.Transactions {
		switch tx.TransactionExternalID {
		case "T-SPEND":
			if tx.NetAmount.String() != "-42.5" {
				t.Errorf("purchase = %s, want negative", tx.NetAmount.String())
			}
		case "T-BILL":
			if tx.NetAmount.String() != "250" {
				t.Errorf("card payment = %s, want positive", tx.NetAmount.String())
			}
		}
	}
}

func TestTheMerchantIsTheDescriptorAndTheCategoryIsTheMCC(t *testing.T) {
	// The API names these the other way round; the projection is where
	// that is corrected, and gold's merchant signature depends on it.
	r := newCardFixture(t)
	insertCardTxn(t, r, "T-1", 2000, -8.0, "EXAMPLE BAKERY EXAMPLETOWN CHE", "Bakeries")
	batch, err := r.cardTransactions(context.Background(), fullWindow())
	if err != nil {
		t.Fatal(err)
	}
	tx := batch.Transactions[0]
	if tx.Counterparty == nil || *tx.Counterparty != "EXAMPLE BAKERY EXAMPLETOWN CHE" {
		t.Errorf("counterparty = %v, want the terminal descriptor", tx.Counterparty)
	}
	if tx.ProviderCategory == nil || *tx.ProviderCategory != "Bakeries" {
		t.Errorf("provider category = %v, want the MCC description", tx.ProviderCategory)
	}
}

// --------------------------------------------------------------------
// The change window
// --------------------------------------------------------------------

func TestChangeWindowReachesCardDates(t *testing.T) {
	// A period end is a billing date and need not fall on any dump time.
	// If the window misses it, gold re-inserts without deleting and the
	// balance duplicates on every load.
	r := newCardFixture(t)
	if _, err := r.db.Exec(`
INSERT INTO card_transactions VALUES
 ('T-1', 'ACCT-1', 1000, 2000, 2000, -1.0, 'CHF', -1.0, 'CHF', NULL,
  'X', 'Bakeries', '0000', NULL, 0, '{}');
INSERT INTO card_invoices VALUES
 ('ACCT-1', 9000, 8000, 'INV-1', 1000, 9000, 9500, 9600, -1.0, NULL,
  'CHF', NULL, NULL, NULL, NULL, 'INVOICE', 'LSV', 'OPEN', 0, '{}')`); err != nil {
		t.Fatal(err)
	}
	lo, hi, err := r.cardRange(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if lo != 2000 || hi != 9000 {
		t.Errorf("card range = (%d, %d), want (2000, 9000)", lo, hi)
	}
}

func TestCardRangeIsEmptyWithoutCardTables(t *testing.T) {
	lo, hi, err := newWebFixture(t).cardRange(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if lo != -1 || hi != -1 {
		t.Errorf("card range = (%d, %d), want (-1, -1)", lo, hi)
	}
}

func TestSettlementDescriptorFolding(t *testing.T) {
	for _, tc := range []struct {
		descriptor string
		want       bool
	}{
		{"DIRECT DEBIT", true},
		{"direct debit", true},
		{"  DIRECT   DEBIT  ", true},
		{"DIRECT DEBIT (SWIFT)", true},
		{"TRANSFER FROM ACCOUNT", true},
		// Whole-descriptor equality, never a substring: a merchant that
		// happens to carry the words is a merchant.
		{"DIRECT DEBIT SUPPLIES AG", false},
		{"EXAMPLE TRANSFER FROM ACCOUNT SERVICES", false},
		{"", false},
	} {
		if got := isCardSettlement(tc.descriptor); got != tc.want {
			t.Errorf("isCardSettlement(%q) = %v, want %v", tc.descriptor, got, tc.want)
		}
	}
}

func TestABalanceWithNoCurrencyIsSkipped(t *testing.T) {
	// Gold keys cash on (account, currency, kind), so a currencyless
	// figure has no unit rather than a zero-currency one. The account
	// still projects; only the balance is withheld.
	r := newCardFixture(t)
	if _, err := r.db.Exec(`
INSERT INTO card_accounts VALUES
 (1000, 'ACCT-1', '0000', NULL, -250.0, NULL, NULL, NULL, NULL, NULL, NULL,
  NULL, NULL, '{}');
INSERT INTO card_invoices VALUES
 ('ACCT-1', 5000, 4000, 'INV-1', 1000, 5000, 5500, 5600, -250.0, NULL,
  NULL, -100.0, -200.0, 50.0, 1, 'INVOICE', 'LSV', 'OPEN', 1, '{}')`); err != nil {
		t.Fatal(err)
	}
	byTime := map[int64]*canonical.SnapshotBatch{1000: {}}
	if err := r.appendWebCards(context.Background(), fullWindow(), byTime); err != nil {
		t.Fatal(err)
	}
	if err := r.appendCardStatementBalances(context.Background(), fullWindow(), byTime); err != nil {
		t.Fatal(err)
	}
	if len(byTime[1000].Accounts) != 1 {
		t.Error("the account itself must still project")
	}
	for at, batch := range byTime {
		if len(batch.CashBalances) != 0 {
			t.Errorf("batch %d emitted a balance with no currency: %+v",
				at, batch.CashBalances)
		}
	}
}

func TestCurrencyIsNormalisedToUpperCase(t *testing.T) {
	r := newCardFixture(t)
	if _, err := r.db.Exec(`
INSERT INTO card_accounts VALUES
 (1000, 'ACCT-1', NULL, ' chf ', -10.0, NULL, NULL, NULL, NULL, NULL, NULL,
  NULL, NULL, '{}')`); err != nil {
		t.Fatal(err)
	}
	byTime := map[int64]*canonical.SnapshotBatch{1000: {}}
	if err := r.appendWebCards(context.Background(), fullWindow(), byTime); err != nil {
		t.Fatal(err)
	}
	if got := byTime[1000].CashBalances[0].Currency; got != "CHF" {
		t.Errorf("currency = %q, want CHF", got)
	}
}
