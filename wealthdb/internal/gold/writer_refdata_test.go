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
	exDay, recordDay := time.Date(1970, 1, 2, 0, 0, 0, 0, time.UTC), time.Date(1970, 1, 3, 0, 0, 0, 0, time.UTC)
	payDay, declaredDay := time.Date(1970, 1, 9, 0, 0, 0, 0, time.UTC), time.Date(1969, 12, 20, 0, 0, 0, 0, time.UTC)
	until := int64(259200)
	b := &canonical.SnapshotBatch{
		FundCompositions: []canonical.FundCompositionChange{{SilverSourceID: "test-src", FundRef: "isin:XX0000000001",
			AsOf: 86400, Basis: canonical.CompositionHoldings, Provider: "p1", SourceKind: "report", FundName: s("Qumbra Fund"),
			Currency: s("QLD"), NetAssets: d("1001"), LineCount: 2, WeightSum: d("99.5"), UnitsOutstanding: d("7"),
			FetchedAt: 172800, Payload: []byte(`{"c":1}`), Origin: canonical.CompositionParsed, ValidUntil: &until}},
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
			Payload: []byte(`{"l":1,"lei":"QQQQ00000000000000L1"}`), Region: s("europe")}},
		RateObservations: []canonical.RateObservationChange{{SilverSourceID: "test-src", SeriesID: "QLRATE10",
			AsOf: 86400, Value: *d("4.06"), Payload: []byte(`{"o":1}`)}},
		StatedPeriods: []canonical.StatedPeriodChange{{SilverSourceID: "test-src", AccountExternalID: "ACCT-Q",
			AsOf: 2592000, PeriodStart: 86400, Currency: "QLN", BeginningValue: d("100"), EndingValue: d("125.5"),
			Accrual: d("1"), NetAdditions: d("-10"), NetGain: d("35.5"), Income: d("2"), Fees: d("0.5"),
			SourceSHA256: "doc-q", Payload: []byte(`{"w":1}`)}},
		ExposureReports: []canonical.ExposureReportChange{{SilverSourceID: "test-src",
			InstrumentExternalID: "XX0000000011", AsOf: 2592000,
			Dimensions: map[string]map[string]float64{"region": {"europe": 60, "north_america": 40}},
			Source:     []byte(`{"lines":2}`), Note: "Example Feeder Fund II"}},
	}
	for _, r := range []struct {
		table string
		row   map[string]any
	}{
		{"fund_attributes", map[string]any{"fund_ref": "isin:XX0000000001", "as_of": int64(86400), "domicile": "QA",
			"legal_form": "SICAV", "ucits": true, "replication": "physical", "distribution": "dis", "ter": *d("0.12"),
			"hedged_currency": "QLH", "lending": false, "index_name": "Qland Index", "depositary": "Example Bank",
			"payload": []byte(`{"a":1}`)}},
		{"fund_counterparties", map[string]any{"fund_ref": "isin:XX0000000001", "as_of": int64(86400), "role": "swap",
			"name": "Example Bank", "lei": "QQQQ00000000000000QQ", "share": *d("4.5"), "payload": []byte(`{"p":1}`)}},
		{"security_identities", map[string]any{"security_key": "symbol:QRV", "as_of": int64(86400), "provider": "p2",
			"isin": "XX0000000003", "cusip": "Q0000AB02", "figi": "BBG00000Q002", "lei": "QQQQ00000000000000QR",
			"ticker": "QRV2", "exchange": "QX", "name": "Qorvant Holdings", "security_type": "Common Stock",
			"listing_currency": "QLI", "payload": []byte(`{"s":1}`)}},
		{"security_facts", map[string]any{"security_key": "symbol:QRV", "field": "sic", "as_of": int64(86400),
			"provider": "p3", "value": "2834", "scheme": "sic", "payload": []byte(`{"f":1}`)}},
		{"fund_risk", map[string]any{"fund_ref": "isin:XX0000000001", "as_of": int64(86400), "currency": "QLJ",
			"tenor": "10Y", "dv01": *d("125.5"), "dv100": *d("12400"), "credit_ig_dv01": *d("3.25"),
			"credit_hy_dv01": *d("1.5"), "payload": []byte(`{"r":1}`)}},
		{"fund_returns", map[string]any{"fund_ref": "isin:XX0000000001", "as_of": int64(86400), "month": "1970-01-31",
			"class_id": "C000000001", "total_return": *d("1.25"), "payload": []byte(`{"t":1}`)}},
		{"fund_characteristics", map[string]any{"fund_ref": "isin:XX0000000001", "as_of": int64(86400), "provider": "p4",
			"field": "credit_quality", "label": "AAA", "value": *d("42.5"), "payload": []byte(`{"q":1}`)}},
		{"security_period_facts", map[string]any{"security_key": "symbol:QRV", "field": "eps_diluted",
			"as_of": int64(2592000), "period_start": periodStart, "provider": "p5", "value": *d("1.23"),
			"scheme": "us-gaap:EarningsPerShareDiluted", "unit": "QLK/shares", "fiscal_year": int64(1970),
			"fiscal_period": "Q1", "form": "10-Q", "filed": filed, "payload": []byte(`{"e":1}`)}},
		{"dividend_calendar", map[string]any{"ref": "isin:XX0000000001", "provider": "p6", "event_id": "ev1",
			"as_of": int64(345600), "kind": "capital_gain", "ex_day": exDay, "record_day": recordDay,
			"pay_day": payDay, "declared_day": declaredDay, "amount": *d("0.1234"), "currency": "QLM",
			"status": "final"}},
	} {
		b.AppendRef(canonical.RefTableOf(r.table), refRow(t, r.table, r.row))
	}
	b.SetSource("test-src")
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
		    net_assets, line_count, weight_sum, units_outstanding, fetched_at, payload, origin, valid_until`,
			"test-src|isin:XX0000000001|86400|holdings|p1|report|Qumbra Fund|QLD|1001.0000|2|99.50000000|7.00000000|172800|{\"c\":1}|" +
				"parsed|259200"},
		{"fund_lines", `silver_source_id, fund_ref, as_of, basis, line_no, security_key, isin, cusip, figi, ticker,
		    exchange, name, asset_type, weight, quantity, market_value, currency, country, risk_country, sector, industry,
		    maturity, coupon, long_currency, long_amount, short_currency, short_amount, coupon_kind, is_default,
		    in_arrears, paid_in_kind, exchange_rate, duration, modified_duration, yield_to_maturity, yield_to_worst, yield_to_call,
		    real_duration, real_yield, accrual_date, lei, payload, region`,
			"test-src|isin:XX0000000001|86400|holdings|3|isin:XX0000000002|XX0000000002|Q0000AB01|BBG00000Q001|QRV|" +
				"Qland Exchange|Qorvant Corp|Equity|12.50000000|40.00000000|125.2500|QLE|Qland|Rland|Mining|Metals|" +
				"2041-03-15|3.33000000|QLF|11.0000|QLG|12.0000|Fixed|false|true|false|1.085500000000|6.25000000|6.10000000|" +
				"4.35000000|4.20000000|4.10000000|5.50000000|1.85000000|2040-09-15|QQQQ00000000000000L1|" +
				"{\"l\":1,\"lei\":\"QQQQ00000000000000L1\"}|europe"},
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
		{"fund_returns", `silver_source_id, fund_ref, as_of, month, class_id, total_return, payload`,
			"test-src|isin:XX0000000001|86400|1970-01-31|C000000001|1.25000000|{\"t\":1}"},
		{"fund_characteristics", `silver_source_id, fund_ref, as_of, provider, field, label, value, payload`,
			"test-src|isin:XX0000000001|86400|p4|credit_quality|AAA|42.50000000|{\"q\":1}"},
		{"security_period_facts", `silver_source_id, security_key, field, as_of, period_start, provider, value,
		    scheme, unit, fiscal_year, fiscal_period, form, filed, payload`,
			"test-src|symbol:QRV|eps_diluted|2592000|1970-01-01|p5|1.23000000|us-gaap:EarningsPerShareDiluted|" +
				"QLK/shares|1970|Q1|10-Q|1970-02-10|{\"e\":1}"},
		{"rate_observations", `silver_source_id, series_id, as_of, value, payload`,
			"test-src|QLRATE10|86400|4.06000000|{\"o\":1}"},
		{"dividend_calendar", `silver_source_id, ref, provider, event_id, as_of, kind, ex_day, record_day, pay_day,
		    declared_day, amount, currency, status`,
			"test-src|isin:XX0000000001|p6|ev1|345600|capital_gain|1970-01-02|1970-01-03|1970-01-09|1969-12-20|" +
				"0.12340000|QLM|final"},
		{"stated_periods", `silver_source_id, account_external_id, as_of, period_start, currency, beginning_value,
		    ending_value, accrual, net_additions, net_gain, income, fees, source_sha256, payload`,
			"test-src|ACCT-Q|2592000|86400|QLN|100.0000|125.5000|1.0000|-10.0000|35.5000|2.0000|0.5000|doc-q|{\"w\":1}"},
		{"exposure_reports", `silver_source_id, instrument_external_id, as_of, dimensions, source, note`,
			"test-src|XX0000000011|2592000|{\"region\":{\"europe\":60,\"north_america\":40}}|{\"lines\":2}|" +
				"Example Feeder Fund II"},
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

	// A later statement of the distribution replaces its row, though the
	// day it was stated on lies past the window that cleared the first.
	calendar := canonical.RefTableOf("dividend_calendar")
	restated := slices.Clone(b.RefRowsOf(calendar)[0])
	restated[calendar.Col("as_of")], restated[calendar.Col("amount")] = int64(777600), *d("0.2")
	inTx(t, db, ctx, func(w *Writer) error { return w.InsertReference(ctx, batch(restated)) })
	var restatedRow string
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) || ' ' || MAX(as_of) || ' ' || MAX(amount) FROM dividend_calendar`).
		Scan(&restatedRow); err != nil || restatedRow != "1 777600 0.20000000" {
		t.Errorf("restated distribution = %q (%v), want its one row as of the later day", restatedRow, err)
	}

	// A writer's first batch of a ref and provider clears that pair's
	// rows, so one the batch does not state leaves; a second batch of the
	// pair adds to the first, and another provider's rows of the ref stay.
	event := func(provider, id string) []any {
		return refRow(t, "dividend_calendar", map[string]any{"ref": "isin:XX0000000001", "provider": provider,
			"event_id": id, "as_of": int64(864000), "kind": "dividend"})
	}
	inTx(t, db, ctx, func(w *Writer) error { return w.InsertReference(ctx, batch(event("p7", "evA"))) })
	inTx(t, db, ctx, func(w *Writer) error {
		if err := w.InsertReference(ctx, batch(event("p6", "ev2"))); err != nil {
			return err
		}
		return w.InsertReference(ctx, batch(event("p6", "ev3")))
	})
	var events string
	if err := db.QueryRowContext(ctx, `SELECT string_agg(provider || ':' || event_id, ' ' ORDER BY provider, event_id)
	    FROM dividend_calendar`).Scan(&events); err != nil || events != "p6:ev2 p6:ev3 p7:evA" {
		t.Errorf("distributions after a replacement = %q (%v), want p6's two new ones and p7's", events, err)
	}

	// A kind or a status outside the vocabulary is refused.
	for _, e := range [][]any{
		refRow(t, "dividend_calendar", map[string]any{"ref": "isin:XX0000000004", "provider": "p6", "event_id": "x",
			"kind": "interest"}),
		refRow(t, "dividend_calendar", map[string]any{"ref": "isin:XX0000000004", "provider": "p6", "event_id": "y",
			"kind": "dividend", "status": "paid"}),
	} {
		tx, err := db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		err = NewWriter(tx).InsertReference(ctx, batch(e))
		if err == nil || !strings.Contains(err.Error(), "invalid") || !strings.Contains(err.Error(), "isin:XX0000000004") {
			t.Errorf("%v: %v", e, err)
		}
		_ = tx.Rollback()
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

	// A parsed split's dimension or value outside the vocabulary is
	// refused; unknown and the rest of the world are values.
	for _, c := range []struct {
		dims map[string]map[string]float64
		want string
	}{
		{map[string]map[string]float64{"region": {"N. America": 100}}, `invalid region "N. America"`},
		{map[string]map[string]float64{"country": {"usa": 100}}, `invalid country "usa"`},
		{map[string]map[string]float64{"currency": {"usd": 100}}, `invalid currency "usd"`},
		{map[string]map[string]float64{"sector": {"Software": 100}}, `invalid sector "Software"`},
		{map[string]map[string]float64{"asset_class": {"stocks": 100}}, `invalid asset_class "stocks"`},
		{map[string]map[string]float64{"style": {"value": 100}}, `invalid dimension "style"`},
		{map[string]map[string]float64{"region": {"other": 50, "unknown": 50}, "country": {"US": 100},
			"currency": {"USD": 100}}, ""},
	} {
		tx, err := db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		err = NewWriter(tx).InsertReference(ctx, &canonical.SnapshotBatch{ExposureReports: []canonical.ExposureReportChange{{
			SilverSourceID: "test-src", InstrumentExternalID: "XX0000000012", AsOf: 86400, Dimensions: c.dims}}})
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%v: %v, want %q", c.dims, err, c.want)
		}
		_ = tx.Rollback()
	}

	// A composition's origin and a line's region outside their
	// vocabularies are refused; a line states no unknown region.
	region := func(r string) *string { return &r }
	for _, b := range []*canonical.SnapshotBatch{
		{FundCompositions: []canonical.FundCompositionChange{{SilverSourceID: "test-src", FundRef: "isin:XX0000000013",
			AsOf: 86400, Basis: canonical.CompositionHoldings, Provider: "p1", Origin: "guessed"}}},
		{FundLines: []canonical.FundLineChange{{SilverSourceID: "test-src", FundRef: "isin:XX0000000013", AsOf: 86400,
			Basis: canonical.CompositionHoldings, LineNo: 1, Region: region("N. America")}}},
		{FundLines: []canonical.FundLineChange{{SilverSourceID: "test-src", FundRef: "isin:XX0000000013", AsOf: 86400,
			Basis: canonical.CompositionHoldings, LineNo: 1, Region: region(canonical.Unknown)}}},
	} {
		tx, err := db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := NewWriter(tx).InsertReference(ctx, b); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Errorf("%+v: %v, want it refused", b, err)
		}
		_ = tx.Rollback()
	}
}

