package bank

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// scratch is a silver SQLite with ddl applied.
func scratch(t *testing.T, ddl string) *sql.DB {
	t.Helper()
	_, db := silvertest.NewDB(t, ddl)
	return db
}

// TestReadFactsReadTheLatestRow pins an account's currency and product:
// its latest roster row, and the default currency where that row names
// none or the account has no row. Without a ProductCol, every account is
// of the source's AccountKind.
func TestReadFactsReadTheLatestRow(t *testing.T) {
	db := scratch(t, `CREATE TABLE accounts (
    snapshot_at INTEGER, account_external_id TEXT, currency TEXT, product TEXT);
INSERT INTO accounts VALUES (1, 'a', 'EUR', 'card'), (2, 'a', 'CHF', 'card'),
                            (1, 'b', 'GBP', 'card'), (2, 'b', NULL, 'deposit');`)
	ctx := context.Background()
	c := &connection{src: &Source{Kind: "bank", DefaultCurrency: "USD", ProductCol: "product", CardProduct: "card"}, db: db}
	f, err := c.readFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"a": "CHF", "b": "USD", "c": "USD"} {
		if got := f.currency(id); got != want {
			t.Errorf("currency(%q) = %q, want %q", id, got, want)
		}
	}
	if !f.card("a") || f.card("b") || f.card("c") {
		t.Errorf("card a, b, c = %v, %v, %v; want the latest row's product", f.card("a"), f.card("b"), f.card("c"))
	}
	for _, kind := range []canonical.AccountKind{canonical.AccountKindCard, canonical.AccountKindCash} {
		c.src = &Source{Kind: "bank", DefaultCurrency: "USD", AccountKind: kind}
		f, err := c.readFacts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if f.currency("a") != "CHF" || f.card("b") != (kind == canonical.AccountKindCard) {
			t.Errorf("%s: currency(a) = %q, card(b) = %v", kind, f.currency("a"), f.card("b"))
		}
	}
	c.src = &Source{Kind: "bank", ProductCol: "nope"}
	if _, err := c.readFacts(ctx); err == nil || !strings.HasPrefix(err.Error(), "bank account facts: ") {
		t.Errorf("a failing query = %v, want it named", err)
	}
}

// TestReadRosterTakesEachAccountsLatestRow pins the bank roster read: an
// account's latest row, its first and latest snapshot, the source's latest
// snapshot, and the name and type columns the caller names.
func TestReadRosterTakesEachAccountsLatestRow(t *testing.T) {
	ctx := context.Background()
	db := scratch(t, `CREATE TABLE accounts (
    snapshot_at INTEGER, account_external_id TEXT, nickname TEXT,
    account_type TEXT, mask TEXT, balance REAL, payload TEXT);
INSERT INTO accounts VALUES
    (1, 'a', NULL,   'Checking', '1111', 10.5, '{"v":1}'),
    (3, 'a', 'Main', 'Checking', '1111', 20.25, '{"v":3}'),
    (2, 'b', NULL,   'Savings',  '2222', NULL,  '{}');`)
	got, err := readRoster(ctx, db, "test", "nickname", "account_type")
	if err != nil {
		t.Fatal(err)
	}
	want := []RosterRow{
		{ID: "a", Name: "Main", AccountType: "Checking", Mask: "1111",
			Balance: sql.NullFloat64{Float64: 20.25, Valid: true}, Payload: `{"v":3}`,
			FirstSeen: 1, LastSeen: 3, SourceLatest: 3},
		{ID: "b", AccountType: "Savings", Mask: "2222", Payload: `{}`,
			FirstSeen: 2, LastSeen: 2, SourceLatest: 3},
	}
	if !slices.Equal(got, want) {
		t.Errorf("ReadBankRoster =\n %+v\nwant\n %+v", got, want)
	}
	if got[0].DisplayName() != "Main" || got[1].DisplayName() != "2222" {
		t.Errorf("DisplayName = %q, %q; want the name, else the mask",
			got[0].DisplayName(), got[1].DisplayName())
	}

	untyped, err := readRoster(ctx, db, "test", "nickname", "")
	if err != nil {
		t.Fatal(err)
	}
	if untyped[1].AccountType != "" {
		t.Errorf("no type column read %q", untyped[1].AccountType)
	}
	if _, err := readRoster(ctx, db, "the roster", "nope", ""); err == nil ||
		!strings.HasPrefix(err.Error(), "the roster: ") {
		t.Errorf("a failing query = %v, want it named", err)
	}
}

