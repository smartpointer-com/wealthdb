package ubs

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// Realized lots from the statements' transaction lists and the
// portfolio export. Every account, portfolio, ISIN, valor, figure and
// day below is invented.

const (
	rlPrinted   = "999-1.S1"         // as a statement prints the custody account
	rlSafe      = "09990000000001S1" // the same account as PSN keys it
	rlPortfolio = "0999000000010001"
	rlStock     = "XX0000000101"
	rlBond      = "XX0000000102"
	rlDay       = int64(86400)
	// 2030-01-01, so every sale below falls in tax year 2030.
	rlYear = int64(1893456000)
)

// newRealizedWeb is a web silver with the documents catalogue, the
// statements' transaction lists and the portfolio export.
func newRealizedWeb(t *testing.T) *webReader {
	t.Helper()
	r := newWebFixture(t)
	if _, err := r.db.Exec(`
CREATE TABLE documents (doc_token TEXT PRIMARY KEY, content_sha256 TEXT NOT NULL);
INSERT INTO documents VALUES ('stmt-q1', 'sha-q1'), ('stmt-m2', 'sha-m2'), ('stmt-q2', 'sha-q2');
CREATE TABLE statement_trades (
    source_doc_token TEXT NOT NULL, seq INTEGER NOT NULL, as_of_date INTEGER NOT NULL,
    portfolio_external_id TEXT NOT NULL, reporting_currency_iso TEXT,
    period_start INTEGER, period_end INTEGER, trade_date INTEGER, trade_time TEXT,
    value_date INTEGER, booking_text TEXT NOT NULL, quantity REAL, security_name TEXT,
    valor TEXT, isin TEXT, currency_iso TEXT, cost_price REAL, acquisition_fx_rate REAL,
    cost_basis REAL, transaction_price REAL, transaction_fx_rate REAL,
    transaction_gain_pct REAL, exchange_gain_pct REAL, realized_pl_pct REAL,
    transaction_value REAL, accrued_interest REAL, settlement_amount REAL,
    settlement_currency_iso TEXT, taxes REAL, fees REAL, commission REAL,
    stock_exchange_fees REAL, third_party_fees REAL, financial_transaction_tax REAL,
    charges_currency_iso TEXT, place_of_execution TEXT, settlement_no TEXT,
    order_no TEXT, custody_account TEXT, account_iban TEXT, payload TEXT NOT NULL,
    PRIMARY KEY (source_doc_token, seq));
CREATE TABLE portfolio_transactions (
    transaction_external_id TEXT NOT NULL, safekeeping_account_external_id TEXT NOT NULL,
    portfolio_external_id TEXT NOT NULL, snapshot_at INTEGER NOT NULL,
    trade_date INTEGER, booking_date INTEGER, value_date INTEGER NOT NULL,
    booking_type TEXT NOT NULL, security_name TEXT, valor TEXT, isin TEXT,
    quantity REAL, settlement_currency_iso TEXT, trans_price REAL, exchange_rate REAL,
    valuation_currency_iso TEXT, trans_value REAL, accrued_interest REAL,
    realized_pl REAL, order_no TEXT, external_reference TEXT, asset_class TEXT,
    sub_asset_class TEXT, instrument_category TEXT, payload TEXT NOT NULL,
    PRIMARY KEY (transaction_external_id, safekeeping_account_external_id));`); err != nil {
		t.Fatal(err)
	}
	return r
}

// stmtRow is one booking of a transaction list.
type stmtRow struct {
	doc               string
	seq               int
	periodStart, end  int64
	trade             int64
	booking           string
	quantity          float64
	isin, valor       any
	costPrice, cost   any
	price, settlement any
	value             any
	settlementNo      string
	custody           string
}

