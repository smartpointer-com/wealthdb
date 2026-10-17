package silver

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

func openScratch(t *testing.T, ddl string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestSnapshotTimesMergesExtraInsideTheWindow pins the batch times: the
// query's times and every extra time inside the window, once each, ascending.
func TestSnapshotTimesMergesExtraInsideTheWindow(t *testing.T) {
	db := openScratch(t, `CREATE TABLE p (t INTEGER);
INSERT INTO p VALUES (30), (10), (30), (50);`)
	w := canonical.Window{Start: 10, End: 40, HasChanges: true}
	got, err := SnapshotTimes(context.Background(), db, w, []int64{5, 20, 30, 45},
		`SELECT t FROM p WHERE t BETWEEN ? AND ?`, w.Start, w.End)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{10, 20, 30}; !slices.Equal(got, want) {
		t.Errorf("SnapshotTimes = %v, want %v", got, want)
	}
}

// TestAccountCurrenciesReadTheLatestRow pins the roster currency: the
// account's latest row, and the fallback where that row names none or the
// account has no row.
func TestAccountCurrenciesReadTheLatestRow(t *testing.T) {
	db := openScratch(t, `CREATE TABLE accounts (
    snapshot_at INTEGER, account_external_id TEXT, currency TEXT);
INSERT INTO accounts VALUES (1, 'a', 'EUR'), (2, 'a', 'CHF'),
                            (1, 'b', 'GBP'), (2, 'b', NULL);`)
	ccy, err := ReadAccountCurrencies(context.Background(), db, "USD")
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"a": "CHF", "b": "USD", "c": "USD"} {
		if got := ccy.Of(id); got != want {
			t.Errorf("Of(%q) = %q, want %q", id, got, want)
		}
	}
}

// TestContentStatusReadsTheExtrema pins the content Status: each value the
// query yields lands in its field, and a NULL leaves the field unknown.
func TestContentStatusReadsTheExtrema(t *testing.T) {
	db := openScratch(t, `CREATE TABLE x (a INTEGER);`)
	s, err := ContentStatus(context.Background(), db, "test", `SELECT 1, 2, NULL, 4, 5`)
	if err != nil {
		t.Fatal(err)
	}
	want := canonical.UnknownStatus()
	want.OldestSnapshotAt, want.LatestSnapshotAt = 1, 2
	want.LatestTransactionAt, want.LatestChangeNumber = 4, 5
	if s != want {
		t.Errorf("ContentStatus = %+v, want %+v", s, want)
	}
}

// TestReadBankRosterTakesEachAccountsLatestRow pins the bank roster read: an
// account's latest row, its first and latest snapshot, the source's latest
// snapshot, and the name and type columns the caller names.
func TestReadBankRosterTakesEachAccountsLatestRow(t *testing.T) {
	ctx := context.Background()
	db := openScratch(t, `CREATE TABLE accounts (
    snapshot_at INTEGER, account_external_id TEXT, nickname TEXT,
    account_type TEXT, mask TEXT, balance REAL, payload TEXT);
INSERT INTO accounts VALUES
    (1, 'a', NULL,   'Checking', '1111', 10.5, '{"v":1}'),
    (3, 'a', 'Main', 'Checking', '1111', 20.25, '{"v":3}'),
    (2, 'b', NULL,   'Savings',  '2222', NULL,  '{}');`)
	got, err := ReadBankRoster(ctx, db, "test", "nickname", "account_type")
	if err != nil {
		t.Fatal(err)
	}
	want := []BankRosterRow{
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

	untyped, err := ReadBankRoster(ctx, db, "test", "nickname", "")
	if err != nil {
		t.Fatal(err)
	}
	if untyped[1].AccountType != "" {
		t.Errorf("no type column read %q", untyped[1].AccountType)
	}
	if _, err := ReadBankRoster(ctx, db, "the roster", "nope", ""); err == nil ||
		!strings.HasPrefix(err.Error(), "the roster: ") {
		t.Errorf("a failing query = %v, want it named", err)
	}
}

// TestBankRosterRowRecords pins the records a roster row becomes: the
// account with the bank defaults, and the CURRENT balance stamped at the
// source's latest snapshot, only when there is a balance and that snapshot
// is inside the window.
func TestBankRosterRowRecords(t *testing.T) {
	r := BankRosterRow{ID: "a", Name: "Main", Mask: "1111",
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

// TestAppendClosingBalances pins the closing-balance loop: one CLOSING row
// per query row, in the query's order, the currency and sign the caller
// supplies, and the figure as stored when no sign is given.
func TestAppendClosingBalances(t *testing.T) {
	ctx := context.Background()
	db := openScratch(t, `CREATE TABLE b (id TEXT, at INTEGER, v REAL);
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
	if err := AppendClosingBalances(ctx, db, "test", q, []any{15}, `{"basis":"x"}`, ccy, neg, &batch); err != nil {
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
	if err := AppendClosingBalances(ctx, db, "test", q, []any{0}, `{}`, ccy, nil, &batch); err != nil {
		t.Fatal(err)
	}
	if n := len(batch.CashBalances); n != 3 || batch.CashBalances[0].Amount.String() != "1.5" {
		t.Errorf("unsigned rows = %+v, want 3 with the figures as stored", batch.CashBalances)
	}
	if err := AppendClosingBalances(ctx, db, "the series", `SELECT nope FROM b`, nil, `{}`,
		ccy, nil, &batch); err == nil || !strings.HasPrefix(err.Error(), "the series: ") {
		t.Errorf("a failing query = %v, want it named", err)
	}
}

// TestRunningBalanceQueryTakesTheDaysLastRow pins the shared ledger read:
// per account and day, the fitid-highest row with a balance, inside the
// bound window.
func TestRunningBalanceQueryTakesTheDaysLastRow(t *testing.T) {
	db := openScratch(t, `CREATE TABLE transactions (
    account_external_id TEXT, posted_at INTEGER, fitid TEXT, balance REAL);
INSERT INTO transactions VALUES
    ('a', 100, 'f1', 10), ('a', 100, 'f2', 12), ('a', 100, 'f3', NULL),
    ('a', 200, 'f4', 15), ('b', 100, 'f5', 7),  ('a', 900, 'f6', 99);`)
	var batch canonical.SnapshotBatch
	if err := AppendClosingBalances(context.Background(), db, "test", RunningBalanceQuery,
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
