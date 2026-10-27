package schwab

import (
	"context"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// openWebAlone opens a seeded web silver as the source's only subsource.
func openWebAlone(t *testing.T, seed string, args ...any) *Connection {
	t.Helper()
	path, web := silvertest.NewDB(t, webSilverSchemaSQL)
	if _, err := web.Exec(seed, args...); err != nil {
		t.Fatalf("seed web: %v", err)
	}
	conn, err := (&Adapter{}).Open(context.Background(), silver.OpenSpec{Subsources: []silver.Subsource{
		{Kind: "schwab-web", Path: path},
	}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn.(*Connection)
}

// With the web silver alone, an account is keyed by its full account
// number (the newest one stated), carries the display name and currency
// the api's row would, and every web row reaches gold under it: the
// statement holdings, cash balances and open lots, and every
// transaction, with no api cut. The change window is the web feed's
// alone and spans every seeded date. Every number in the fixture is
// invented.
func TestWebAloneKeysAccountsByTheirFullNumber(t *testing.T) {
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
            ('T2', 2000, '5678', 'Buy',     'VTI', 'statement_pdf', 'sha-stmt', '{"amount":-100,"quantity":1,"description":"VTI"}');
    `, asOf, periodStart)
	ctx := context.Background()
	cw, err := c.ChangeWindow(ctx, -1)
	if err != nil {
		t.Fatal(err)
	}
	if !cw.HasChanges || cw.Start > 500 || cw.Start > periodStart || cw.End < asOf {
		t.Errorf("ChangeWindow = %+v, want it to span 500 through %d", cw, asOf)
	}
	stream, err := c.Snapshots(ctx, everything)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var accts []canonical.AccountChange
	var pos []canonical.PositionChange
	var instrs []canonical.InstrumentChange
	var cash []canonical.CashBalanceChange
	var lots []canonical.PositionLotChange
	for {
		b, more, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		accts, pos = append(accts, b.Accounts...), append(pos, b.Positions...)
		instrs = append(instrs, b.Instruments...)
		cash, lots = append(cash, b.CashBalances...), append(lots, b.PositionLots...)
		if !more {
			break
		}
	}
	if len(pos) != 1 || pos[0].AccountExternalID != "11115678" || pos[0].PositionKey != "VTI" {
		t.Fatalf("positions = %+v, want VTI under 11115678", pos)
	}
	if len(cash) != 2 {
		t.Errorf("cash balances = %+v, want the opening and the closing", cash)
	}
	for _, cb := range cash {
		if cb.AccountExternalID != "11115678" {
			t.Errorf("cash balance %s on %q, want 11115678", cb.BalanceKind, cb.AccountExternalID)
		}
	}
	if len(lots) != 1 || lots[0].AccountExternalID != "11115678" || lots[0].PositionKey != "VTI" {
		t.Errorf("open lots = %+v, want VTI's lot under 11115678", lots)
	}
	if len(instrs) != 1 || instrs[0].Name == nil || *instrs[0].Name != "EXAMPLE TOTAL MARKET ETF" {
		t.Errorf("instruments = %+v, want VTI named by its statement line", instrs)
	}
	if len(accts) == 0 {
		t.Fatal("no account rows")
	}
	for _, a := range accts {
		if a.AccountExternalID != "11115678" || a.DisplayName == nil || *a.DisplayName != "11115678" ||
			a.BaseCurrency == nil || *a.BaseCurrency != "USD" {
			t.Errorf("account row = %+v, want id and display name 11115678 in USD", a)
		}
	}

	ts, err := c.Transactions(ctx, everything)
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	var txs []canonical.TransactionChange
	for {
		b, more, err := ts.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		txs = append(txs, b.Transactions...)
		if !more {
			break
		}
	}
	if len(txs) != 2 {
		t.Fatalf("transactions = %d, want both: no api cut applies", len(txs))
	}
	for _, x := range txs {
		if x.AccountExternalID != "11115678" {
			t.Errorf("transaction %s on %q, want 11115678", x.TransactionExternalID, x.AccountExternalID)
		}
		if x.TransactionExternalID == "T2" && (x.InstrumentExternalID == nil || *x.InstrumentExternalID != "VTI") {
			t.Errorf("a web trade keeps its own key with no api to resolve it: %v", x.InstrumentExternalID)
		}
	}
	st, err := c.Status(ctx)
	if err != nil || st.LatestChangeNumber < 0 {
		t.Errorf("Status = %+v, %v", st, err)
	}
}

// An account the web silver states no full number for has no id
// without the api silver; the load fails naming it rather than keying
// it by its last digits.
func TestWebAloneRefusesAnAccountWithNoFullNumber(t *testing.T) {
	c := openWebAlone(t, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES (1000, '0042', '{}');
    `)
	_, err := c.Snapshots(context.Background(), everything)
	if err == nil || !strings.Contains(err.Error(), `"0042"`) {
		t.Fatalf("Snapshots error = %v, want it to name the account", err)
	}
	c2 := openWebAlone(t, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES
            (1000, '0042', '{"account_number_full":"99990043"}');
    `)
	if _, err := c2.Snapshots(context.Background(), everything); err == nil {
		t.Fatal("a full number that does not end in its suffix must be refused")
	}
}

// With web alone, a year-end document's realized lot lands under the
// account's full number, and the newest full number an account states
// is the one it is keyed by, whatever older rows stated.
func TestWebAloneRealizedLotsAndTheNewestNumber(t *testing.T) {
	c := openWebAlone(t, `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO accounts(snapshot_at, account_external_id, payload) VALUES
            (800,  '5678', '{"account_number_full":"2222-5678"}'),
            (900,  '5678', '{"account_number_full":"1111-5678"}'),
            (1000, '5678', '{}');
        INSERT INTO closed_lots
            (logical_doc_key, document_kind, lot_index, account_external_id, tax_year, security_name, cusip,
             instrument_key, quantity, acquired_date, disposed_date, proceeds, cost_basis, wash_sale_disallowed,
             accrued_market_discount, realized_gain_loss, term, covered, form_8949_box, footnotes, source_sha256, payload)
        VALUES ('5678|1706745600|1099-2023', 'form_1099b', 0, '5678', 2023, 'VTI', NULL, 'VTI',
                10, '2020-01-02', '2023-06-01', 1500, 1000, NULL, NULL, 500, 'LONG', 1, 'D', NULL, 'sha-1099', '{}');
    `)
	lots, err := c.RealizedLots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(lots) != 1 || lots[0].AccountExternalID != "11115678" {
		t.Fatalf("realized lots = %+v, want one under 11115678", lots)
	}
}
