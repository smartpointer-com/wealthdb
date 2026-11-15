package ubs

import (
	"context"
	_ "embed"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

//go:embed testdata/web_silver_schema.sql
var webSilverSchemaSQL string

// webFixture is a web silver at the newest schema, of two dumps: one relationship, one
// portfolio, a cash account and a custody holding, a statement's
// historical holding and cash period, and a few ledger rows, one valued
// after its dump. Every identifier, name and number is invented.
const webFixture = `
INSERT INTO schema_meta(silver_schema_version, applied_at) VALUES (17, 0);
INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir, window_since, window_until) VALUES
    (1000, 17, '/x/1', 0, 1000), (2000, 17, '/x/2', 1000, 2000);
INSERT INTO banking_relationships(snapshot_at, banking_relationship_id, account_number_prefix, description, payload) VALUES
    (1000, 'REL1', 'BBBB AAAAAAAA', 'Main', '{}'), (2000, 'REL1', 'BBBB AAAAAAAA', 'Main', '{}');
INSERT INTO portfolios(snapshot_at, portfolio_external_id, banking_relationship_id, portfolio_full_id,
                       portfolio_uid, base_currency, description, payload) VALUES
    (1000, 'P1', 'REL1', 'BBBB AAAAAAAA P1', 'uid-p1', 'CHF', 'Example portfolio', '{}'),
    (2000, 'P1', 'REL1', 'BBBB AAAAAAAA P1', 'uid-p1', 'CHF', 'Example portfolio', '{}');
INSERT INTO accounts(snapshot_at, account_external_id, kind, account_number_raw, account_opaque_id,
                     banking_relationship_id, portfolio_external_id, currency_iso, description, payload) VALUES
    (1000, 'CH00WEBCASH', 'cash', 'BBBB AAAAAAAA.40', 'tok-cash', 'REL1', 'P1', 'CHF', 'CHF Checking, Example Account', '{}'),
    (2000, 'CH00WEBCASH', 'cash', 'BBBB AAAAAAAA.40', 'tok-cash', 'REL1', 'P1', 'CHF', 'CHF Checking, Example Account', '{}'),
    (2000, 'CH00WEBSAFE', 'safekeeping', 'BBBB AAAAAAAA.S1', 'tok-safe', 'REL1', 'P1', 'CHF', 'Custody account', '{}');
INSERT INTO positions(snapshot_at, portfolio_external_id, account_external_id, instrument_isin, valor,
                      currency_iso, units, market_value, market_value_currency, cost_price, description, payload) VALUES
    (2000, 'P1', '', 'CH0000000001', '1000001', 'CHF', 100, 4500, 'CHF', 40,
     'Shs Example Materials AG', '{"Rating":"A"}'),
    (2000, 'P1', 'CH00WEBCASH', NULL, NULL, 'CHF', NULL, 1234.5, 'CHF', NULL,
     'CHF Checking, Example Account', '{}');
INSERT INTO historical_position_snapshots(as_of_date, portfolio_external_id, account_external_id,
    instrument_isin, currency_iso, units, market_value, market_value_currency, cost_price,
    description, source_doc_token, payload) VALUES
    (800, 'P1', 'CH00WEBSAFE', 'CH0000000001', 'CHF', 80, 3200, 'CHF', 38,
     'Shs Example Materials AG', 'doc-1', '{}');
INSERT INTO historical_cash_balances(period_end, account_external_id, currency_iso, period_start,
    opening_balance, closing_balance, total_debits, total_credits, source_doc_token, payload) VALUES
    (800, 'CH00WEBCASH', 'CHF', 500, 100, 200, 0, 100, 'doc-1', '{}');
INSERT INTO transactions(transaction_external_id, account_external_id, snapshot_at, booking_date,
    value_date, currency_iso, amount_debit, amount_credit, counterparty, description_kind, payload) VALUES
    ('WT1', 'CH00WEBCASH', 1000, 900, 900, 'CHF', NULL, 250, 'Example Payer', 'Credit',
     '{"Description1":"Example Payer"}'),
    ('WT2', 'CH00WEBCASH', 2000, 1500, 1500, 'CHF', 40, NULL, 'Example Shop', 'e-banking payment order',
     '{"Description1":"Example Shop"}'),
    ('WT3', 'CH00WEBCASH', 2000, 2000, 2600, 'CHF', 15, NULL, 'Example Bank', 'Account fee',
     '{"Description1":"Account fee"}');`

// newWebFixtureSilver is a web silver with the full schema and
// webFixture's rows.
func newWebFixtureSilver(t *testing.T) string {
	t.Helper()
	path, db := silvertest.NewDB(t, webSilverSchemaSQL)
	silvertest.Exec(t, db, webFixture)
	return path
}

// openSubsources opens the adapter over the given feeds, PSN first.
func openSubsources(t *testing.T, psnPath, webPath string) silver.Connection {
	t.Helper()
	var subs []silver.Subsource
	if psnPath != "" {
		subs = append(subs, silver.Subsource{Kind: "ubs-psn", Path: psnPath})
	}
	if webPath != "" {
		subs = append(subs, silver.Subsource{Kind: "ubs-web", Path: webPath})
	}
	conn, err := (&Adapter{}).Open(context.Background(), silver.OpenSpec{ID: "ubs", Subsources: subs})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// A web silver at the newest schema lacks nothing: the load advises
// nothing.
func TestACurrentWebSilverAdvisesNothing(t *testing.T) {
	conn := openSubsources(t, "", newWebFixtureSilver(t))
	if n := conn.(silver.SchemaAdviser).SchemaAdvice(); len(n) != 0 {
		t.Errorf("advice = %v, want none", n)
	}
}
