package relevate

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// TestCanonicalGolden holds every record the adapter yields to
// testdata/canonical.jsonl, over the rows of the snapshot and the credit
// note tests in one silver.
func TestCanonicalGolden(t *testing.T) {
	path, db := newFixtureSilver(t)
	silvertest.Exec(t, db, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, currency_code, name, product_name, payload) VALUES
            (1000, 'ACC1', 'CHF', 'Strategy 50', 'PensFree', '{}');
        INSERT INTO cash_balances(snapshot_at, account_external_id, currency, balance_kind, amount, payload) VALUES
            (1000, 'ACC1', 'CHF', 'cash', 200.00, NULL),
            (1000, 'ACC1', 'CHF', 'securities', 1000.00, NULL);
        INSERT INTO positions(snapshot_at, account_external_id, instrument_external_id, isin, asset_class, allocation, payload) VALUES
            (1000, 'ACC1', 'I1', 'CH0000000001', 'Stocks', 0.6, '{}'),
            (1000, 'ACC1', 'I2', '',             'Strategy', 0.4, '{}');
        INSERT INTO instruments(instrument_external_id, isin, name, asset_class, first_seen_at, last_seen_at, payload) VALUES
            ('I1', 'CH0000000001', 'Equity Fund', 'Stocks', 1000, 1000, '{}'),
            ('I2', '', 'Strategy Fund', 'Strategy', 1000, 1000, '{}');
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (2000, 1, '/x/1');
        INSERT INTO transactions (transaction_external_id, snapshot_at, occurred_at,
            account_external_id, instrument_external_id, kind, currency,
            gross_amount, net_amount, quantity, price, source, payload)
        VALUES ('credit_note:42', 2000, 1500, 'ACC1', NULL, 'contribution',
                'CHF', 1234.56, 1234.56, NULL, NULL, 'credit_note_pdf', '{}');`)
	silvertest.Golden(t, openAdapter(t, path), "testdata/canonical.jsonl")
}
