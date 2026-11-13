package cointracking

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// canonicalFixture reaches every type of the kind map, inbound and
// outbound, in fiat and in crypto, plus a type the map does not know and
// a row that states no amount. Its trades take every path: one side in
// the base currency either way, two non-base sides priced by the
// portfolio's prices, by the coin prices, and by neither, and a fee in a
// third currency. A second portfolio keeps its books in EUR. Two days of
// balances hold crypto and fiat. Every id, amount and price is invented.
const canonicalFixture = `
INSERT INTO dump_runs VALUES (1000, 2, '/x/1', NULL), (2000, 2, '/x/2', NULL);
INSERT INTO portfolios VALUES
    ('cu1', 1, 'First Book', 2000, NULL),
    ('cu2', 2, 'Euro Book', 2000, '{"note":"invented"}');
INSERT INTO wallets VALUES
    ('cu1', 'cu1:Exchange', 'Exchange', 2000, NULL),
    ('cu1', 'cu1:Cold', 'Cold', 2000, NULL),
    ('cu2', 'cu2:Exchange', 'Exchange', 2000, NULL);
INSERT INTO transactions (transaction_external_id, portfolio_external_id, wallet_external_id, snapshot_at,
                          occurred_at, type, buy_amount, buy_currency, sell_amount, sell_currency,
                          fee_amount, fee_currency, comment, lpn, payload) VALUES
    ('d-fiat',   'cu1', 'cu1:Exchange', 1000, TIMESTAMP '2024-01-01 09:00:00', 'Deposit',
     10000.0, 'USD', NULL, NULL, NULL, NULL, NULL, NULL, NULL),
    ('d-fx',     'cu1', 'cu1:Exchange', 1000, TIMESTAMP '2024-01-01 09:30:00', 'Deposit',
     500.0, 'EUR', NULL, NULL, NULL, NULL, NULL, NULL, NULL),
    ('d-coin',   'cu1', 'cu1:Cold', 1000, TIMESTAMP '2024-01-01 10:00:00', 'Deposit',
     0.5, 'BTC', NULL, NULL, NULL, NULL, 'from elsewhere', NULL, '{"tx_hash":"0xinvented"}'),
    ('t-buy',    'cu1', 'cu1:Exchange', 1000, TIMESTAMP '2024-01-02 10:00:00', 'Trade',
     1.0, 'BTC', 30000.0, 'USD', 15.0, 'USD', NULL, NULL, NULL),
    ('t-sell',   'cu1', 'cu1:Exchange', 1000, TIMESTAMP '2024-01-03 10:00:00', 'Trade',
     3200.0, 'USD', 2.0, 'ETH', 0.01, 'BNB', NULL, NULL, NULL),
    ('t-pp',     'cu1', 'cu1:Exchange', 1000, TIMESTAMP '2024-01-04 10:00:00', 'Trade',
     10.0, 'ETH', 0.5, 'BTC', NULL, NULL, NULL, NULL, NULL),
    ('t-cp',     'cu1', 'cu1:Exchange', 1000, TIMESTAMP '2024-01-04 11:00:00', 'Trade',
     100.0, 'SOL', 1.0, 'ETH', NULL, NULL, NULL, NULL, NULL),
    ('t-none',   'cu1', 'cu1:Exchange', 1000, TIMESTAMP '2024-01-04 12:00:00', 'Trade',
     1000.0, 'XYZ', 5.0, 'SOL', NULL, NULL, NULL, NULL, NULL),
    ('w-fiat',   'cu1', 'cu1:Exchange', 1000, TIMESTAMP '2024-01-05 09:00:00', 'Withdrawal',
     NULL, NULL, 2000.0, 'USD', NULL, NULL, NULL, NULL, NULL),
    ('w-coin',   'cu1', 'cu1:Cold', 1000, TIMESTAMP '2024-01-05 10:00:00', 'Withdrawal',
     NULL, NULL, 0.1, 'BTC', 0.0001, 'BTC', NULL, NULL, NULL),
    ('fee',      'cu1', 'cu1:Exchange', 1000, TIMESTAMP '2024-01-06 09:00:00', 'Other Fee',
     NULL, NULL, 5.0, 'USD', NULL, NULL, NULL, NULL, NULL),
    ('fee-coin', 'cu1', 'cu1:Exchange', 1000, TIMESTAMP '2024-01-06 10:00:00', 'Other Fee',
     NULL, NULL, 0.002, 'ETH', NULL, NULL, NULL, NULL, NULL),
    ('stake',    'cu1', 'cu1:Exchange', 2000, TIMESTAMP '2024-01-07 09:00:00', 'Staking',
     0.05, 'ETH', NULL, NULL, NULL, NULL, NULL, NULL, NULL),
    ('reward',   'cu1', 'cu1:Exchange', 2000, TIMESTAMP '2024-01-07 10:00:00', 'Reward / Bonus',
     1.0, 'SOL', NULL, NULL, NULL, NULL, 'promo', NULL, NULL),
    ('income',   'cu1', 'cu1:Exchange', 2000, TIMESTAMP '2024-01-07 11:00:00', 'Income',
     25.0, 'USD', NULL, NULL, NULL, NULL, NULL, NULL, NULL),
    ('in-nt',    'cu1', 'cu1:Cold', 2000, TIMESTAMP '2024-01-08 09:00:00', 'Income (non taxable)',
     0.2, 'BTC', NULL, NULL, NULL, NULL, NULL, NULL, NULL),
    ('out-nt',   'cu1', 'cu1:Exchange', 2000, TIMESTAMP '2024-01-08 09:00:00', 'Expense (non taxable)',
     NULL, NULL, 0.2, 'BTC', NULL, NULL, NULL, NULL, NULL),
    ('drop',     'cu1', 'cu1:Exchange', 2000, TIMESTAMP '2024-01-09 09:00:00', 'Airdrop',
     50.0, 'XYZ', NULL, NULL, NULL, NULL, NULL, NULL, NULL),
    ('drop-nt',  'cu1', 'cu1:Exchange', 2000, TIMESTAMP '2024-01-09 10:00:00', 'Airdrop (non taxable)',
     20.0, 'XYZ', NULL, NULL, NULL, NULL, NULL, NULL, NULL),
    ('tip',      'cu1', 'cu1:Exchange', 2000, TIMESTAMP '2024-01-10 09:00:00', 'Gift / Tip',
     0.01, 'ETH', NULL, NULL, NULL, NULL, NULL, NULL, NULL),
    ('gift',     'cu1', 'cu1:Exchange', 2000, TIMESTAMP '2024-01-10 10:00:00', 'Gift',
     NULL, NULL, 0.01, 'ETH', NULL, NULL, NULL, NULL, NULL),
    ('donate',   'cu1', 'cu1:Exchange', 2000, TIMESTAMP '2024-01-10 11:00:00', 'Donation',
     NULL, NULL, 10.0, 'USD', NULL, NULL, NULL, NULL, NULL),
    ('spend',    'cu1', 'cu1:Exchange', 2000, TIMESTAMP '2024-01-11 09:00:00', 'Spend',
     NULL, NULL, 0.001, 'BTC', NULL, NULL, 'coffee', NULL, NULL),
    ('lost',     'cu1', 'cu1:Cold', 2000, TIMESTAMP '2024-01-11 10:00:00', 'Lost',
     NULL, NULL, 0.001, 'BTC', NULL, NULL, NULL, NULL, NULL),
    ('stolen',   'cu1', 'cu1:Cold', 2000, TIMESTAMP '2024-01-11 11:00:00', 'Stolen',
     NULL, NULL, 1.0, 'XYZ', NULL, NULL, NULL, NULL, NULL),
    ('unknown',  'cu1', 'cu1:Exchange', 2000, TIMESTAMP '2024-01-12 09:00:00', 'Margin Profit',
     3.0, 'USD', NULL, NULL, NULL, NULL, NULL, NULL, NULL),
    ('unknown-out', 'cu1', 'cu1:Exchange', 2000, TIMESTAMP '2024-01-12 10:00:00', 'Margin Loss',
     NULL, NULL, 1.0, 'SOL', NULL, NULL, NULL, NULL, NULL),
    ('empty',    'cu1', 'cu1:Exchange', 2000, TIMESTAMP '2024-01-12 11:00:00', 'Deposit',
     NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
    ('eu-dep',   'cu2', 'cu2:Exchange', 1000, TIMESTAMP '2024-01-01 09:00:00', 'Deposit',
     5000.0, 'EUR', NULL, NULL, NULL, NULL, NULL, NULL, NULL),
    ('eu-buy',   'cu2', 'cu2:Exchange', 1000, TIMESTAMP '2024-01-02 09:00:00', 'Trade',
     0.1, 'BTC', 2800.0, 'EUR', NULL, NULL, NULL, NULL, NULL),
    ('eu-wd',    'cu2', 'cu2:Exchange', 2000, TIMESTAMP '2024-01-08 09:00:00', 'Withdrawal',
     NULL, NULL, 100.0, 'USD', NULL, NULL, NULL, NULL, NULL);
INSERT INTO positions_daily VALUES
    (DATE '2024-01-04', 'cu1', 'cu1:Exchange', 'BTC', 0.5, 1000),
    (DATE '2024-01-04', 'cu1', 'cu1:Exchange', 'ETH', 7.0, 1000),
    (DATE '2024-01-04', 'cu1', 'cu1:Exchange', 'USD', 3185.0, 1000),
    (DATE '2024-01-04', 'cu1', 'cu1:Exchange', 'EUR', 500.0, 1000),
    (DATE '2024-01-04', 'cu1', 'cu1:Cold', 'BTC', 0.5, 1000),
    (DATE '2024-01-12', 'cu1', 'cu1:Exchange', 'ETH', 7.048, 2000),
    (DATE '2024-01-12', 'cu1', 'cu1:Exchange', 'SOL', 95.0, 2000),
    (DATE '2024-01-12', 'cu1', 'cu1:Cold', 'BTC', 0.598, 2000),
    (DATE '2024-01-02', 'cu2', 'cu2:Exchange', 'BTC', 0.1, 1000),
    (DATE '2024-01-02', 'cu2', 'cu2:Exchange', 'EUR', 2200.0, 1000);
INSERT INTO portfolio_prices VALUES
    (DATE '2024-01-02', 'cu1', 'BTC', 'USD', 30000.0, 1000),
    (DATE '2024-01-04', 'cu1', 'BTC', 'USD', 32000.0, 1000),
    (DATE '2024-01-04', 'cu1', 'ETH', 'USD', 1600.0, 1000),
    (DATE '2024-01-04', 'cu1', 'EUR', 'USD', 1.1, 1000),
    (DATE '2024-01-12', 'cu1', 'ETH', 'USD', 1700.0, 2000),
    (DATE '2024-01-02', 'cu2', 'BTC', 'EUR', 28000.0, 1000);
INSERT INTO coin_prices VALUES
    (DATE '2024-01-04', 'SOL', 16.0, 'invented', 1000),
    (DATE '2024-01-12', 'SOL', 18.0, 'invented', 2000),
    (DATE '2024-01-12', 'BTC', 33000.0, 'invented', 2000);
`

// TestCanonicalGolden holds every record the adapter yields over
// canonicalFixture to testdata/canonical.jsonl.
func TestCanonicalGolden(t *testing.T) {
	path := newFixtureSilver(t, canonicalFixture)
	silvertest.Golden(t, openAdapter(t, path), "testdata/canonical.jsonl")
}