// TestRosterRowRecords pins the records a roster row becomes: the
// account with the bank defaults, and the CURRENT balance stamped at the
// source's latest snapshot, only when there is a balance and that snapshot
// is inside the window.
func TestRosterRowRecords(t *testing.T) {
	r := RosterRow{ID: "a", Name: "Main", Mask: "1111",
		Balance: sql.NullFloat64{Float64: 20.25, Valid: true}, Payload: `{"v":3}`,
		FirstSeen: 1, LastSeen: 3, SourceLatest: 5}

	acc := r.Account(canonical.AccountKindCard, "Shown", "EUR")
	if acc.AccountExternalID != "a" || acc.AccountKind != canonical.AccountKindCard ||
		*acc.DisplayName != "Shown" || *acc.BaseCurrency != "EUR" ||
		*acc.TaxWrapper != canonical.TaxWrapperTaxablePersonal ||
		*acc.ManagementStyle != canonical.ManagementStyleSelfDirected ||
		acc.FirstSeenAt != 1 || acc.LastSeenAt != 3 || string(acc.Payload) != `{"v":3}` ||
		acc.Nickname != nil {
		t.Errorf("Account = %+v", acc)
	}
	if acc := r.Account(canonical.AccountKindCash, "", "EUR"); acc.DisplayName != nil {
		t.Errorf("an empty display name = %q, want none", *acc.DisplayName)
	}

	amount := canonical.NewDecimalFromFloat(-20.25)
	cb, ok := r.CurrentBalance(canonical.Window{Start: 0, End: 5}, "EUR", amount)
	if !ok || cb.SnapshotAt != 5 || cb.AccountExternalID != "a" || cb.Currency != "EUR" ||
		cb.BalanceKind != canonical.BalanceKindCurrent || cb.Amount.String() != "-20.25" ||
		string(cb.Payload) != `{"basis":"roster"}` {
		t.Errorf("CurrentBalance = %+v, %v", cb, ok)
	}
	if _, ok := r.CurrentBalance(canonical.Window{Start: 0, End: 4}, "EUR", amount); ok {
		t.Error("a source snapshot outside the window emitted a balance")
	}
	r.Balance = sql.NullFloat64{}
	if _, ok := r.CurrentBalance(canonical.Window{Start: 0, End: 5}, "EUR", amount); ok {
		t.Error("a roster row with no balance emitted one")
	}
}

// TestAppendReportedBalances pins the per-run roster balances: one CURRENT
// row per account and run that listed it inside the window, valued at its
// latest roster row at or before the run, in the account's currency. A run
// that listed an account twice states it once, a roster row counts as a
// listing, and a row with no balance states none. A sign function sets the
// canonical sign. A failing query is named for the collector.
func TestAppendReportedBalances(t *testing.T) {
	ctx := context.Background()
	db := scratch(t, `CREATE TABLE accounts (snapshot_at INTEGER, account_external_id TEXT, balance REAL);
INSERT INTO accounts VALUES (1, 'a', 10), (4, 'a', 40), (1, 'b', NULL), (3, 'b', 30);
CREATE TABLE runs (snapshot_at INTEGER, account_external_id TEXT, kind TEXT);
INSERT INTO runs VALUES (2, 'a', 'x'), (2, 'a', 'y'), (5, 'a', 'x'), (9, 'a', 'x'), (2, 'b', 'x'), (5, 'b', 'x');`)
	ccy := func(id string) string { return map[string]string{"a": "CHF"}[id] + "X" }
	emitSigned := func(table string, sign func(string, float64) canonical.Decimal) ([]string, error) {
		var batch canonical.SnapshotBatch
		err := appendReportedBalances(ctx, db, "bank", table, canonical.Window{Start: 0, End: 8}, ccy, sign, &batch)
		var out []string
		for _, cb := range batch.CashBalances {
			if cb.BalanceKind != canonical.BalanceKindCurrent || string(cb.Payload) != `{"basis":"roster"}` {
				t.Errorf("%+v, want a CURRENT roster balance", cb)
			}
			out = append(out, fmt.Sprintf("%s@%d=%s %s", cb.AccountExternalID, cb.SnapshotAt,
				cb.Amount.String(), cb.Currency))
		}
		return out, err
	}
	emit := func(table string) ([]string, error) { return emitSigned(table, nil) }
	got, err := emit("runs")
	if want := []string{"a@1=10 CHFX", "a@2=10 CHFX", "a@4=40 CHFX", "a@5=40 CHFX",
		"b@3=30 X", "b@5=30 X"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("AppendReportedBalances = %v, %v; want %v", got, err, want)
	}
	negB := func(id string, v float64) canonical.Decimal {
		d := canonical.NewDecimalFromFloat(v)
		if id == "b" {
			return d.Neg()
		}
		return d
	}
	got, err = emitSigned("runs", negB)
	if want := []string{"a@1=10 CHFX", "a@2=10 CHFX", "a@4=40 CHFX", "a@5=40 CHFX",
		"b@3=-30 X", "b@5=-30 X"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("signed AppendReportedBalances = %v, %v; want %v", got, err, want)
	}
	if _, err := db.Exec(`DROP TABLE accounts`); err != nil {
		t.Fatal(err)
	}
	if _, err := emit("runs"); err == nil || !strings.HasPrefix(err.Error(), "bank roster balances: ") {
		t.Errorf("a failing query = %v, want it named", err)
	}
}

