package plaid

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"

	_ "modernc.org/sqlite"
)

//go:embed testdata/silver_schema.sql
var silverSchemaSQL string

// day is a calendar day of an invented month at UTC midnight; a run starts
// some hours into its day.
func day(d int) int64 {
	return time.Date(2031, 1, d, 0, 0, 0, 0, time.UTC).Unix()
}

func runAt(d int) int64 { return day(d) + 7*3600 }

func newFixture(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := t.TempDir() + "/plaid.db"
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

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
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

func null(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// noInvestments are the statuses a run records for an Item linked without
// investments: download marks the products it was not linked with.
var noInvestments = map[string]string{"holdings": "not_linked",
	"investment_transactions": "not_linked"}

// run records one loaded run at `at`, as load.py records it: every product
// read in full unless `statuses` says otherwise, and a ledger's window,
// starting at `since`, only where the ledger was read.
func run(t *testing.T, db *sql.DB, at, since int64, statuses map[string]string) {
	t.Helper()
	exec(t, db, `INSERT INTO dump_runs (snapshot_at, silver_schema_version, run_dir, loaded_at)
	             VALUES (?, 1, 'run', ?)`, at, at)
	for _, p := range []string{"accounts", "holdings", "investment_transactions",
		"transactions", "liabilities"} {
		status := statuses[p]
		if status == "" {
			status = "fetched"
		}
		var start, end any
		if strings.HasSuffix(p, "transactions") && (status == "fetched" || status == "partial") {
			start, end = since, at
		}
		exec(t, db, `INSERT INTO run_products (run_at, product, status, window_start, window_end)
		             VALUES (?, ?, ?, ?, ?)`, at, p, status, start, end)
	}
}

func acct(t *testing.T, db *sql.DB, at int64, id, typ, subtype, balance string) {
	t.Helper()
	exec(t, db, `INSERT INTO accounts (snapshot_at, account_id, name, mask, type, subtype,
	             currency, balance_current, payload) VALUES (?, ?, ?, '0000', ?, ?, 'USD', ?, ?)`,
		at, id, "Synthetic "+subtype, typ, subtype, null(balance), `{"account_id":"`+id+`"}`)
}

func sec(t *testing.T, db *sql.DB, id, name, ticker, typ string) {
	t.Helper()
	exec(t, db, `INSERT OR IGNORE INTO securities (security_id, name, ticker_symbol, type, currency,
	             first_seen_at, last_seen_at, payload) VALUES (?, ?, ?, ?, 'USD', 0, 0, '{}')`,
		id, name, null(ticker), typ)
}

func hold(t *testing.T, db *sql.DB, at int64, accountID, securityID string, seq int,
	quantity, value, cost string) {
	t.Helper()
	exec(t, db, `INSERT INTO holdings (snapshot_at, account_id, security_id, seq, quantity,
	             institution_value, cost_basis, currency, tax_lots, payload)
	             VALUES (?, ?, ?, ?, ?, ?, ?, 'USD', '[]', ?)`,
		at, accountID, securityID, seq, null(quantity), null(value), null(cost),
		`{"security_id":"`+securityID+`"}`)
}

// bankRow is one row of the bank and card ledger; the zero value of an
// optional field is NULL, except the currency, which is USD unless
// noCurrency says the row states none. `legacy` is Plaid's older category
// id, kept in the payload.
type bankRow struct {
	id, account, amount, name, original, merchant, primary, detailed, check string
	legacy                                                                  string
	pending, noCurrency                                                     bool
	at                                                                      int64
}

func bank(t *testing.T, db *sql.DB, r bankRow) {
	t.Helper()
	pending := 0
	if r.pending {
		pending = 1
	}
	currency := any("USD")
	if r.noCurrency {
		currency = nil
	}
	legacy := "null"
	if r.legacy != "" {
		legacy = `"` + r.legacy + `"`
	}
	exec(t, db, `INSERT INTO transactions (transaction_id, account_id, posted_at, amount,
	             currency, name, merchant_name, original_description, pending,
	             category_primary, category_detailed, check_number, run_at, payload)
	             VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		r.id, r.account, r.at, r.amount, currency, null(r.name), null(r.merchant), null(r.original),
		pending, null(r.primary), null(r.detailed), null(r.check),
		`{"transaction_id":"`+r.id+`","category_id":`+legacy+
			`,"pending":`+map[bool]string{true: "true", false: "false"}[r.pending]+`}`)
}

// invRow is one row of the investment ledger, as load.py stores it: the
// amount in the fleet's sign, the quantity in Plaid's own. `traded` is
// Plaid's transaction time, NULL when zero. `name` is the institution's
// text, "<type> synthetic" when empty.
type invRow struct {
	id, account, security, typ, subtype, amount, quantity, price, fees, cancels string
	name                                                                        string
	at, traded                                                                  int64
}

func inv(t *testing.T, db *sql.DB, r invRow) {
	t.Helper()
	var traded any
	if r.traded != 0 {
		traded = r.traded
	}
	name := r.name
	if name == "" {
		name = r.typ + " synthetic"
	}
	exec(t, db, `INSERT INTO investment_transactions (investment_transaction_id, account_id,
	             security_id, posted_at, transaction_at, name, type, subtype, amount, quantity,
	             price, fees, currency, cancel_transaction_id, run_at, payload)
	             VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'USD', ?, 0, '{}')`,
		r.id, r.account, null(r.security), r.at, traded, name, r.typ, r.subtype,
		r.amount, null(r.quantity), null(r.price), null(r.fees), null(r.cancels))
}

func liability(t *testing.T, db *sql.DB, at int64, accountID, balance string, issued int64) {
	t.Helper()
	exec(t, db, `INSERT INTO liabilities (snapshot_at, account_id, kind, last_statement_balance,
	             last_statement_issue_date, payload) VALUES (?, ?, 'credit', ?, ?, '{}')`,
		at, accountID, balance, issued)
}

func status(t *testing.T, conn silver.Connection) canonical.Status {
	t.Helper()
	s, err := conn.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	return s
}

func window(t *testing.T, conn silver.Connection) canonical.Window {
	t.Helper()
	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatalf("ChangeWindow: %v", err)
	}
	return w
}

func collectSnapshots(t *testing.T, conn silver.Connection) []canonical.SnapshotBatch {
	t.Helper()
	stream, err := conn.Snapshots(context.Background(), window(t, conn))
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	defer stream.Close()
	var out []canonical.SnapshotBatch
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Snapshots Next: %v", err)
		}
		if len(b.Accounts)+len(b.Instruments)+len(b.Positions)+len(b.CashBalances) > 0 {
			out = append(out, b)
		}
		if !more {
			return out
		}
	}
}

func collectTransactions(t *testing.T, conn silver.Connection) map[string]canonical.TransactionChange {
	t.Helper()
	stream, err := conn.Transactions(context.Background(), window(t, conn))
	if err != nil {
		t.Fatalf("Transactions: %v", err)
	}
	defer stream.Close()
	out := map[string]canonical.TransactionChange{}
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Transactions Next: %v", err)
		}
		for _, tx := range b.Transactions {
			if _, dup := out[tx.TransactionExternalID]; dup {
				t.Fatalf("transaction %s emitted twice", tx.TransactionExternalID)
			}
			out[tx.TransactionExternalID] = tx
		}
		if !more {
			return out
		}
	}
}

func dec(t *testing.T, s string) canonical.Decimal {
	t.Helper()
	d, err := canonical.NewDecimalFromString(s)
	if err != nil {
		t.Fatalf("decimal %q: %v", s, err)
	}
	return d
}

func assertAmount(t *testing.T, what string, got *canonical.Decimal, want string) {
	t.Helper()
	if want == "" {
		if got != nil {
			t.Errorf("%s = %s, want none", what, got)
		}
		return
	}
	if got == nil || !got.Equal(dec(t, want)) {
		t.Errorf("%s = %v, want %s", what, got, want)
	}
}

func cashAt(batches []canonical.SnapshotBatch, at int64, account string,
	kind canonical.BalanceKind) []canonical.CashBalanceChange {
	var out []canonical.CashBalanceChange
	for _, b := range batches {
		for _, cb := range b.CashBalances {
			if cb.SnapshotAt == at && cb.AccountExternalID == account && cb.BalanceKind == kind {
				out = append(out, cb)
			}
		}
	}
	return out
}

func positionsAt(batches []canonical.SnapshotBatch, at int64) []canonical.PositionChange {
	var out []canonical.PositionChange
	for _, b := range batches {
		for _, p := range b.Positions {
			if p.SnapshotAt == at {
				out = append(out, p)
			}
		}
	}
	return out
}

// accountChanges is every account change the batches carry for `id`.
func accountChanges(batches []canonical.SnapshotBatch, id string) []canonical.AccountChange {
	var out []canonical.AccountChange
	for _, b := range batches {
		for _, a := range b.Accounts {
			if a.AccountExternalID == id {
				out = append(out, a)
			}
		}
	}
	return out
}

// newestCashInstant is the latest instant any cash balance is stamped at,
// which is what gold's current view reads a source's cash from.
func newestCashInstant(batches []canonical.SnapshotBatch) int64 {
	var newest int64
	for _, b := range batches {
		for _, cb := range b.CashBalances {
			newest = max(newest, cb.SnapshotAt)
		}
	}
	return newest
}

// ---- the maps ----------------------------------------------------------------

func TestAccountKindFor(t *testing.T) {
	cases := []struct {
		typ, subtype string
		kind         canonical.AccountKind
		projected    bool
	}{
		{"depository", "checking", canonical.AccountKindCash, true},
		{"depository", "hsa", canonical.AccountKindCash, true},
		{"credit", "credit card", canonical.AccountKindCard, true},
		{"loan", "mortgage", canonical.AccountKindMortgage, true},
		{"loan", "home equity", canonical.AccountKindMortgage, true},
		{"loan", "home equity loan", canonical.AccountKindMortgage, true},
		{"loan", "construction", canonical.AccountKindMortgage, true},
		{"investment", "ira", canonical.AccountKindBrokerage, true},
		{"investment", "crypto exchange", canonical.AccountKindCrypto, true},
		{"investment", "non-custodial wallet", canonical.AccountKindCrypto, true},
		{"Investment", "401K", canonical.AccountKindBrokerage, true},
		{"loan", "student", "", false},
		{"loan", "auto", "", false},
		{"loan", "line of credit", "", false},
		{"other", "other", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		kind, projected := accountKindFor(c.typ, c.subtype)
		if kind != c.kind || projected != c.projected {
			t.Errorf("accountKindFor(%q, %q) = (%q, %v), want (%q, %v)",
				c.typ, c.subtype, kind, projected, c.kind, c.projected)
		}
		if projected && !kind.Valid() {
			t.Errorf("accountKindFor(%q, %q): %q is not a gold account kind", c.typ, c.subtype, kind)
		}
	}
}

func TestWrapperFor(t *testing.T) {
	cases := []struct {
		kind    canonical.AccountKind
		subtype string
		want    canonical.TaxWrapper
	}{
		{canonical.AccountKindBrokerage, "ira", canonical.TaxWrapperTraditionalIRA},
		{canonical.AccountKindBrokerage, "roth", canonical.TaxWrapperRothIRA},
		{canonical.AccountKindBrokerage, "403B", canonical.TaxWrapper403b},
		{canonical.AccountKindBrokerage, "roth 403B", canonical.TaxWrapper403b},
		{canonical.AccountKindBrokerage, "roth 457b", canonical.TaxWrapper457b},
		{canonical.AccountKindBrokerage, "roth 401k", canonical.TaxWrapper401k},
		{canonical.AccountKindBrokerage, "roth profit sharing plan", canonical.TaxWrapper401k},
		{canonical.AccountKindBrokerage, "roth thrift savings plan", canonical.TaxWrapper401k},
		{canonical.AccountKindBrokerage, "brokerage", canonical.TaxWrapperTaxablePersonal},
		{canonical.AccountKindBrokerage, "utma", canonical.TaxWrapperCustodialUTMA},
		{canonical.AccountKindBrokerage, "pension", ""},
		// Revocable or irrevocable: Plaid does not say which.
		{canonical.AccountKindBrokerage, "trust", ""},
		{canonical.AccountKindCash, "hsa", canonical.TaxWrapperHSA},
		{canonical.AccountKindCash, "checking", canonical.TaxWrapperTaxablePersonal},
		{canonical.AccountKindCard, "credit card", canonical.TaxWrapperTaxablePersonal},
	}
	for _, c := range cases {
		got := wrapperFor(c.kind, c.subtype)
		switch {
		case c.want == "" && got != nil:
			t.Errorf("wrapperFor(%q, %q) = %q, want none", c.kind, c.subtype, *got)
		case c.want != "" && (got == nil || *got != c.want):
			t.Errorf("wrapperFor(%q, %q) = %v, want %q", c.kind, c.subtype, got, c.want)
		}
	}
	for subtype, w := range investmentWrappers {
		if !w.Valid() {
			t.Errorf("investmentWrappers[%q] = %q is not a gold wrapper", subtype, w)
		}
		if subtype != norm(subtype) {
			t.Errorf("investmentWrappers key %q is not folded", subtype)
		}
	}
}

func TestPairForIsAlwaysAnAdmittedPair(t *testing.T) {
	cases := []struct {
		typ, ticker, cfi, name string
		ac                     canonical.AssetClass
		veh                    canonical.Vehicle
		known                  bool
	}{
		{"equity", "", "", "PLACEHOLDER CORP", canonical.AssetClassPublicEquity, canonical.VehicleStock, true},
		{"etf", "", "", "PLACEHOLDER TOTAL MARKET ETF", canonical.AssetClassPublicEquity, canonical.VehicleETF, true},
		{"etf", "", "", "PLACEHOLDER TREASURY BOND ETF", canonical.AssetClassFixedIncome, canonical.VehicleETF, true},
		{"mutual fund", "", "", "PLACEHOLDER MONEY MARKET FUND", canonical.AssetClassCash, canonical.VehicleFund, true},
		{"mutual fund", "", "", "PLACEHOLDER GROWTH FUND", canonical.AssetClassPublicEquity, canonical.VehicleFund, true},
		{"cash", "EXMXX", "", "PLACEHOLDER SWEEP FUND", canonical.AssetClassCash, canonical.VehicleFund, true},
		{"fixed income", "", "", "PLACEHOLDER NOTE", canonical.AssetClassFixedIncome, canonical.VehicleBond, true},
		{"derivative", "", "", "PLACEHOLDER CALL", canonical.AssetClassPublicEquity, canonical.VehicleOption, true},
		{"cryptocurrency", "", "", "PLACEHOLDER TOKEN", canonical.AssetClassCrypto, canonical.VehiclePhysical, true},
		{"loan", "", "", "PLACEHOLDER LOAN", canonical.AssetClassPrivateDebt, canonical.VehicleLoan, true},
		// Plaid's own type wins over the CFI code.
		{"equity", "", "CEXXXX", "PLACEHOLDER BOND CORP", canonical.AssetClassPublicEquity, canonical.VehicleStock, true},
		// A type Plaid leaves as `other` falls back on a collective
		// investment vehicle's CFI code.
		{"other", "", "CEXXXX", "PLACEHOLDER TREASURY BOND ETF", canonical.AssetClassFixedIncome, canonical.VehicleETF, false},
		{"other", "", "cexxxx", "PLACEHOLDER TOTAL MARKET ETF", canonical.AssetClassPublicEquity, canonical.VehicleETF, false},
		{"other", "", "CIXXXX", "PLACEHOLDER BOND FUND", canonical.AssetClassFixedIncome, canonical.VehicleFund, false},
		{"", "", "CIXXXX", "PLACEHOLDER MONEY MARKET FUND", canonical.AssetClassCash, canonical.VehicleFund, false},
		{"other", "", "ESXXXX", "PLACEHOLDER BOND CORP", canonical.AssetClassOther, canonical.VehicleOther, false},
		{"other", "", "", "PLACEHOLDER THING", canonical.AssetClassOther, canonical.VehicleOther, false},
		{"", "", "", "PLACEHOLDER THING", canonical.AssetClassOther, canonical.VehicleOther, false},
		// Cash with no ticker reaches here only priced: Plaid's type is
		// wrong, and the security falls back as `other` does.
		{"cash", "", "", "PLACEHOLDER NOTE 2031", canonical.AssetClassOther, canonical.VehicleOther, false},
		{"cash", "", "CIXXXX", "PLACEHOLDER BOND FUND", canonical.AssetClassFixedIncome, canonical.VehicleFund, false},
	}
	for _, c := range cases {
		ac, veh, known := pairFor(security{typ: c.typ, ticker: c.ticker, cfi: c.cfi, name: c.name})
		if ac != c.ac || veh != c.veh || known != c.known {
			t.Errorf("pairFor(%q, %q, %q) = (%q, %q, %v), want (%q, %q, %v)",
				c.typ, c.cfi, c.name, ac, veh, known, c.ac, c.veh, c.known)
		}
		if !canonical.ValidTaxonomyPair(ac, veh) {
			t.Errorf("pairFor(%q): (%q, %q) is not an admitted pair", c.typ, ac, veh)
		}
	}
}

func TestIsCashAndInstrumentKey(t *testing.T) {
	cases := []struct {
		s    security
		cash bool
		key  string
	}{
		{security{id: "s1", typ: "cash"}, true, "plaid:s1"},
		{security{id: "s2", typ: "cash", ticker: "CUR:USD"}, true, "CUR:USD"},
		{security{id: "s3", typ: "cash", ticker: "USD"}, true, "USD"},
		{security{id: "s4", typ: "cash", ticker: "EXMXX"}, false, "EXMXX"},
		// A price other than one gives away a security typed as cash; a
		// currency stays cash at any price.
		{security{id: "s8", typ: "cash", priced: true}, false, "plaid:s8"},
		{security{id: "s9", typ: "cash", ticker: "CUR:EUR", priced: true}, true, "CUR:EUR"},
		{security{id: "s5", typ: "equity", ticker: "EXA", isin: "XX0000000000"}, false, "XX0000000000"},
		{security{id: "s6", typ: "equity", ticker: "EXA", cusip: "000000000", isin: "XX0000000000"}, false, "000000000"},
		{security{id: "s7", typ: "mutual fund"}, false, "plaid:s7"},
	}
	for _, c := range cases {
		if got := isCash(c.s, "USD"); got != c.cash {
			t.Errorf("isCash(%+v) = %v, want %v", c.s, got, c.cash)
		}
		if got := instrumentKey(c.s); got != c.key {
			t.Errorf("instrumentKey(%+v) = %q, want %q", c.s, got, c.key)
		}
	}
}

func TestBankTxKind(t *testing.T) {
	cash, card := canonical.AccountKindCash, canonical.AccountKindCard
	cases := []struct {
		kind                      canonical.AccountKind
		amount                    string
		primary, detailed, legacy string
		want                      canonical.TxKind
	}{
		{cash, "0.12", "INCOME", "INCOME_INTEREST_EARNED", "", canonical.TxKindInterest},
		{cash, "-5", "BANK_FEES", "BANK_FEES_OVERDRAFT_FEES", "", canonical.TxKindFee},
		{cash, "-5", "BANK_FEES", "BANK_FEES_INTEREST_CHARGE", "", canonical.TxKindInterest},
		{cash, "-40", "FOOD_AND_DRINK", "FOOD_AND_DRINK_GROCERIES", "", canonical.TxKindWithdrawal},
		{cash, "-500", "TRANSFER_OUT", "TRANSFER_OUT_ACCOUNT_TRANSFER", "", canonical.TxKindWithdrawal},
		{cash, "2500", "INCOME", "INCOME_SALARY", "", canonical.TxKindDeposit},
		{cash, "5", "BANK_FEES", "BANK_FEES_OVERDRAFT_FEES", "", canonical.TxKindDeposit},
		// A debit only Plaid's older category files as a bank fee is one.
		{cash, "-5", "GENERAL_SERVICES", "GENERAL_SERVICES_OTHER_GENERAL_SERVICES", "10001000", canonical.TxKindFee},
		// Money borrowed arriving on a bank account is a deposit.
		{cash, "900", "LOAN_DISBURSEMENTS", "LOAN_DISBURSEMENTS_OTHER_DISBURSEMENT", "", canonical.TxKindDeposit},
		{card, "-40", "FOOD_AND_DRINK", "FOOD_AND_DRINK_GROCERIES", "", canonical.TxKindPurchase},
		{card, "-25", "BANK_FEES", "BANK_FEES_LATE_FEES", "", canonical.TxKindFee},
		{card, "-9", "BANK_FEES", "BANK_FEES_INTEREST_CHARGE", "", canonical.TxKindInterest},
		{card, "-30", "GENERAL_SERVICES", "GENERAL_SERVICES_OTHER_GENERAL_SERVICES", "10000000", canonical.TxKindFee},
		{card, "-40", "GENERAL_SERVICES", "GENERAL_SERVICES_OTHER_GENERAL_SERVICES", "18000000", canonical.TxKindPurchase},
		{card, "-40", "GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE", "11000000", canonical.TxKindPurchase},
		{card, "-60", "LOAN_PAYMENTS", "LOAN_PAYMENTS_BNPL", "", canonical.TxKindPurchase},
		{card, "300", "LOAN_PAYMENTS", "LOAN_PAYMENTS_CREDIT_CARD_PAYMENT", "", canonical.TxKindCardPayment},
		{card, "300", "TRANSFER_IN", "TRANSFER_IN_ACCOUNT_TRANSFER", "", canonical.TxKindCardPayment},
		// A card cannot be lent to with a credit: a "disbursement" there
		// is the bill paid.
		{card, "300", "LOAN_DISBURSEMENTS", "LOAN_DISBURSEMENTS_OTHER_DISBURSEMENT", "", canonical.TxKindCardPayment},
		// Plaid's older category names the payment where the newer one
		// misfiles it.
		{card, "300", "INCOME", "INCOME_OTHER", legacyCardPayment, canonical.TxKindCardPayment},
		{card, "40", "FOOD_AND_DRINK", "FOOD_AND_DRINK_GROCERIES", "13005000", canonical.TxKindRefund},
		{card, "25", "BANK_FEES", "BANK_FEES_LATE_FEES", "", canonical.TxKindRefund},
		{card, "30", "GENERAL_SERVICES", "GENERAL_SERVICES_OTHER_GENERAL_SERVICES", "10000000", canonical.TxKindRefund},
		// A debit is never a payment, whatever its older category says.
		{card, "-40", "GENERAL_MERCHANDISE", "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE", legacyCardPayment, canonical.TxKindPurchase},
	}
	for _, c := range cases {
		if got := bankTxKind(c.kind, dec(t, c.amount), c.primary, c.detailed, c.legacy); got != c.want {
			t.Errorf("bankTxKind(%s, %s, %s, %q) = %q, want %q", c.kind, c.amount, c.detailed, c.legacy, got, c.want)
		}
	}
}

func TestDescribedKind(t *testing.T) {
	cases := []struct {
		typ, subtype, text string
		want               canonical.TxKind
	}{
		{"cash", "deposit", "EXAMPLE NOTE 2031 - INT RECEIVED EXAMPLE NOTE", canonical.TxKindInterest},
		{"cash", "deposit", "BANK INTEREST EARNED", canonical.TxKindInterest},
		{"cash", "deposit", "EXAMPLE FUND - CAPITAL GAINS DISTRIBUTION", canonical.TxKindCapitalGain},
		{"cash", "withdrawal", "ACCOUNT SERVICE FEE", canonical.TxKindFee},
		{"cash", "withdrawal", "TAX FILING FEE", canonical.TxKindFee},
		{"cash", "withdrawal", "EXAMPLE STATE TAXPYMT", canonical.TxKindTax},
		{"cash", "deposit", "EXAMPLE STATE TAX REFUND", canonical.TxKindTax},
		// Only the action after the security's name is read.
		{"cash", "deposit", "EXAMPLE TAX REVENUE BOND - REDEMPTION", ""},
		{"cash", "deposit", "EXAMPLE INT'L EQUITY FUND - DEP", ""},
		{"cash", "withdrawal", "OUTGOING WIRE", ""},
		{"cash", "withdrawal", "INT'L WIRE FEE", canonical.TxKindFee},
		{"cash", "deposit", "JOURNAL FROM OTHER ACCOUNT", ""},
		// Other types and subtypes keep their own reading.
		{"cash", "dividend", "EXAMPLE FUND - INTEREST", ""},
		{"fee", "adjustment", "EXAMPLE FUND - FEE", ""},
		{"transfer", "transfer", "EXAMPLE FUND - INT", ""},
	}
	for _, c := range cases {
		got, ok := describedKind(c.typ, c.subtype, c.text)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("describedKind(%q, %q, %q) = (%q, %v), want %q", c.typ, c.subtype, c.text, got, ok, c.want)
		}
	}
}

func TestInvestmentTxKind(t *testing.T) {
	cases := []struct {
		typ, subtype   string
		inKind, inward bool
		want           canonical.TxKind
		known          bool
	}{
		{"buy", "buy", false, false, canonical.TxKindBuy, true},
		{"buy", "dividend reinvestment", false, false, canonical.TxKindBuy, true},
		{"sell", "sell short", false, true, canonical.TxKindSell, true},
		{"cash", "qualified dividend", false, true, canonical.TxKindDividend, true},
		{"cash", "interest", false, true, canonical.TxKindInterest, true},
		{"cash", "long-term capital gain", false, true, canonical.TxKindCapitalGain, true},
		{"fee", "account fee", false, false, canonical.TxKindFee, true},
		{"cash", "tax withheld", false, false, canonical.TxKindTax, true},
		{"cash", "contribution", false, true, canonical.TxKindDeposit, true},
		{"cash", "withdrawal", false, false, canonical.TxKindWithdrawal, true},
		{"transfer", "transfer", true, true, canonical.TxKindTransferIn, true},
		{"transfer", "transfer", true, false, canonical.TxKindTransferOut, true},
		{"transfer", "transfer", false, false, canonical.TxKindWithdrawal, true},
		{"buy", "exercise", false, false, canonical.TxKindBuy, true},
		{"transfer", "assignment", true, false, canonical.TxKindCorporateAction, true},
		{"transfer", "split", true, true, canonical.TxKindCorporateAction, true},
		// An adjustment of a fee is a fee; of a holding, a corporate action.
		{"fee", "adjustment", false, false, canonical.TxKindFee, true},
		{"transfer", "adjustment", true, true, canonical.TxKindCorporateAction, true},
		// One cryptocurrency for another is neither a trade gold values
		// nor money in or out.
		{"transfer", "trade", true, true, canonical.TxKindOther, true},
		{"buy", "something new", false, false, canonical.TxKindBuy, false},
		{"fee", "something new", false, false, canonical.TxKindFee, false},
		{"transfer", "something new", true, true, canonical.TxKindOther, false},
		{"cash", "something new", false, true, canonical.TxKindOther, false},
	}
	for _, c := range cases {
		got, known := investmentTxKind(c.typ, c.subtype, c.inKind, c.inward)
		if got != c.want || known != c.known {
			t.Errorf("investmentTxKind(%q, %q, inKind=%v, inward=%v) = (%q, %v), want (%q, %v)",
				c.typ, c.subtype, c.inKind, c.inward, got, known, c.want, c.known)
		}
	}
	for subtype, k := range investmentKinds {
		if k == canonical.TxKindContribution {
			t.Errorf("investmentKinds[%q] maps to contribution, a capital call", subtype)
		}
	}
}

// ---- snapshots --------------------------------------------------------------

// seedItem is an Item read by one run. It holds a checking account, a
// card, a mortgage and an IRA, plus a student loan the adapter leaves out.
func seedItem(t *testing.T, db *sql.DB, at int64) {
	t.Helper()
	run(t, db, at, day(1), nil)
	acct(t, db, at, "acct-checking", "depository", "checking", "1200.50")
	acct(t, db, at, "acct-card", "credit", "credit card", "410.25")
	acct(t, db, at, "acct-home", "loan", "mortgage", "250000")
	acct(t, db, at, "acct-student", "loan", "student", "9000")
	acct(t, db, at, "acct-ira", "investment", "ira", "99999")
	sec(t, db, "sec-eq", "PLACEHOLDER CORP", "EXA", "equity")
	sec(t, db, "sec-cash", "U S Dollar", "", "cash")
	hold(t, db, at, "acct-ira", "sec-eq", 0, "10", "1500", "1000")
	hold(t, db, at, "acct-ira", "sec-cash", 0, "300", "300", "")
}

func TestARunProjectsEachKindWithItsSign(t *testing.T) {
	path, db := newFixture(t)
	at := runAt(10)
	seedItem(t, db, at)
	batches := collectSnapshots(t, openConn(t, path))

	assertCurrent := func(account, want string) {
		t.Helper()
		got := cashAt(batches, at, account, canonical.BalanceKindCurrent)
		if len(got) != 1 {
			t.Fatalf("%s: %d CURRENT balances at the run, want 1", account, len(got))
		}
		assertAmount(t, account+" CURRENT", &got[0].Amount, want)
	}
	assertCurrent("acct-checking", "1200.50")
	assertCurrent("acct-card", "-410.25") // owed, negated exactly once
	assertCurrent("acct-ira", "300")      // the cash holding, not the account's total
	if got := cashAt(batches, at, "acct-home", canonical.BalanceKindCurrent); len(got) != 0 {
		t.Errorf("a mortgage has no cash balance: %+v", got)
	}

	positions := positionsAt(batches, at)
	byKey := map[string]canonical.PositionChange{}
	for _, p := range positions {
		byKey[p.PositionKey] = p
		if !canonical.ValidTaxonomyPair(p.AssetClass, p.Vehicle) {
			t.Errorf("position %s: (%s, %s) is not an admitted pair", p.PositionKey, p.AssetClass, p.Vehicle)
		}
	}
	if len(positions) != 2 {
		t.Fatalf("positions = %+v, want the equity and the mortgage", positions)
	}
	eq := byKey["EXA"]
	assertAmount(t, "equity quantity", eq.Quantity, "10")
	assertAmount(t, "equity value", eq.MarketValue, "1500")
	assertAmount(t, "equity book", eq.BookValue, "1000")
	home := byKey["acct-home"]
	if home.AssetClass != canonical.AssetClassRealEstate || home.Vehicle != canonical.VehicleMortgage {
		t.Errorf("mortgage pair = (%s, %s)", home.AssetClass, home.Vehicle)
	}
	assertAmount(t, "mortgage value", home.MarketValue, "-250000")

	want := map[string]canonical.AccountKind{
		"acct-checking": canonical.AccountKindCash, "acct-card": canonical.AccountKindCard,
		"acct-home": canonical.AccountKindMortgage, "acct-ira": canonical.AccountKindBrokerage,
	}
	for id, k := range want {
		changes := accountChanges(batches, id)
		if len(changes) != 1 || changes[0].AccountKind != k {
			t.Errorf("account %s = %+v, want one of kind %q", id, changes, k)
		}
	}
	if got := accountChanges(batches, "acct-student"); len(got) != 0 {
		t.Errorf("the student loan is projected: %+v", got)
	}

	// How the run's account rows reach gold.
	ira := accountChanges(batches, "acct-ira")[0]
	if ira.TaxWrapper == nil || *ira.TaxWrapper != canonical.TaxWrapperTraditionalIRA ||
		ira.ManagementStyle != nil || ira.AccountCategory == nil ||
		*ira.AccountCategory != "investment / ira" || ira.DisplayName == nil ||
		*ira.DisplayName != "Synthetic ira …0000" || ira.FirstSeenAt != at || ira.LastSeenAt != at {
		t.Errorf("acct-ira = %+v", ira)
	}
	checking := accountChanges(batches, "acct-checking")[0]
	if checking.ManagementStyle == nil || *checking.ManagementStyle != canonical.ManagementStyleSelfDirected ||
		checking.TaxWrapper == nil || *checking.TaxWrapper != canonical.TaxWrapperTaxablePersonal {
		t.Errorf("acct-checking = %+v", checking)
	}

	// The investment account's cash is read off its holdings, the bank
	// account's off Plaid's account list.
	for id, basis := range map[string]string{"acct-ira": "holdings", "acct-checking": "roster"} {
		got := cashAt(batches, at, id, canonical.BalanceKindCurrent)
		if len(got) != 1 || string(got[0].Payload) != `{"basis":"`+basis+`"}` {
			t.Errorf("%s CURRENT payload = %+v, want basis %s", id, got, basis)
		}
	}
}

// An account is shown by the institution's official name and its mask. The
// account's own name is its nickname where an official name stands beside
// it, and the category carries the official name next to Plaid's subtype.
func TestAccountNames(t *testing.T) {
	path, db := newFixture(t)
	run(t, db, runAt(10), day(1), noInvestments)
	acct(t, db, runAt(10), "acct-a", "depository", "checking", "1")
	acct(t, db, runAt(10), "acct-b", "depository", "savings", "2")
	exec(t, db, `UPDATE accounts SET official_name = 'Official Savings' WHERE account_id = 'acct-b'`)
	acct(t, db, runAt(10), "acct-c", "depository", "cd", "3")
	exec(t, db, `UPDATE accounts SET name = NULL WHERE account_id = 'acct-c'`)
	acct(t, db, runAt(10), "acct-d", "depository", "money market", "4")
	exec(t, db, `UPDATE accounts SET official_name = name WHERE account_id = 'acct-d'`)
	acct(t, db, runAt(10), "acct-e", "depository", "hsa", "5")
	exec(t, db, `UPDATE accounts SET official_name = 'Official HSA', mask = NULL WHERE account_id = 'acct-e'`)
	batches := collectSnapshots(t, openConn(t, path))

	cases := map[string]struct{ display, nickname, category string }{
		"acct-a": {"Synthetic checking …0000", "", "depository / checking"},
		"acct-b": {"Official Savings …0000", "Synthetic savings", "depository / savings · Official Savings"},
		"acct-c": {"0000", "", "depository / cd"},
		"acct-d": {"Synthetic money market …0000", "", "depository / money market · Synthetic money market"},
		"acct-e": {"Official HSA", "Synthetic hsa", "depository / hsa · Official HSA"},
	}
	for id, want := range cases {
		got := accountChanges(batches, id)
		if len(got) != 1 {
			t.Fatalf("%s = %+v, want one account change", id, got)
		}
		a := got[0]
		if a.DisplayName == nil || *a.DisplayName != want.display {
			t.Errorf("%s display name = %v, want %q", id, a.DisplayName, want.display)
		}
		if (want.nickname == "") != (a.Nickname == nil) || a.Nickname != nil && *a.Nickname != want.nickname {
			t.Errorf("%s nickname = %v, want %q", id, a.Nickname, want.nickname)
		}
		if a.AccountCategory == nil || *a.AccountCategory != want.category {
			t.Errorf("%s category = %v, want %q", id, a.AccountCategory, want.category)
		}
	}
}

// An Item linked without investments records its holdings `not_linked`,
// and one whose institution holds no investment account records them
// `absent`. Both settle the holdings, so the run carries its balances.
func TestABankOrCardItemCarriesItsBalances(t *testing.T) {
	for _, holdings := range []string{"not_linked", "absent"} {
		t.Run(holdings, func(t *testing.T) {
			path, db := newFixture(t)
			run(t, db, runAt(10), day(1), map[string]string{"holdings": holdings,
				"investment_transactions": holdings})
			acct(t, db, runAt(10), "acct-checking", "depository", "checking", "1200.50")
			acct(t, db, runAt(10), "acct-card", "credit", "credit card", "410.25")
			batches := collectSnapshots(t, openConn(t, path))
			for id, want := range map[string]string{"acct-checking": "1200.50", "acct-card": "-410.25"} {
				got := cashAt(batches, runAt(10), id, canonical.BalanceKindCurrent)
				if len(got) != 1 || !got[0].Amount.Equal(dec(t, want)) {
					t.Errorf("%s CURRENT = %+v, want %s", id, got, want)
				}
				if len(accountChanges(batches, id)) != 1 {
					t.Errorf("%s: no account change", id)
				}
			}
		})
	}
}

func TestACashAccountWithNoCurrentBalanceUsesTheAvailableOne(t *testing.T) {
	path, db := newFixture(t)
	run(t, db, runAt(10), day(1), noInvestments)
	acct(t, db, runAt(10), "acct-checking", "depository", "checking", "")
	// A card's available balance is unused credit, never cash.
	acct(t, db, runAt(10), "acct-card", "credit", "credit card", "")
	exec(t, db, `UPDATE accounts SET balance_available = '75.5' WHERE account_id = 'acct-checking'`)
	exec(t, db, `UPDATE accounts SET balance_available = '4600' WHERE account_id = 'acct-card'`)
	batches := collectSnapshots(t, openConn(t, path))
	if got := cashAt(batches, runAt(10), "acct-checking", canonical.BalanceKindCurrent); len(got) != 0 {
		t.Errorf("CURRENT = %+v, want none", got)
	}
	got := cashAt(batches, runAt(10), "acct-checking", canonical.BalanceKindAvailable)
	if len(got) != 1 {
		t.Fatalf("AVAILABLE balances = %+v, want one", got)
	}
	assertAmount(t, "available", &got[0].Amount, "75.5")
	for _, b := range batches {
		for _, cb := range b.CashBalances {
			if cb.AccountExternalID == "acct-card" {
				t.Errorf("a card with no current balance emitted %+v", cb)
			}
		}
	}
}

func TestEveryRunRestatesEveryAccountAtItsStart(t *testing.T) {
	// A quiet account must carry a mark at the source's latest snapshot,
	// or gold's current view would drop it, debt and all.
	path, db := newFixture(t)
	seedItem(t, db, runAt(10))
	seedItem(t, db, runAt(11))
	batches := collectSnapshots(t, openConn(t, path))
	for _, id := range []string{"acct-checking", "acct-card", "acct-ira"} {
		if got := cashAt(batches, runAt(11), id, canonical.BalanceKindCurrent); len(got) != 1 {
			t.Errorf("%s: %d CURRENT balances at the latest run, want 1", id, len(got))
		}
	}
	if got := len(positionsAt(batches, runAt(11))); got != 2 {
		t.Errorf("%d positions at the latest run, want 2", got)
	}
}

// An account Plaid no longer lists has left the Item. It reads zero once,
// at the first run without it, so gold does not keep its last balance even
// when no other account of the Item states cash.
func TestAnAccountThatLeftTheItemReadsZeroOnce(t *testing.T) {
	path, db := newFixture(t)
	run(t, db, runAt(10), day(1), noInvestments)
	acct(t, db, runAt(10), "acct-checking", "depository", "checking", "100")
	acct(t, db, runAt(10), "acct-savings", "depository", "savings", "900")
	for d := 11; d <= 12; d++ {
		run(t, db, runAt(d), day(1), noInvestments)
		acct(t, db, runAt(d), "acct-checking", "depository", "checking", "110")
	}
	batches := collectSnapshots(t, openConn(t, path))
	got := cashAt(batches, runAt(11), "acct-savings", canonical.BalanceKindCurrent)
	if len(got) != 1 || !got[0].Amount.IsZero() ||
		!strings.Contains(string(got[0].Payload), `"closure_marker": true`) {
		t.Errorf("the departed account at the next run = %+v, want one zero marker", got)
	}
	if got := cashAt(batches, runAt(12), "acct-savings", canonical.BalanceKindCurrent); len(got) != 0 {
		t.Errorf("the marker repeats: %+v", got)
	}
	if got := cashAt(batches, runAt(11), "acct-checking", canonical.BalanceKindCurrent); len(got) != 1 ||
		!got[0].Amount.Equal(dec(t, "110")) {
		t.Errorf("the staying account at the next run = %+v, want 110", got)
	}
}

// An account the Item no longer lists keeps its ledger, in the description
// of the last run that listed it.
func TestAnAccountThatLeftTheItemKeepsItsLedger(t *testing.T) {
	path, db := newFixture(t)
	for d := 10; d <= 12; d++ {
		run(t, db, runAt(d), day(1), noInvestments)
		acct(t, db, runAt(d), "acct-checking", "depository", "checking", "100")
	}
	for d := 10; d <= 11; d++ {
		acct(t, db, runAt(d), "acct-card", "credit", "credit card", "50")
	}
	exec(t, db, `UPDATE accounts SET name = 'Synthetic old name'
	             WHERE account_id = 'acct-card' AND snapshot_at = ?`, runAt(10))
	bank(t, db, bankRow{id: "tx-card", account: "acct-card", amount: "-5", name: "Kiosk", at: day(3)})
	conn := openConn(t, path)
	if _, ok := collectTransactions(t, conn)["tx-card"]; !ok {
		t.Errorf("the departed card's ledger is gone")
	}
	var named []canonical.AccountChange
	for _, a := range accountChanges(collectSnapshots(t, conn), "acct-card") {
		if a.FirstSeenAt == day(3) {
			named = append(named, a)
		}
	}
	if len(named) != 1 || named[0].DisplayName == nil ||
		*named[0].DisplayName != "Synthetic credit card …0000" {
		t.Errorf("the departed card = %+v, want its newest description", named)
	}
}

func TestARunWhoseHoldingsWereNotReadAddsNothing(t *testing.T) {
	path, db := newFixture(t)
	seedItem(t, db, runAt(10))
	run(t, db, runAt(11), day(1), map[string]string{"holdings": "failed"})
	acct(t, db, runAt(11), "acct-checking", "depository", "checking", "1300")
	batches := collectSnapshots(t, openConn(t, path))
	for _, b := range batches {
		for _, cb := range b.CashBalances {
			if cb.SnapshotAt == runAt(11) {
				t.Errorf("a run with failed holdings emitted %+v", cb)
			}
		}
	}
}

func TestHoldingsThatEmptyLeaveOneClosureMarker(t *testing.T) {
	path, db := newFixture(t)
	sec(t, db, "sec-eq", "PLACEHOLDER CORP", "EXA", "equity")
	for d := 10; d <= 12; d++ {
		run(t, db, runAt(d), day(1), nil)
		acct(t, db, runAt(d), "acct-ira", "investment", "ira", "0")
	}
	hold(t, db, runAt(10), "acct-ira", "sec-eq", 0, "10", "1500", "1000")
	batches := collectSnapshots(t, openConn(t, path))
	marker := positionsAt(batches, runAt(11))
	if len(marker) != 1 || !marker[0].MarketValue.IsZero() || !marker[0].Quantity.IsZero() {
		t.Fatalf("the run after the last holding = %+v, want one zero position", marker)
	}
	if got := positionsAt(batches, runAt(12)); len(got) != 0 {
		t.Errorf("a second empty run repeats the marker: %+v", got)
	}
}

// Plaid lists no line for cash that was invested or moved out, or for an
// account it no longer lists. Gold keeps a key's last figure until the
// source restates it, so the run after the last line states zero, once.
func TestCashThatLeavesTheHoldingsIsZeroedOnce(t *testing.T) {
	path, db := newFixture(t)
	sec(t, db, "sec-eq", "PLACEHOLDER CORP", "EXA", "equity")
	sec(t, db, "sec-cash", "U S Dollar", "", "cash")
	for d := 10; d <= 12; d++ {
		run(t, db, runAt(d), day(1), nil)
		acct(t, db, runAt(d), "acct-ira", "investment", "ira", "1500")
		hold(t, db, runAt(d), "acct-ira", "sec-eq", 0, "10", "1500", "")
	}
	hold(t, db, runAt(10), "acct-ira", "sec-cash", 0, "500", "500", "")
	// A second account holds cash and leaves the Item.
	acct(t, db, runAt(10), "acct-roth", "investment", "roth", "200")
	hold(t, db, runAt(10), "acct-roth", "sec-cash", 0, "200", "200", "")
	batches := collectSnapshots(t, openConn(t, path))

	for _, id := range []string{"acct-ira", "acct-roth"} {
		got := cashAt(batches, runAt(11), id, canonical.BalanceKindCurrent)
		if len(got) != 1 || !got[0].Amount.IsZero() ||
			!strings.Contains(string(got[0].Payload), `"closure_marker": true`) {
			t.Errorf("%s at the run without its cash line = %+v, want one zero marker", id, got)
		}
		if got := cashAt(batches, runAt(12), id, canonical.BalanceKindCurrent); len(got) != 0 {
			t.Errorf("%s: the marker repeats: %+v", id, got)
		}
	}
}

// An account that never lists a cash line states no cash at all.
func TestAnAccountWithoutCashLinesStatesNoCash(t *testing.T) {
	path, db := newFixture(t)
	sec(t, db, "sec-eq", "PLACEHOLDER CORP", "EXA", "equity")
	for d := 10; d <= 11; d++ {
		run(t, db, runAt(d), day(1), nil)
		acct(t, db, runAt(d), "acct-ira", "investment", "ira", "1500")
		hold(t, db, runAt(d), "acct-ira", "sec-eq", 0, "10", "1500", "")
	}
	for _, b := range collectSnapshots(t, openConn(t, path)) {
		if len(b.CashBalances) != 0 {
			t.Errorf("cash balances = %+v, want none", b.CashBalances)
		}
	}
}

// A balance Plaid leaves empty for one run is unknown, not zero: the
// account's last figure is carried into the run, so neither the current
// view nor the closure marker reads it as gone.
func TestABalancePlaidLeavesEmptyIsCarried(t *testing.T) {
	path, db := newFixture(t)
	run(t, db, runAt(10), day(1), noInvestments)
	acct(t, db, runAt(10), "acct-checking", "depository", "checking", "1200.50")
	acct(t, db, runAt(10), "acct-card", "credit", "credit card", "410.25")
	acct(t, db, runAt(10), "acct-home", "loan", "mortgage", "250000")
	run(t, db, runAt(11), day(1), noInvestments)
	acct(t, db, runAt(11), "acct-checking", "depository", "checking", "")
	acct(t, db, runAt(11), "acct-card", "credit", "credit card", "")
	acct(t, db, runAt(11), "acct-home", "loan", "mortgage", "")
	exec(t, db, `UPDATE accounts SET balance_available = '4600' WHERE account_id = 'acct-card'`)
	batches := collectSnapshots(t, openConn(t, path))

	for id, want := range map[string]string{"acct-checking": "1200.50", "acct-card": "-410.25"} {
		got := cashAt(batches, runAt(11), id, canonical.BalanceKindCurrent)
		if len(got) != 1 || !got[0].Amount.Equal(dec(t, want)) ||
			!strings.Contains(string(got[0].Payload), `"basis":"carried"`) {
			t.Errorf("%s at the empty run = %+v, want %s carried", id, got, want)
		}
	}
	home := positionsAt(batches, runAt(11))
	if len(home) != 1 || !home[0].MarketValue.Equal(dec(t, "-250000")) ||
		!strings.Contains(string(home[0].Payload), `"basis":"carried"`) {
		t.Errorf("the mortgage at the empty run = %+v, want -250000 carried", home)
	}
}

// A listing that names no currency states no usable balance either. The
// last figure is carried in the currency it was stated in, so no key
// closes, and the card's statements keep that currency.
func TestAListingWithoutACurrencyCarriesTheStatedOne(t *testing.T) {
	path, db := newFixture(t)
	run(t, db, runAt(10), day(1), noInvestments)
	acct(t, db, runAt(10), "acct-checking", "depository", "checking", "100")
	acct(t, db, runAt(10), "acct-card", "credit", "credit card", "410")
	acct(t, db, runAt(10), "acct-home", "loan", "mortgage", "250000")
	liability(t, db, runAt(10), "acct-card", "380", day(5))
	run(t, db, runAt(11), day(1), noInvestments)
	acct(t, db, runAt(11), "acct-checking", "depository", "checking", "110")
	acct(t, db, runAt(11), "acct-card", "credit", "credit card", "420")
	acct(t, db, runAt(11), "acct-home", "loan", "mortgage", "249000")
	exec(t, db, `UPDATE accounts SET currency = NULL WHERE snapshot_at = ?`, runAt(11))
	batches := collectSnapshots(t, openConn(t, path))

	for id, want := range map[string]string{"acct-checking": "100", "acct-card": "-410"} {
		got := cashAt(batches, runAt(11), id, canonical.BalanceKindCurrent)
		if len(got) != 1 || got[0].Currency != "USD" || !got[0].Amount.Equal(dec(t, want)) ||
			!strings.Contains(string(got[0].Payload), `"basis":"carried"`) {
			t.Errorf("%s at the run without a currency = %+v, want %s USD carried", id, got, want)
		}
	}
	home := positionsAt(batches, runAt(11))
	if len(home) != 1 || home[0].Currency != "USD" || !home[0].MarketValue.Equal(dec(t, "-250000")) {
		t.Errorf("the mortgage at the run without a currency = %+v, want -250000 USD carried", home)
	}
	closes := cashAt(batches, day(5), "acct-card", canonical.BalanceKindClosing)
	if len(closes) != 1 || closes[0].Currency != "USD" {
		t.Errorf("the statement close = %+v, want one in USD", closes)
	}
}

// A listing whose currency changed while its balance is empty carries the
// last figure in the currency that figure was stated in, never relabelled.
func TestACarryKeepsTheStatedCurrency(t *testing.T) {
	path, db := newFixture(t)
	run(t, db, runAt(10), day(1), noInvestments)
	acct(t, db, runAt(10), "acct-checking", "depository", "checking", "100")
	run(t, db, runAt(11), day(1), noInvestments)
	acct(t, db, runAt(11), "acct-checking", "depository", "checking", "")
	exec(t, db, `UPDATE accounts SET currency = 'EUR' WHERE snapshot_at = ?`, runAt(11))
	batches := collectSnapshots(t, openConn(t, path))
	got := cashAt(batches, runAt(11), "acct-checking", canonical.BalanceKindCurrent)
	if len(got) != 1 || got[0].Currency != "USD" || !got[0].Amount.Equal(dec(t, "100")) {
		t.Errorf("the carried balance = %+v, want one 100 USD", got)
	}
}

// A run whose holdings failed carries no snapshots, but its balances are
// still the newest Plaid stated. A later empty balance carries them.
func TestACarryRestatesTheNewestFigureAnyRunStated(t *testing.T) {
	path, db := newFixture(t)
	run(t, db, runAt(10), day(1), nil)
	acct(t, db, runAt(10), "acct-checking", "depository", "checking", "100")
	run(t, db, runAt(11), day(1), map[string]string{"holdings": "failed"})
	acct(t, db, runAt(11), "acct-checking", "depository", "checking", "200")
	run(t, db, runAt(12), day(1), nil)
	acct(t, db, runAt(12), "acct-checking", "depository", "checking", "")
	batches := collectSnapshots(t, openConn(t, path))
	got := cashAt(batches, runAt(12), "acct-checking", canonical.BalanceKindCurrent)
	want := fmt.Sprintf(`"stated_at":%d`, runAt(11))
	if len(got) != 1 || !got[0].Amount.Equal(dec(t, "200")) ||
		!strings.Contains(string(got[0].Payload), want) {
		t.Errorf("the carried balance = %+v, want 200 stated at the failed run", got)
	}
	if got := cashAt(batches, runAt(11), "acct-checking", canonical.BalanceKindCurrent); len(got) != 0 {
		t.Errorf("the failed run emitted %+v", got)
	}
}

// An account that held positions at the last run and holds none now is
// closed on its own, even while other accounts still hold: gold's history
// would carry its positions otherwise. A refinance replaces one mortgage
// with another; a rollover moves securities from one plan to another.
func TestPositionsCloseAccountByAccount(t *testing.T) {
	path, db := newFixture(t)
	sec(t, db, "sec-a", "PLACEHOLDER CORP", "EXA", "equity")
	sec(t, db, "sec-b", "PLACEHOLDER OTHER CORP", "EXB", "equity")
	run(t, db, runAt(10), day(1), nil)
	acct(t, db, runAt(10), "acct-old-home", "loan", "mortgage", "250000")
	acct(t, db, runAt(10), "acct-401k", "investment", "401k", "1000")
	hold(t, db, runAt(10), "acct-401k", "sec-a", 0, "10", "1000", "")
	for d := 11; d <= 12; d++ {
		run(t, db, runAt(d), day(1), nil)
		acct(t, db, runAt(d), "acct-new-home", "loan", "mortgage", "245000")
		acct(t, db, runAt(d), "acct-ira", "investment", "ira", "1000")
		hold(t, db, runAt(d), "acct-ira", "sec-b", 0, "10", "1000", "")
	}
	batches := collectSnapshots(t, openConn(t, path))

	closed := map[string]canonical.PositionChange{}
	for _, p := range positionsAt(batches, runAt(11)) {
		if p.MarketValue.IsZero() {
			closed[p.AccountExternalID+"/"+p.PositionKey] = p
		}
	}
	for _, key := range []string{"acct-old-home/acct-old-home", "acct-401k/EXA"} {
		p, ok := closed[key]
		if !ok || !strings.Contains(string(p.Payload), `"closure_marker": true`) {
			t.Errorf("%s at the next run = %+v, want a zero closure marker", key, p)
		}
	}
	if len(closed) != 2 {
		t.Errorf("zero positions at the next run = %v, want the two closed ones", closed)
	}
	for _, p := range positionsAt(batches, runAt(12)) {
		if p.MarketValue.IsZero() {
			t.Errorf("a marker repeats at the following run: %+v", p)
		}
	}
}

func TestTwoHoldingsOfOneInstrumentAreOnePosition(t *testing.T) {
	path, db := newFixture(t)
	run(t, db, runAt(10), day(1), nil)
	acct(t, db, runAt(10), "acct-ira", "investment", "ira", "0")
	sec(t, db, "sec-a", "PLACEHOLDER CORP", "EXA", "equity")
	sec(t, db, "sec-b", "PLACEHOLDER CORP", "EXA", "equity")
	hold(t, db, runAt(10), "acct-ira", "sec-a", 0, "1", "150", "100")
	hold(t, db, runAt(10), "acct-ira", "sec-a", 1, "2", "300", "")
	hold(t, db, runAt(10), "acct-ira", "sec-b", 0, "3", "450", "400")
	batches := collectSnapshots(t, openConn(t, path))
	got := positionsAt(batches, runAt(10))
	if len(got) != 1 {
		t.Fatalf("positions = %+v, want one", got)
	}
	assertAmount(t, "quantity", got[0].Quantity, "6")
	assertAmount(t, "value", got[0].MarketValue, "900")
	assertAmount(t, "book", got[0].BookValue, "") // one holding states no cost
	var payload map[string]any
	if err := json.Unmarshal(got[0].Payload, &payload); err != nil || len(payload["holdings"].([]any)) != 3 {
		t.Errorf("payload = %s, want the three holdings", got[0].Payload)
	}
}

// A security Plaid types as `other` reaches gold by its CFI code, on its
// position and its instrument, with Plaid's type kept in the payload.
func TestAnOtherSecurityFallsBackOnItsCFICode(t *testing.T) {
	path, db := newFixture(t)
	run(t, db, runAt(10), day(1), nil)
	acct(t, db, runAt(10), "acct-ira", "investment", "ira", "0")
	sec(t, db, "sec-x", "PLACEHOLDER TREASURY BOND ETF", "EXE", "other")
	exec(t, db, `UPDATE securities SET cfi_code = 'CEXXXX' WHERE security_id = 'sec-x'`)
	hold(t, db, runAt(10), "acct-ira", "sec-x", 0, "10", "300", "")
	batches := collectSnapshots(t, openConn(t, path))
	got := positionsAt(batches, runAt(10))
	if len(got) != 1 || got[0].AssetClass != canonical.AssetClassFixedIncome ||
		got[0].Vehicle != canonical.VehicleETF ||
		!strings.Contains(string(got[0].Payload), `"source_type":"other"`) {
		t.Errorf("position = %+v, want (fixed_income, etf) keeping Plaid's type", got)
	}
	found := false
	for _, b := range batches {
		for _, i := range b.Instruments {
			if i.InstrumentExternalID == "EXE" {
				found = true
				if i.AssetClass != canonical.AssetClassFixedIncome || i.Vehicle != canonical.VehicleETF {
					t.Errorf("instrument = %+v, want (fixed_income, etf)", i)
				}
			}
		}
	}
	if !found {
		t.Error("no instrument EXE")
	}
}

// Shares not yet vested are not the holder's. A position holds the vested
// ones at their vested value, else at the price, else at the value pro
// rata, and notes the rest.
func TestOnlyVestedSharesArePositions(t *testing.T) {
	path, db := newFixture(t)
	run(t, db, runAt(10), day(1), nil)
	acct(t, db, runAt(10), "acct-plan", "investment", "brokerage", "0")
	cases := []struct {
		security, price, vestedQuantity, vestedValue string
		quantity, value, unvested                    string
	}{
		{"sec-a", "50", "40", "2000", "40", "2000", "60"},
		{"sec-b", "55", "40", "", "40", "2200", "60"},
		{"sec-c", "", "25", "", "25", "1250", "75"},
		{"sec-d", "50", "100", "5000", "100", "5000", ""}, // all vested
		{"sec-e", "50", "", "", "100", "5000", ""},        // nothing stated
	}
	for i, c := range cases {
		sec(t, db, c.security, "PLACEHOLDER CORP "+c.security, fmt.Sprintf("EX%d", i), "equity")
		hold(t, db, runAt(10), "acct-plan", c.security, 0, "100", "5000", "")
		exec(t, db, `UPDATE holdings SET institution_price = ?, vested_quantity = ?, vested_value = ?
		             WHERE security_id = ?`, null(c.price), null(c.vestedQuantity), null(c.vestedValue), c.security)
	}
	byKey := map[string]canonical.PositionChange{}
	for _, p := range positionsAt(collectSnapshots(t, openConn(t, path)), runAt(10)) {
		byKey[p.PositionKey] = p
	}
	for i, c := range cases {
		p, ok := byKey[fmt.Sprintf("EX%d", i)]
		if !ok {
			t.Fatalf("%s: no position", c.security)
		}
		assertAmount(t, c.security+" quantity", p.Quantity, c.quantity)
		assertAmount(t, c.security+" value", p.MarketValue, c.value)
		want := `"unvested_quantity":"` + c.unvested + `"`
		if has := strings.Contains(string(p.Payload), `"unvested_quantity"`); has != (c.unvested != "") ||
			c.unvested != "" && !strings.Contains(string(p.Payload), want) {
			t.Errorf("%s payload = %s, want unvested %q", c.security, p.Payload, c.unvested)
		}
	}
}

// ---- card statements --------------------------------------------------------

func TestStatementClosesAreCardHistoryWithTheNewestRunsFigure(t *testing.T) {
	path, db := newFixture(t)
	for _, d := range []int{10, 11} {
		run(t, db, runAt(d), day(1), noInvestments)
		acct(t, db, runAt(d), "acct-card", "credit", "credit card", "410.25")
	}
	liability(t, db, runAt(10), "acct-card", "380", day(5))
	liability(t, db, runAt(11), "acct-card", "385", day(5)) // restated
	batches := collectSnapshots(t, openConn(t, path))
	got := cashAt(batches, day(5), "acct-card", canonical.BalanceKindClosing)
	if len(got) != 1 {
		t.Fatalf("closing balances at the issue date = %+v, want one", got)
	}
	assertAmount(t, "closing", &got[0].Amount, "-385")
}

// Plaid may restate a statement without its balance. The newest run that
// states the figure then stands.
func TestAStatementRestatedWithoutItsBalanceKeepsItsClose(t *testing.T) {
	path, db := newFixture(t)
	for _, d := range []int{10, 11} {
		run(t, db, runAt(d), day(1), noInvestments)
		acct(t, db, runAt(d), "acct-card", "credit", "credit card", "410.25")
	}
	liability(t, db, runAt(10), "acct-card", "380", day(5))
	exec(t, db, `INSERT INTO liabilities (snapshot_at, account_id, kind, last_statement_balance,
	             last_statement_issue_date, payload) VALUES (?, 'acct-card', 'credit', NULL, ?, '{}')`,
		runAt(11), day(5))
	batches := collectSnapshots(t, openConn(t, path))
	got := cashAt(batches, day(5), "acct-card", canonical.BalanceKindClosing)
	if len(got) != 1 || !got[0].Amount.Equal(dec(t, "-380")) {
		t.Errorf("the close = %+v, want the older figure -380", got)
	}
}

func TestEveryStatementKeepsItsClose(t *testing.T) {
	path, db := newFixture(t)
	for _, d := range []int{10, 40} {
		run(t, db, runAt(d), day(1), noInvestments)
		acct(t, db, runAt(d), "acct-card", "credit", "credit card", "410.25")
	}
	liability(t, db, runAt(10), "acct-card", "380", day(5))
	liability(t, db, runAt(40), "acct-card", "420", day(35))
	batches := collectSnapshots(t, openConn(t, path))
	for issued, want := range map[int64]string{day(5): "-380", day(35): "-420"} {
		got := cashAt(batches, issued, "acct-card", canonical.BalanceKindClosing)
		if len(got) != 1 || !got[0].Amount.Equal(dec(t, want)) {
			t.Errorf("close at %d = %+v, want %s", issued, got, want)
		}
	}
}

// A statement can be older than every ledger window a run read. Its issue
// date still opens the window, so its close is emitted and a reload clears
// it.
func TestAStatementOlderThanEveryLedgerWindow(t *testing.T) {
	path, db := newFixture(t)
	run(t, db, runAt(10), day(5), noInvestments)
	acct(t, db, runAt(10), "acct-card", "credit", "credit card", "410.25")
	liability(t, db, runAt(10), "acct-card", "380", day(2))
	conn := openConn(t, path)
	if s := status(t, conn); s.OldestSnapshotAt != day(2) {
		t.Errorf("oldest snapshot = %d, want the issue date %d", s.OldestSnapshotAt, day(2))
	}
	if w := window(t, conn); w.Start != day(2) {
		t.Errorf("window starts %d, want the issue date %d", w.Start, day(2))
	}
	got := cashAt(collectSnapshots(t, conn), day(2), "acct-card", canonical.BalanceKindClosing)
	if len(got) != 1 || !got[0].Amount.Equal(dec(t, "-380")) {
		t.Errorf("close = %+v, want -380", got)
	}
}

// A run whose holdings failed carries no snapshots, but it may still have
// read a newer statement. That close waits for the next complete run: on
// its own it would be the source's latest instant and hide every other
// balance.
func TestAStatementNewerThanTheLastCompleteRunWaits(t *testing.T) {
	path, db := newFixture(t)
	seedItem(t, db, runAt(10))
	run(t, db, runAt(20), day(1), map[string]string{"holdings": "failed"})
	acct(t, db, runAt(20), "acct-card", "credit", "credit card", "480")
	liability(t, db, runAt(20), "acct-card", "480", day(15))
	batches := collectSnapshots(t, openConn(t, path))
	if got := cashAt(batches, day(15), "acct-card", canonical.BalanceKindClosing); len(got) != 0 {
		t.Errorf("a close newer than the last complete run = %+v", got)
	}
	if newest := newestCashInstant(batches); newest != runAt(10) {
		t.Errorf("the newest cash instant is %d, want the complete run %d", newest, runAt(10))
	}

	// The next complete run restates the statement, and its close appears.
	seedItem(t, db, runAt(30))
	liability(t, db, runAt(30), "acct-card", "480", day(15))
	batches = collectSnapshots(t, openConn(t, path))
	if got := cashAt(batches, day(15), "acct-card", canonical.BalanceKindClosing); len(got) != 1 {
		t.Errorf("the close after the next complete run = %+v, want one", got)
	}
}

// ---- transactions -----------------------------------------------------------

func TestTheBankLedgerKindsSignsAndText(t *testing.T) {
	path, db := newFixture(t)
	seedItem(t, db, runAt(10))
	rows := []bankRow{
		{id: "tx-grocer", account: "acct-checking", amount: "-40", name: "Grocer",
			original: "GROCER 0001", merchant: "Grocer", primary: "FOOD_AND_DRINK",
			detailed: "FOOD_AND_DRINK_GROCERIES", check: "1001", at: day(3)},
		{id: "tx-pay", account: "acct-checking", amount: "2500", name: "Payroll",
			primary: "INCOME", detailed: "INCOME_SALARY", check: "77", at: day(4)},
		{id: "tx-zero", account: "acct-checking", amount: "0", name: "Void",
			check: "1002", at: day(4)},
		{id: "tx-nocur", account: "acct-checking", amount: "-5", name: "Kiosk",
			noCurrency: true, at: day(4)},
		{id: "tx-card", account: "acct-card", amount: "-12.5", name: "Cafe",
			primary: "FOOD_AND_DRINK", detailed: "FOOD_AND_DRINK_COFFEE", pending: true, at: day(5)},
		{id: "tx-bill", account: "acct-card", amount: "300", name: "Payment",
			primary: "LOAN_PAYMENTS", detailed: "LOAN_PAYMENTS_CREDIT_CARD_PAYMENT", at: day(6)},
		{id: "tx-loan", account: "acct-student", amount: "200", name: "Payment",
			primary: "LOAN_PAYMENTS", detailed: "LOAN_PAYMENTS_STUDENT_LOAN_PAYMENT", at: day(6)},
		{id: "tx-home", account: "acct-home", amount: "1500", name: "Payment",
			primary: "LOAN_PAYMENTS", detailed: "LOAN_PAYMENTS_MORTGAGE_PAYMENT", at: day(6)},
	}
	for _, r := range rows {
		bank(t, db, r)
	}
	got := collectTransactions(t, openConn(t, path))

	for _, id := range []string{"tx-loan", "tx-home"} {
		if _, ok := got[id]; ok {
			t.Errorf("%s: a loan's own ledger is booked", id)
		}
	}
	grocer := got["tx-grocer"]
	if grocer.Kind != canonical.TxKindWithdrawal || *grocer.Description != "GROCER 0001" ||
		*grocer.Counterparty != "Grocer" || *grocer.ProviderCategory != "FOOD_AND_DRINK_GROCERIES" ||
		grocer.CheckNumber == nil || *grocer.CheckNumber != "1001" {
		t.Errorf("tx-grocer = %+v", grocer)
	}
	assertAmount(t, "tx-grocer net", grocer.NetAmount, "-40")
	if pay := got["tx-pay"]; pay.Kind != canonical.TxKindDeposit || *pay.Description != "Payroll" {
		t.Errorf("tx-pay = %+v", pay)
	}
	// A cheque number names an outflow only.
	for id, tx := range got {
		if id != "tx-grocer" && tx.CheckNumber != nil {
			t.Errorf("%s carries cheque number %q", id, *tx.CheckNumber)
		}
	}
	if nocur := got["tx-nocur"]; nocur.Currency != "USD" {
		t.Errorf("a row with no currency = %q, want the account's", nocur.Currency)
	}
	cardTx := got["tx-card"]
	assertAmount(t, "tx-card net", cardTx.NetAmount, "-12.5")
	if cardTx.Kind != canonical.TxKindPurchase || !strings.Contains(string(cardTx.Payload), `"pending":true`) {
		t.Errorf("tx-card = %+v", cardTx)
	}
	if bill := got["tx-bill"]; bill.Kind != canonical.TxKindCardPayment {
		t.Errorf("tx-bill kind = %q", bill.Kind)
	}
}

// A card's bill paid can arrive filed as a loan disbursement, or under a
// category Plaid's older taxonomy corrects. Both are the bill paid; a
// merchant's credit stays a refund. A card's own fee the older taxonomy
// alone names is a fee, not a purchase.
func TestACardRowIsFoundWhereverPlaidFilesIt(t *testing.T) {
	path, db := newFixture(t)
	seedItem(t, db, runAt(10))
	for _, r := range []bankRow{
		{id: "tx-paid", account: "acct-card", amount: "300", name: "CARD PAYMENT RECEIVED",
			primary: "LOAN_DISBURSEMENTS", detailed: "LOAN_DISBURSEMENTS_OTHER_DISBURSEMENT",
			legacy: legacyCardPayment, at: day(3)},
		{id: "tx-autopay", account: "acct-card", amount: "120", name: "EXAMPLE AUTOPAY",
			primary: "INCOME", detailed: "INCOME_OTHER", legacy: legacyCardPayment, at: day(4)},
		{id: "tx-credit", account: "acct-card", amount: "15", name: "Merchant credit",
			primary: "GENERAL_MERCHANDISE", detailed: "GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE",
			legacy: "19000000", at: day(5)},
		{id: "tx-annual", account: "acct-card", amount: "-30", name: "EXAMPLE CARD FEE",
			primary: "GENERAL_SERVICES", detailed: "GENERAL_SERVICES_OTHER_GENERAL_SERVICES",
			legacy: "10000000", at: day(6)},
	} {
		bank(t, db, r)
	}
	got := collectTransactions(t, openConn(t, path))
	for id, want := range map[string]canonical.TxKind{"tx-paid": canonical.TxKindCardPayment,
		"tx-autopay": canonical.TxKindCardPayment, "tx-credit": canonical.TxKindRefund,
		"tx-annual": canonical.TxKindFee} {
		if got[id].Kind != want {
			t.Errorf("%s kind = %q, want %q", id, got[id].Kind, want)
		}
	}
}

// Coupons, fund distributions, fees and taxes can arrive as plain cash
// deposits and withdrawals. Where the text says which, the row books as
// that, keeping its amount and the security it names.
func TestCashRowsBookAsTheirTextSays(t *testing.T) {
	path, db := newFixture(t)
	seedItem(t, db, runAt(10))
	sec(t, db, "sec-bond", "EXAMPLE NOTE 2031", "", "equity")
	sec(t, db, "sec-cashbond", "EXAMPLE NOTE 2032", "", "cash")
	sec(t, db, "sec-fund", "EXAMPLE FUND", "EXF", "mutual fund")
	cash := func(id, security, subtype, amount, text string) invRow {
		return invRow{id: id, account: "acct-ira", security: security, typ: "cash",
			subtype: subtype, amount: amount, quantity: "0", price: "0", name: text, at: day(3)}
	}
	rows := []invRow{
		cash("itx-coupon", "sec-bond", "deposit", "250", "EXAMPLE NOTE 2031 - INTEREST PAID"),
		cash("itx-coupon-cash", "sec-cashbond", "deposit", "125", "EXAMPLE NOTE 2032 - INT RECEIVED"),
		cash("itx-gain", "sec-fund", "deposit", "40", "EXAMPLE FUND - SHORT-TERM CAP GAIN"),
		cash("itx-fee", "", "withdrawal", "-75", "ACCOUNT SERVICE FEE"),
		cash("itx-tax", "", "withdrawal", "-60", "EXAMPLE STATE TAXPYMT"),
		cash("itx-wire", "", "withdrawal", "-900", "OUTGOING WIRE"),
		cash("itx-redeem", "sec-bond", "deposit", "5000", "EXAMPLE NOTE 2031 - REDEMPTION"),
	}
	for _, r := range rows {
		inv(t, db, r)
	}
	got := collectTransactions(t, openConn(t, path))
	for id, want := range map[string]struct {
		kind       canonical.TxKind
		instrument string
	}{
		"itx-coupon":      {canonical.TxKindInterest, "plaid:sec-bond"},
		"itx-coupon-cash": {canonical.TxKindInterest, ""},
		"itx-gain":        {canonical.TxKindCapitalGain, "EXF"},
		"itx-fee":         {canonical.TxKindFee, ""},
		"itx-tax":         {canonical.TxKindTax, ""},
		"itx-wire":        {canonical.TxKindWithdrawal, ""},
		// Text that states nothing leaves the row to the other rules: a
		// cash deposit naming a security is a sale of it.
		"itx-redeem": {canonical.TxKindSell, "plaid:sec-bond"},
	} {
		tx := got[id]
		if tx.Kind != want.kind {
			t.Errorf("%s kind = %q, want %q", id, tx.Kind, want.kind)
		}
		named := ""
		if tx.InstrumentExternalID != nil {
			named = *tx.InstrumentExternalID
		}
		if named != want.instrument {
			t.Errorf("%s names %q, want %q", id, named, want.instrument)
		}
	}
	for _, r := range rows {
		assertAmount(t, r.id+" net", got[r.id].NetAmount, r.amount)
	}
}

func TestTheInvestmentLedger(t *testing.T) {
	path, db := newFixture(t)
	seedItem(t, db, runAt(10))
	acct(t, db, runAt(10), "acct-coin", "investment", "crypto exchange", "0")
	sec(t, db, "sec-old", "PLACEHOLDER SOLD CORP", "EXB", "equity")
	sec(t, db, "sec-coin-a", "PLACEHOLDER TOKEN A", "EXC", "cryptocurrency")
	sec(t, db, "sec-coin-b", "PLACEHOLDER TOKEN B", "EXD", "cryptocurrency")
	// Cash rows in the shape load.py stores: Plaid's quantity keeps its
	// own sign, opposite to the fleet's amount.
	rows := []invRow{
		{id: "itx-buy", account: "acct-ira", security: "sec-eq", typ: "buy", subtype: "buy",
			amount: "-1500", quantity: "10", price: "150", at: day(2)},
		{id: "itx-div", account: "acct-ira", security: "sec-eq", typ: "cash",
			subtype: "qualified dividend", amount: "8.72", quantity: "0", price: "0", at: day(3)},
		{id: "itx-in", account: "acct-ira", security: "sec-cash", typ: "cash",
			subtype: "contribution", amount: "1200", quantity: "-1200", price: "1", at: day(4)},
		{id: "itx-out", account: "acct-ira", security: "sec-cash", typ: "cash",
			subtype: "withdrawal", amount: "-500", quantity: "500", price: "1", at: day(4)},
		{id: "itx-fee", account: "acct-ira", security: "sec-cash", typ: "fee",
			subtype: "account fee", amount: "-3", quantity: "3", price: "1", at: day(4)},
		// A cash row on a security silver does not describe still moves cash.
		{id: "itx-hint", account: "acct-ira", security: "sec-unknown", typ: "cash",
			subtype: "contribution", amount: "1200", quantity: "-1200", price: "1", at: day(4)},
		{id: "itx-xfer", account: "acct-ira", security: "sec-old", typ: "transfer",
			subtype: "transfer", amount: "0", quantity: "-5", price: "20", at: day(5)},
		{id: "itx-claw", account: "acct-ira", security: "sec-eq", typ: "cash",
			subtype: "dividend", amount: "-3", at: day(6)},
		{id: "itx-odd", account: "acct-ira", typ: "cash", subtype: "something new",
			amount: "1", at: day(8)},
		{id: "itx-newxfer", account: "acct-ira", security: "sec-old", typ: "transfer",
			subtype: "something new", amount: "0", quantity: "-5", price: "20", at: day(8)},
		{id: "itx-trade-out", account: "acct-coin", security: "sec-coin-a", typ: "transfer",
			subtype: "trade", amount: "0", quantity: "-0.1", price: "30000", at: day(9)},
		{id: "itx-trade-in", account: "acct-coin", security: "sec-coin-b", typ: "transfer",
			subtype: "trade", amount: "0", quantity: "1.5", price: "2000", at: day(9)},
	}
	for _, r := range rows {
		inv(t, db, r)
	}
	got := collectTransactions(t, openConn(t, path))

	buy := got["itx-buy"]
	if buy.Kind != canonical.TxKindBuy || buy.InstrumentExternalID == nil ||
		*buy.InstrumentExternalID != "EXA" || buy.ProviderCategory != nil {
		t.Errorf("itx-buy = %+v", buy)
	}
	assertAmount(t, "itx-buy net", buy.NetAmount, "-1500")
	assertAmount(t, "itx-buy quantity", buy.Quantity, "10")

	if div := got["itx-div"]; div.Kind != canonical.TxKindDividend || *div.InstrumentExternalID != "EXA" {
		t.Errorf("itx-div = %+v (a dividend names the security that paid it)", div)
	}
	for id, want := range map[string]struct {
		kind canonical.TxKind
		net  string
	}{
		"itx-in":  {canonical.TxKindDeposit, "1200"},
		"itx-out": {canonical.TxKindWithdrawal, "-500"},
		"itx-fee": {canonical.TxKindFee, "-3"},
	} {
		tx := got[id]
		if tx.Kind != want.kind || tx.InstrumentExternalID != nil || tx.Quantity != nil {
			t.Errorf("%s = %+v, want %s moving no instrument", id, tx, want.kind)
		}
		assertAmount(t, id+" net", tx.NetAmount, want.net)
	}
	hint := got["itx-hint"]
	if hint.Kind != canonical.TxKindDeposit || hint.InstrumentHint != "sec-unknown" {
		t.Errorf("itx-hint = %+v, want a deposit naming the undescribed security", hint)
	}
	assertAmount(t, "itx-hint net", hint.NetAmount, "1200")
	xfer := got["itx-xfer"]
	if xfer.Kind != canonical.TxKindTransferOut {
		t.Errorf("itx-xfer kind = %q", xfer.Kind)
	}
	assertAmount(t, "itx-xfer net", xfer.NetAmount, "-100") // 5 × 20, valued
	// A dividend clawed back keeps its own sign.
	assertAmount(t, "itx-claw net", got["itx-claw"].NetAmount, "-3")
	for id, raw := range map[string]string{"itx-odd": "cash/something new",
		"itx-newxfer": "transfer/something new", "itx-trade-out": "transfer/trade",
		"itx-trade-in": "transfer/trade"} {
		tx := got[id]
		if tx.Kind != canonical.TxKindOther ||
			!strings.Contains(string(tx.Payload), `"source_kind":"`+raw+`"`) {
			t.Errorf("%s = %+v, want other with its raw pair", id, tx)
		}
	}
	// A crypto trade moves no money in or out: nothing valued, sign kept.
	assertAmount(t, "itx-trade-in net", got["itx-trade-in"].NetAmount, "0")
}

// A cash or fee row that names a security that is not cash is read by what
// it names: tax withheld from that day's dividend, a fee, or the cash paid
// in lieu of a fractional share.
func TestCashNamingASecurityIsReadByWhatItNames(t *testing.T) {
	path, db := newFixture(t)
	seedItem(t, db, runAt(10))
	acct(t, db, runAt(10), "acct-roth", "investment", "roth", "0")
	sec(t, db, "sec-old", "PLACEHOLDER OTHER CORP", "EXB", "equity")
	cash := func(id, account, security, typ, subtype, amount string, d int) invRow {
		return invRow{id: id, account: account, security: security, typ: typ, subtype: subtype,
			amount: amount, quantity: "0", price: "1", at: day(d)}
	}
	rows := []invRow{
		cash("itx-div", "acct-ira", "sec-eq", "cash", "dividend", "100", 3),
		cash("itx-withheld", "acct-ira", "sec-eq", "fee", "adjustment", "-12", 3),
		cash("itx-tax", "acct-ira", "sec-eq", "cash", "withdrawal", "-6", 3),
		cash("itx-lieu", "acct-ira", "sec-eq", "cash", "deposit", "7.5", 3),
		// The dividend fixes the account, the security and the day.
		cash("itx-next-day", "acct-ira", "sec-eq", "fee", "adjustment", "-2", 4),
		cash("itx-other-sec", "acct-ira", "sec-old", "fee", "adjustment", "-2", 3),
		cash("itx-other-acct", "acct-roth", "sec-eq", "cash", "withdrawal", "-2", 3),
		// A fee Plaid names stays a fee, dividend or not.
		cash("itx-named-fee", "acct-ira", "sec-eq", "fee", "miscellaneous fee", "-0.5", 3),
		// Cash rows on the cash itself move money in or out.
		cash("itx-out", "acct-ira", "sec-cash", "cash", "withdrawal", "-500", 3),
		cash("itx-in", "acct-ira", "sec-cash", "cash", "deposit", "500", 3),
		cash("itx-adj", "acct-ira", "", "fee", "adjustment", "3", 3),
	}
	for _, r := range rows {
		inv(t, db, r)
	}
	got := collectTransactions(t, openConn(t, path))
	want := map[string]canonical.TxKind{
		"itx-div": canonical.TxKindDividend, "itx-withheld": canonical.TxKindTax,
		"itx-tax": canonical.TxKindTax, "itx-lieu": canonical.TxKindSell,
		"itx-next-day": canonical.TxKindFee, "itx-other-sec": canonical.TxKindFee,
		"itx-other-acct": canonical.TxKindFee, "itx-named-fee": canonical.TxKindFee,
		"itx-out": canonical.TxKindWithdrawal, "itx-in": canonical.TxKindDeposit,
		"itx-adj": canonical.TxKindFee,
	}
	for _, r := range rows {
		tx := got[r.id]
		if tx.Kind != want[r.id] {
			t.Errorf("%s kind = %q, want %q", r.id, tx.Kind, want[r.id])
		}
		// Each keeps its own amount and sign, and states no quantity.
		assertAmount(t, r.id+" net", tx.NetAmount, r.amount)
		if tx.Quantity != nil || tx.Price != nil {
			t.Errorf("%s states placeholder quantity %v and price %v", r.id, tx.Quantity, tx.Price)
		}
		if strings.Contains(string(tx.Payload), "source_kind") {
			t.Errorf("%s payload = %s, want no source_kind", r.id, tx.Payload)
		}
	}
	for _, id := range []string{"itx-withheld", "itx-tax", "itx-lieu"} {
		if tx := got[id]; tx.InstrumentExternalID == nil || *tx.InstrumentExternalID != "EXA" {
			t.Errorf("%s names %v, want EXA", id, tx.InstrumentExternalID)
		}
	}
}

// Plaid can type a bond as cash, with no ticker. A price other than 0 or 1
// gives it away, on a holding or on a trade: it is a position, a buy moves
// it, and its coupon names it. Cash stays the account's cash, whatever
// placeholder price a row that is no trade states.
func TestABondPlaidTypesAsCashIsAPosition(t *testing.T) {
	path, db := newFixture(t)
	seedItem(t, db, runAt(10))
	sec(t, db, "sec-note", "EXAMPLE NOTE 2031", "", "cash")
	sec(t, db, "sec-held", "EXAMPLE NOTE 2033", "", "cash")
	sec(t, db, "sec-idle", "EXAMPLE CASH", "", "cash")
	hold(t, db, runAt(10), "acct-ira", "sec-note", 0, "5000", "5100", "")
	hold(t, db, runAt(10), "acct-ira", "sec-held", 0, "2000", "1960", "")
	hold(t, db, runAt(10), "acct-ira", "sec-idle", 0, "50", "50", "")
	exec(t, db, `UPDATE holdings SET institution_price = CASE security_id
	             WHEN 'sec-held' THEN '98' WHEN 'sec-cash' THEN '1' WHEN 'sec-idle' THEN '0' END`)
	for _, r := range []invRow{
		{id: "itx-buy", account: "acct-ira", security: "sec-note", typ: "buy", subtype: "buy",
			amount: "-5050", quantity: "5000", price: "101", at: day(3)},
		{id: "itx-coupon", account: "acct-ira", security: "sec-note", typ: "cash",
			subtype: "deposit", amount: "125", quantity: "0", price: "0",
			name: "EXAMPLE NOTE 2031 - INTEREST PAID", at: day(4)},
		{id: "itx-interest", account: "acct-ira", security: "sec-cash", typ: "cash",
			subtype: "interest", amount: "2", quantity: "0", price: "0.05", at: day(4)},
		// A priced buy that names no security marks no security as priced.
		{id: "itx-unnamed", account: "acct-ira", typ: "buy", subtype: "buy",
			amount: "-10", quantity: "1", price: "10", at: day(4)},
	} {
		inv(t, db, r)
	}

	batches := collectSnapshots(t, openConn(t, path))
	positions := map[string]canonical.PositionChange{}
	for _, pos := range positionsAt(batches, runAt(10)) {
		if pos.InstrumentExternalID != nil {
			positions[*pos.InstrumentExternalID] = pos
		}
	}
	for key, value := range map[string]string{"plaid:sec-note": "5100", "plaid:sec-held": "1960"} {
		pos, ok := positions[key]
		if !ok || pos.AssetClass != canonical.AssetClassOther ||
			!strings.Contains(string(pos.Payload), `"source_type":"cash"`) {
			t.Errorf("%s = %+v, want an (other, other) position keeping Plaid's type", key, pos)
			continue
		}
		assertAmount(t, key+" value", pos.MarketValue, value)
	}
	if cash := cashAt(batches, runAt(10), "acct-ira", canonical.BalanceKindCurrent); len(cash) != 1 ||
		!cash[0].Amount.Equal(dec(t, "350")) {
		t.Errorf("cash = %+v, want the 350 of the two cash lines", cash)
	}

	got := collectTransactions(t, openConn(t, path))
	for id, want := range map[string]canonical.TxKind{
		"itx-buy": canonical.TxKindBuy, "itx-coupon": canonical.TxKindInterest} {
		tx := got[id]
		if tx.Kind != want || tx.InstrumentExternalID == nil || *tx.InstrumentExternalID != "plaid:sec-note" {
			t.Errorf("%s = %q on %v, want %q on plaid:sec-note", id, tx.Kind, tx.InstrumentExternalID, want)
		}
	}
	assertAmount(t, "buy quantity", got["itx-buy"].Quantity, "5000")
	if tx := got["itx-interest"]; tx.Kind != canonical.TxKindInterest || tx.InstrumentExternalID != nil {
		t.Errorf("itx-interest = %q on %v, want interest on the cash", tx.Kind, tx.InstrumentExternalID)
	}
}

// A movement of a security at no worth Plaid states is a corporate action
// where the security is a derivative (an option that expired) or has a
// corporate action in the account that day (the other leg of a merger).
// Any other is marked unvalued.
func TestMovementsAtNoWorth(t *testing.T) {
	path, db := newFixture(t)
	seedItem(t, db, runAt(10))
	acct(t, db, runAt(10), "acct-roth", "investment", "roth", "0")
	sec(t, db, "sec-call", "PLACEHOLDER CALL", "EXA311219C00050000", "derivative")
	sec(t, db, "sec-fund", "PLACEHOLDER FUND ETF", "EXF", "etf")
	sec(t, db, "sec-new", "PLACEHOLDER NEW CORP", "EXN", "equity")
	move := func(id, account, security, subtype, quantity, price string, d int) invRow {
		return invRow{id: id, account: account, security: security, typ: "transfer",
			subtype: subtype, amount: "0", quantity: quantity, price: price, at: day(d)}
	}
	rows := []invRow{
		move("itx-expire", "acct-ira", "sec-call", "transfer", "-3", "0", 3),
		move("itx-merge-out", "acct-ira", "sec-fund", "merger", "-40", "0", 4),
		move("itx-merge-in", "acct-ira", "sec-fund", "transfer", "40", "0", 4),
		move("itx-roth-in", "acct-roth", "sec-fund", "transfer", "10", "0", 4),
		move("itx-receipt", "acct-ira", "sec-new", "transfer", "50", "", 5),
		move("itx-priced", "acct-ira", "sec-new", "transfer", "-5", "20", 6),
	}
	for _, r := range rows {
		inv(t, db, r)
	}
	got := collectTransactions(t, openConn(t, path))
	for id, want := range map[string]struct {
		kind     canonical.TxKind
		net      string
		unvalued bool
	}{
		"itx-expire":    {canonical.TxKindCorporateAction, "0", false},
		"itx-merge-out": {canonical.TxKindCorporateAction, "0", false},
		"itx-merge-in":  {canonical.TxKindCorporateAction, "0", false},
		"itx-roth-in":   {canonical.TxKindTransferIn, "0", true},
		"itx-receipt":   {canonical.TxKindTransferIn, "0", true},
		"itx-priced":    {canonical.TxKindTransferOut, "-100", false},
	} {
		tx := got[id]
		if tx.Kind != want.kind {
			t.Errorf("%s kind = %q, want %q", id, tx.Kind, want.kind)
		}
		assertAmount(t, id+" net", tx.NetAmount, want.net)
		if has := strings.Contains(string(tx.Payload), `"unvalued":true`); has != want.unvalued {
			t.Errorf("%s payload = %s, want unvalued %v", id, tx.Payload, want.unvalued)
		}
	}
	// A movement keeps the quantity it moved.
	assertAmount(t, "itx-receipt quantity", got["itx-receipt"].Quantity, "50")
}

// Plaid dates a trade at its settlement and states its trade date apart. A
// buy or a sell books at the UTC day of its trade date. Every other row,
// and a trade whose stated time is past its posting date, books at the
// posting date. No trade books before the oldest window the ledger was
// read in.
func TestTradesBookAtTheirTradeDate(t *testing.T) {
	path, db := newFixture(t)
	seedItem(t, db, runAt(10)) // the ledger's window starts at day 1
	trade := func(id, typ, amount, quantity string, posted, traded int64) invRow {
		return invRow{id: id, account: "acct-ira", security: "sec-eq", typ: typ, subtype: typ,
			amount: amount, quantity: quantity, price: "100", at: posted, traded: traded}
	}
	rows := []invRow{
		trade("itx-buy", "buy", "-100", "1", day(5), day(3)+15*3600),
		trade("itx-sell", "sell", "100", "-1", day(6), day(4)),
		trade("itx-untimed", "sell", "100", "-1", day(6), 0),
		trade("itx-late", "buy", "-100", "1", day(5), day(6)),
		trade("itx-early", "buy", "-100", "1", day(2), day(0)),
		{id: "itx-div", account: "acct-ira", security: "sec-eq", typ: "cash", subtype: "dividend",
			amount: "8", at: day(5), traded: day(3)},
	}
	for _, r := range rows {
		inv(t, db, r)
	}
	got := collectTransactions(t, openConn(t, path))
	for id, want := range map[string]int64{"itx-buy": day(3), "itx-sell": day(4),
		"itx-untimed": day(6), "itx-late": day(5), "itx-early": day(1), "itx-div": day(5)} {
		if at := got[id].OccurredAt; at != want {
			t.Errorf("%s books at %d, want %d", id, at, want)
		}
	}
}

// A trade date opens the transaction range, and the span over which the
// account and the instrument are seen.
func TestATradeDateOpensTheRanges(t *testing.T) {
	path, db := newFixture(t)
	seedItem(t, db, runAt(10))
	inv(t, db, invRow{id: "itx-buy", account: "acct-ira", security: "sec-eq", typ: "buy",
		subtype: "buy", amount: "-100", quantity: "1", price: "100", at: day(5), traded: day(3)})
	conn := openConn(t, path)
	if s := status(t, conn); s.OldestTransactionAt != day(3) || s.LatestTransactionAt != day(3) {
		t.Errorf("transaction range = [%d, %d], want the trade date %d", s.OldestTransactionAt,
			s.LatestTransactionAt, day(3))
	}
	batches := collectSnapshots(t, conn)
	var account, instrument bool
	for _, a := range accountChanges(batches, "acct-ira") {
		account = account || a.FirstSeenAt == day(3)
	}
	for _, b := range batches {
		for _, i := range b.Instruments {
			instrument = instrument || i.InstrumentExternalID == "EXA" && i.FirstSeenAt == day(3)
		}
	}
	if !account || !instrument {
		t.Errorf("seen from the trade date: acct-ira %v, EXA %v", account, instrument)
	}
}

// A trade's gross amount is its net amount before the fees Plaid states.
func TestATradesGrossAmountIsBeforeFees(t *testing.T) {
	path, db := newFixture(t)
	seedItem(t, db, runAt(10))
	rows := []invRow{
		{id: "itx-buy", typ: "buy", subtype: "buy", amount: "-1005", quantity: "10", fees: "5"},
		{id: "itx-sell", typ: "sell", subtype: "sell", amount: "995", quantity: "-10", fees: "5"},
		{id: "itx-free", typ: "sell", subtype: "sell", amount: "1000", quantity: "-10", fees: "0"},
		{id: "itx-unstated", typ: "sell", subtype: "sell", amount: "1000", quantity: "-10"},
		{id: "itx-div", typ: "cash", subtype: "dividend", amount: "8", fees: "1"},
	}
	for _, r := range rows {
		r.account, r.security, r.price, r.at = "acct-ira", "sec-eq", "100", day(3)
		inv(t, db, r)
	}
	got := collectTransactions(t, openConn(t, path))
	for id, want := range map[string][2]string{"itx-buy": {"-1000", "-1005"},
		"itx-sell": {"1000", "995"}, "itx-free": {"1000", "1000"},
		"itx-unstated": {"1000", "1000"}, "itx-div": {"8", "8"}} {
		assertAmount(t, id+" gross", got[id].GrossAmount, want[0])
		assertAmount(t, id+" net", got[id].NetAmount, want[1])
	}
}

// A cancel row reverses the row it cancels: that row's kind, and its amount
// negated, so the pair nets inside one kind.
func TestACancelNetsInsideTheKindItCancels(t *testing.T) {
	path, db := newFixture(t)
	seedItem(t, db, runAt(10))
	sec(t, db, "sec-old", "PLACEHOLDER SOLD CORP", "EXB", "equity")
	rows := []invRow{
		{id: "itx-buy", account: "acct-ira", security: "sec-eq", typ: "buy", subtype: "buy",
			amount: "-1500", quantity: "10", price: "150", at: day(2)},
		{id: "itx-buy-x", account: "acct-ira", security: "sec-eq", typ: "cancel",
			subtype: "cancel", amount: "1500", quantity: "-10", price: "150",
			cancels: "itx-buy", at: day(3)},
		{id: "itx-tin", account: "acct-ira", security: "sec-old", typ: "transfer",
			subtype: "transfer", amount: "0", quantity: "5", price: "20", at: day(2)},
		{id: "itx-tin-x", account: "acct-ira", security: "sec-old", typ: "cancel",
			subtype: "cancel", amount: "0", quantity: "-5", price: "20",
			cancels: "itx-tin", at: day(3)},
		{id: "itx-dep", account: "acct-ira", security: "sec-cash", typ: "cash",
			subtype: "deposit", amount: "1200", quantity: "-1200", price: "1", at: day(2)},
		{id: "itx-dep-x", account: "acct-ira", security: "sec-cash", typ: "cancel",
			subtype: "cancel", amount: "-1200", quantity: "1200", price: "1",
			cancels: "itx-dep", at: day(3)},
		{id: "itx-orphan-x", account: "acct-ira", security: "sec-eq", typ: "cancel",
			subtype: "cancel", amount: "7", cancels: "itx-gone", at: day(3)},
		// A cancel that states neither a quantity nor an amount still
		// reverses the transfer it cancels.
		{id: "itx-tout", account: "acct-ira", security: "sec-old", typ: "transfer",
			subtype: "transfer", amount: "0", quantity: "-2", price: "30", at: day(2)},
		{id: "itx-tout-x", account: "acct-ira", security: "sec-old", typ: "cancel",
			subtype: "cancel", amount: "0", price: "30", cancels: "itx-tout", at: day(3)},
	}
	for _, r := range rows {
		inv(t, db, r)
	}
	got := collectTransactions(t, openConn(t, path))
	for orig, kind := range map[string]canonical.TxKind{"itx-buy": canonical.TxKindBuy,
		"itx-tin": canonical.TxKindTransferIn, "itx-dep": canonical.TxKindDeposit,
		"itx-tout": canonical.TxKindTransferOut} {
		a, b := got[orig], got[orig+"-x"]
		if a.Kind != kind || b.Kind != kind {
			t.Errorf("%s = %q and its cancel = %q, want both %q", orig, a.Kind, b.Kind, kind)
		}
		if a.NetAmount == nil || b.NetAmount == nil || !a.NetAmount.Add(*b.NetAmount).IsZero() {
			t.Errorf("%s nets to %v + %v, want zero", orig, a.NetAmount, b.NetAmount)
		}
	}
	orphan := got["itx-orphan-x"]
	if orphan.Kind != canonical.TxKindOther ||
		!strings.Contains(string(orphan.Payload), `"source_kind":"cancel/cancel"`) {
		t.Errorf("a cancel of a row silver does not hold = %+v", orphan)
	}
	assertAmount(t, "itx-orphan-x net", orphan.NetAmount, "7")
}

func TestAnInstrumentNamedOnlyByTransactionsTravelsOnTheSnapshotStream(t *testing.T) {
	path, db := newFixture(t)
	seedItem(t, db, runAt(10))
	sec(t, db, "sec-old", "PLACEHOLDER SOLD CORP", "EXB", "equity")
	inv(t, db, invRow{id: "itx-b", account: "acct-ira", security: "sec-old", typ: "buy",
		subtype: "buy", amount: "-100", quantity: "1", price: "100", at: day(2)})
	inv(t, db, invRow{id: "itx-s", account: "acct-ira", security: "sec-old", typ: "sell",
		subtype: "sell", amount: "120", quantity: "-1", price: "120", at: day(4)})
	batches := collectSnapshots(t, openConn(t, path))
	spanned := false
	for _, a := range accountChanges(batches, "acct-ira") {
		spanned = spanned || (a.FirstSeenAt == day(2) && a.LastSeenAt == day(4))
	}
	if !spanned {
		t.Errorf("acct-ira is not seen over its trades' span: %+v", accountChanges(batches, "acct-ira"))
	}
	for _, b := range batches {
		for _, i := range b.Instruments {
			if i.InstrumentExternalID == "EXB" {
				if i.FirstSeenAt != day(2) || i.LastSeenAt != day(4) {
					t.Errorf("EXB seen over [%d, %d], want the trades' span", i.FirstSeenAt, i.LastSeenAt)
				}
				return
			}
		}
	}
	t.Fatal("EXB, named only by trades, is not on the snapshot stream")
}

// A first run can read the bank ledger while Plaid is still assembling the
// holdings. No run carries snapshots then, and the accounts the ledger
// names still need a batch to travel on.
func TestATransactionOnlyWindowGetsABatch(t *testing.T) {
	path, db := newFixture(t)
	run(t, db, runAt(10), day(1), map[string]string{"holdings": "not_ready",
		"investment_transactions": "not_ready"})
	acct(t, db, runAt(10), "acct-checking", "depository", "checking", "100")
	bank(t, db, bankRow{id: "tx-1", account: "acct-checking", amount: "-5", name: "Kiosk", at: day(3)})
	batches := collectSnapshots(t, openConn(t, path))
	if len(batches) != 1 {
		t.Fatalf("batches = %+v, want one", batches)
	}
	b := batches[0]
	if len(b.Positions)+len(b.CashBalances) != 0 || len(b.Accounts) != 1 ||
		b.Accounts[0].AccountExternalID != "acct-checking" || b.Accounts[0].FirstSeenAt != day(3) {
		t.Errorf("batch = %+v, want only the checking account, seen at its transaction", b)
	}
}

// ---- the load clock ---------------------------------------------------------

func TestStatusAndChangeWindow(t *testing.T) {
	path, db := newFixture(t)
	seedItem(t, db, runAt(10))
	liability(t, db, runAt(10), "acct-card", "380", day(5))
	inv(t, db, invRow{id: "itx-buy", account: "acct-ira", security: "sec-eq", typ: "buy",
		subtype: "buy", amount: "-1", quantity: "1", price: "1", at: day(3)})
	bank(t, db, bankRow{id: "tx-1", account: "acct-checking", amount: "-5", name: "Kiosk", at: day(7)})
	conn := openConn(t, path)
	s := status(t, conn)
	if s.LatestChangeNumber != runAt(10) {
		t.Errorf("change number = %d, want the run's start", s.LatestChangeNumber)
	}
	// The window start of a fetched ledger is the oldest date touched.
	if s.OldestSnapshotAt != day(1) || s.LatestSnapshotAt != runAt(10) {
		t.Errorf("snapshot range = [%d, %d], want [%d, %d]", s.OldestSnapshotAt,
			s.LatestSnapshotAt, day(1), runAt(10))
	}
	// The transaction range spans both ledgers.
	if s.OldestTransactionAt != day(3) || s.LatestTransactionAt != day(7) {
		t.Errorf("transaction range = [%d, %d], want [%d, %d]", s.OldestTransactionAt,
			s.LatestTransactionAt, day(3), day(7))
	}

	w := window(t, conn)
	if !w.HasChanges || w.Start != day(1) || w.End != runAt(10) || w.NewChangeNumber != runAt(10) {
		t.Errorf("window = %+v", w)
	}
	idle, err := conn.ChangeWindow(context.Background(), w.NewChangeNumber)
	if err != nil {
		t.Fatalf("ChangeWindow: %v", err)
	}
	if idle.HasChanges {
		t.Errorf("a load with nothing new has changes: %+v", idle)
	}
}

// An Item linked for investments alone has no bank ledger, and still has a
// transaction range.
func TestAnInvestmentOnlyItemHasATransactionRange(t *testing.T) {
	path, db := newFixture(t)
	seedItem(t, db, runAt(10))
	inv(t, db, invRow{id: "itx-buy", account: "acct-ira", security: "sec-eq", typ: "buy",
		subtype: "buy", amount: "-1", quantity: "1", price: "1", at: day(3)})
	if s := status(t, openConn(t, path)); s.OldestTransactionAt != day(3) || s.LatestTransactionAt != day(3) {
		t.Errorf("transaction range = [%d, %d]", s.OldestTransactionAt, s.LatestTransactionAt)
	}
}

func TestEmptySilverIsNotAnError(t *testing.T) {
	path, _ := newFixture(t)
	conn := openConn(t, path)
	if s := status(t, conn); s.LatestChangeNumber != -1 || s.OldestTransactionAt != -1 {
		t.Errorf("empty status = %+v", s)
	}
	if w := window(t, conn); w.HasChanges {
		t.Errorf("empty window = %+v", w)
	}
}