func seedStmt(t *testing.T, r *webReader, row stmtRow) {
	t.Helper()
	if row.custody == "" {
		row.custody = rlPrinted
	}
	if _, err := r.db.Exec(`
        INSERT INTO statement_trades (source_doc_token, seq, as_of_date, portfolio_external_id,
            reporting_currency_iso, period_start, period_end, trade_date, value_date,
            booking_text, quantity, security_name, valor, isin, currency_iso,
            cost_price, cost_basis, transaction_price, realized_pl_pct, transaction_value,
            settlement_amount, settlement_currency_iso, settlement_no, custody_account, payload)
        VALUES (?, ?, ?, ?, 'CHF', ?, ?, ?, ?, ?, ?, 'Example Holding', ?, ?, 'USD',
                ?, ?, ?, 5.0, ?, ?, 'USD', ?, ?, '{}')`,
		row.doc, row.seq, row.end, rlPortfolio, row.periodStart, row.end,
		row.trade, row.trade+2*rlDay, row.booking, row.quantity, row.valor, row.isin,
		row.costPrice, row.cost, row.price, row.value, row.settlement,
		row.settlementNo, row.custody); err != nil {
		t.Fatalf("seed statement row %s/%d: %v", row.doc, row.seq, err)
	}
}

// A first-quarter statement and the month-end one inside it share a
// sale, which the month-end statement keeps because it is dated first.
// The second-quarter statement prints a sale its bank reversed and
// rebooked.
var (
	q1Start, q1End = rlYear, rlYear + 89*rlDay
	m2Start, m2End = rlYear + 31*rlDay, rlYear + 58*rlDay
	q2Start, q2End = rlYear + 90*rlDay, rlYear + 180*rlDay
)

func seedStatementSales(t *testing.T, r *webReader) {
	t.Helper()
	sale := func(doc string, seq int, start, end int64) stmtRow {
		return stmtRow{doc: doc, seq: seq, periodStart: start, end: end,
			trade: rlYear + 40*rlDay, booking: "Sale Spot", quantity: -10, isin: rlStock,
			costPrice: 100.0, cost: 900.0, price: 110.0, settlement: 1100.0, value: -1000.0,
			settlementNo: "SETTLE-1"}
	}
	seedStmt(t, r, sale("stmt-q1", 1, q1Start, q1End))
	seedStmt(t, r, sale("stmt-m2", 1, m2Start, m2End))
	// A bond: the cost price is a percent of the nominal, so units ×
	// price is no cost. The cost value is.
	seedStmt(t, r, stmtRow{doc: "stmt-q1", seq: 2, periodStart: q1Start, end: q1End,
		trade: rlYear + 50*rlDay, booking: "Sale Spot", quantity: -10000, isin: rlBond,
		costPrice: 101.5, cost: 10150.0, price: 99.0, settlement: 9900.0, value: -9900.0,
		settlementNo: "SETTLE-2"})
	// Not sales: a corporate action prints no price, a delivery free of
	// payment settles nothing, a purchase is no disposal.
	seedStmt(t, r, stmtRow{doc: "stmt-q1", seq: 3, periodStart: q1Start, end: q1End,
		trade: rlYear + 60*rlDay, booking: "Available from Split", quantity: -5, isin: rlStock,
		settlementNo: "SETTLE-3"})
	seedStmt(t, r, stmtRow{doc: "stmt-q1", seq: 4, periodStart: q1Start, end: q1End,
		trade: rlYear + 61*rlDay, booking: "Delivery without payment", quantity: -5, isin: rlStock,
		costPrice: 100.0, cost: 450.0, price: 110.0, value: -550.0, settlementNo: "SETTLE-4"})
	seedStmt(t, r, stmtRow{doc: "stmt-q1", seq: 5, periodStart: q1Start, end: q1End,
		trade: rlYear + 62*rlDay, booking: "Purchase Spot", quantity: 5, isin: rlStock,
		costPrice: 100.0, settlement: -500.0, settlementNo: "SETTLE-5"})
	// Sold, reversed the same day, rebooked three days later.
	reversed := stmtRow{doc: "stmt-q2", seq: 1, periodStart: q2Start, end: q2End,
		trade: rlYear + 100*rlDay, booking: "Sale Spot", quantity: -20, isin: rlStock,
		costPrice: 100.0, cost: 1800.0, price: 120.0, settlement: 2400.0, value: -2400.0,
		settlementNo: "SETTLE-6"}
	seedStmt(t, r, reversed)
	reversal := reversed
	reversal.seq, reversal.booking, reversal.quantity, reversal.value = 2, "Reversal Sale Spot", 20, 2400.0
	reversal.cost, reversal.settlementNo = nil, "SETTLE-7"
	seedStmt(t, r, reversal)
	rebooked := reversed
	rebooked.seq, rebooked.trade, rebooked.settlementNo = 3, rlYear+103*rlDay, "SETTLE-8"
	seedStmt(t, r, rebooked)
	// A sale without a cost value, named by its valor alone.
	seedStmt(t, r, stmtRow{doc: "stmt-q2", seq: 4, periodStart: q2Start, end: q2End,
		trade: rlYear + 120*rlDay, booking: "Sale from Opt. Dividend", quantity: -3, valor: "0004242",
		price: 2.0, settlement: 6.0, value: -6.0, settlementNo: "SETTLE-9"})
}

