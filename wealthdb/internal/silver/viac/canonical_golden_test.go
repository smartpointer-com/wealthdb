package viac

import (
	"flag"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

var update = flag.Bool("update", false, "rewrite testdata/canonical.jsonl")

// TestCanonicalGolden holds every record the adapter yields to
// testdata/canonical.jsonl, over the rows of the adapter tests in one
// silver: the three product codes, report and live positions with their
// cash, and a ledger of linked, unlinked and narrative-less rows.
func TestCanonicalGolden(t *testing.T) {
	path, db := newFixtureSilver(t)
	silvertest.Exec(t, db, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES
            (1000, 5, '/x/1'), (2000, 5, '/x/2000'), (3000, 5, '/x/3000');
        INSERT INTO accounts(snapshot_at, account_external_id, product_code, name, state, currency_code, management_style, payload) VALUES
            (1000, 'P3A1', '3', 'Pillar 3a', 'ACTIVE', 'CHF', 'automated', '{}'),
            (1000, 'PVB1', '2', 'Vested', 'ACTIVE', 'CHF', 'automated', '{}'),
            (1000, 'INV1', '1', 'Free Invest', 'ACTIVE', 'CHF', 'automated', '{}'),
            (2000, 'P3A1', '3', 'Pillar 3a', 'ACTIVE', 'CHF', 'automated', '{}'),
            (3000, 'P3A1', '3', 'Pillar 3a', 'ACTIVE', 'CHF', 'automated', '{}');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_external_id, asset_class, currency_code, name, quantity, market_value_chf, acquisition_price, asset_price, source, payload) VALUES
            (1000, 'P3A1', 'CH0000000001', 'equity', 'CHF', 'Old Fund', 5, 600.00, 100.00, 120.00, 'report:DOC1000', '{}'),
            (1500, 'P3A1', 'CH0000000001', 'equity', 'CHF', 'Old Fund', 7, 900.00, 100.00, 128.00, 'report:DOC1500', '{}'),
            (2000, 'P3A1', 'CH0000000001', 'equity', 'CHF', 'Old Fund', 10, 1500.00, 120.00, 150.00, 'live', '{}'),
            (3000, 'P3A1', 'CH0000000001', 'equity', 'CHF', 'Old Fund', 11, 1700.00, 120.00, 155.00, 'live', '{}');
        INSERT INTO instruments(instrument_external_id, isin, name, currency_code, asset_class, first_seen_at, last_seen_at, payload) VALUES
            ('CH0000000001', 'CH0000000001', 'Old Fund', 'CHF', 'equity', 1000, 3000, '{}'),
            ('CH0000000002', 'CH0000000002', 'Example Bond Index', 'CHF', 'bond', 1, 1, '{}');
        INSERT INTO cash_balances(snapshot_at, account_external_id, currency, balance_kind, amount, source, payload) VALUES
            (1000, 'P3A1', 'CHF', 'cash', 50.00, 'report:DOC1000', NULL),
            (2000, 'P3A1', 'CHF', 'cash', 80.00, 'live', NULL);
        INSERT INTO transactions(transaction_external_id, snapshot_at, occurred_at, account_external_id, type, kind, amount_chf, currency, payload, instrument_external_id) VALUES
            ('tx1', 1000, 900, 'P3A1', 'TRADE_BUY',  'buy',  -100.00, 'CHF', '{"description":"Old Fund"}',             'CH0000000001'),
            ('tx2', 1000, 910, 'P3A1', 'TRADE_SELL', 'sell',   50.00, 'CHF', '{"description":"Example Bond Index"}',   'CH0000000002'),
            ('tx3', 1000, 920, 'P3A1', 'TRADE_BUY',  'buy',   -25.00, 'CHF', '{"description":"Something Unresolved"}', NULL),
            ('tx4', 1000, 930, 'P3A1', 'FEE_CHARGE', 'fee',    -4.00, 'CHF', '{}', NULL),
            ('tx5', 1000, 950, 'P3A1', 'DIVIDEND',   'dividend', 42.00, 'CHF', '{}', NULL);`)
	silvertest.Golden(t, openAdapter(t, path), "testdata/canonical.jsonl", *update)
}
