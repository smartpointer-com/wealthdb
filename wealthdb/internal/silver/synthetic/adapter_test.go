package synthetic

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"

	_ "modernc.org/sqlite"
)

//go:embed testdata/silver_schema.sql
var silverSchemaSQL string

// day is a calendar day of an invented month at UTC midnight, the instant a
// snapshot is stamped at.
func day(d int) int64 {
	return time.Date(2031, 1, d, 0, 0, 0, 0, time.UTC).Unix()
}

func newFixture(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := t.TempDir() + "/synthetic.db"
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

const (
	insertPosition = `INSERT INTO positions
    (snapshot_at, account_id, position_key, instrument_id, asset_class, vehicle,
     currency, quantity, market_value, book_value, accrued_interest, acquisition_date, payload)
    VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`
	insertCash = `INSERT INTO cash_balances
    (snapshot_at, account_id, currency, balance_kind, amount, payload) VALUES (?,?,?,?,?,?)`
	insertFx = `INSERT INTO fx_rates
    (snapshot_at, base_currency, quote_currency, mid_rate, bid_rate, ask_rate, payload)
    VALUES (?,?,?,?,?,?,?)`
	insertTxn = `INSERT INTO transactions
    (transaction_id, occurred_at, account_id, instrument_id, asset_class, vehicle,
     instrument_hint, kind, currency, gross_amount, net_amount, quantity, price,
     description, memo, counterparty, provider_category, check_number, payload)
    VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
)

// txn is one transactions row; the zero value of an optional field is NULL.
type txn struct {
	id, account, instrument, assetClass, vehicle, hint string
	kind, gross, net, quantity, price                  string
	description, memo, counterparty, category, check   string
	payload                                            string
	at                                                 int64
}

func null(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func insertTx(t *testing.T, db *sql.DB, x txn) {
	t.Helper()
	payload := x.payload
	if payload == "" {
		payload = "{}"
	}
	exec(t, db, insertTxn, x.id, x.at, x.account, null(x.instrument), null(x.assetClass),
		null(x.vehicle), null(x.hint), x.kind, "USD", null(x.gross), null(x.net),
		null(x.quantity), null(x.price), null(x.description), null(x.memo),
		null(x.counterparty), null(x.category), null(x.check), payload)
}

// seed builds a household of two appended runs, every id and figure
// invented:
//
//   - run 1 covers days 1-2, run 2 days 3-4;
//   - day 1 carries only an fx rate (and a trade), so the snapshot range
//     opens on a table other than positions;
//   - inst-eq is renamed from day 3, and inst-late's only version starts
//     after the first position that names it;
//   - acct-odd, inst-late and one cash row carry values outside their
//     vocabularies;
//   - acct-side and inst-bond are named only by run 2's transactions.
func seed(t *testing.T, db *sql.DB) {
	t.Helper()
	exec(t, db, `INSERT INTO meta (key, value) VALUES ('schema_version', '1')`)
	exec(t, db, `INSERT INTO dump_runs VALUES (1, ?, ?, '2031-01-02'), (2, ?, ?, '2031-01-04')`,
		day(1), day(2), day(3), day(4))

	exec(t, db, `INSERT INTO portfolios (portfolio_id, display_name, base_currency, nickname, payload) VALUES
        ('pf-main', 'Example Household', 'USD', NULL, '{}'),
        ('pf-side', 'Example Side Book', 'USD', 'Side', '{}')`)
	exec(t, db, `INSERT INTO accounts (account_id, account_kind, display_name, base_currency,
            nickname, account_category, tax_wrapper, management_style, portfolio_id, payload) VALUES
        ('acct-cash', 'cash',       'Example Checking',  'USD', 'Everyday', 'Checking',
            'taxable_personal', 'self_directed', 'pf-main', '{"bank":"Example Bank"}'),
        ('acct-brok', 'brokerage',  'Example Brokerage', 'USD', NULL, '',
            'roth_ira', 'advisory', 'pf-main', '{}'),
        ('acct-odd',  'piggy_bank', 'Example Jar',       'EUR', NULL, NULL,
            'mattress', 'whim', NULL, '{}'),
        ('acct-side', 'cash',       'Example Side Cash', 'USD', NULL, NULL,
            NULL, NULL, 'pf-side', '{}')`)
	exec(t, db, `INSERT INTO instruments (instrument_id, valid_from, asset_class, vehicle,
            isin, cusip, symbol, name, currency, payload) VALUES
        ('inst-eq',   ?, 'public_equity', 'etf',  NULL, NULL, 'EXIX', 'Example Index Fund',           'USD', '{}'),
        ('inst-eq',   ?, 'public_equity', 'etf',  NULL, NULL, 'EXIY', 'Example Index Fund (Renamed)', 'USD', '{}'),
        ('inst-late', ?, 'moon_rocks',    'etf',  NULL, NULL, NULL,   'Example Oddity',               'EUR', '{}'),
        ('inst-bond', ?, 'fixed_income',  'fund', NULL, NULL, 'EXBF', 'Example Bond Fund',            'USD', '{}'),
        ('inst-bond', ?, 'fixed_income',  'fund', NULL, NULL, 'EXBG', 'Example Bond Fund II',         'USD', '{}')`,
		day(1), day(3), day(3), day(1), day(4))

	// Day 1: an fx rate and nothing else snapshot-grain.
	exec(t, db, insertFx, day(1), "USD", "EUR", "1.1", nil, nil, "{}")
	// Day 2.
	exec(t, db, insertPosition, day(2), "acct-brok", "inst-eq", "inst-eq", "public_equity", "etf",
		"USD", "10", "1000", "900", nil, nil, "{}")
	exec(t, db, insertPosition, day(2), "acct-odd", "inst-late", "inst-late", "moon_rocks", "etf",
		"EUR", "1", "5", nil, nil, nil, "{}")
	exec(t, db, insertCash, day(2), "acct-cash", "USD", "closing", "500", "{}")
	// Day 3.
	exec(t, db, insertPosition, day(3), "acct-brok", "inst-eq", "inst-eq", "public_equity", "etf",
		"USD", "10", "1020", "900", nil, nil, "{}")
	exec(t, db, insertCash, day(3), "acct-cash", "USD", "closing", "400", "{}")
	exec(t, db, insertFx, day(3), "USD", "EUR", "1.2", "1.19", "1.21", `{"source":"example"}`)
	// Day 4.
	exec(t, db, insertPosition, day(4), "acct-brok", "inst-eq", "inst-eq", "public_equity", "etf",
		"USD", "12", "1250", "1100", "3.5", "2030-06-30", `{"lot":"a"}`)
	exec(t, db, insertCash, day(4), "acct-cash", "USD", "closing", "350", "{}")
	exec(t, db, insertCash, day(4), "acct-odd", "EUR", "wobbly", "7", "{}")

	for _, x := range []txn{
		// Stored with the wrong sign for a buy; the instrument is known,
		// so the hint is not carried.
		{id: "t-buy", at: day(1), account: "acct-brok", instrument: "inst-eq", hint: "EXIX",
			kind: "buy", gross: "100", net: "100", quantity: "1", price: "100"},
		// Stored negative; a card payment pays the balance up.
		{id: "t-card", at: day(2), account: "acct-cash", kind: "card_payment", net: "-50"},
		// Interest keeps its own sign.
		{id: "t-int", at: day(3), account: "acct-cash", kind: "interest", net: "-2.5"},
		{id: "t-chk", at: day(3), account: "acct-cash", kind: "withdrawal", net: "-40",
			description: "Example rent", memo: "flat 1", counterparty: "Example Landlord",
			category: "RENT_AND_UTILITIES_RENT", check: "1001",
			payload: `{"bank_ref":"REF-0001","counter_account":"XX00EXAMPLE0001"}`},
		{id: "t-bond-1", at: day(3), account: "acct-side", instrument: "inst-bond",
			kind: "dividend", net: "5"},
		// A cheque number on an inflow is a slip, not a cheque.
		{id: "t-chk-in", at: day(4), account: "acct-cash", kind: "deposit", net: "25", check: "2002"},
		{id: "t-weird", at: day(4), account: "acct-cash", kind: "teleport", net: "-3"},
		// No instrument: the hint rides along. The pair is not one the
		// taxonomy admits.
		{id: "t-pair", at: day(4), account: "acct-brok", hint: "EXAMPLE-TICKER",
			assetClass: "public_equity", vehicle: "mortgage", kind: "sell", net: "-30"},
		{id: "t-bond-2", at: day(4), account: "acct-side", instrument: "inst-bond",
			kind: "dividend", net: "6"},
	} {
		insertTx(t, db, x)
	}
}

func collectSnapshots(t *testing.T, conn silver.Connection, w canonical.Window) []canonical.SnapshotBatch {
	t.Helper()
	stream, err := conn.Snapshots(context.Background(), w)
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	defer stream.Close()
	var batches []canonical.SnapshotBatch
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Snapshots Next: %v", err)
		}
		if len(b.Accounts)+len(b.Portfolios)+len(b.Instruments)+len(b.Positions)+
			len(b.CashBalances)+len(b.FxRates) > 0 {
			batches = append(batches, b)
		}
		if !more {
			return batches
		}
	}
}

func collectTransactions(t *testing.T, conn silver.Connection, w canonical.Window) []canonical.TransactionChange {
	t.Helper()
	stream, err := conn.Transactions(context.Background(), w)
	if err != nil {
		t.Fatalf("Transactions: %v", err)
	}
	defer stream.Close()
	var out []canonical.TransactionChange
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Transactions Next: %v", err)
		}
		out = append(out, b.Transactions...)
		if !more {
			return out
		}
	}
}

func window(t *testing.T, conn silver.Connection, since int64) canonical.Window {
	t.Helper()
	w, err := conn.ChangeWindow(context.Background(), since)
	if err != nil {
		t.Fatalf("ChangeWindow(%d): %v", since, err)
	}
	return w
}

func payloadOf(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	m := map[string]any{}
	if len(raw) == 0 {
		return m
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("payload %s: %v", raw, err)
	}
	return m
}

func dec(d *canonical.Decimal) string {
	if d == nil {
		return "<nil>"
	}
	return d.String()
}

func str(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func accountIDs(as []canonical.AccountChange) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.AccountExternalID)
	}
	return out
}

func TestKind(t *testing.T) {
	if got := (&Adapter{}).Kind(); got != "synthetic" {
		t.Fatalf("Kind = %q, want synthetic", got)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	path, _ := newFixture(t)
	conn, err := (&Adapter{}).Open(context.Background(), silver.OpenSpec{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestStatusEmpty(t *testing.T) {
	path, _ := newFixture(t)
	s, err := openConn(t, path).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := canonical.Status{
		OldestSnapshotAt: -1, LatestSnapshotAt: -1,
		OldestTransactionAt: -1, LatestTransactionAt: -1,
		LatestChangeNumber: -1,
	}
	if s != want {
		t.Errorf("Status = %+v, want %+v", s, want)
	}
	if w := window(t, openConn(t, path), -1); w.HasChanges || w.NewChangeNumber != -1 {
		t.Errorf("ChangeWindow(-1) on an empty silver = %+v, want no changes", w)
	}
}

func TestStatus(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	s, err := openConn(t, path).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Day 1 holds only an fx rate: the snapshot range is the union of the
	// three snapshot-grain tables, not the positions alone.
	want := canonical.Status{
		OldestSnapshotAt: day(1), LatestSnapshotAt: day(4),
		OldestTransactionAt: day(1), LatestTransactionAt: day(4),
		LatestChangeNumber: 2,
	}
	if s != want {
		t.Errorf("Status = %+v, want %+v", s, want)
	}
}

// TestChangeWindowIsIncremental pins the kind's contract: a fresh gold takes
// every run at once, a later load takes only the runs appended since, and a
// load at the latest run is idle.
func TestChangeWindowIsIncremental(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	conn := openConn(t, path)

	all := window(t, conn, -1)
	if want := (canonical.Window{Start: day(1), End: day(4), NewChangeNumber: 2, HasChanges: true}); all != want {
		t.Errorf("ChangeWindow(-1) = %+v, want %+v", all, want)
	}
	// Past the first run, only the second run's days: gold's windowed
	// delete leaves days 1-2 as the first load wrote them.
	appended := window(t, conn, 1)
	if want := (canonical.Window{Start: day(3), End: day(4), NewChangeNumber: 2, HasChanges: true}); appended != want {
		t.Errorf("ChangeWindow(1) = %+v, want %+v", appended, want)
	}
	idle := window(t, conn, 2)
	if idle.HasChanges || idle.NewChangeNumber != 2 {
		t.Errorf("ChangeWindow(2) = %+v, want no changes at change number 2", idle)
	}

	// An idle window yields nothing on either stream.
	if got := collectSnapshots(t, conn, idle); len(got) != 0 {
		t.Errorf("idle Snapshots = %d batches, want none", len(got))
	}
	if got := collectTransactions(t, conn, idle); len(got) != 0 {
		t.Errorf("idle Transactions = %d rows, want none", len(got))
	}
}

func TestSnapshotsGroupByInstant(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	conn := openConn(t, path)
	batches := collectSnapshots(t, conn, window(t, conn, -1))

	if len(batches) != 4 {
		t.Fatalf("batches = %d, want one per instant (4)", len(batches))
	}
	for i, b := range batches {
		for _, p := range b.Positions {
			if p.SnapshotAt != day(i+1) {
				t.Errorf("batch %d holds a position at %d", i, p.SnapshotAt)
			}
		}
		for _, c := range b.CashBalances {
			if c.SnapshotAt != day(i+1) {
				t.Errorf("batch %d holds a cash balance at %d", i, c.SnapshotAt)
			}
		}
		for _, f := range b.FxRates {
			if f.SnapshotAt != day(i+1) {
				t.Errorf("batch %d holds an fx rate at %d", i, f.SnapshotAt)
			}
		}
	}

	// Day 1: the fx rate alone — no fact names an account or an instrument.
	d1 := batches[0]
	if len(d1.FxRates) != 1 || len(d1.Positions)+len(d1.CashBalances) != 0 {
		t.Errorf("day 1 = %d fx / %d positions / %d cash, want 1/0/0",
			len(d1.FxRates), len(d1.Positions), len(d1.CashBalances))
	}
	if len(d1.Accounts)+len(d1.Instruments)+len(d1.Portfolios) != 0 {
		t.Errorf("day 1 emitted dimensions no fact of it references")
	}
	if got := d1.FxRates[0]; got.BaseCurrency != "USD" || got.QuoteCurrency != "EUR" ||
		got.MidRate.String() != "1.1" || got.BidRate != nil || got.AskRate != nil {
		t.Errorf("day 1 fx = %+v", got)
	}

	// Day 2: the complete instant, and every dimension it references, seen
	// at that instant.
	d2 := batches[1]
	if len(d2.Positions) != 2 || len(d2.CashBalances) != 1 || len(d2.FxRates) != 0 {
		t.Fatalf("day 2 = %d positions / %d cash / %d fx, want 2/1/0",
			len(d2.Positions), len(d2.CashBalances), len(d2.FxRates))
	}
	if got := strings.Join(accountIDs(d2.Accounts), ","); got != "acct-brok,acct-cash,acct-odd" {
		t.Errorf("day 2 accounts = %s, want every account its facts name, sorted", got)
	}
	for _, a := range d2.Accounts {
		if a.FirstSeenAt != day(2) || a.LastSeenAt != day(2) {
			t.Errorf("%s seen [%d,%d], want the instant", a.AccountExternalID, a.FirstSeenAt, a.LastSeenAt)
		}
	}
	if len(d2.Portfolios) != 1 || d2.Portfolios[0].PortfolioExternalID != "pf-main" ||
		d2.Portfolios[0].FirstSeenAt != day(2) {
		t.Errorf("day 2 portfolios = %+v, want pf-main seen at day 2", d2.Portfolios)
	}
	if len(d2.Instruments) != 2 || d2.Instruments[0].InstrumentExternalID != "inst-eq" ||
		d2.Instruments[1].InstrumentExternalID != "inst-late" {
		t.Errorf("day 2 instruments = %+v, want inst-eq, inst-late", d2.Instruments)
	}

	brok := d2.Accounts[0]
	if brok.AccountKind != canonical.AccountKindBrokerage ||
		brok.TaxWrapper == nil || *brok.TaxWrapper != canonical.TaxWrapperRothIRA ||
		brok.ManagementStyle == nil || *brok.ManagementStyle != canonical.ManagementStyleAdvisory ||
		str(brok.PortfolioExternalID) != "pf-main" || brok.AccountCategory != nil {
		t.Errorf("acct-brok = %+v, want the row's taxonomy and portfolio, and an empty category absent", brok)
	}
	cash := d2.Accounts[1]
	if str(cash.DisplayName) != "Example Checking" || str(cash.Nickname) != "Everyday" ||
		str(cash.BaseCurrency) != "USD" || str(cash.AccountCategory) != "Checking" ||
		payloadOf(t, cash.Payload)["bank"] != "Example Bank" {
		t.Errorf("acct-cash = %+v, want its attributes and payload passed through", cash)
	}

	// Day 4 carries the position's optional columns through.
	var lot canonical.PositionChange
	for _, p := range batches[3].Positions {
		if p.AccountExternalID == "acct-brok" {
			lot = p
		}
	}
	if dec(lot.Quantity) != "12" || dec(lot.MarketValue) != "1250" || dec(lot.BookValue) != "1100" ||
		dec(lot.AccruedInterest) != "3.5" || lot.Currency != "USD" ||
		str(lot.InstrumentExternalID) != "inst-eq" ||
		lot.AcquisitionDate == nil || !lot.AcquisitionDate.Equal(time.Date(2030, 6, 30, 0, 0, 0, 0, time.UTC)) ||
		payloadOf(t, lot.Payload)["lot"] != "a" {
		t.Errorf("day 4 position = %+v", lot)
	}
	if got := batches[2].FxRates; len(got) != 1 || dec(got[0].BidRate) != "1.19" || dec(got[0].AskRate) != "1.21" {
		t.Errorf("day 3 fx = %+v, want bid and ask passed through", got)
	}
}

// TestInstrumentVersionInEffect: an instrument is emitted as the version in
// effect at the instant that references it, and a reference earlier than
// every version gets the earliest.
func TestInstrumentVersionInEffect(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	conn := openConn(t, path)
	batches := collectSnapshots(t, conn, window(t, conn, -1))

	nameAt := func(b canonical.SnapshotBatch, id string) string {
		for _, i := range b.Instruments {
			if i.InstrumentExternalID == id {
				return str(i.Name) + "/" + str(i.Symbol)
			}
		}
		return "<absent>"
	}
	for _, tc := range []struct {
		batch int
		id    string
		want  string
	}{
		{1, "inst-eq", "Example Index Fund/EXIX"},           // day 2: before the rename
		{2, "inst-eq", "Example Index Fund (Renamed)/EXIY"}, // day 3: on it
		{3, "inst-eq", "Example Index Fund (Renamed)/EXIY"}, // day 4: after it
		{1, "inst-late", "Example Oddity/<nil>"},            // day 2: before its only version
	} {
		if got := nameAt(batches[tc.batch], tc.id); got != tc.want {
			t.Errorf("day %d %s = %s, want %s", tc.batch+1, tc.id, got, tc.want)
		}
	}
}

// TestTransactionOnlyDimensions: dimensions travel only on the snapshot
// stream, so an account or an instrument the window's transactions name and
// no snapshot does still arrives, seen across those transactions, with the
// instrument in the version in effect at the latest of them.
func TestTransactionOnlyDimensions(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	conn := openConn(t, path)
	batches := collectSnapshots(t, conn, window(t, conn, 1))

	if len(batches) != 2 {
		t.Fatalf("batches = %d, want days 3 and 4 only", len(batches))
	}
	for _, b := range batches[:1] {
		for _, a := range b.Accounts {
			if a.AccountExternalID == "acct-side" {
				t.Error("a transaction-only account landed on a batch other than the last")
			}
		}
	}
	last := batches[1]
	var side *canonical.AccountChange
	for i := range last.Accounts {
		if last.Accounts[i].AccountExternalID == "acct-side" {
			side = &last.Accounts[i]
		}
	}
	if side == nil {
		t.Fatalf("acct-side missing from the last batch: %v", accountIDs(last.Accounts))
	}
	if side.FirstSeenAt != day(3) || side.LastSeenAt != day(4) {
		t.Errorf("acct-side seen [%d,%d], want its transactions' span [%d,%d]",
			side.FirstSeenAt, side.LastSeenAt, day(3), day(4))
	}
	var sawSide bool
	for _, p := range last.Portfolios {
		if p.PortfolioExternalID == "pf-side" {
			sawSide = true
			if p.FirstSeenAt != day(3) || p.LastSeenAt != day(4) {
				t.Errorf("pf-side seen [%d,%d], want its account's span", p.FirstSeenAt, p.LastSeenAt)
			}
		}
	}
	if !sawSide {
		t.Error("the transaction-only account's portfolio was not emitted")
	}
	var bond *canonical.InstrumentChange
	for i := range last.Instruments {
		if last.Instruments[i].InstrumentExternalID == "inst-bond" {
			bond = &last.Instruments[i]
		}
	}
	if bond == nil {
		t.Fatal("inst-bond missing from the last batch")
	}
	if str(bond.Name) != "Example Bond Fund II" || bond.FirstSeenAt != day(3) || bond.LastSeenAt != day(4) {
		t.Errorf("inst-bond = %s seen [%d,%d], want the day-4 version seen [%d,%d]",
			str(bond.Name), bond.FirstSeenAt, bond.LastSeenAt, day(3), day(4))
	}

	// A dimension a snapshot already carried is not emitted a second time:
	// acct-cash and acct-brok are named by transactions and by snapshots.
	count := map[string]int{}
	for _, b := range batches {
		for _, a := range b.Accounts {
			count[a.AccountExternalID]++
		}
	}
	if count["acct-cash"] != 2 || count["acct-brok"] != 2 {
		t.Errorf("per-instant account emissions = %v, want one per instant", count)
	}
}

// TestTransactionDimensionsJoinAnExistingBatch: day 1 holds a snapshot (an fx
// rate) and a trade whose account and instrument no snapshot of the window
// names, so the dimensions land on that batch in the version in effect then.
func TestTransactionDimensionsJoinAnExistingBatch(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	conn := openConn(t, path)
	batches := collectSnapshots(t, conn,
		canonical.Window{Start: day(1), End: day(1), NewChangeNumber: 1, HasChanges: true})

	if len(batches) != 1 {
		t.Fatalf("batches = %d, want the day-1 batch alone", len(batches))
	}
	b := batches[0]
	if len(b.FxRates) != 1 {
		t.Errorf("the day-1 fx rate is missing: %+v", b)
	}
	if got := strings.Join(accountIDs(b.Accounts), ","); got != "acct-brok" {
		t.Errorf("accounts = %s, want acct-brok", got)
	}
	if len(b.Portfolios) != 1 || b.Portfolios[0].PortfolioExternalID != "pf-main" {
		t.Errorf("portfolios = %+v, want pf-main", b.Portfolios)
	}
	if len(b.Instruments) != 1 || str(b.Instruments[0].Name) != "Example Index Fund" ||
		b.Instruments[0].FirstSeenAt != day(1) {
		t.Errorf("instruments = %+v, want inst-eq's first version seen at day 1", b.Instruments)
	}
}

// TestTransactionOnlyWindowGetsABatch: a window with transactions and no
// snapshot still carries the dimensions they name.
func TestTransactionOnlyWindowGetsABatch(t *testing.T) {
	path, db := newFixture(t)
	exec(t, db, `INSERT INTO dump_runs VALUES (0, ?, ?, '2031-01-01')`, day(1), day(1))
	exec(t, db, `INSERT INTO portfolios (portfolio_id, payload) VALUES ('pf-main', '{}')`)
	exec(t, db, `INSERT INTO accounts (account_id, account_kind, portfolio_id, payload)
        VALUES ('acct-cash', 'cash', 'pf-main', '{}')`)
	insertTx(t, db, txn{id: "t-dep", at: day(1), account: "acct-cash", kind: "deposit", net: "10"})
	conn := openConn(t, path)

	w := window(t, conn, -1)
	if !w.HasChanges || w.NewChangeNumber != 0 {
		t.Fatalf("ChangeWindow(-1) = %+v, want the change-number-0 run", w)
	}
	batches := collectSnapshots(t, conn, w)
	if len(batches) != 1 {
		t.Fatalf("batches = %d, want one carrying the dimensions", len(batches))
	}
	b := batches[0]
	if len(b.Accounts) != 1 || b.Accounts[0].FirstSeenAt != day(1) || len(b.Portfolios) != 1 {
		t.Errorf("batch = %+v, want acct-cash and pf-main seen at day 1", b)
	}
	if len(b.Positions)+len(b.CashBalances)+len(b.FxRates) != 0 {
		t.Errorf("a transaction-only window emitted facts: %+v", b)
	}
}

func TestSnapshotFallbacks(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	conn := openConn(t, path)
	batches := collectSnapshots(t, conn, window(t, conn, -1))

	// An account kind outside the vocabulary becomes `other`; a wrapper or
	// a style outside theirs becomes absent. Each raw value is kept.
	var odd canonical.AccountChange
	for _, a := range batches[1].Accounts {
		if a.AccountExternalID == "acct-odd" {
			odd = a
		}
	}
	if odd.AccountKind != canonical.AccountKindOther || odd.TaxWrapper != nil || odd.ManagementStyle != nil {
		t.Errorf("acct-odd = kind %q wrapper %v style %v, want other/nil/nil",
			odd.AccountKind, odd.TaxWrapper, odd.ManagementStyle)
	}
	p := payloadOf(t, odd.Payload)
	if p["source_account_kind"] != "piggy_bank" || p["source_tax_wrapper"] != "mattress" ||
		p["source_management_style"] != "whim" {
		t.Errorf("acct-odd payload = %v, want the raw values kept", p)
	}
	// A valid account carries no annotations.
	for _, a := range batches[1].Accounts {
		if a.AccountExternalID == "acct-brok" {
			if _, ok := payloadOf(t, a.Payload)["source_account_kind"]; ok {
				t.Error("a valid account was annotated")
			}
		}
	}

	// A pair the taxonomy does not admit becomes (other, other) on the
	// position and on the instrument alike.
	var pos canonical.PositionChange
	for _, x := range batches[1].Positions {
		if x.AccountExternalID == "acct-odd" {
			pos = x
		}
	}
	if pos.AssetClass != canonical.AssetClassOther || pos.Vehicle != canonical.VehicleOther {
		t.Errorf("acct-odd position pair = (%s, %s), want (other, other)", pos.AssetClass, pos.Vehicle)
	}
	if p := payloadOf(t, pos.Payload); p["source_asset_class"] != "moon_rocks" || p["source_vehicle"] != "etf" {
		t.Errorf("position payload = %v, want the raw pair kept", p)
	}
	for _, i := range batches[1].Instruments {
		if i.InstrumentExternalID != "inst-late" {
			continue
		}
		if i.AssetClass != canonical.AssetClassOther || i.Vehicle != canonical.VehicleOther {
			t.Errorf("inst-late pair = (%s, %s), want (other, other)", i.AssetClass, i.Vehicle)
		}
		if p := payloadOf(t, i.Payload); p["source_asset_class"] != "moon_rocks" {
			t.Errorf("instrument payload = %v, want the raw pair kept", p)
		}
	}

	// A balance kind outside the vocabulary becomes `closing`.
	var wobbly canonical.CashBalanceChange
	for _, c := range batches[3].CashBalances {
		if c.AccountExternalID == "acct-odd" {
			wobbly = c
		}
	}
	if wobbly.BalanceKind != canonical.BalanceKindClosing || wobbly.Amount.String() != "7" {
		t.Errorf("acct-odd balance = %+v, want a closing 7", wobbly)
	}
	if p := payloadOf(t, wobbly.Payload); p["source_balance_kind"] != "wobbly" {
		t.Errorf("balance payload = %v, want the raw kind kept", p)
	}
}

func TestTransactions(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	conn := openConn(t, path)
	txs := collectTransactions(t, conn, window(t, conn, -1))

	var order []string
	byID := map[string]canonical.TransactionChange{}
	for _, x := range txs {
		order = append(order, x.TransactionExternalID)
		byID[x.TransactionExternalID] = x
	}
	wantOrder := "t-buy,t-card,t-bond-1,t-chk,t-int,t-bond-2,t-chk-in,t-pair,t-weird"
	if got := strings.Join(order, ","); got != wantOrder {
		t.Errorf("order = %s, want %s (occurred_at, then id)", got, wantOrder)
	}

	// The sign guard: fixed-direction kinds come out signed their way, a
	// context-dependent kind keeps the row's sign.
	for _, tc := range []struct {
		id, kind, net string
	}{
		{"t-buy", "buy", "-100"},
		{"t-card", "card_payment", "50"},
		{"t-int", "interest", "-2.5"},
		{"t-chk", "withdrawal", "-40"},
		{"t-chk-in", "deposit", "25"},
		{"t-pair", "sell", "30"},
		{"t-weird", "other", "-3"},
	} {
		x := byID[tc.id]
		if string(x.Kind) != tc.kind || dec(x.NetAmount) != tc.net {
			t.Errorf("%s = (%s, %s), want (%s, %s)", tc.id, x.Kind, dec(x.NetAmount), tc.kind, tc.net)
		}
	}
	buy := byID["t-buy"]
	if dec(buy.GrossAmount) != "-100" || dec(buy.Quantity) != "1" || dec(buy.Price) != "100" {
		t.Errorf("t-buy gross/qty/price = %s/%s/%s", dec(buy.GrossAmount), dec(buy.Quantity), dec(buy.Price))
	}
	if str(buy.InstrumentExternalID) != "inst-eq" || buy.InstrumentHint != "" {
		t.Errorf("t-buy instrument %s hint %q, want the instrument and no hint",
			str(buy.InstrumentExternalID), buy.InstrumentHint)
	}
	if byID["t-card"].GrossAmount != nil {
		t.Error("an absent gross amount must stay absent")
	}

	// Pass-through of the text fields and the payload keys gold reads.
	chk := byID["t-chk"]
	if str(chk.Description) != "Example rent" || str(chk.Memo) != "flat 1" ||
		str(chk.Counterparty) != "Example Landlord" ||
		str(chk.ProviderCategory) != "RENT_AND_UTILITIES_RENT" || str(chk.CheckNumber) != "1001" {
		t.Errorf("t-chk text = desc %s memo %s cp %s cat %s chk %s", str(chk.Description),
			str(chk.Memo), str(chk.Counterparty), str(chk.ProviderCategory), str(chk.CheckNumber))
	}
	if p := payloadOf(t, chk.Payload); p["bank_ref"] != "REF-0001" || p["counter_account"] != "XX00EXAMPLE0001" {
		t.Errorf("t-chk payload = %v, want it verbatim", p)
	}
	if byID["t-chk-in"].CheckNumber != nil {
		t.Error("a cheque number on an inflow was kept")
	}

	// The fallbacks.
	weird := byID["t-weird"]
	if p := payloadOf(t, weird.Payload); p["source_kind"] != "teleport" {
		t.Errorf("t-weird payload = %v, want the raw kind kept", p)
	}
	pair := byID["t-pair"]
	if pair.AssetClass != "" || pair.Vehicle != "" {
		t.Errorf("t-pair pair = (%q, %q), want both empty", pair.AssetClass, pair.Vehicle)
	}
	if p := payloadOf(t, pair.Payload); p["source_asset_class"] != "public_equity" || p["source_vehicle"] != "mortgage" {
		t.Errorf("t-pair payload = %v, want the raw pair kept", p)
	}
	if pair.InstrumentExternalID != nil || pair.InstrumentHint != "EXAMPLE-TICKER" {
		t.Errorf("t-pair instrument %s hint %q, want no instrument and the hint",
			str(pair.InstrumentExternalID), pair.InstrumentHint)
	}
}

func TestTransactionsWindowed(t *testing.T) {
	path, db := newFixture(t)
	seed(t, db)
	conn := openConn(t, path)
	for _, x := range collectTransactions(t, conn, window(t, conn, 1)) {
		if x.OccurredAt < day(3) || x.OccurredAt > day(4) {
			t.Errorf("%s at %d is outside the appended run's window", x.TransactionExternalID, x.OccurredAt)
		}
		if x.TransactionExternalID == "t-buy" || x.TransactionExternalID == "t-card" {
			t.Errorf("%s belongs to the first run", x.TransactionExternalID)
		}
	}
	if n := len(collectTransactions(t, conn, window(t, conn, 1))); n != 7 {
		t.Errorf("appended-run transactions = %d, want 7", n)
	}
}

// TestTradedPairKeepsAValidHalf: a transaction may state one half of the
// pair where it belongs to its own vocabulary, as the gold writer allows.
func TestTradedPairKeepsAValidHalf(t *testing.T) {
	for _, tc := range []struct {
		assetClass, vehicle string
		wantA               canonical.AssetClass
		wantV               canonical.Vehicle
		kept                bool
	}{
		{"", "", "", "", false},
		{"public_equity", "etf", canonical.AssetClassPublicEquity, canonical.VehicleETF, false},
		{"", "option", "", canonical.VehicleOption, false},
		{"public_equity", "", canonical.AssetClassPublicEquity, "", false},
		{"etf", "", "", "", true}, // a wrapper in the exposure column
		{"", "spaceship", "", "", true},
		{"crypto", "mortgage", "", "", true},
	} {
		var extra annotations
		a, v := tradedPair(tc.assetClass, tc.vehicle, &extra)
		if a != tc.wantA || v != tc.wantV || (len(extra) > 0) != tc.kept {
			t.Errorf("tradedPair(%q, %q) = (%q, %q) kept=%v, want (%q, %q) kept=%v",
				tc.assetClass, tc.vehicle, a, v, len(extra) > 0, tc.wantA, tc.wantV, tc.kept)
		}
	}
}

// TestRequiredValuesFailTheLoad: a cash amount or an fx mid rate that does
// not parse would otherwise land as nothing at all, so the load fails and
// names the row.
func TestRequiredValuesFailTheLoad(t *testing.T) {
	for _, tc := range []struct {
		name, insert, want string
		args               []any
	}{
		{"cash amount", insertCash, "amount",
			[]any{day(1), "acct-cash", "USD", "closing", "twelve", "{}"}},
		{"fx mid", insertFx, "mid_rate",
			[]any{day(1), "USD", "EUR", "", nil, nil, "{}"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, db := newFixture(t)
			exec(t, db, `INSERT INTO dump_runs VALUES (1, ?, ?, '2031-01-01')`, day(1), day(1))
			exec(t, db, `INSERT INTO accounts (account_id, account_kind, payload)
                VALUES ('acct-cash', 'cash', '{}')`)
			exec(t, db, tc.insert, tc.args...)
			conn := openConn(t, path)
			_, err := conn.Snapshots(context.Background(), window(t, conn, -1))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Snapshots error = %v, want one naming %s", err, tc.want)
			}
		})
	}
}

// TestDanglingReferenceFailsTheLoad: a fact naming an id its dimension table
// does not hold is a defect in the silver, not something to load around.
func TestDanglingReferenceFailsTheLoad(t *testing.T) {
	path, db := newFixture(t)
	exec(t, db, `INSERT INTO dump_runs VALUES (1, ?, ?, '2031-01-01')`, day(1), day(1))
	exec(t, db, insertCash, day(1), "acct-ghost", "USD", "closing", "1", "{}")
	conn := openConn(t, path)
	_, err := conn.Snapshots(context.Background(), window(t, conn, -1))
	if err == nil || !strings.Contains(err.Error(), "acct-ghost") {
		t.Errorf("Snapshots error = %v, want one naming the missing account", err)
	}
}