// newRealizedPSN is a PSN silver whose roster knows the custody account.
func newRealizedPSN(t *testing.T) *psnReader {
	t.Helper()
	_, db := newFixtureSilver(t)
	if _, err := db.Exec(`
        INSERT INTO safekeeping_accounts (snapshot_at, relationship_id, account_external_id, portfolio_external_id, payload)
        VALUES (1, 'R1', ?, ?, '{}')`, rlSafe, rlPortfolio); err != nil {
		t.Fatal(err)
	}
	return &psnReader{db: db}
}

func realizedByDocSeq(t *testing.T, c *Connection) map[string]canonical.RealizedLotChange {
	t.Helper()
	lots, err := c.RealizedLots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]canonical.RealizedLotChange{}
	for _, l := range lots {
		var p struct {
			Doc string `json:"source_doc_token"`
			Seq int    `json:"seq"`
		}
		key := l.RealizedLotExternalID
		if l.DocumentKind == canonical.RealizedStatement {
			if err := json.Unmarshal(l.Payload, &p); err != nil {
				t.Fatalf("payload %s: %v", l.Payload, err)
			}
			key = p.Doc + "/" + strconv.Itoa(p.Seq)
		}
		if _, dup := out[key]; dup {
			t.Fatalf("two lots under %s", key)
		}
		out[key] = l
	}
	return out
}

