package swissquote

import (
	"context"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// An export that lists one movement twice gives two rows that agree on
// every column the id is made of. The first keeps the id it has always
// had, the copy takes a suffix, and a row of its own keeps its id.
func TestTransactionsDuplicateRowsGetDistinctIDs(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO transactions(account_external_id, occurred_at, transaction_type, isin, symbol, currency, net_amount, payload) VALUES
            ('1234567', 1500, 'Custody Fees', NULL, NULL, 'CHF', -12.50, '{"name":"Fee b"}'),
            ('1234567', 1600, 'Dividend', 'IE00BJK9H753', 'IUSQ', 'USD', 25.00, '{"name":"Fund"}'),
            ('1234567', 1500, 'Custody Fees', NULL, NULL, 'CHF', -12.50, '{"name":"Fee a"}'),
            ('1234567', 1500, 'Custody Fees', NULL, NULL, 'CHF', -12.50, '{"name":"Fee c"}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, err := conn.Transactions(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	batch, _, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	fee := syntheticTxID("1234567", 1500, "Custody Fees", "", "CHF", mustDecimal(t, "-12.5"))
	got := map[string]string{}
	for _, tx := range batch.Transactions {
		if _, dup := got[tx.TransactionExternalID]; dup {
			t.Fatalf("id %q minted twice", tx.TransactionExternalID)
		}
		got[tx.TransactionExternalID] = string(tx.Payload)
	}
	want := map[string]string{
		fee:        `{"name":"Fee a"}`,
		fee + "-2": `{"name":"Fee b"}`,
		fee + "-3": `{"name":"Fee c"}`,
		syntheticTxID("1234567", 1600, "Dividend", "IUSQ", "USD", mustDecimal(t, "25")): `{"name":"Fund"}`,
	}
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for id, payload := range want {
		if got[id] != payload {
			t.Errorf("id %s: payload %q, want %q", id, got[id], payload)
		}
	}
}

func mustDecimal(t *testing.T, s string) canonical.Decimal {
	t.Helper()
	d, err := canonical.NewDecimalFromString(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
