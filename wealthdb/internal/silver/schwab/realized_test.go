package schwab

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// The realized lots of the three year-end documents, as silver keeps
// them: every copy, keyed by its document. Every account, document key,
// security and figure is invented; document dates are Unix seconds, as
// the collector writes them into logical_doc_key.
const realizedFixture = `
INSERT INTO closed_lots
    (logical_doc_key, document_kind, lot_index, account_external_id, tax_year, security_name, cusip,
     instrument_key, quantity, acquired_date, disposed_date, proceeds, cost_basis, wash_sale_disallowed,
     accrued_market_discount, realized_gain_loss, term, covered, form_8949_box, footnotes, source_sha256, payload)
VALUES
    -- 2023, account 5678: a 1099-B, its correction, and the Year-End Summary.
    ('5678|1706745600|1099-2023', 'form_1099b', 0, '5678', 2023, 'VTI', NULL, NULL,
     10, '2020-01-15', '2023-03-01', 1500, 1000, NULL, 0, NULL, 'LONG', 1, 'D', NULL, 'sha-1099-a', '{"n":"orig-0"}'),
    ('5678|1706745600|1099-2023', 'form_1099b', 1, '5678', 2023, 'SPX 06/16/2023 4000.00 C', NULL, NULL,
     1, 'Various', '2023-05-01', 300, NULL, NULL, 0, NULL, 'SHORT', 0, 'X', NULL, 'sha-1099-a', '{"n":"orig-1"}'),
    ('5678|1708560000|1099-2023', 'form_1099b', 0, '5678', 2023, 'VTI', NULL, NULL,
     10, '2020-01-15', '2023-03-01', 1500, 1000, 25, 0, NULL, 'LONG', 1, 'D', NULL, 'sha-1099-b', '{"n":"corr-0"}'),
    ('5678|1708560000|1099-2023', 'form_1099b', 1, '5678', 2023, 'SPX 06/16/2023 4000.00 C', NULL, NULL,
     1, 'Various', '2023-05-01', 300, NULL, NULL, 0, NULL, 'SHORT', 0, 'X', NULL, 'sha-1099-b', '{"n":"corr-1"}'),
    ('5678|1708560000|1099-2023', 'form_1099b', 2, '5678', 2023, 'EXAMPLE CORP CLASS A', NULL, NULL,
     -3, '2022-11-01', '2023-08-01', 90, 120, NULL, 0, NULL, 'Undetermined', 0, 'X', NULL, 'sha-1099-b', '{"n":"corr-2"}'),
    ('5678|1706745600|YES-2023.PDF', 'year_end_summary', 0, '5678', 2023, 'VANGUARD TOTAL STOCK MARKET ETF', 'CUSIPVTI0', 'CUSIPVTI0',
     10, '2020-01-15', '2023-03-01', 1500, 1000, 25, NULL, 500, 'LONG', NULL, 'C,F', NULL, 'sha-yes-23',
     '{"adjusted_cost_basis":990}'),
    -- 2024, account 5678: the Year-End Summary twice under one date, and a Gain/Loss Report.
    ('5678|1738368000|YES-2024.PDF', 'year_end_summary', 0, '5678', 2024, 'INVESCO QQQ', 'CUSIPQQQ0', 'CUSIPQQQ0',
     2, '2023-02-01', '2024-06-03', 800, 700, NULL, NULL, 100, 'LONG', 1, 'D', NULL, 'sha-yes-24a', '{}'),
    ('5678|1738368000|YES-2024.2.PDF', 'year_end_summary', 0, '5678', 2024, 'INVESCO QQQ', 'CUSIPQQQ0', 'CUSIPQQQ0',
     2, '2023-02-01', '2024-06-03', 800, 700, NULL, NULL, 100, 'LONG', 1, 'D', NULL, 'sha-yes-24b', '{}'),
    ('5678|1738368000|GLR-2024.PDF', 'gain_loss_report', 0, '5678', 2024, 'VANGUARD TOTAL', NULL, 'VTI',
     2, '2023-02-01', '2024-06-03', 800, 700, NULL, NULL, 100, 'LONG', NULL, NULL, NULL, 'sha-glr-24', '{}'),
    -- 2022, account 0042: a Gain/Loss Report alone; it prints no tax year here.
    ('0042|1675209600|GLR-2022.PDF', 'gain_loss_report', 0, '0042', NULL, 'EXAMPLE FUND', NULL, 'XMPF',
     5, '2021-01-04', '2022-07-01', 50, 60, NULL, NULL, -10, 'LONG', NULL, NULL, NULL, 'sha-glr-22', '{}'),
    -- An account the api roster does not hold.
    ('9999|1706745600|1099-2023', 'form_1099b', 0, '9999', 2023, 'VTI', NULL, NULL,
     1, '2020-01-15', '2023-03-01', 150, 100, NULL, 0, NULL, 'LONG', 1, 'D', NULL, 'sha-other', '{}');
`

