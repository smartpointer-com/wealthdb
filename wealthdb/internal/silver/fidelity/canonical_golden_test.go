package fidelity

import (
	"flag"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

var update = flag.Bool("update", false, "rewrite testdata/canonical_*.jsonl")

// TestCanonicalGolden holds every record the adapter yields to a golden
// file per case, over the rows of the adapter tests: open lots carried
// across dumps, realized lots of every document kind, the portfolio and
// pool taxonomy, and a ledger of the verbs whose kind the sign decides.
func TestCanonicalGolden(t *testing.T) {
	for _, c := range []struct{ name, seed string }{
		{"lots", `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES
            (1000, 13, '/x/1'), (2000, 13, '/x/2'), (3000, 13, '/x/3'), (4000, 13, '/x/4');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES
            (1000, 'ACC1', '{}'), (2000, 'ACC1', '{}'), (3000, 'ACC1', '{}'), (4000, 'ACC1', '{}');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, description, asset_class, quantity, current_value, cost_basis_total, payload) VALUES
            (1000, 'ACC1', 'VTI', 'Total Market ETF', 'etf', 10, 2500, 1000, '{}'),
            (2000, 'ACC1', 'VTI', 'Total Market ETF', 'etf', 10, 2600, 1000, '{}'),
            (3000, 'ACC1', 'VTI', 'Total Market ETF', 'etf', 12, 3100, 1500, '{}'),
            (4000, 'ACC1', 'QQQ', 'Nasdaq ETF', 'etf', 6, 3000, 2400, '{}'),
            (4000, 'ACC1', 'BND1', 'Treasury Note', 'bond', 5000, 4950, NULL, '{}');
        INSERT INTO open_lots(snapshot_at, account_external_id, instrument_key, lot_index, cusip, quantity, unit_cost, cost_basis, acquired_date, unrealized_gain_loss, current_value, term, source_sha256, payload) VALUES
            (1000, 'ACC1', 'VTI', 0, 'SYNCUSIP1', 6, 100, 600, '2020-03-02', 900, 1500, 'LONG', 'sha-a', '{"cells":["Mar-02-2020","6.000"],"page":1}'),
            (1000, 'ACC1', 'VTI', 1, 'SYNCUSIP1', 4, 99.9975, 399.99, '2024-11-15', 600.01, 1000, 'SHORT', 'sha-a', '{"cells":["Nov-15-2024","4.000"],"page":1}'),
            (4000, 'ACC1', 'VTI', 0, 'SYNCUSIP1', 12, 125, 1500, '2020-03-02', 1600, 3100, 'LONG', 'sha-d', '{}'),
            (4000, 'ACC1', 'QQQ', 0, 'SYNCUSIP2', 5, 400, 2000, '2023-01-10', 500, 2500, 'LONG', 'sha-b', '{}'),
            (4000, 'ACC1', 'BND1', 0, 'BND1', 5000, 99, 4950, 'Transferred', 0, 4950, NULL, 'sha-c', '{}');`},
		{"realized", `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 13, '/x/1');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, description, quantity, payload) VALUES
            (1000, 'ACC1', 'VTI', 'Total Market ETF', 10, '{}'),
            (1000, 'ACC1', 'BND1', 'Treasury Note', 5000, '{}');
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, instrument_key, amount, payload) VALUES
            ('t1', 1000, 'ACC1', 'SELL', 'QQQ', 100, '{}');
        INSERT INTO open_lots(snapshot_at, account_external_id, instrument_key, lot_index, cusip, quantity) VALUES
            (1000, 'ACC1', 'VTI', 0, 'SYNCUSIP1', 10);
        INSERT INTO closed_lots(lot_id, document_kind, account_external_id, tax_year, form_prepared, security_name, instrument_key, cusip, action, quantity, acquired_date, disposed_date, settlement_date, proceeds, cost_basis, wash_sale_disallowed, realized_gain_loss, fees, term, covered, form_8949_box, specific_share_id, corrected, source_sha256) VALUES
            ('f1', 'form_1099b', 'ACC1', 2024, '2025-02-14', 'TOTAL MARKET ETF', 'SYNCUSIP1', 'SYNCUSIP1', 'Sale', 3, 'Various', '2024-05-01', NULL, 900, 600, NULL, 300, NULL, 'LONG', 1, 'D', NULL, 0, 'sha-1099'),
            ('f2', 'form_1099b', 'ACC1', 2024, '2025-02-14', 'NASDAQ ETF', 'QQQ', 'SYNCUSIP2', 'Sale', 2, '2024-01-03', '2024-06-03', NULL, 1000, 1100, 50, -50, NULL, 'SHORT', 1, 'A', NULL, 1, 'sha-1099'),
            ('s1', 'statement', 'ACC1', NULL, NULL, 'NASDAQ ETF', 'SYNCUSIP2', NULL, 'You Sold', -2, NULL, NULL, '2024-06-05', 1000, 1100, NULL, -100, -0.5, 'SHORT', NULL, NULL, 1, NULL, 'sha-stmt'),
            ('s2', 'statement', 'ACC1', NULL, NULL, 'OLD HOLDING INC', 'SYNCUSIP9', NULL, 'You Sold', 7, NULL, NULL, '2023-08-01', 700, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'sha-stmt'),
            ('w1', 'closed_positions', 'ACC1', 2025, NULL, 'NASDAQ ETF', 'QQQ', 'SYNCUSIP6', NULL, 1, '2024-02-01', '2025-03-03', NULL, 500, 450, NULL, 50, NULL, 'LONG', NULL, NULL, NULL, NULL, 'sha-page'),
            ('s3', 'statement', 'ACC1', NULL, NULL, 'NASDAQ ETF', 'SYNCUSIP6', NULL, 'You Sold', 1, NULL, NULL, '2025-03-05', 500, 450, NULL, 50, NULL, 'LONG', NULL, NULL, NULL, NULL, 'sha-stmt'),
            ('f3', 'form_1099b', 'ACC2', 2024, '2025-02-14', 'NASDAQ ETF', 'QQQX', 'SYNCUSIP6', 'Sale', 1, 'Unknown', '2024-09-09', NULL, 400, NULL, NULL, NULL, NULL, NULL, 0, NULL, NULL, NULL, 'sha-1099'),
            ('f4', 'form_1099b', 'ACC2', 2024, '2025-02-14', 'RENAMED CO', 'AAA', 'SYNCUSIP7', 'Sale', 1, '2020-01-01', '2024-09-10', NULL, 10, 8, NULL, 2, NULL, 'LONG', 1, 'D', NULL, NULL, 'sha-1099'),
            ('f5', 'form_1099b', 'ACC2', 2024, '2025-02-14', 'RENAMED CO', 'BBB', 'SYNCUSIP7', 'Sale', 1, '2020-01-01', '2024-09-11', NULL, 10, 8, NULL, 2, NULL, 'LONG', 1, 'D', NULL, NULL, 'sha-1099'),
            ('s4', 'statement', 'ACC2', NULL, NULL, 'RENAMED CO', 'SYNCUSIP7', NULL, 'You Sold', 1, NULL, NULL, '2024-09-12', 10, 8, NULL, 2, NULL, NULL, NULL, NULL, NULL, NULL, 'sha-stmt'),
            ('s5', 'statement', 'ACC2', NULL, NULL, 'NASDAQ ETF', 'SYNCUSIP6', NULL, 'You Sold', 1, NULL, NULL, '2024-09-12', 400, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'sha-stmt'),
            ('s6', 'statement', 'ACC2', NULL, NULL, 'TREASURY NOTE', 'BND1', NULL, 'You Sold', 1000, NULL, NULL, '2024-10-01', 990, 1000, NULL, -10, NULL, NULL, NULL, NULL, NULL, NULL, 'sha-stmt');`},
		{"daf", `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 7, '/x/1');
        INSERT INTO portfolios(snapshot_at, portfolio_external_id, kind, payload) VALUES
            (1000, 'PORT1', '529', '{}'),
            (1000, 'Fidelity Charitable Giving', 'daf', '{}');
        INSERT INTO accounts(snapshot_at, account_external_id, portfolio_external_id, nickname, management_style, payload) VALUES
            (1000, 'ACC1', 'PORT1', 'College', NULL, '{}'),
            (1000, '9990001', 'Fidelity Charitable Giving', 'Example Giving Account', 'automated', '{}');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, description, asset_class, currency, is_core_position, quantity, current_value, payload) VALUES
            (1000, 'ACC1', 'VTI', 'Vanguard Total Market', 'etf', 'USD', 0, 10, 2500.00, '{}'),
            (1000, '9990001', 'POOLX1', 'Example Growth Pool', 'daf_pool', 'USD', 0, 1000, 100000.00, '{}');`},
		{"ledger", `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 4, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, portfolio_external_id, nickname, management_style, payload) VALUES
            (1000, '000000001', 'PORT1', NULL, NULL, '{}');
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, instrument_key, currency, quantity, price, amount, payload) VALUES
            ('act1', 880, 'ACC1', 'DIVIDEND', 'VTI', 'USD', 0, 0, 12.00, '{}'),
            ('act2', 890, 'ACC1', 'BUY', 'VTI', 'USD', 5, 250.00, -1250.00, '{}'),
            ('adv',  900, 'ACC1', 'ADVISOR', NULL, 'USD', 0, 0, -1234.00,
             '{"Action": "ADVISOR FEE DEDUCTED Investment Mgr Fee (Cash)"}'),
            ('wire', 910, 'ACC1', 'WIRE',    NULL, 'USD', 0, 0, -5678.00,
             '{"Action": "WIRE TRANSFER TO BANK (Cash)"}'),
            ('wirein', 915, 'ACC1', 'WIRE',   NULL, 'USD', 0, 0,  5678.00,
             '{"Action": "WIRE TRANSFER FROM BANK (Cash)"}'),
            ('achout', 916, 'ACC1', 'DIRECT_DEBIT', NULL, 'USD', 0, 0, -2000.00,
             '{"Action": "DIRECT DEBIT EXAMPLE BROKERAGE MONEYLINK"}'),
            ('achin', 917, 'ACC1', 'DIRECT_DEPOSIT', NULL, 'USD', 0, 0, 2000.00,
             '{"Action": "DIRECT DEPOSIT EXAMPLE BROKERAGE MONEYLINK"}'),
            ('achrev', 918, 'ACC1', 'DIRECT_DEBIT', NULL, 'USD', 0, 0, 15.00,
             '{"Action": "DIRECT DEBIT EXAMPLE BROKERAGE MONEYLINK"}'),
            ('adr',  920, 'ACC1', 'FEE',     'XYZ', 'USD', 0, 0, -1.50,
             '{"Action": "FEE CHARGED EXAMPLE CORP SPON ADR (XYZ) (Cash)", "Description": "EXAMPLE CORP SPON ADR"}'),
            ('cashout',   930, 'ACC1', 'TRANSFER', NULL,  'USD', NULL, NULL, -2500.00,
             '{"Action": "INTER BROKER DEBIT EXAMPLE BROKERAGE"}'),
            ('cashin',    931, 'ACC1', 'TRANSFER', NULL,  'USD', NULL, NULL, 2500.00,
             '{"Action": "INTER BROKER CREDIT EXAMPLE BROKERAGE"}'),
            ('sharesout', 932, 'ACC1', 'TRANSFER', 'VTI', 'USD', -10, NULL, -2000.00,
             '{"Action": "INTER BROKER DELIVER VANGUARD TOTAL STK MKT ETF"}'),
            ('sharesin',  933, 'ACC1', 'TRANSFER', 'VTI', 'USD', 10, NULL, 0.00,
             '{"Action": "INTER BROKER RECEIVE VANGUARD TOTAL STK MKT ETF"}'),
            ('unvalued',  934, 'ACC1', 'TRANSFER', 'VTI', 'USD', -10, NULL, NULL,
             '{"Action": "INTER BROKER DELIVER VANGUARD TOTAL STK MKT ETF"}'),
            ('disagree',  935, 'ACC1', 'TRANSFER', 'VTI', 'USD', 10, NULL, -2000.00,
             '{"Action": "RECEIVED FROM YOU VANGUARD TOTAL STK MKT ETF"}'),
            ('nosign',    936, 'ACC1', 'TRANSFER', NULL,  'USD', 0, NULL, 0.00,
             '{"Action": "TRANSFERRED FROM EXAMPLE ACCOUNT"}'),
            ('sibling',   937, '000000001', 'TRANSFER', NULL, 'USD', NULL, NULL, -100.00,
             '{"Action": "TRANSFERRED TO VS 000-000003-1 (Cash)"}'),
            ('selfout',   940, '000000001', 'TRANSFER', NULL, 'USD', NULL, NULL, -100.00,
             '{"Action": "TRANSFERRED TO VS 000-000001-2 (Margin)"}'),
            ('selfin',    938, '000000001', 'TRANSFER', NULL, 'USD', NULL, NULL, 100.00,
             '{"Action": "TRANSFERRED FROM VS 000-000001-1 (Cash)"}'),
            ('journal',   939, 'ACC1', 'JOURNAL',  NULL,  'USD', NULL, NULL, -50.00,
             '{"Action": "JOURNALED EXAMPLE"}');`},
	} {
		t.Run(c.name, func(t *testing.T) {
			path, db := newFixtureSilver(t)
			addLotsSchema(t, db)
			seedSQL(t, db, c.seed)
			silvertest.Golden(t, openAdapter(t, path), "testdata/canonical_"+c.name+".jsonl", *update)
		})
	}
}
