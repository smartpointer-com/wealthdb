package schwab

import (
	"flag"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

var update = flag.Bool("update", false, "rewrite testdata/canonical_*.jsonl")

// TestCanonicalGolden holds every record the adapter yields to a golden
// file per case, over the rows of the adapter tests: the api feed alone,
// the web feed alone, and both merged, with statement holdings, open
// lots and the realized lots of every year-end document.
func TestCanonicalGolden(t *testing.T) {
	t.Run("api", func(t *testing.T) {
		path, db := newFixtureSilver(t)
		silvertest.Exec(t, db, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES
            (1000, 'ACC1', '{"hashValue":"ACC1","accountNumber":"redacted"}');
        INSERT INTO account_balances(snapshot_at, account_external_id, balance_kind, payload) VALUES
            (1000, 'ACC1', 'current', '{"cashBalance":1234.56}');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, payload) VALUES
            (1000, 'ACC1', '037833100',
             '{"longQuantity":10,"shortQuantity":0,"marketValue":1500.00,
               "instrument":{"assetType":"EQUITY","cusip":"037833100","symbol":"AAPL","description":"Apple Inc"}}');
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, payload) VALUES
            ('A1', 1500, 'ACC1', 'TRADE',
             '{"netAmount":-1505.00,"transferItems":[
                {"instrument":{"assetType":"EQUITY","cusip":"037833100","symbol":"AAPL"},
                 "amount":10,"cost":-1500.00,"price":150.00,"positionEffect":"OPENING"},
                {"instrument":{"assetType":"CURRENCY","cusip":"9ZZZFD494"},
                 "amount":-1505.00,"cost":-1505.00}]}'),
            ('A2', 2000, 'ACC1', 'DIVIDEND_OR_INTEREST',
             '{"netAmount":50.00,"transferItems":[]}');`)
		silvertest.Golden(t, openAdapter(t, path), "testdata/canonical_api.jsonl", *update)
	})
	t.Run("web", func(t *testing.T) {
		periodStart, asOf := utcDay("2023-12-01"), utcDay("2023-12-31")
		c := openWebAlone(t, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, nickname, payload) VALUES
            (900,  '5678', NULL,     '{}'),
            (1000, '5678', 'Saving', '{"account_number_full":"1111-5678"}');
        INSERT INTO historical_position_snapshots
            (as_of_date, account_external_id, instrument_key, quantity, market_value, cost_basis,
             source_sha256, payload) VALUES
            (?1, '5678', 'VTI', 10, 1500, 1000, 'sha-stmt', '{"section":"Exchange Traded Funds","description":"EXAMPLE TOTAL MARKET ETF"}');
        INSERT INTO historical_cash_balances(period_end, period_start, account_external_id, currency_iso,
                                             opening_balance, closing_balance, source_sha256, payload) VALUES
            (?1, ?2, '5678', 'USD', 50, 75, 'sha-stmt', '{}');
        INSERT INTO open_lots
            (as_of_date, account_external_id, instrument_key, lot_index, quantity, unit_cost, cost_basis,
             acquired_date, unrealized_gain_loss, term, covered, footnotes, source_sha256, payload) VALUES
            (?1, '5678', 'VTI', 0, 10, 100, 1000, '2021-03-02', 500, 'LONG', NULL, NULL, 'sha-stmt', '{}');
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, instrument_key,
            source, source_sha256, payload) VALUES
            ('T1', 500,  '5678', 'Deposit', NULL, 'statement_pdf', 'sha-stmt', '{"amount":250,"description":"FUNDS"}'),
            ('T2', 2000, '5678', 'Buy',     'VTI', 'statement_pdf', 'sha-stmt', '{"amount":-100,"quantity":1,"description":"VTI"}');`,
			asOf, periodStart)
		silvertest.Golden(t, c, "testdata/canonical_web.jsonl", *update)
	})
	t.Run("merged", func(t *testing.T) {
		f := newMergedFixture(t)
		asOf := utcDay("2023-12-31")
		silvertest.Exec(t, f.api, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, payload) VALUES
            ('API1', ?, 'HASH1', 'TRADE', '{"netAmount":-100}')`, utcDay("2022-01-03"))
		silvertest.Exec(t, f.web, realizedFixture)
		silvertest.Exec(t, f.web, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO historical_cash_balances(period_end, period_start, account_external_id, currency_iso,
                                             opening_balance, closing_balance, source_sha256, payload) VALUES
            (?1, ?1 - 30*86400, '5678', 'USD', 1, 2, 'sha-stmt', '{}');
        INSERT INTO historical_position_snapshots
            (as_of_date, account_external_id, instrument_key, quantity, market_value, cost_basis,
             unrealized_gain_loss, source_sha256, payload) VALUES
            (?1, '5678', 'VTI',  10, 1500, 1000, 500,  'sha-stmt', '{"section":"Exchange Traded Funds"}'),
            (?1, '5678', 'QQQ',   5,  700, NULL, 200,  'sha-stmt', '{"section":"Exchange Traded Funds"}'),
            (?1, '5678', 'SPX 01/16/2026 50.00 C', -2, -300, NULL, 100, 'sha-stmt', '{"section":"Options"}'),
            (?1, '5678', 'XMPL',  1, NULL, NULL, NULL, 'sha-stmt', '{"section":"Equities"}'),
            (?1, '9999', 'VTI',   1,  150,  100,  50,  'sha-other', '{"section":"Exchange Traded Funds"}');
        INSERT INTO open_lots
            (as_of_date, account_external_id, instrument_key, lot_index, quantity, unit_cost, cost_basis,
             acquired_date, unrealized_gain_loss, term, covered, footnotes, source_sha256, payload) VALUES
            (?1, '5678', 'VTI', 0, 4, 100, 400, '2021-03-02', 200, 'LONG',  NULL, NULL, 'sha-stmt',
             '{"holding_days":1000,"raw_line":"lot line 0"}'),
            (?1, '5678', 'VTI', 1, 6, 100, 600, '2020-05-01', 300, 'SHORT', NULL, 't',  'sha-stmt',
             '{"holding_days":1300,"raw_line":"lot line 1"}'),
            (?1, '5678', 'QQQ', 0, 5, NULL, NULL, NULL, NULL, NULL, NULL, 'r', 'sha-stmt',
             '{"holding_days":null,"raw_line":"lot line r"}'),
            (?1, '5678', 'SPX 01/16/2026 50.00 C', 0, -2, 2, -400, '2023-11-01', 100, NULL, NULL, 'S', 'sha-stmt',
             '{"holding_days":60,"raw_line":"lot line S"}'),
            (?1, '9999', 'VTI', 0, 1, 100, 100, '2020-01-02', 50, 'LONG', NULL, NULL, 'sha-other',
             '{"holding_days":1400,"raw_line":"unbridged"}');`, asOf)
		silvertest.Golden(t, f.open(t), "testdata/canonical_merged.jsonl", *update)
	})
}
