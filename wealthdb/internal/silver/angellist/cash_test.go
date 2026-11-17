package angellist

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

const day = 86400

func rb(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }

// The wallet's balance is read at each ledger day's end. Running balances
// that follow the amounts are read with their opening balance, the first
// day read as a whole: its rows may state theirs in any order. Running
// balances that do not follow, or that are missing, leave the amounts'
// cumulative sum, anchored to the stated balance where there is one.
func TestWalletDays(t *testing.T) {
	t.Parallel()
	none := sql.NullInt64{}
	cases := []struct {
		name   string
		txs    []walletTx
		stated sql.NullInt64
		want   []walletDay
		how    int
	}{
		{"following, from an empty wallet", []walletTx{
			{day + 10, 500, rb(500)},
			{day + 20, -200, rb(300)},
			{3*day + 5, 100, rb(400)},
		}, none, []walletDay{{day + 20, 300}, {3*day + 5, 400}}, walletRunning},
		{"following, from a funded wallet, the first day's rows out of order", []walletTx{
			{day + 10, 500, rb(1300)},
			{day + 20, -200, rb(800)},
			{2*day + 10, -200, rb(900)},
			{2*day + 10, -200, rb(1100)},
		}, none, []walletDay{{day + 20, 1300}, {2*day + 10, 900}}, walletRunning},
		{"not following, anchored to the stated balance", []walletTx{
			{day + 10, 500, rb(500)},
			{2 * day, -200, rb(900)},
		}, rb(1300), []walletDay{{day + 10, 1500}, {2 * day, 1300}}, walletAnchored},
		{"none stated, on a funded wallet with a stated balance", []walletTx{
			{day + 10, 500, none},
			{2 * day, -200, none},
		}, rb(1300), []walletDay{{day + 10, 1500}, {2 * day, 1300}}, walletAnchored},
		{"none stated and no stated balance", []walletTx{
			{day + 10, 500, none},
			{2 * day, -200, none},
		}, none, []walletDay{{day + 10, 500}, {2 * day, 300}}, walletSum},
	}
	for _, c := range cases {
		got, how := walletDays(c.txs, c.stated)
		if !slices.Equal(got, c.want) || how != c.how {
			t.Errorf("%s: %v how=%d, want %v how=%d", c.name, got, how, c.want, c.how)
		}
	}
}

// The wallet's cash is a balance on every ledger day, dated at the day's
// last row. The balance the latest dump states holds from the dump's
// time: equal to the ledger's end, it adds a point at the dump; a ledger
// that ends elsewhere keeps its own days, and the dump's balance holds
// from the dump on.
func TestSnapshotsFundingCash(t *testing.T) {
	type bal struct {
		at     int64
		amount string
	}
	for _, c := range []struct {
		name   string
		stated int64
		want   []bal
	}{
		{"the ledger ends on the stated balance", 260000,
			[]bal{{day + 20, "3000.00"}, {2*day + 5, "2500.00"}, {3*day + 30, "2600.00"}, {5 * day, "2600.00"}}},
		{"the ledger ends elsewhere", 270000,
			[]bal{{day + 20, "3000.00"}, {2*day + 5, "2500.00"}, {3*day + 30, "2600.00"}, {5 * day, "2700.00"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			path, db := newFixtureSilver(t)
			if _, err := db.Exec(`
                INSERT INTO dump_runs(snapshot_at, invest_account_slug) VALUES (5 * 86400, 'acct');
                INSERT INTO funding_accounts(funding_account_external_id, currency, balance_minor, snapshot_at)
                    VALUES ('fa1', 'USD', ?, 5 * 86400);
                INSERT INTO funding_transactions(transaction_external_id, occurred_at, type, amount_minor,
                    currency, running_balance_minor) VALUES
                    ('t1', 86400 + 10,  'deposit',     500000, 'USD', 500000),
                    ('t2', 86400 + 20,  'investment', -200000, 'USD', 300000),
                    ('t3', 172800 + 5,  'withdrawal',  -50000, 'USD', 250000),
                    ('t4', 259200 + 30, 'refund',       10000, 'USD', 260000);
            `, c.stated); err != nil {
				t.Fatal(err)
			}
			conn := openAdapter(t, path)
			w, _ := conn.ChangeWindow(context.Background(), -1)
			var got []bal
			for _, cb := range silvertest.Merged(t, conn, w).CashBalances {
				if cb.BalanceKind != canonical.BalanceKindCurrent || cb.AccountExternalID != "acct" || cb.Currency != "USD" {
					t.Errorf("balance = %+v, want the account's current USD cash", cb)
				}
				got = append(got, bal{cb.SnapshotAt, cb.Amount.StringFixed(2)})
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("cash = %v, want %v", got, c.want)
			}
			if s, err := conn.Status(context.Background()); err != nil || s.LatestSnapshotAt != 5*day {
				t.Errorf("Status latest snapshot = %d %v, want the dump that states the balance", s.LatestSnapshotAt, err)
			}
		})
	}
}
