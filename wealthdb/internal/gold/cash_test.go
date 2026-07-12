package gold

import (
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// TestCashAsOfMultiCurrencyAndZeroFiltering covers the meat of
// CashAsOf: one row per (account, currency) with non-zero amount;
// zero balances are filtered out; the synthetic
// position_key/symbol/name/asset_class fields are populated.
func TestCashAsOfMultiCurrencyAndZeroFiltering(t *testing.T) {
	db, ctx := openMigrated(t)

	// Seed two accounts. ACC1 has USD + EUR cash (multi-currency);
	// ACC2 has a zero CHF balance that should be filtered out.
	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{
			{SilverSourceID: "test-src", AccountExternalID: "ACC1",
				AccountKind: canonical.AccountKindBrokerage, FirstSeenAt: 1000, LastSeenAt: 1000},
			{SilverSourceID: "test-src", AccountExternalID: "ACC2",
				AccountKind: canonical.AccountKindBrokerage, FirstSeenAt: 1000, LastSeenAt: 1000},
		})
	})

	usd, _ := canonical.NewDecimalFromString("1234.56")
	eur, _ := canonical.NewDecimalFromString("789.00")
	zero, _ := canonical.NewDecimalFromString("0")
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertCashBalances(ctx, []canonical.CashBalanceChange{
			{SilverSourceID: "test-src", SnapshotAt: 1000, AccountExternalID: "ACC1",
				Currency: "USD", BalanceKind: canonical.BalanceKindCurrent, Amount: usd},
			{SilverSourceID: "test-src", SnapshotAt: 1000, AccountExternalID: "ACC1",
				Currency: "EUR", BalanceKind: canonical.BalanceKindCurrent, Amount: eur},
			{SilverSourceID: "test-src", SnapshotAt: 1000, AccountExternalID: "ACC2",
				Currency: "CHF", BalanceKind: canonical.BalanceKindClosing, Amount: zero},
		})
	})

	rows, err := CashAsOf(ctx, db, 2000, "USD")
	if err != nil {
		t.Fatalf("CashAsOf: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (ACC1/USD + ACC1/EUR; ACC2/CHF filtered as zero)", len(rows))
	}

	byKey := map[string]PositionRow{}
	for _, r := range rows {
		byKey[r.AccountExternalID+":"+r.Currency] = r
	}
	for key, want := range map[string]string{
		"ACC1:USD": "1234.56",
		"ACC1:EUR": "789",
	} {
		r, ok := byKey[key]
		if !ok {
			t.Errorf("missing row %q", key)
			continue
		}
		if r.MarketValue == nil || *r.MarketValue != want {
			t.Errorf("%s market_value = %v, want %s", key, r.MarketValue, want)
		}
		if r.AssetClass != "cash" {
			t.Errorf("%s asset_class = %q, want cash", key, r.AssetClass)
		}
		wantKey := "cash:" + r.Currency
		if r.PositionKey != wantKey {
			t.Errorf("%s position_key = %q, want %q", key, r.PositionKey, wantKey)
		}
		if r.Symbol == nil || *r.Symbol != r.Currency {
			t.Errorf("%s symbol = %v, want %s", key, r.Symbol, r.Currency)
		}
	}
}

// TestCashAsOfBalanceKindPrecedence asserts that when multiple
// balance_kind rows exist for the same (account, currency,
// snapshot), the highest-precedence kind wins — `current` beats
// `closing` beats `available` etc.
func TestCashAsOfBalanceKindPrecedence(t *testing.T) {
	db, ctx := openMigrated(t)

	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{
			SilverSourceID: "test-src", AccountExternalID: "ACC",
			AccountKind: canonical.AccountKindBrokerage, FirstSeenAt: 1000, LastSeenAt: 1000,
		}})
	})

	closing := canonical.NewDecimalFromInt(100)
	current := canonical.NewDecimalFromInt(200)
	available := canonical.NewDecimalFromInt(300)
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertCashBalances(ctx, []canonical.CashBalanceChange{
			{SilverSourceID: "test-src", SnapshotAt: 1000, AccountExternalID: "ACC",
				Currency: "USD", BalanceKind: canonical.BalanceKindClosing, Amount: closing},
			{SilverSourceID: "test-src", SnapshotAt: 1000, AccountExternalID: "ACC",
				Currency: "USD", BalanceKind: canonical.BalanceKindCurrent, Amount: current},
			{SilverSourceID: "test-src", SnapshotAt: 1000, AccountExternalID: "ACC",
				Currency: "USD", BalanceKind: canonical.BalanceKindAvailable, Amount: available},
		})
	})

	rows, err := CashAsOf(ctx, db, 2000, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (one winner per (acct, ccy))", len(rows))
	}
	if rows[0].MarketValue == nil || *rows[0].MarketValue != "200" {
		t.Errorf("market_value = %v, want 200 (current beats closing/available)", rows[0].MarketValue)
	}
}

// TestCashAsOfPicksLatestSnapshotPerSource confirms cash queries
// follow the same latest-per-source semantics as PositionsAsOf —
// older snapshots aren't summed in.
func TestCashAsOfPicksLatestSnapshotPerSource(t *testing.T) {
	db, ctx := openMigrated(t)

	inTx(t, db, ctx, func(w *Writer) error {
		return w.UpsertAccounts(ctx, []canonical.AccountChange{{
			SilverSourceID: "test-src", AccountExternalID: "ACC",
			AccountKind: canonical.AccountKindBrokerage, FirstSeenAt: 1000, LastSeenAt: 2000,
		}})
	})

	older := canonical.NewDecimalFromInt(50)
	newer := canonical.NewDecimalFromInt(75)
	inTx(t, db, ctx, func(w *Writer) error {
		return w.InsertCashBalances(ctx, []canonical.CashBalanceChange{
			{SilverSourceID: "test-src", SnapshotAt: 1000, AccountExternalID: "ACC",
				Currency: "USD", BalanceKind: canonical.BalanceKindCurrent, Amount: older},
			{SilverSourceID: "test-src", SnapshotAt: 2000, AccountExternalID: "ACC",
				Currency: "USD", BalanceKind: canonical.BalanceKindCurrent, Amount: newer},
		})
	})

	rows, err := CashAsOf(ctx, db, 3000, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].MarketValue == nil || *rows[0].MarketValue != "75" {
		t.Fatalf("got rows=%+v, want one row with market_value=75", rows)
	}
}
