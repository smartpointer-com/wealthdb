package swissquote

import (
	"flag"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

var update = flag.Bool("update", false, "rewrite testdata/canonical.jsonl")

// TestCanonicalGolden holds every record the adapter yields to
// testdata/canonical.jsonl, over the cost-basis seed and the rows of the
// adapter tests in one silver: every asset class label, a live and a
// historical row of one ISIN, account types, currency balances and a
// ledger with and without an instrument.
func TestCanonicalGolden(t *testing.T) {
	path, db := newFixtureSilver(t)
	seedCostRows(t, db)
	silvertest.Exec(t, db, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (2000, 6, '/x/2');
        INSERT INTO accounts(snapshot_at, account_external_id, payload, account_type) VALUES
            (1000, '1234567', '{"customer_id":"1234567"}', 'Trading'),
            (1000, '7654321', '{"customer_id":"7654321"}', ''),
            (2000, '1234567', '{}', 'Trading');
        INSERT INTO positions(snapshot_at, account_external_id, symbol, currency, payload, name, isin) VALUES
            (1000, '1234567', 'SYNSHR', 'CHF',
             '{"asset_class":"Shares","currency":"CHF","symbol":"SYNSHR","quantity":1,"total_value":1}',
             'Placeholder Equity Co', NULL),
            (1000, '1234567', 'SYNETFEQ', 'USD',
             '{"asset_class":"ETFs","currency":"USD","symbol":"SYNETFEQ","quantity":1,"total_value":1}',
             'Placeholder Broad World ETF', NULL),
            (1000, '1234567', 'SYNETFAU', 'USD',
             '{"asset_class":"ETFs","currency":"USD","symbol":"SYNETFAU","quantity":1,"total_value":1}',
             'Placeholder Physical Gold ETF', NULL),
            (1000, '1234567', 'SYNETFBTC', 'USD',
             '{"asset_class":"ETFs","currency":"USD","symbol":"SYNETFBTC","quantity":1,"total_value":1}',
             'Placeholder Bitcoin ETF', NULL),
            (1000, '1234567', 'SYNBOND', 'CHF',
             '{"asset_class":"Bonds","currency":"CHF","symbol":"SYNBOND","quantity":1,"total_value":1}',
             'Placeholder Corp Note', NULL),
            (1000, '1234567', 'SYNFNDEQ', 'CHF',
             '{"asset_class":"Funds","currency":"CHF","symbol":"SYNFNDEQ","quantity":1,"total_value":1}',
             'Placeholder Growth Fund', NULL),
            (1000, '1234567', 'SYNFNDBND', 'CHF',
             '{"asset_class":"Funds","currency":"CHF","symbol":"SYNFNDBND","quantity":1,"total_value":1}',
             'Placeholder Government Bond Fund', NULL),
            (1000, '1234567', 'SYNOPT', 'USD',
             '{"asset_class":"Options","currency":"USD","symbol":"SYNOPT","quantity":1,"total_value":1}',
             'Placeholder Call Option', NULL),
            (1000, '1234567', 'SYNPM', 'CHF',
             '{"asset_class":"Precious Metals","currency":"CHF","symbol":"SYNPM","quantity":1,"total_value":1}',
             'Placeholder Bullion Bar', NULL),
            (1000, '1234567', 'SYNSP', 'CHF',
             '{"asset_class":"Structured Products","currency":"CHF","symbol":"SYNSP","quantity":1,"total_value":1}',
             'Placeholder Structured Note', NULL);
        INSERT INTO positions(snapshot_at, account_external_id, symbol, currency, payload, name, isin, source) VALUES
            (2000, '1234567', 'SMMCHA', 'CHF',
             '{"asset_class":"ETFs","currency":"CHF","symbol":"SMMCHA","quantity":100,"total_value":10000.00}',
             'Example Fund CHF dis', 'CH0000000060', 'live'),
            (1500, '1234567', 'Example Fund CHF DIS', 'CHF',
             '{"asset_class":"ETFs","currency":"CHF","symbol":"Example Fund CHF DIS","quantity":80,"total_value":7500.00}',
             'Example Fund CHF DIS', 'CH0000000060', 'pp:doc-abc');
        INSERT INTO currency_balances(snapshot_at, account_external_id, currency, payload) VALUES
            (1000, '1234567', 'CHF', '{"cash_balance":5000.00,"rate_to_chf":1.0,"currency":"CHF"}'),
            (1000, '1234567', 'USD', '{"cash_balance":100.00,"rate_to_chf":0.7862,"currency":"USD"}'),
            (1000, '1234567', 'EUR', '{"cash_balance":0.00,"rate_to_chf":0.914,"currency":"EUR"}');
        INSERT INTO transactions(account_external_id, occurred_at, transaction_type, isin, symbol, currency, net_amount, payload) VALUES
            ('1234567', 1500, 'Buy', 'IE00BJK9H753', 'IUSQ', 'USD', -1500.00,
             '{"name":"iShares Core MSCI World","quantity":10,"unit_price":150.00}'),
            ('1234567', 1700, 'Dividend', 'IE00BJK9H753', 'IUSQ', 'USD', 25.00,
             '{"name":"iShares Core MSCI World"}'),
            ('1234567', 1800, 'Debit', NULL, NULL, 'CHF', -200.00,
             '{"name":"Outgoing wire"}'),
            ('1234567', 1900, 'Custody Fees', NULL, NULL, 'CHF', -30.00, '{"name":""}');`)
	silvertest.Golden(t, openAdapter(t, path), "testdata/canonical.jsonl", *update)
}
