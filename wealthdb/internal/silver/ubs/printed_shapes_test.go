package ubs

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// The document rows ubs-web silver holds, in the shape a silver before
// ubs-web migration 0018 holds them and in the shape the parsers write
// now: the parser's internal-transfer verdict and an advice's source
// value on the payload, the precious-metals total as a synthetic
// position, and a maturity notice's principal negated. Every identifier,
// name and figure is invented.
const printedShapesCommon = `
INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir, window_since, window_until) VALUES
    (1000, 17, '/x/1', 0, 1000);
INSERT INTO historical_position_snapshots(as_of_date, portfolio_external_id, account_external_id,
    instrument_isin, currency_iso, units, market_value, market_value_currency, cost_price,
    description, source_doc_token, payload) VALUES
    (800, '0999000000000006', '', 'XX0000000011', 'USD', 100, 1250, 'USD', 10,
     'Reg.shs Placeholder Equity AG', 'doc-usd', '{"valor": "111", "isin": "XX0000000011"}');`

const printedShapesOld = printedShapesCommon + `
INSERT INTO schema_meta(silver_schema_version, applied_at) VALUES (17, 0);
DROP TABLE statement_overview_totals;
INSERT INTO historical_position_snapshots(as_of_date, portfolio_external_id, account_external_id,
    instrument_isin, currency_iso, units, market_value, market_value_currency,
    description, source_doc_token, payload) VALUES
    (800, '0999000000000001', '', 'PM-0999000000000001', 'USD', NULL, 987654, 'USD',
     'Precious metals & commodities', 'doc-usd',
     '{"kind": "overview_asset_class", "asset_class": "precious_metals", "portfolio_no": "01", "market_value": "987 654"}');
INSERT INTO historical_mortgages(as_of_date, account_external_id, currency_iso, outstanding_balance,
    product_name, rate_type, collateral_description, source_doc_token, payload) VALUES
    (800, '0999 12345678.MMM 0001', 'CHF', -1234567.89, 'UBS Fixed-Rate Mortgage', 'fixed',
     'EXAMPLE ROAD 1', 'doc-mort', '{"as_of": "2098-09-30", "product_name": "Fixed-Rate", "rate_type": "fixed"}');
INSERT INTO transactions(transaction_external_id, account_external_id, snapshot_at, booking_date,
    value_date, currency_iso, amount_debit, amount_credit, counterparty, description_kind, payload) VALUES
    ('stmt:0000000000000001', 'CH00WEBCASH', 1000, 700, 700, 'CHF', NULL, 1000, 'UEBERTRAG EXAMPLE MANDATE', 'CREDIT',
     '{"booking_type": "CREDIT", "internal_transfer": true, "counter_account": null, "continuation": ["UEBERTRAG EXAMPLE MANDATE"], "source": "account_statement_pdf"}'),
    ('stmt:0000000000000002', 'CH00WEBCASH', 1000, 710, 710, 'CHF', 200, NULL, 'EXAMPLE SHOP', 'E-BANKING PAYMENT ORDER',
     '{"booking_type": "E-BANKING PAYMENT ORDER", "internal_transfer": false, "counter_account": "CH0000000000000000009", "continuation": ["EXAMPLE SHOP", "CH00 0000 0000 0000 0000 9"], "source": "account_statement_pdf"}'),
    ('0000036ED0000123', 'CH00WEBMANDATE', 1000, 720, 720, 'CHF', NULL, 12345.67, 'Increase Example Mandate', NULL,
     '{"booking_type": null, "internal_transfer": true, "counter_account": null, "continuation": ["Increase Example Mandate"], "source": "account_statement_pdf", "document": "payment_advice_pdf"}');`