// TestStatementSalesAreRealizedLots: each printed sale is one realized
// lot in the reporting currency, with the cost value as its book value
// and the percentages in the payload. A booking two statements print
// counts once, a reversed sale not at all, and rows that are no sale
// are not lots.
func TestStatementSalesAreRealizedLots(t *testing.T) {
	web := newRealizedWeb(t)
	seedStatementSales(t, web)
	got := realizedByDocSeq(t, &Connection{web: web, psn: newRealizedPSN(t)})

	if len(got) != 6 {
		t.Fatalf("lots = %d, want 6 (two copies of one sale, a bond sale, a reversed sale, its rebooking, a rights sale)", len(got))
	}

	first := got["stmt-m2/1"] // the earlier statement prints it first
	if first.DocumentKind != canonical.RealizedStatement || !first.IsPrimary {
		t.Errorf("first copy: kind %s primary %v, want statement and primary", first.DocumentKind, first.IsPrimary)
	}
	if first.AccountExternalID != rlSafe {
		t.Errorf("account = %q, want the custody account in PSN's form %q", first.AccountExternalID, rlSafe)
	}
	if first.Currency != "CHF" || first.TaxYear != 2030 {
		t.Errorf("currency %s tax year %d, want CHF 2030", first.Currency, first.TaxYear)
	}
	for name, d := range map[string]struct {
		got  any
		want string
	}{
		"quantity": {first.Quantity, "10"},
		"proceeds": {first.Proceeds, "1000"},
		"book":     {first.BookValue, "900"},
	} {
		if v, ok := d.got.(*canonical.Decimal); !ok || v == nil || v.String() != d.want {
			t.Errorf("%s = %v, want %s", name, d.got, d.want)
		}
	}
	if first.Basis != statedAverageBasis {
		t.Errorf("basis = %+v, want stated/average/excluded", first.Basis)
	}
	if first.RealizedGainLoss != nil {
		t.Errorf("gain = %v, want NULL: the list prints it only as a percentage", first.RealizedGainLoss)
	}
	if first.InstrumentExternalID == nil || *first.InstrumentExternalID != rlStock {
		t.Errorf("instrument = %v, want %s", first.InstrumentExternalID, rlStock)
	}
	if first.DisposalDate == nil || first.DisposalDate.Unix() != rlYear+40*rlDay ||
		first.SettlementDate == nil || first.SettlementDate.Unix() != rlYear+42*rlDay {
		t.Errorf("dates = %v / %v, want the trade and value days", first.DisposalDate, first.SettlementDate)
	}
	if first.SourceDocument == nil || *first.SourceDocument != "sha-m2" {
		t.Errorf("source document = %v, want the statement's hash", first.SourceDocument)
	}
	var p map[string]any
	if err := json.Unmarshal(first.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p["realized_pl_pct"] != 5.0 || p["settlement_no"] != "SETTLE-1" {
		t.Errorf("payload = %s, want the printed percentage and the settlement number", first.Payload)
	}

	if copy := got["stmt-q1/1"]; copy.IsPrimary || copy.RealizedLotExternalID == first.RealizedLotExternalID {
		t.Errorf("second copy: primary %v id %s, want a distinct id that is not primary", copy.IsPrimary, copy.RealizedLotExternalID)
	}

	bond := got["stmt-q1/2"]
	if bond.BookValue == nil || bond.BookValue.String() != "10150" || !bond.IsPrimary {
		t.Errorf("bond: book %v primary %v, want the cost value 10150 and primary", bond.BookValue, bond.IsPrimary)
	}

	reversed := got["stmt-q2/1"]
	if reversed.IsPrimary {
		t.Error("a reversed sale is primary; it never happened")
	}
	if err := json.Unmarshal(reversed.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p["reversed"] != true || p["reversed_by"] != "SETTLE-7" {
		t.Errorf("reversed payload = %s, want reversed by SETTLE-7", reversed.Payload)
	}
	if rebooked := got["stmt-q2/3"]; !rebooked.IsPrimary {
		t.Error("the rebooked sale is not primary")
	}

	rights := got["stmt-q2/4"]
	if rights.BookValue != nil || !rights.Basis.IsZero() || !rights.IsPrimary {
		t.Errorf("rights sale: book %v basis %+v primary %v, want no basis and primary", rights.BookValue, rights.Basis, rights.IsPrimary)
	}
	if rights.InstrumentExternalID != nil || rights.InstrumentHint != "4242" {
		t.Errorf("rights sale: instrument %v hint %q, want the valor as the hint", rights.InstrumentExternalID, rights.InstrumentHint)
	}
}

// TestAnUnknownCustodyAccountFallsBackToThePortfolio: without PSN the
// statement era keeps a portfolio's securities on its overlay, and its
// sales sit there too.
func TestAnUnknownCustodyAccountFallsBackToThePortfolio(t *testing.T) {
	web := newRealizedWeb(t)
	seedStatementSales(t, web)
	got := realizedByDocSeq(t, &Connection{web: web})
	if a := got["stmt-q1/1"].AccountExternalID; a != overlayAccountID(rlPortfolio) {
		t.Errorf("account = %q, want the portfolio overlay", a)
	}
}

func TestCustodyAccountCanonical(t *testing.T) {
	for printed, want := range map[string]string{
		"999-1.S1":           "09990000000001S1",
		"0999-123456.T2":     "09990000123456T2",
		"0999 123456.S1":     "09990000123456S1",
		"999-123456":         "",
		"not an account":     "",
		"999-12345678901.S1": "",
	} {
		if got := custodyAccountCanonical(printed); got != want {
			t.Errorf("custodyAccountCanonical(%q) = %q, want %q", printed, got, want)
		}
	}
}

// TestExportSalesCountWhereNoStatementReaches: the export restates a
// statement's sale, which then does not count, and reaches past the
// last statement, where its sale counts beside the statement's in the
// same tax year. Its P/L and value are in the valuation currency, and
// the cost is the one less the other.
func TestExportSalesCountWhereNoStatementReaches(t *testing.T) {
	web := newRealizedWeb(t)
	seedStatementSales(t, web)
	for _, row := range []struct {
		id    string
		trade int64
	}{
		{"ptx:inside", rlYear + 40*rlDay}, // the first quarter's sale again
		{"ptx:after", rlYear + 200*rlDay}, // after the last statement
	} {
		if _, err := web.db.Exec(`
            INSERT INTO portfolio_transactions (transaction_external_id, safekeeping_account_external_id,
                portfolio_external_id, snapshot_at, trade_date, value_date, booking_type,
                security_name, isin, quantity, settlement_currency_iso, valuation_currency_iso,
                trans_value, realized_pl, payload)
            VALUES (?, ?, ?, 1, ?, ?, 'Stock Market Spot Sale', 'Example Holding', ?, -10, 'USD', 'CHF',
                    -1000, 100, '{"Booking":"x"}')`,
			row.id, rlSafe, rlPortfolio, row.trade, row.trade+2*rlDay, rlStock); err != nil {
			t.Fatal(err)
		}
	}
	got := realizedByDocSeq(t, &Connection{web: web, psn: newRealizedPSN(t)})

	inside := got[realizedID("trade", "ptx:inside", rlSafe)]
	after := got[realizedID("trade", "ptx:after", rlSafe)]
	if inside.DocumentKind != canonical.RealizedTrade || inside.IsPrimary {
		t.Errorf("restated sale: kind %s primary %v, want trade and not primary", inside.DocumentKind, inside.IsPrimary)
	}
	if !after.IsPrimary || after.TaxYear != 2030 {
		t.Errorf("sale past the statements: primary %v year %d, want primary in 2030", after.IsPrimary, after.TaxYear)
	}
	if !got["stmt-m2/1"].IsPrimary {
		t.Error("the statement's sale lost its primary to the export")
	}
	if after.AccountExternalID != rlSafe || after.Currency != "CHF" {
		t.Errorf("account %q currency %s, want %s CHF", after.AccountExternalID, after.Currency, rlSafe)
	}
	if after.Proceeds == nil || after.Proceeds.String() != "1000" ||
		after.RealizedGainLoss == nil || after.RealizedGainLoss.String() != "100" ||
		after.BookValue == nil || after.BookValue.String() != "900" {
		t.Errorf("proceeds %v gain %v book %v, want 1000, 100, 900", after.Proceeds, after.RealizedGainLoss, after.BookValue)
	}
	if after.Basis != derivedAverageBasis {
		t.Errorf("basis = %+v, want derived/average/excluded", after.Basis)
	}
}

// TestASilverWithoutTheListsStatesNoRealizedLots: an older web silver,
// or none at all, yields no lots and no error.
func TestASilverWithoutTheListsStatesNoRealizedLots(t *testing.T) {
	for name, c := range map[string]*Connection{
		"no web":  {psn: newRealizedPSN(t)},
		"old web": {web: newWebFixture(t), psn: newRealizedPSN(t)},
	} {
		lots, err := c.RealizedLots(context.Background())
		if err != nil || len(lots) != 0 {
			t.Errorf("%s: lots %d err %v, want none", name, len(lots), err)
		}
	}
}