// realizedLots reads every realized lot the connection offers.
func realizedLots(t *testing.T, conn *Connection) []canonical.RealizedLotChange {
	t.Helper()
	lots, err := conn.RealizedLots(context.Background())
	if err != nil {
		t.Fatalf("RealizedLots: %v", err)
	}
	return lots
}

// TestRealizedLots maps closed_lots onto realized lots, field by field,
// and marks the primaries: the best document kind per account and tax
// year, and within it the latest document, so a correction and a
// duplicated summary count once.
func TestRealizedLots(t *testing.T) {
	f := newMergedFixture(t)
	// The api covers account HASH1 from early 2022: the lots ignore that
	// cutoff, unlike the web transactions.
	if _, err := f.api.Exec(`
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, payload) VALUES
            ('API1', ?, 'HASH1', 'TRADE', '{"netAmount":-100}')`, utcDay("2022-01-03")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.web.Exec(realizedFixture); err != nil {
		t.Fatalf("seed: %v", err)
	}
	conn := f.open(t)
	lots := realizedLots(t, conn)

	if len(lots) != 10 {
		t.Fatalf("lots = %d, want 10 (every copy of a bridged account; the unbridged one dropped)", len(lots))
	}
	bySource := map[string][]canonical.RealizedLotChange{}
	ids := map[string]bool{}
	for _, r := range lots {
		bySource[*r.SourceDocument] = append(bySource[*r.SourceDocument], r)
		if ids[r.RealizedLotExternalID] {
			t.Errorf("duplicate id %s", r.RealizedLotExternalID)
		}
		ids[r.RealizedLotExternalID] = true
		if r.Currency != "USD" {
			t.Errorf("%s: currency %q", r.RealizedLotExternalID, r.Currency)
		}
		if err := canonical.ValidateBookValue(r.BookValue, r.Basis); err != nil {
			t.Errorf("%s: %v", r.RealizedLotExternalID, err)
		}
	}

	// Primaries: per account and tax year, the best kind present and,
	// within it, the latest document only.
	wantPrimary := map[string]bool{
		"sha-1099-a":  false, // superseded by its correction
		"sha-1099-b":  true,  // the correction
		"sha-yes-23":  false, // a 1099-B is present
		"sha-yes-24a": true,  // the greater key of two copies dated alike
		"sha-yes-24b": false,
		"sha-glr-24":  false, // a Year-End Summary is present
		"sha-glr-22":  true,  // the only document of its year
	}
	for src, rs := range bySource {
		for _, r := range rs {
			if r.IsPrimary != wantPrimary[src] {
				t.Errorf("%s lot: primary = %v, want %v", src, r.IsPrimary, wantPrimary[src])
			}
		}
	}

	// The correction's lots, field by field.
	corr := bySource["sha-1099-b"]
	if len(corr) != 3 {
		t.Fatalf("corrected 1099-B lots = %d, want 3", len(corr))
	}
	byMarker := map[string]canonical.RealizedLotChange{}
	for _, r := range corr {
		var p struct{ N string }
		_ = json.Unmarshal(r.Payload, &p)
		byMarker[p.N] = r
	}
	stock := byMarker["corr-0"]
	if stock.AccountExternalID != "HASH1" || stock.TaxYear != 2023 || stock.DocumentKind != canonical.RealizedForm1099B {
		t.Errorf("stock lot placed at (%s, %d, %s)", stock.AccountExternalID, stock.TaxYear, stock.DocumentKind)
	}
	if stock.InstrumentExternalID == nil || *stock.InstrumentExternalID != "CUSIPVTI0" || stock.InstrumentHint != "" {
		t.Errorf("a plain ticker resolves through the symbol bridge: got %v / %q", stock.InstrumentExternalID, stock.InstrumentHint)
	}
	if decStr(stock.Quantity) != "10" || decStr(stock.Proceeds) != "1500" || decStr(stock.BookValue) != "1000" ||
		decStr(stock.WashSaleDisallowed) != "25" || decStr(stock.AccruedMarketDiscount) != "0" ||
		stock.RealizedGainLoss != nil {
		t.Errorf("stock lot figures: qty %s proceeds %s book %s wash %s discount %s gain %s",
			decStr(stock.Quantity), decStr(stock.Proceeds), decStr(stock.BookValue),
			decStr(stock.WashSaleDisallowed), decStr(stock.AccruedMarketDiscount), decStr(stock.RealizedGainLoss))
	}
	if stock.Basis != statementBasis || dateStr(stock.AcquisitionDate) != "2020-01-15" || stock.AcquiredVarious ||
		dateStr(stock.DisposalDate) != "2023-03-01" || stock.SettlementDate != nil {
		t.Errorf("stock lot: stamp %+v, acquired %s (various %v), disposed %s",
			stock.Basis, dateStr(stock.AcquisitionDate), stock.AcquiredVarious, dateStr(stock.DisposalDate))
	}
	if stock.Term != canonical.LotTermLong || stock.Covered == nil || !*stock.Covered ||
		stock.Form8949Box == nil || *stock.Form8949Box != "D" || stock.Description == nil || *stock.Description != "VTI" {
		t.Errorf("stock lot: term %q covered %v box %v description %v", stock.Term, stock.Covered, stock.Form8949Box, stock.Description)
	}

	option := byMarker["corr-1"]
	if option.InstrumentExternalID == nil || *option.InstrumentExternalID != "SPX 06/16/2023 4000.00 C" {
		t.Errorf("an option keeps its contract: got %v", option.InstrumentExternalID)
	}
	if !option.AcquiredVarious || option.AcquisitionDate != nil {
		t.Errorf("Various: acquired %s, various %v", dateStr(option.AcquisitionDate), option.AcquiredVarious)
	}
	if option.BookValue != nil || !option.Basis.IsZero() {
		t.Errorf("an unprinted basis stays NULL and unstamped: %s %+v", decStr(option.BookValue), option.Basis)
	}
	if option.Covered == nil || *option.Covered || option.Term != canonical.LotTermShort {
		t.Errorf("noncovered short lot: covered %v term %q", option.Covered, option.Term)
	}

	unnamed := byMarker["corr-2"]
	if unnamed.InstrumentExternalID != nil || unnamed.InstrumentHint != "EXAMPLE CORP CLASS A" ||
		unnamed.Description == nil || *unnamed.Description != "EXAMPLE CORP CLASS A" {
		t.Errorf("a name of neither shape is a hint: %v / %q", unnamed.InstrumentExternalID, unnamed.InstrumentHint)
	}
	if decStr(unnamed.Quantity) != "3" || unnamed.Term != "" {
		t.Errorf("magnitude and unknown term: qty %s term %q", decStr(unnamed.Quantity), unnamed.Term)
	}

	// The Year-End Summary keys by CUSIP, prints the gain, and keeps the
	// adjusted basis in its payload.
	yes := bySource["sha-yes-23"][0]
	if yes.InstrumentExternalID == nil || *yes.InstrumentExternalID != "CUSIPVTI0" ||
		decStr(yes.RealizedGainLoss) != "500" || yes.Covered != nil {
		t.Errorf("year-end summary lot: instrument %v gain %s covered %v",
			yes.InstrumentExternalID, decStr(yes.RealizedGainLoss), yes.Covered)
	}
	if string(yes.Payload) != `{"adjusted_cost_basis":990}` {
		t.Errorf("payload = %s", yes.Payload)
	}

	// The Gain/Loss Report keys by ticker, bridged like a transaction's.
	glr := bySource["sha-glr-24"][0]
	if glr.InstrumentExternalID == nil || *glr.InstrumentExternalID != "CUSIPVTI0" {
		t.Errorf("gain/loss report lot: instrument %v", glr.InstrumentExternalID)
	}
	// A lot without a printed tax year takes its disposal year.
	lone := bySource["sha-glr-22"][0]
	if lone.TaxYear != 2022 || lone.AccountExternalID != "HASH2" || decStr(lone.RealizedGainLoss) != "-10" {
		t.Errorf("lone report lot: year %d account %s gain %s", lone.TaxYear, lone.AccountExternalID, decStr(lone.RealizedGainLoss))
	}

	// Ids are the silver row's own key: a second read returns the same.
	again := realizedLots(t, conn)
	for _, r := range again {
		if !ids[r.RealizedLotExternalID] {
			t.Errorf("id %s not stable across reads", r.RealizedLotExternalID)
		}
	}
}

// The Connection offers its realized lots to the loader; an api-only
// source has none to offer, and a web silver without the table loads.
func TestRealizedLotsOptional(t *testing.T) {
	var _ silver.RealizedLotReader = (*Connection)(nil)

	path, _ := newFixtureSilver(t)
	conn := openAdapter(t, path).(*Connection)
	if lots, err := conn.RealizedLots(context.Background()); err != nil || lots != nil {
		t.Errorf("api only: %v, %v", lots, err)
	}

	f := newMergedFixture(t)
	if _, err := f.web.Exec(`DROP TABLE closed_lots`); err != nil {
		t.Fatal(err)
	}
	if lots, err := f.open(t).RealizedLots(context.Background()); err != nil || lots != nil {
		t.Errorf("web silver without closed_lots: %v, %v", lots, err)
	}
}

// The 1099-B rows stay out of the transaction stream: a year the
// statements cover carries their sells, and a year only the 1099-B
// states carries its sales as realized lots alone.
func TestForm1099BLeavesTheTransactionStream(t *testing.T) {
	f := newMergedFixture(t)
	if _, err := f.web.Exec(`
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, instrument_key, source, source_sha256, payload) VALUES
            ('f-2021', ?1, '5678', 'Sale', NULL, 'form_1099b',    'sha-f', '{"tax_year":2021,"security_name":"VTI","proceeds":1000}'),
            ('p-2021', ?2, '5678', 'Sale', 'VTI', 'statement_pdf', 'sha-p', '{"amount":1000,"description":"Sold VTI"}'),
            ('p-div',  ?2, '5678', 'Dividend', 'VTI', 'statement_pdf', 'sha-p', '{"amount":12}'),
            ('f-2020', ?3, '5678', 'Sale', NULL, 'form_1099b',    'sha-g', '{"tax_year":2020,"security_name":"QQQ","proceeds":400}');
        INSERT INTO closed_lots
            (logical_doc_key, document_kind, lot_index, account_external_id, tax_year, security_name,
             quantity, disposed_date, proceeds, cost_basis, term, covered, source_sha256, payload) VALUES
            ('5678|1612137600|1099-2020', 'form_1099b', 0, '5678', 2020, 'QQQ', 2, '2020-06-01', 400, 300, 'LONG', 1, 'sha-g', '{}');
    `, utcDay("2021-03-01"), utcDay("2021-03-03"), utcDay("2020-06-01")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	conn := f.open(t)
	stream, err := conn.Transactions(context.Background(), everything)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for {
		b, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, tx := range b.Transactions {
			got = append(got, tx.TransactionExternalID)
			if tx.TransactionExternalID == "p-2021" &&
				(tx.Kind != canonical.TxKindSell || tx.InstrumentExternalID == nil || *tx.InstrumentExternalID != "CUSIPVTI0") {
				t.Errorf("statement sell: kind %q instrument %v", tx.Kind, tx.InstrumentExternalID)
			}
		}
		if !more {
			break
		}
	}
	if len(got) != 2 || !slices.Contains(got, "p-2021") || !slices.Contains(got, "p-div") {
		t.Errorf("transactions = %v, want the statement rows only", got)
	}

	lots := realizedLots(t, conn)
	if len(lots) != 1 || !lots[0].IsPrimary || lots[0].TaxYear != 2020 || decStr(lots[0].Proceeds) != "400" {
		t.Errorf("the 1099-B-only year's sale is a primary realized lot: %+v", lots)
	}
}

// The 1099-B rows widen no window and move no transaction extreme: they
// reach no stream, so a load whose only new rows are theirs has no
// transactions to carry.
func TestForm1099BWidensNoWindow(t *testing.T) {
	f := newMergedFixture(t)
	stmt := utcDay("2021-03-03")
	if _, err := f.web.Exec(`
        INSERT INTO transactions(activity_id, timestamp, account_external_id, kind, instrument_key, source, source_sha256, payload) VALUES
            ('p-2021',  ?1, '5678', 'Sale', 'VTI', 'statement_pdf', 'sha-p', '{"amount":1000}'),
            ('f-early', ?2, '5678', 'Sale', NULL,  'form_1099b',    'sha-f', '{"tax_year":2020}'),
            ('f-late',  ?3, '5678', 'Sale', NULL,  'form_1099b',    'sha-f', '{"tax_year":2021}');
    `, stmt, utcDay("2020-06-01"), utcDay("2021-09-01")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := &webReader{db: f.web}
	ctx := context.Background()

	s, err := r.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.OldestTransactionAt != stmt || s.LatestTransactionAt != stmt {
		t.Errorf("transaction extremes = [%d, %d], want the statement row's day %d",
			s.OldestTransactionAt, s.LatestTransactionAt, stmt)
	}

	w, err := r.ChangeWindow(ctx, -1)
	if err != nil {
		t.Fatal(err)
	}
	if !w.HasChanges || w.Start != stmt || w.End != stmt || w.NewChangeNumber != stmt {
		t.Errorf("window = %+v, want the statement row's day alone", w)
	}
	if w, err := r.ChangeWindow(ctx, stmt); err != nil || w.HasChanges {
		t.Errorf("past the statement row only 1099-B rows are new: window %+v, %v", w, err)
	}
}

// A lot of a document kind gold does not know, and a lot that states
// neither a tax year nor a disposal date, are dropped; the others load.
func TestRealizedLotsDropUnplacedAndUnknownKinds(t *testing.T) {
	f := newMergedFixture(t)
	if _, err := f.web.Exec(`
        INSERT INTO closed_lots
            (logical_doc_key, document_kind, lot_index, account_external_id, tax_year, security_name,
             quantity, disposed_date, proceeds, cost_basis, term, source_sha256, payload) VALUES
            ('5678|1706745600|1099-2023', 'form_1099b',    0, '5678', 2023, 'VTI', 1, '2023-03-01', 150, 100, 'LONG', 'sha-kept',     '{}'),
            ('5678|1706745600|1099-2023', 'form_1099b',    1, '5678', NULL, 'VTI', 1, NULL,         150, 100, 'LONG', 'sha-unplaced', '{}'),
            ('5678|1706745600|1099-2023', 'form_1099b',    2, '5678', NULL, 'VTI', 1, 'Various',    150, 100, 'LONG', 'sha-undated',  '{}'),
            ('5678|1706745600|1099-DIV',  'form_1099_div', 0, '5678', 2023, 'VTI', 1, '2023-03-01', 150, 100, 'LONG', 'sha-unknown',  '{}');
    `); err != nil {
		t.Fatalf("seed: %v", err)
	}
	lots := realizedLots(t, f.open(t))
	if len(lots) != 1 || *lots[0].SourceDocument != "sha-kept" || !lots[0].IsPrimary {
		t.Errorf("lots = %+v, want the placed lot of a known kind alone", lots)
	}
}

// Documents of one kind, account and tax year whose keys carry no
// readable date tie at date zero: the greater key wins, and a dated
// copy beats them all.
func TestRealizedLotsTieOnUnreadableDocDates(t *testing.T) {
	f := newMergedFixture(t)
	if _, err := f.web.Exec(`
        INSERT INTO closed_lots
            (logical_doc_key, document_kind, lot_index, account_external_id, tax_year, security_name,
             quantity, disposed_date, proceeds, cost_basis, term, source_sha256, payload) VALUES
            ('5678|undated|YES-A.PDF',  'year_end_summary', 0, '5678', 2023, 'VTI', 1, '2023-03-01', 150, 100, 'LONG', 'sha-a', '{}'),
            ('5678|undated|YES-B.PDF',  'year_end_summary', 0, '5678', 2023, 'VTI', 1, '2023-03-01', 150, 100, 'LONG', 'sha-b', '{}'),
            ('5678-no-date',            'year_end_summary', 0, '5678', 2023, 'VTI', 1, '2023-03-01', 150, 100, 'LONG', 'sha-c', '{}'),
            ('0042|undated|GLR-A.PDF',  'gain_loss_report', 0, '0042', 2023, 'VTI', 1, '2023-03-01', 150, 100, 'LONG', 'sha-d', '{}'),
            ('0042|1706745600|GLR.PDF', 'gain_loss_report', 0, '0042', 2023, 'VTI', 1, '2023-03-01', 150, 100, 'LONG', 'sha-e', '{}'),
            ('0042|undated|GLR-Z.PDF',  'gain_loss_report', 0, '0042', 2023, 'VTI', 1, '2023-03-01', 150, 100, 'LONG', 'sha-f', '{}');
    `); err != nil {
		t.Fatalf("seed: %v", err)
	}
	want := map[string]bool{
		"sha-a": false,
		"sha-b": true, // the greatest of the keys that tie at date zero
		"sha-c": false,
		"sha-d": false,
		"sha-e": true, // the one dated copy
		"sha-f": false,
	}
	lots := realizedLots(t, f.open(t))
	if len(lots) != len(want) {
		t.Fatalf("lots = %d, want %d", len(lots), len(want))
	}
	for _, r := range lots {
		if src := *r.SourceDocument; r.IsPrimary != want[src] {
			t.Errorf("%s: primary = %v, want %v", src, r.IsPrimary, want[src])
		}
	}
}
