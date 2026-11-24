package cointracking

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/duckconn/duckconntest"
)

// pointPrice is the latest price on or before the instant at, by one
// query per lookup: the reference the ASOF joins of transactionsQuery
// are held to.
func pointPrice(t *testing.T, db *sql.DB, q string, args ...any) sql.NullString {
	t.Helper()
	var p sql.NullString
	if err := db.QueryRow(q, args...).Scan(&p); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	return p
}

// TestTheASOFJoinsReadTheSamePriceRowsAsAPointQuery holds each price the
// main query joins beside a row to the row a point query picks: the
// latest on or before the trade's day, for either side, from the
// portfolio's prices in its base currency and from the coin prices.
// Two coin prices of one day from two sources tie; the point query keeps
// the first stored, and so does the join.
func TestTheASOFJoinsReadTheSamePriceRowsAsAPointQuery(t *testing.T) {
	path := newFixtureSilver(t, canonicalFixture+`
INSERT INTO coin_prices VALUES
    (DATE '2024-01-04', 'SOL', 17.0, 'second', 1000),
    (DATE '2024-01-03', 'ETH', 1500.0, 'invented', 1000),
    (DATE '2024-01-03', 'ETH', 1400.0, 'other', 1000);
`)
	db, err := sql.Open("duckdb", path+"?access_mode=read_only")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	q, args := transactionsQuery(canonical.FullWindow(1))
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	const portfolioQ = `
SELECT CAST(price AS VARCHAR) FROM portfolio_prices
 WHERE portfolio_external_id = ? AND instrument_external_id = ? AND quote_currency = ?
   AND as_of_date <= to_timestamp(?)::DATE
 ORDER BY as_of_date DESC LIMIT 1`
	const coinQ = `
SELECT CAST(price_usd AS VARCHAR) FROM coin_prices
 WHERE instrument_external_id = ? AND as_of_date <= to_timestamp(?)::DATE
 ORDER BY as_of_date DESC LIMIT 1`
	priced := 0
	for rows.Next() {
		var (
			txID, portfolio, wallet, ctType, buyCcy, sellCcy, feeCcy, comment, base string
			at                                                                      int64
			buyAmt, sellAmt, feeAmt, payload                                        sql.NullString
			p                                                                       tradePrices
		)
		if err := rows.Scan(&txID, &portfolio, &wallet, &at, &ctType, &buyAmt, &buyCcy,
			&sellAmt, &sellCcy, &feeAmt, &feeCcy, &payload, &comment, &base,
			&p.sellPortfolio, &p.sellCoin, &p.buyPortfolio, &p.buyCoin); err != nil {
			t.Fatal(err)
		}
		if base == "" {
			base = "USD"
		}
		base = strings.ToUpper(base)
		sell, buy := strings.ToUpper(sellCcy), strings.ToUpper(buyCcy)
		want := tradePrices{
			sellPortfolio: pointPrice(t, db, portfolioQ, portfolio, sell, base, at),
			sellCoin:      pointPrice(t, db, coinQ, sell, at),
			buyPortfolio:  pointPrice(t, db, portfolioQ, portfolio, buy, base, at),
			buyCoin:       pointPrice(t, db, coinQ, buy, at),
		}
		if p != want {
			t.Errorf("%s prices = %+v, want the point queries' %+v", txID, p, want)
		}
		for _, v := range []sql.NullString{p.sellPortfolio, p.sellCoin, p.buyPortfolio, p.buyCoin} {
			if v.Valid {
				priced++
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if priced < 8 {
		t.Errorf("priced lookups = %d, want the fixture to reach the prices", priced)
	}
}

// A trade shortly before midnight UTC takes that day's price, also on a
// machine whose own day has already turned.
func TestATradesPriceDayIsItsUTCDay(t *testing.T) {
	path := newFixtureSilver(t, `
        INSERT INTO dump_runs VALUES (1000, 2, '/x/1', NULL);
        INSERT INTO portfolios VALUES ('cu1', 1, 'aaa', 1000, NULL);
        INSERT INTO wallets VALUES ('cu1', 'cu1:Kraken', 'Kraken', 1000, NULL);
        INSERT INTO transactions (transaction_external_id, portfolio_external_id, wallet_external_id, snapshot_at,
                                  occurred_at, type, buy_amount, buy_currency, sell_amount, sell_currency,
                                  fee_amount, fee_currency, comment) VALUES
            ('t1', 'cu1', 'cu1:Kraken', 1000, TIMESTAMP '2024-01-01 23:30:00', 'Trade',
             10.0, 'ETH', 0.5, 'BTC', 0, NULL, NULL);
        INSERT INTO portfolio_prices VALUES
            (DATE '2024-01-01', 'cu1', 'BTC', 'USD', 30000.0, 1000),
            (DATE '2024-01-02', 'cu1', 'BTC', 'USD', 40000.0, 1000);
    `)
	conn := openAdapter(t, path)
	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := conn.Transactions(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	batch, _, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Transactions) != 2 {
		t.Fatalf("%d rows, want the trade's two legs", len(batch.Transactions))
	}
	for _, tx := range batch.Transactions {
		if tx.NetAmount == nil || tx.NetAmount.Abs().String() != "15000" {
			t.Errorf("%s: net amount %v, want 15000 either way: 0.5 BTC at the price of 2024-01-01",
				tx.TransactionExternalID, tx.NetAmount)
		}
	}

	duckconntest.AlsoInZone(t, "Pacific/Auckland")
}