// refRow is a row of the reference table gold (canonical.RefTables): the
// value values names for each column, nil for one it leaves out.
func refRow(t *testing.T, gold string, values map[string]any) []any {
	t.Helper()
	tbl := canonical.RefTableOf(gold)
	row := make([]any, len(tbl.Cols))
	for name, v := range values {
		i := tbl.Col(name)
		if i < 0 {
			t.Fatalf("%s has no column %s", gold, name)
		}
		row[i] = v
	}
	return row
}

// batch is a batch of distributions of the source test-src.
func batch(events ...[]any) *canonical.SnapshotBatch {
	b := &canonical.SnapshotBatch{}
	for _, e := range events {
		b.AppendRef(canonical.RefTableOf("dividend_calendar"), e)
	}
	b.SetSource("test-src")
	return b
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

// A line's LEI is the text its payload states under `lei` exactly; an
// empty one, a number, an object or another key is none.
func TestPayloadLEI(t *testing.T) {
	t.Parallel()
	for payload, want := range map[string]string{
		`{"lei":"QQQQ00000000000000L1"}`: "QQQQ00000000000000L1", `{"lei":""}`: "", `{"lei":12}`: "",
		`{"lei":{"a":1}}`: "", `{"LEI":"QQQQ00000000000000L2"}`: "", `{"l":1}`: "", `not json`: "", ``: "",
	} {
		got := ""
		if p := payloadLEI([]byte(payload)); p != nil {
			got = *p
		}
		if got != want {
			t.Errorf("%q: %q, want %q", payload, got, want)
		}
	}
}

// TestEveryReferenceTableHasAKind pins that a load's windowed delete clears
// each reference table over the span the stream re-emits it over: every
// table names its kind of record but rate_observations, stated_periods
// and exposure_reports, which their streams write over the window whole,
// and a table canonical.RefTables declares names the registry's.
func TestEveryReferenceTableHasAKind(t *testing.T) {
	t.Parallel()
	kinds := map[string]bool{}
	for _, k := range canonical.RefKinds {
		kinds[k] = true
	}
	whole := map[string]bool{"rate_observations": true, "stated_periods": true, "exposure_reports": true}
	for _, st := range SourceTables {
		if st.Class != ClassReference {
			continue
		}
		switch {
		case whole[st.Name] && st.RefKind != "":
			t.Errorf("%s names kind %q, want none", st.Name, st.RefKind)
		case !whole[st.Name] && !kinds[st.RefKind]:
			t.Errorf("%s names kind %q, want one of the reference kinds", st.Name, st.RefKind)
		}
	}
	for table := range whole {
		if !slices.Contains(ReferenceTables, table) {
			t.Errorf("%s is not a reference table", table)
		}
	}
	// A table the registry declares is a reference table whose kind is
	// the registry's.
	for _, rt := range canonical.RefTables {
		i := slices.IndexFunc(SourceTables, func(st SourceTable) bool { return st.Name == rt.Gold })
		if i < 0 || SourceTables[i].Class != ClassReference || SourceTables[i].RefKind != rt.Kind {
			t.Errorf("registry table %s (kind %s) is no reference table of that kind", rt.Gold, rt.Kind)
		}
	}
}
