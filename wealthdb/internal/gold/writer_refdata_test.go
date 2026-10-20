package gold

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// Every reference record reads back with each of its fields in its own
// column: the values all differ, so two swapped placeholders would show.
// Every identifier is invented.
func TestInsertReferenceRoundTrips(t *testing.T) {
	t.Parallel()
	db, ctx := openMigrated(t)
	s := func(v string) *string { return &v }
	d := func(v string) *canonical.Decimal {
		x, err := canonical.NewDecimalFromString(v)
		if err != nil {
			t.Fatal(err)
		}
		return &x
	}
	yes, no := true, false
	maturity := time.Date(2041, 3, 15, 0, 0, 0, 0, time.UTC)
	accrual := time.Date(2040, 9, 15, 0, 0, 0, 0, time.UTC)
	periodStart := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	filed := time.Date(1970, 2, 10, 0, 0, 0, 0, time.UTC)
	year := int64(1970)
	b := &canonical.SnapshotBatch{
		FundCompositions: []canonical.FundCompositionChange{{SilverSourceID: "test-src", FundRef: "isin:XX0000000001",
			AsOf: 86400, Basis: canonical.CompositionHoldings, Provider: "p1", SourceKind: "report", FundName: s("Qumbra Fund"),
			Currency: s("QLD"), NetAssets: d("1001"), LineCount: 2, WeightSum: d("99.5"), UnitsOutstanding: d("7"),
			FetchedAt: 172800, Payload: []byte(`{"c":1}`)}},
		FundLines: []canonical.FundLineChange{{SilverSourceID: "test-src", FundRef: "isin:XX0000000001", AsOf: 86400,
			Basis: canonical.CompositionHoldings, LineNo: 3, SecurityKey: "isin:XX0000000002", ISIN: s("XX0000000002"),
			CUSIP: s("Q0000AB01"), FIGI: s("BBG00000Q001"), Ticker: s("QRV"), Exchange: s("Qland Exchange"),
			Name: s("Qorvant Corp"), AssetType: s("Equity"), Weight: d("12.5"), Quantity: d("40"), MarketValue: d("125.25"),
			Currency: s("QLE"), Country: s("Qland"), RiskCountry: s("Rland"), Sector: s("Mining"), Industry: s("Metals"),
			Maturity: &maturity, Coupon: d("3.33"), LongCurrency: s("QLF"), LongAmount: d("11"), ShortCurrency: s("QLG"),
			ShortAmount: d("12"), CouponKind: s("Fixed"), IsDefault: &no, InArrears: &yes, PaidInKind: &no,
			ExchangeRate: d("1.0855"),
			Duration:     d("6.25"), ModifiedDuration: d("6.1"), YieldToMaturity: d("4.35"), YieldToWorst: d("4.2"),
			YieldToCall: d("4.1"), RealDuration: d("5.5"), RealYield: d("1.85"), AccrualDate: &accrual,
			Payload: []byte(`{"l":1}`)}},
		FundAttributes: []canonical.FundAttributesChange{{SilverSourceID: "test-src", FundRef: "isin:XX0000000001",
			AsOf: 86400, Domicile: s("QA"), LegalForm: s("SICAV"), UCITS: &yes, Replication: s("physical"),
			Distribution: s("dis"), TER: d("0.12"), HedgedCurrency: s("QLH"), Lending: &no, IndexName: s("Qland Index"),
			Depositary: s("Example Bank"), Payload: []byte(`{"a":1}`)}},
		FundCounterparties: []canonical.FundCounterpartyChange{{SilverSourceID: "test-src", FundRef: "isin:XX0000000001",
			AsOf: 86400, Role: "swap", Name: "Example Bank", LEI: s("QQQQ00000000000000QQ"), Share: d("4.5"),
			Payload: []byte(`{"p":1}`)}},
		Securities: []canonical.SecurityChange{{SilverSourceID: "test-src", SecurityKey: "symbol:QRV", AsOf: 86400,
			Provider: "p2", ISIN: s("XX0000000003"), CUSIP: s("Q0000AB02"), FIGI: s("BBG00000Q002"),
			LEI: s("QQQQ00000000000000QR"), Ticker: s("QRV2"), Exchange: s("QX"), Name: s("Qorvant Holdings"),
			SecurityType: s("Common Stock"), ListingCurrency: s("QLI"), Payload: []byte(`{"s":1}`)}},
		SecurityFacts: []canonical.SecurityFactChange{{SilverSourceID: "test-src", SecurityKey: "symbol:QRV",
			Field: "sic", AsOf: 86400, Provider: "p3", Value: "2834", Scheme: s("sic"), Payload: []byte(`{"f":1}`)}},
		FundRisk: []canonical.FundRiskChange{{SilverSourceID: "test-src", FundRef: "isin:XX0000000001", AsOf: 86400,
			Currency: "QLJ", Tenor: "10Y", DV01: d("125.5"), DV100: d("12400"), CreditIGDV01: d("3.25"),
			CreditHYDV01: d("1.5"), Payload: []byte(`{"r":1}`)}},
		FundCharacteristics: []canonical.FundCharacteristicChange{{SilverSourceID: "test-src",
			FundRef: "isin:XX0000000001", AsOf: 86400, Provider: "p4", Field: "credit_quality", Label: "AAA",
			Value: *d("42.5"), Payload: []byte(`{"q":1}`)}},
		SecurityPeriodFacts: []canonical.SecurityPeriodFactChange{{SilverSourceID: "test-src", SecurityKey: "symbol:QRV",
			Field: "eps_diluted", AsOf: 2592000, PeriodStart: periodStart, Provider: "p5", Value: *d("1.23"),
			Scheme: s("us-gaap:EarningsPerShareDiluted"), Unit: s("QLK/shares"), FiscalYear: &year,
			FiscalPeriod: s("Q1"), Form: s("10-Q"), Filed: &filed, Payload: []byte(`{"e":1}`)}},
		RateObservations: []canonical.RateObservationChange{{SilverSourceID: "test-src", SeriesID: "QLRATE10",
			AsOf: 86400, Value: *d("4.06"), Payload: []byte(`{"o":1}`)}},
	}
	before := filledTables(t, db, ctx)
	inTx(t, db, ctx, func(w *Writer) error { return w.InsertReference(ctx, b) })

	// InsertReference writes ReferenceTables and no other table: one it
	// wrote and ReferenceTables left out would escape the load's windowed
	// delete and a reset. Each of them has its round trip below.
	want := slices.Sorted(slices.Values(ReferenceTables))
	var written []string
	for _, tbl := range filledTables(t, db, ctx) {
		if !slices.Contains(before, tbl) {
			written = append(written, tbl)
		}
	}
	if !slices.Equal(written, want) {
		t.Errorf("InsertReference wrote %v, want ReferenceTables %v", written, want)
	}
	var tables []string
	for _, c := range []struct{ table, cols, want string }{
		{"fund_compositions", `silver_source_id, fund_ref, as_of, basis, provider, source_kind, fund_name, currency,
		    net_assets, line_count, weight_sum, units_outstanding, fetched_at, payload`,
			"test-src|isin:XX0000000001|86400|holdings|p1|report|Qumbra Fund|QLD|1001.0000|2|99.50000000|7.00000000|172800|{\"c\":1}"},
		{"fund_lines", `silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, cusip, figi, ticker,
		    exchange, name, asset_type, weight, quantity, market_value, currency, country, risk_country, sector, industry,
		    maturity, coupon, long_currency, long_amount, short_currency, short_amount, coupon_kind, is_default,
		    in_arrears, paid_in_kind, exchange_rate, duration, modified_duration, yield_to_maturity, yield_to_worst, yield_to_call,
		    real_duration, real_yield, accrual_date, payload`,
			"test-src|isin:XX0000000001|86400|holdings|3|isin:XX0000000002|XX0000000002|Q0000AB01|BBG00000Q001|QRV|" +
				"Qland Exchange|Qorvant Corp|Equity|12.50000000|40.00000000|125.2500|QLE|Qland|Rland|Mining|Metals|" +
				"2041-03-15|3.33000000|QLF|11.0000|QLG|12.0000|Fixed|false|true|false|1.085500000000|6.25000000|6.10000000|" +
				"4.35000000|4.20000000|4.10000000|5.50000000|1.85000000|2040-09-15|{\"l\":1}"},
		{"fund_attributes", `silver_source_id, fund_ref, as_of, domicile, legal_form, ucits, replication, distribution,
		    ter, hedged_currency, lending, index_name, depositary, payload`,
			"test-src|isin:XX0000000001|86400|QA|SICAV|true|physical|dis|0.12000000|QLH|false|Qland Index|Example Bank|{\"a\":1}"},
		{"fund_counterparties", `silver_source_id, fund_ref, as_of, role, name, lei, share, payload`,
			"test-src|isin:XX0000000001|86400|swap|Example Bank|QQQQ00000000000000QQ|4.50000000|{\"p\":1}"},
		{"security_identities", `silver_source_id, security_key, as_of, provider, isin, cusip, figi, lei, ticker,
		    exchange, name, security_type, listing_currency, payload`,
			"test-src|symbol:QRV|86400|p2|XX0000000003|Q0000AB02|BBG00000Q002|QQQQ00000000000000QR|QRV2|QX|" +
				"Qorvant Holdings|Common Stock|QLI|{\"s\":1}"},
		{"security_facts", `silver_source_id, security_key, field, as_of, provider, value, scheme, payload`,
			"test-src|symbol:QRV|sic|86400|p3|2834|sic|{\"f\":1}"},
		{"fund_risk", `silver_source_id, fund_ref, as_of, currency, tenor, dv01, dv100, credit_ig_dv01,
		    credit_hy_dv01, payload`,
			"test-src|isin:XX0000000001|86400|QLJ|10Y|125.5000|12400.0000|3.2500|1.5000|{\"r\":1}"},
		{"fund_characteristics", `silver_source_id, fund_ref, as_of, provider, field, label, value, payload`,
			"test-src|isin:XX0000000001|86400|p4|credit_quality|AAA|42.50000000|{\"q\":1}"},
		{"security_period_facts", `silver_source_id, security_key, field, as_of, period_start, provider, value,
		    scheme, unit, fiscal_year, fiscal_period, form, filed, payload`,
			"test-src|symbol:QRV|eps_diluted|2592000|1970-01-01|p5|1.23000000|us-gaap:EarningsPerShareDiluted|" +
				"QLK/shares|1970|Q1|10-Q|1970-02-10|{\"e\":1}"},
		{"rate_observations", `silver_source_id, series_id, as_of, value, payload`,
			"test-src|QLRATE10|86400|4.06000000|{\"o\":1}"},
	} {
		tables = append(tables, c.table)
		var got string
		cols := strings.Join(strings.Fields(strings.ReplaceAll(c.cols, ",", " ")), " AS VARCHAR), CAST(")
		if err := db.QueryRowContext(ctx, `SELECT concat_ws('|', CAST(`+cols+` AS VARCHAR)) FROM `+c.table).
			Scan(&got); err != nil {
			t.Fatalf("%s: %v", c.table, err)
		}
		if got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.table, got, c.want)
		}
	}
	slices.Sort(tables)
	if !slices.Equal(tables, want) {
		t.Errorf("round trips of %v, want ReferenceTables %v", tables, want)
	}

	// A basis outside the vocabulary is refused, on a composition and on
	// a line alike.
	bad := canonical.CompositionBasis("swap")
	for _, batch := range []*canonical.SnapshotBatch{
		{FundCompositions: []canonical.FundCompositionChange{{SilverSourceID: "test-src", FundRef: "isin:XX0000000004",
			AsOf: 86400, Basis: bad, Provider: "p1"}}},
		{FundLines: []canonical.FundLineChange{{SilverSourceID: "test-src", FundRef: "isin:XX0000000004", AsOf: 86400,
			Basis: bad, LineNo: 1}}},
	} {
		tx, err := db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := NewWriter(tx).InsertReference(ctx, batch); err == nil || !strings.Contains(err.Error(), "invalid basis") {
			t.Errorf("a basis of %q: %v", bad, err)
		}
		_ = tx.Rollback()
	}
}

// filledTables are gold's tables that hold a row, by name.
func filledTables(t *testing.T, db *sql.DB, ctx context.Context) []string {
	t.Helper()
	var all, out []string
	if err := scanAll(ctx, db, `SELECT table_name FROM information_schema.tables WHERE table_type = 'BASE TABLE'
	    ORDER BY 1`, func(rows *sql.Rows) error {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		all = append(all, name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range all {
		if countRows(t, db, ctx, tbl) > 0 {
			out = append(out, tbl)
		}
	}
	return out
}
