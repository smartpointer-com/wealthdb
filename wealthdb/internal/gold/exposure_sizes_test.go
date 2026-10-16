package gold

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// A stock's market cap is EDGAR's shares times the price a position or a
// fund line implies, never a receipt's. Its size comes from the market
// cap where known, else from its rank among its country's stocks in a
// size fund: by falling weight, large until 70% of the market, mid until
// 85%. A fund that covers 85% of its markets holds no small stock, and a
// stock in two size funds takes the one that covers most. A size fund
// that names its stocks alone sizes the shares its lines name. A receipt
// takes its share's size, found for that alone where a source describes
// it. Size funds are wanted as reference funds; the
// sizes view splits public equity.
func TestExposureSizesStocks(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	day := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC).Unix()
	for _, q := range []string{
		`INSERT INTO accounts (silver_source_id, account_external_id, account_kind, display_name, base_currency,
		 first_seen_at, last_seen_at) VALUES ('test-src', 'A', 'brokerage', 'Brokerage', 'USD', 0, 0)`,
		`INSERT INTO instruments (silver_source_id, instrument_external_id, asset_class, vehicle, isin, symbol, currency,
		 first_seen_at, last_seen_at) VALUES
		 ('test-src', 'QBIG', 'public_equity', 'stock', 'US0000000A01', 'QBIG', 'USD', 0, 0),
		 ('test-src', 'QMID', 'public_equity', 'stock', 'US0000000A02', 'QMID', 'USD', 0, 0),
		 ('test-src', 'QRCP', 'public_equity', 'stock', 'US0000000A03', 'QRCP', 'USD', 0, 0),
		 ('test-src', 'QRC2', 'public_equity', 'stock', 'US0000000A04', 'QRC2', 'USD', 0, 0),
		 ('test-src', 'QALL', 'public_equity', 'etf', NULL, 'QALL', 'USD', 0, 0),
		 ('test-src', 'QETF', 'public_equity', 'etf', NULL, 'QETF', 'USD', 0, 0),
		 ('test-src', 'QBND', 'fixed_income', 'bond', 'XX0000000A05', NULL, 'USD', 0, 0),
		 ('test-src', 'QPRC', 'public_equity', 'stock', 'CH0000000D01', NULL, 'CHF', 0, 0),
		 ('test-src', 'QRPX', 'public_equity', 'fund', NULL, 'QRPX', 'USD', 0, 0)`,
		`INSERT INTO positions (silver_source_id, snapshot_at, account_external_id, position_key, instrument_external_id,
		 asset_class, vehicle, currency, quantity, market_value) VALUES
		 ('test-src', ?, 'A', 'QBIG', 'QBIG', 'public_equity', 'stock', 'USD', 1, 100),
		 ('test-src', ?, 'A', 'QMID', 'QMID', 'public_equity', 'stock', 'USD', 2, 100),
		 ('test-src', ?, 'A', 'QRCP', 'QRCP', 'public_equity', 'stock', 'USD', 2, 100),
		 ('test-src', ?, 'A', 'QRC2', 'QRC2', 'public_equity', 'stock', 'USD', 2, 100),
		 ('test-src', ?, 'A', 'QALL', 'QALL', 'public_equity', 'etf', 'USD', 1, 100),
		 ('test-src', ?, 'A', 'QETF', 'QETF', 'public_equity', 'etf', 'USD', 1, 100),
		 ('test-src', ?, 'A', 'QBND', 'QBND', 'fixed_income', 'bond', 'USD', 1, 100),
		 ('test-src', ?, 'A', 'QPRC', 'QPRC', 'public_equity', 'stock', 'USD', 1, 100),
		 ('test-src', ?, 'A', 'QRPX', 'QRPX', 'public_equity', 'fund', 'USD', 1, 100)`,
		`INSERT INTO security_facts (silver_source_id, security_key, field, as_of, provider, value) VALUES
		 ('refdata', 'isin:US0000000A01', 'shares_outstanding', ?, 'sec-edgar', '200000000'),
		 ('refdata', 'isin:US0000000A02', 'shares_outstanding', ?, 'sec-edgar', '50000000'),
		 ('refdata', 'isin:US0000000A03', 'shares_outstanding', ?, 'sec-edgar', '1000000000'),
		 ('refdata', 'isin:US0000000B02', 'shares_outstanding', ?, 'sec-edgar', '1000000'),
		 ('refdata', 'isin:US0000000A03', 'issuer_isins', ?, 'gleif', 'JP0000000C02'),
		 ('refdata', 'isin:US0000000A04', 'sector', ?, 'testfact', 'Materials'),
		 ('refdata', 'isin:US0000000A04', 'risk_country', ?, 'testfact', 'JP'),
		 ('refdata', 'symbol:QETF', 'market_cap', ?, 'testfact', '50000000000'),
		 ('refdata', 'isin:US0000000B04', 'shares_outstanding', ?, 'sec-edgar', '1'),
		 ('refdata', 'isin:CH0000000D02', 'market_cap', ?, 'testfact', '1000000000')`,
		`UPDATE instruments SET name = 'QLAND GAMMA SPONSORED ADR' WHERE instrument_external_id = 'QRC2'`,
		`INSERT INTO security_identities (silver_source_id, security_key, as_of, provider, security_type)
		 VALUES ('refdata', 'isin:US0000000A03', ?, 'openfigi', 'Depositary Receipt')`,
		`INSERT INTO exposure_size_funds (fund_ref, coverage_pct) VALUES ('symbol:QALL', 100), ('symbol:QLM', 85),
		 ('symbol:QNAM', 100)`,
		`INSERT INTO fund_compositions (silver_source_id, fund_ref, as_of, basis, provider, line_count, currency) VALUES
		 ('test-src', 'symbol:QALL', ?, 'holdings', 'testprov', 6, 'USD'),
		 ('test-src', 'symbol:QLM', ?, 'holdings', 'testprov', 4, 'USD'),
		 ('test-src', 'symbol:QNEW', ? + 86400, 'holdings', 'testprov', 1, 'USD'),
		 ('test-src', 'symbol:QEUR', ? + 2 * 86400, 'holdings', 'testprov', 1, 'EUR'),
		 ('test-src', 'symbol:QNAM', ?, 'holdings', 'testprov', 2, 'USD'),
		 ('test-src', 'symbol:QWD2', ?, 'holdings', 'testprov', 2, 'USD'),
		 ('test-src', 'symbol:QRPX', ?, 'holdings', 'swissfunddata', 1, 'USD')`,
		// A size fund whose file names its stocks alone, with their
		// sector and country; another fund names one under its ISIN.
		`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, name, asset_type,
		 weight, currency, country, sector) VALUES
		 ('test-src', 'symbol:QNAM', ?, 'holdings', 1, 'name:qorbit', NULL, 'QLAND ORBIT AG', 'Equity', 80, 'CHF', 'Switzerland', 'Industrials'),
		 ('test-src', 'symbol:QNAM', ?, 'holdings', 2, 'name:qperch', NULL, 'QLAND PERCH AG', 'Equity', 20, 'CHF', 'Switzerland', 'Industrials'),
		 ('test-src', 'symbol:QWD2', ?, 'holdings', 1, 'isin:CH0000000D01', 'CH0000000D01', 'QLAND PERCH AG', 'Equity', 50, 'CHF', 'Switzerland', 'Industrials'),
		 ('test-src', 'symbol:QWD2', ?, 'holdings', 2, 'isin:CH0000000D02', 'CH0000000D02', 'QLAND ORBIT AG', 'Equity', 50, 'CHF', 'Switzerland', 'Industrials'),
		 ('test-src', 'symbol:QRPX', ?, 'holdings', 1, 'name:qorbit', NULL, 'Qland Orbit AG', 'Common Stocks', 100, 'CHF', NULL, NULL)`,
		`INSERT INTO fund_lines (silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, asset_type,
		 weight, quantity, market_value, currency, country) VALUES
		 ('test-src', 'symbol:QALL', ?, 'holdings', 1, 'isin:US0000000B01', 'US0000000B01', 'Equity', 30, NULL, NULL, 'USD', 'United States'),
		 ('test-src', 'symbol:QALL', ?, 'holdings', 2, 'isin:US0000000B02', 'US0000000B02', 'Equity', 25, 10, 1000, 'USD', 'United States'),
		 ('test-src', 'symbol:QALL', ?, 'holdings', 3, 'isin:US0000000B03', 'US0000000B03', 'Equity', 10, NULL, NULL, 'USD', 'United States'),
		 ('test-src', 'symbol:QALL', ?, 'holdings', 4, 'isin:US0000000B04', 'US0000000B04', 'Equity', 10, 1, 10, 'USD', 'United States'),
		 ('test-src', 'symbol:QALL', ?, 'holdings', 5, 'isin:US0000000B05', 'US0000000B05', 'Equity', 5, NULL, NULL, 'USD', 'United States'),
		 ('test-src', 'symbol:QALL', ?, 'holdings', 6, 'isin:US0000000B05', 'US0000000B05', 'Equity', 20, NULL, NULL, 'USD', 'United States'),
		 ('test-src', 'symbol:QNEW', ? + 86400, 'holdings', 1, 'isin:US0000000A02', 'US0000000A02', 'Equity', 100, 2, 500, 'USD', 'United States'),
		 ('test-src', 'symbol:QEUR', ? + 2 * 86400, 'holdings', 1, 'isin:US0000000B02', 'US0000000B02', 'Equity', 100, 1, 1000000, 'USD', 'United States'),
		 ('test-src', 'symbol:QLM', ?, 'holdings', 1, 'isin:JP0000000C01', 'JP0000000C01', 'Equity', 55, NULL, NULL, 'JPY', 'Japan'),
		 ('test-src', 'symbol:QLM', ?, 'holdings', 2, 'isin:JP0000000C02', 'JP0000000C02', 'Equity', 30, NULL, NULL, 'JPY', 'Japan'),
		 ('test-src', 'symbol:QLM', ?, 'holdings', 3, 'isin:JP0000000C03', 'JP0000000C03', 'Equity', 10, NULL, NULL, 'JPY', 'Japan'),
		 ('test-src', 'symbol:QLM', ?, 'holdings', 4, 'isin:US0000000B03', 'US0000000B03', 'Equity', 5, NULL, NULL, 'USD', 'United States')`,
		`UPDATE fund_lines SET sector = 'Industrials', name = CASE line_no WHEN 1 THEN 'QLAND ALPHA' WHEN 2 THEN 'QLAND BETA'
		 ELSE 'QLAND GAMMA' END WHERE fund_ref = 'symbol:QLM'`,
	} {
		if _, err := db.ExecContext(ctx, q, slices.Repeat([]any{day}, strings.Count(q, "?"))...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := RebuildExposure(ctx, db); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		// 200m shares at 100: 20bn. 50m at 250, the price of a fund line
		// newer than the position's 50: 12.5bn. A receipt has no market
		// cap of its own: it takes its share's size.
		"isin:US0000000A01": "20000000000 large market_cap",
		"isin:US0000000A02": "12500000000 large market_cap",
		"isin:US0000000A03": "- large lei:isin:JP0000000C02",
		// A receipt a source describes finds its share by name, for the
		// share's size alone.
		"isin:US0000000A04": "- mid underlying:isin:JP0000000C03",
		// Its fund line's price, 100, times 1m shares: small, though it
		// ranks large. A line in a euro fund gives no US-dollar price.
		"isin:US0000000B02": "100000000 small market_cap",
		// Weights 30, 25, 25 (two lines of one stock), 10, 10: 0%, 55%,
		// 80% and 90% before the last three.
		"isin:US0000000B01": "- large size_fund:symbol:QALL",
		"isin:US0000000B05": "- large size_fund:symbol:QALL",
		"isin:US0000000B03": "- mid size_fund:symbol:QALL",
		// A filing's placeholder count of one share makes no market cap.
		"isin:US0000000B04": "- small size_fund:symbol:QALL",
		// 55% then 85% of a fund covering 85%: 47% and 72% of the market.
		"isin:JP0000000C02": "- large size_fund:symbol:QLM",
		// 80% of its country before it in a fund that names it alone.
		"isin:CH0000000D01": "- mid size_fund_name:symbol:QNAM",
		// A share with a market cap keeps it, whatever a size fund's line
		// of its name ranks; a report's line of that name takes it.
		"isin:CH0000000D02": "1000000000 small market_cap",
		"name:qorbit":       "- small underlying:isin:CH0000000D02",
		"isin:JP0000000C03": "- mid size_fund:symbol:QLM",
	} {
		var got string
		if err := db.QueryRowContext(ctx, `SELECT concat_ws(' ', COALESCE(CAST(round(market_cap) AS BIGINT)::VARCHAR, '-'),
		  COALESCE(size, '-'), COALESCE(size_origin, '-')) FROM securities WHERE security_key = ?`, key).Scan(&got); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	rows, err := ExposureSizes(ctx, db, day+86400, "USD")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.AssetClass+" "+r.Size+" "+*r.Value)
	}
	// Held: QBIG, QMID and QRCP 100 each (large), QRC2 and QPRC 100
	// each (mid), QALL 100 through its lines (55 large, 10 mid, 35
	// small), QRPX 100 through its one named line (small), QETF 100 (a
	// fund nothing looks through: no size, whatever its own cap) and a
	// bond 100 (no size applies). The rows sum to what is held.
	want := []string{"public_equity large 355", "public_equity mid 210", "public_equity small 135",
		"public_equity unknown 100", "fixed_income (none) 100"}
	if !slices.Equal(got, want) {
		t.Errorf("sizes = %v, want %v", got, want)
	}
	secs, err := ExposureSecurities(ctx, db, day+86400, "USD", "risk")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, r := range secs {
		switch r.Security {
		case "isin:US0000000A01":
			seen++
			if r.Size == nil || *r.Size != "large" || r.MarketCap == nil {
				t.Errorf("the securities view's QBIG = %+v, want its market cap and size", r)
			}
		case "symbol:QETF":
			seen++
			if r.Size != nil {
				t.Errorf("the securities view sizes a fund nothing looks through: %+v", r)
			}
		}
	}
	if seen != 2 {
		t.Errorf("the securities view lists %d of QBIG and QETF", seen)
	}
	var attrs string
	if err := db.QueryRowContext(ctx, `SELECT concat_ws(' ', sector, sector_origin, risk_country, risk_country_origin)
	  FROM securities WHERE security_key = 'isin:US0000000A04'`).Scan(&attrs); err != nil {
		t.Fatal(err)
	}
	if attrs != "materials fact:testfact JP fact:testfact" {
		t.Errorf("the described receipt = %s, want its own sector and country", attrs)
	}
	var flags string
	if err := db.QueryRowContext(ctx, `SELECT array_to_string(flags, ',') FROM exposure_leaves(?, 'USD')
	  WHERE top_key = 'isin:US0000000A04'`, day+86400).Scan(&flags); err != nil || flags != "" {
		t.Errorf("the described receipt's flags = %q (%v), want none", flags, err)
	}
	w, err := RefdataWanted(ctx, db, time.Unix(day+86400, 0))
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string][]string{}
	for _, f := range w.Funds {
		roles[f.Ref] = f.Roles
	}
	if !slices.Equal(roles["symbol:QLM"], []string{"reference"}) {
		t.Errorf("the size fund's roles = %v, want reference", roles["symbol:QLM"])
	}
}