// TestAppendClosingBalances pins the closing-balance loop: one CLOSING row
// per query row, in the query's order, the currency and sign the caller
// supplies, and the figure as stored when no sign is given.
func TestAppendClosingBalances(t *testing.T) {
	ctx := context.Background()
	db := scratch(t, `CREATE TABLE b (id TEXT, at INTEGER, v REAL);
INSERT INTO b VALUES ('a', 10, 1.5), ('c', 20, -2), ('a', 30, 4);`)
	const q = `SELECT id, at, v FROM b WHERE at >= ? ORDER BY at`
	ccy := func(id string) string { return map[string]string{"a": "CHF"}[id] + "X" }
	neg := func(id string, v float64) canonical.Decimal {
		d := canonical.NewDecimalFromFloat(v)
		if id == "a" {
			return d.Neg()
		}
		return d
	}

	var batch canonical.SnapshotBatch
	if err := appendClosingBalances(ctx, db, "test", q, []any{15}, `{"basis":"x"}`, ccy, neg, &batch); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, cb := range batch.CashBalances {
		if cb.BalanceKind != canonical.BalanceKindClosing || string(cb.Payload) != `{"basis":"x"}` {
			t.Errorf("row %+v: want a CLOSING row with the payload", cb)
		}
		got = append(got, fmt.Sprintf("%d %s %s %s", cb.SnapshotAt, cb.AccountExternalID, cb.Currency, cb.Amount))
	}
	if want := []string{"20 c X -2", "30 a CHFX -4"}; !slices.Equal(got, want) {
		t.Errorf("signed rows = %q, want %q", got, want)
	}

	batch = canonical.SnapshotBatch{}
	if err := appendClosingBalances(ctx, db, "test", q, []any{0}, `{}`, ccy, nil, &batch); err != nil {
		t.Fatal(err)
	}
	if n := len(batch.CashBalances); n != 3 || batch.CashBalances[0].Amount.String() != "1.5" {
		t.Errorf("unsigned rows = %+v, want 3 with the figures as stored", batch.CashBalances)
	}
	if err := appendClosingBalances(ctx, db, "the series", `SELECT nope FROM b`, nil, `{}`,
		ccy, nil, &batch); err == nil || !strings.HasPrefix(err.Error(), "the series: ") {
		t.Errorf("a failing query = %v, want it named", err)
	}
}

// TestRunningBalanceQueryTakesTheDaysLastRow pins the shared ledger read:
// per account and day, the fitid-highest row with a balance, inside the
// bound window.
func TestRunningBalanceQueryTakesTheDaysLastRow(t *testing.T) {
	db := scratch(t, `CREATE TABLE transactions (
    account_external_id TEXT, posted_at INTEGER, fitid TEXT, balance REAL);
INSERT INTO transactions VALUES
    ('a', 100, 'f1', 10), ('a', 100, 'f2', 12), ('a', 100, 'f3', NULL),
    ('a', 200, 'f4', 15), ('b', 100, 'f5', 7),  ('a', 900, 'f6', 99);`)
	var batch canonical.SnapshotBatch
	if err := appendClosingBalances(context.Background(), db, "test", RunningBalanceQuery,
		[]any{0, 500}, `{}`, func(string) string { return "USD" }, nil, &batch); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, cb := range batch.CashBalances {
		got = append(got, fmt.Sprintf("%s %d %s", cb.AccountExternalID, cb.SnapshotAt, cb.Amount))
	}
	if want := []string{"a 100 12", "a 200 15", "b 100 7"}; !slices.Equal(got, want) {
		t.Errorf("running balances = %q, want %q", got, want)
	}
}
