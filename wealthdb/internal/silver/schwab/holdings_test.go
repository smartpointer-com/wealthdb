package schwab

import (
	"context"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// holdingSeed is a schwab-api silver's rows, its holdings stated in the
// payload: a fund with a name a dividend matches, a bond with its terms
// and an interest line that names them, cash, a short option.
const holdingSeed = `
INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 5, '/x/1'), (2000, 5, '/x/2');
INSERT INTO positions(snapshot_at, account_external_id, instrument_key, payload) VALUES
    (1000, 'ACC1', '999999999',
     '{"longQuantity":10.5,"shortQuantity":0,"marketValue":2500.25,"instrument":{"assetType":"COLLECTIVE_INVESTMENT",
       "type":"EXCHANGE_TRADED_FUND","cusip":"999999999","symbol":"VTI","description":"EXAMPLE TOTAL MARKET ETF"}}'),
    (1000, 'ACC1', '912796ZZ9',
     '{"longQuantity":10000,"marketValue":9950.0,"instrument":{"assetType":"FIXED_INCOME","cusip":"912796ZZ9",
       "symbol":"912796ZZ9","description":"US TREASURY BILL","maturityDate":"2099-09-09T05:00:00.000+00:00","variableRate":4.125}}'),
    (2000, 'ACC1', 'MMDA1',
     '{"longQuantity":1234.56,"marketValue":1234.56,"instrument":{"assetType":"CASH_EQUIVALENT","symbol":"MMDA1"}}'),
    (2000, 'ACC1', 'SPX   990119C09990000',
     '{"longQuantity":0,"shortQuantity":2,"marketValue":-300.0,"instrument":{"assetType":"OPTION",
       "symbol":"SPX   990119C09990000","description":"SPX 01/19/2099 999 C"}}');
INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, payload) VALUES
    ('A1', 1500, 'ACC1', 'DIVIDEND_OR_INTEREST',
     '{"netAmount":5.00,"description":"EXAMPLE TOTAL MARKET ETF","transferItems":[]}'),
    ('A2', 1600, 'ACC1', 'DIVIDEND_OR_INTEREST',
     '{"netAmount":206.25,"description":"US TREASU NT 4.125% UST NOTE DUE 09/09/2099","transferItems":[]}');`

// A silver older than migration 0006 states a holding in its payload
// alone, and reads as the same records as the silver the migration
// fills, with no advice.
func TestASilverOlderThanTheHoldingColumnsReadsTheSame(t *testing.T) {
	migratedPath, migrated := newFixtureSilver(t)
	silvertest.Exec(t, migrated, holdingSeed)
	olderPath, older := newFixtureSilver(t)
	silvertest.Exec(t, older, `DROP TRIGGER positions_promote`)
	for _, c := range holdingColumns {
		silvertest.Exec(t, older, `ALTER TABLE positions DROP COLUMN `+c)
	}
	silvertest.Exec(t, older, holdingSeed)

	dump := func(path string) string {
		conn := openAdapter(t, path)
		if notes := conn.(silver.SchemaAdviser).SchemaAdvice(); len(notes) != 0 {
			t.Errorf("advice = %+v, want none", notes)
		}
		out, err := silvertest.DumpCanonical(context.Background(), conn, 0)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	want, got := dump(migratedPath), dump(olderPath)
	if got != want {
		t.Errorf("older silver reads\n%s\nwant\n%s", got, want)
	}
	if !strings.Contains(want, "912796ZZ9") || !strings.Contains(want, `"interest_rate"`) {
		t.Errorf("records lack the bond and its terms:\n%s", want)
	}
	// The interest line reaches the bond through the held-bond read.
	if !strings.Contains(want, `"InstrumentExternalID":"912796ZZ9","InstrumentHint":"","Kind":"interest"`) {
		t.Errorf("the bill's interest line names no bond:\n%s", want)
	}
}

// A holding reads from the promoted columns, not from its payload: a
// bond's terms, its quantity and its value are the columns'.
func TestAHoldingReadsItsColumns(t *testing.T) {
	path, db := newFixtureSilver(t)
	silvertest.Exec(t, db, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 6, '/x/1');
        INSERT INTO positions(snapshot_at, account_external_id, instrument_key, payload) VALUES
            (1000, 'ACC1', '912796ZZ9', '{}');
        UPDATE positions SET asset_type = 'FIXED_INCOME', cusip = '912796ZZ9', symbol = '912796ZZ9',
            maturity_date = 4092595200, variable_rate = '4.125',
            long_quantity = '10000', short_quantity = '0.00001', market_value = '9950.50';`)
	conn := openAdapter(t, path)
	batch := silvertest.Merged(t, conn, silvertest.WindowSince0(t, conn))
	if len(batch.Positions) != 1 || len(batch.Instruments) != 1 {
		t.Fatalf("positions %d, instruments %d, want one of each", len(batch.Positions), len(batch.Instruments))
	}
	p, i := batch.Positions[0], batch.Instruments[0]
	if p.Quantity.String() != "9999.99999" || p.MarketValue.String() != "9950.5" || p.MarketValue.Exponent() != -2 {
		t.Errorf("quantity %s, value %v, want 9999.99999 and 9950.50", p.Quantity, p.MarketValue)
	}
	if got := string(i.Payload); got != `{"interest_rate":"4.125","maturity_date":"2099-09-09"}` {
		t.Errorf("bond terms = %s", got)
	}
	if i.CUSIP == nil || *i.CUSIP != "912796ZZ9" {
		t.Errorf("cusip = %v, want the column's", i.CUSIP)
	}
}
