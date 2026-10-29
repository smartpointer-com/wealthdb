package manual

import (
	"context"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// An exit is a sell of the position on its exit date, for the proceeds,
// on the account the position is held in; the status spans the exits.
func TestAnExitSellsThePosition(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	if _, err := db.Exec(`
        UPDATE positions SET closed_at = '2023-06-01' WHERE id = 'spv-1';
        INSERT INTO exits(position_id, exit_date, proceeds, currency, notes) VALUES
            ('spv-1', '2023-06-01', '150.25', 'USD', 'sold with the company'),
            ('cn-1',  '2022-03-01', '0',      'CHF', NULL);`); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, err := conn.Transactions(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())
	if len(batch.Transactions) != 2 {
		t.Fatalf("transactions = %d, want 2", len(batch.Transactions))
	}
	for i, want := range []struct {
		id, acct, inst, ccy, net, desc string
		at                             int64
	}{
		{"exit:cn-1", defaultAccountKey, "cn-1", "CHF", "0.00", "", silvertest.ISO(t, "2022-03-01")},
		{"exit:spv-1", defaultAccountKey, "spv-1", "USD", "150.25", "sold with the company", silvertest.ISO(t, "2023-06-01")},
	} {
		tx := batch.Transactions[i]
		desc := ""
		if tx.Description != nil {
			desc = *tx.Description
		}
		if tx.TransactionExternalID != want.id || tx.AccountExternalID != want.acct || tx.Kind != canonical.TxKindSell ||
			tx.InstrumentExternalID == nil || *tx.InstrumentExternalID != want.inst || tx.Currency != want.ccy ||
			tx.NetAmount == nil || tx.NetAmount.StringFixed(2) != want.net || tx.OccurredAt != want.at || desc != want.desc {
			t.Errorf("transaction %d = %+v, want %+v", i, tx, want)
		}
	}
	s, err := conn.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.OldestTransactionAt != silvertest.ISO(t, "2022-03-01") || s.LatestTransactionAt != silvertest.ISO(t, "2023-06-01") {
		t.Errorf("transaction range = [%d, %d]", s.OldestTransactionAt, s.LatestTransactionAt)
	}
}

// A silver last loaded before the exits table existed has no transactions.
func TestASilverWithoutTheExitsTableHasNoTransactions(t *testing.T) {
	path, db := newFixtureSilver(t)
	seed(t, db)
	if _, err := db.Exec(`DROP TABLE exits`); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, err := conn.Transactions(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if batch, _, _ := stream.Next(context.Background()); len(batch.Transactions) != 0 {
		t.Errorf("transactions = %d, want none", len(batch.Transactions))
	}
	if s, err := conn.Status(context.Background()); err != nil || s.OldestTransactionAt != -1 {
		t.Errorf("status = %+v, err %v", s, err)
	}
}