const printedShapesNew = printedShapesCommon + `
INSERT INTO schema_meta(silver_schema_version, applied_at) VALUES (18, 0);
INSERT INTO statement_overview_totals(source_doc_token, portfolio_external_id, asset_class, as_of_date,
    currency_iso, market_value, payload) VALUES
    ('doc-chf', '0999000000000001', 'precious_metals', 800, 'CHF', 888888,
     '{"kind": "overview_asset_class", "asset_class": "precious_metals", "portfolio_no": "01", "market_value": "888 888"}'),
    ('doc-usd', '0999000000000001', 'precious_metals', 800, 'USD', 987654,
     '{"kind": "overview_asset_class", "asset_class": "precious_metals", "portfolio_no": "01", "market_value": "987 654"}');
INSERT INTO historical_mortgages(as_of_date, account_external_id, currency_iso, outstanding_balance,
    product_name, rate_type, collateral_description, source_doc_token, payload) VALUES
    (800, '0999 12345678.MMM 0001', 'CHF', 1234567.89, 'UBS Fixed-Rate Mortgage', 'fixed',
     'EXAMPLE ROAD 1', 'doc-mort', '{"as_of": "2098-09-30", "product_name": "Fixed-Rate"}');
INSERT INTO transactions(transaction_external_id, account_external_id, snapshot_at, booking_date,
    value_date, currency_iso, amount_debit, amount_credit, counterparty, description_kind, payload) VALUES
    ('stmt:0000000000000001', 'CH00WEBCASH', 1000, 700, 700, 'CHF', NULL, 1000, 'UEBERTRAG EXAMPLE MANDATE', 'CREDIT',
     '{"booking_type": "CREDIT", "counter_account": null, "continuation": ["UEBERTRAG EXAMPLE MANDATE"], "source": "account_statement_pdf"}'),
    ('stmt:0000000000000002', 'CH00WEBCASH', 1000, 710, 710, 'CHF', 200, NULL, 'EXAMPLE SHOP', 'E-BANKING PAYMENT ORDER',
     '{"booking_type": "E-BANKING PAYMENT ORDER", "counter_account": "CH0000000000000000009", "continuation": ["EXAMPLE SHOP", "CH00 0000 0000 0000 0000 9"], "source": "account_statement_pdf"}'),
    ('0000036ED0000123', 'CH00WEBMANDATE', 1000, 720, 720, 'CHF', NULL, 12345.67, 'Increase Example Mandate', NULL,
     '{"booking_type": null, "counter_account": null, "continuation": ["Increase Example Mandate"], "source": "payment_advice_pdf", "document": "payment_advice_pdf"}');`

// printedShapesDump is the canonical dump of a web silver of the schema
// copy with rows.
func printedShapesDump(t *testing.T, rows string) []byte {
	t.Helper()
	path, db := silvertest.NewDB(t, webSilverSchemaSQL)
	silvertest.Exec(t, db, rows)
	out, err := silvertest.DumpCanonical(context.Background(), openSubsources(t, "", path), 0)
	if err != nil {
		t.Fatalf("DumpCanonical: %v", err)
	}
	return out
}

// withoutMovedKeys is a dump with what the parsers no longer write taken
// out of every payload: the internal-transfer verdict and the rate type,
// and an advice's source read as the advice's.
func withoutMovedKeys(t *testing.T, dump []byte) string {
	t.Helper()
	var walk func(v any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			delete(x, "internal_transfer")
			delete(x, "rate_type")
			if x["document"] == sourceAdvicePDF {
				x["source"] = sourceAdvicePDF
			}
			for k, e := range x {
				x[k] = walk(e)
			}
		case []any:
			for i, e := range x {
				x[i] = walk(e)
			}
		}
		return v
	}
	var out []string
	for _, line := range strings.Split(string(dump), "\n") {
		if !strings.HasPrefix(line, "{") {
			out = append(out, line)
			continue
		}
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(walk(v)); err != nil {
			t.Fatal(err)
		}
		out = append(out, strings.TrimSuffix(buf.String(), "\n"))
	}
	return strings.Join(out, "\n")
}

// TestOldAndNewDocumentRowsReadAlike holds both shapes to one canonical
// output: the same positions, values, kinds and amounts, and payloads that
// differ only by the keys the parsers no longer write.
func TestOldAndNewDocumentRowsReadAlike(t *testing.T) {
	old := printedShapesDump(t, printedShapesOld)
	now := printedShapesDump(t, printedShapesNew)
	if a, b := withoutMovedKeys(t, old), withoutMovedKeys(t, now); a != b {
		t.Errorf("old and new shapes read differently:\nold:\n%s\nnew:\n%s", a, b)
	}
	for _, want := range []string{`"InstrumentExternalID":"PM-0999000000000001"`,
		`"MarketValue":"-1234567.89"`} {
		if !strings.Contains(string(now), want) {
			t.Errorf("new shape: want %s in the dump", want)
		}
	}
	// The mandate credit and the advice are internal transfers, the
	// payment is not: the rule reads the continuation in either shape.
	if n := strings.Count(string(now), `"returns_flow":"internal"`); n != 2 {
		t.Errorf("new shape: %d rows read as internal transfers, want 2", n)
	}
}

// TestAMigratedSilverReadsAsAFreshOne: ubs-web migration 0018 moves the
// USD total of the old shape into statement_overview_totals, and a fresh
// load writes every copy's. The copies in other currencies change nothing.
func TestAMigratedSilverReadsAsAFreshOne(t *testing.T) {
	migrated := strings.Replace(printedShapesNew, `
    ('doc-chf', '0999000000000001', 'precious_metals', 800, 'CHF', 888888,
     '{"kind": "overview_asset_class", "asset_class": "precious_metals", "portfolio_no": "01", "market_value": "888 888"}'),`, "", 1)
	if migrated == printedShapesNew {
		t.Fatal("the fixture's CHF copy is not where the test expects it")
	}
	if a, b := printedShapesDump(t, migrated), printedShapesDump(t, printedShapesNew); !bytes.Equal(a, b) {
		t.Errorf("a migrated silver reads differently from a fresh one:\n%s\n---\n%s", a, b)
	}
}
