package ubs

import (
	"context"
	"database/sql"
	_ "embed"
	"path/filepath"
	"testing"

	"github.com/ptu/wealthdb/internal/canonical"
	"github.com/ptu/wealthdb/internal/silver"
)

//go:embed testdata/silver_schema.sql
var silverSchemaSQL string

func newFixtureSilver(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ubs.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(silverSchemaSQL); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return path, db
}

func openAdapter(t *testing.T, path string) silver.Connection {
	t.Helper()
	conn, err := (&Adapter{}).Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestKindIsUBS(t *testing.T) {
	if got := (&Adapter{}).Kind(); got != "ubs" {
		t.Errorf("Kind() = %q, want %q", got, "ubs")
	}
}

func TestStatusEmpty(t *testing.T) {
	path, _ := newFixtureSilver(t)
	conn := openAdapter(t, path)
	s, err := conn.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := canonical.Status{
		OldestSnapshotAt: -1, LatestSnapshotAt: -1,
		OldestTransactionAt: -1, LatestTransactionAt: -1,
		LatestChangeNumber: -1,
	}
	if s != want {
		t.Errorf("Status = %+v, want %+v", s, want)
	}
}

func TestSnapshotsAccountsAndInstruments(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO cash_accounts(snapshot_at, relationship_id, account_external_id, payload) VALUES
            (1000, 'SFTPCHxx', 'CH00CASH', '{"AcctCcyIsoCd":"CHF","AcctTpDesc":"Private"}');
        INSERT INTO safekeeping_accounts(snapshot_at, relationship_id, account_external_id, payload) VALUES
            (1000, 'SFTPCHxx', 'CH00SAFE', '{"InvstmtCcyIsoCd":"CHF","AcctTpDesc":"Custody"}');
        INSERT INTO portfolios(snapshot_at, relationship_id, portfolio_external_id, payload) VALUES
            (1000, 'SFTPCHxx', 'P1', '{"PrtflKey":"P1"}');
        INSERT INTO instruments(snapshot_at, relationship_id, isin, payload) VALUES
            (1000, 'SFTPCHxx', 'CH0000000001',
             '{"InstrCtgyCFI":"ESVTFR","InstrNm":"Acme AG","GacInstrRskCcyIsoCd":"CHF"}'),
            (1000, 'SFTPCHxx', 'XX0000000002',
             '{"InstrCtgyCFI":"CECIMX","InstrNm":"Euro Fund","GacInstrRskCcyIsoCd":"EUR"}'),
            (1000, 'SFTPCHxx', 'XX0000000003',
             '{"InstrCtgyCFI":"","InstrNm":"No CFI","GacInstrRskCcyIsoCd":"USD"}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, err := conn.Snapshots(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	batch, _, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(batch.Accounts) != 3 {
		t.Fatalf("accounts = %d, want 3", len(batch.Accounts))
	}
	kinds := map[canonical.AccountKind]int{}
	for _, a := range batch.Accounts {
		kinds[a.AccountKind]++
		if a.RelationshipID == nil || *a.RelationshipID != "SFTPCHxx" {
			t.Errorf("account %q RelationshipID = %v, want SFTPCHxx", a.AccountExternalID, a.RelationshipID)
		}
	}
	for _, want := range []canonical.AccountKind{
		canonical.AccountKindCash, canonical.AccountKindSafekeeping, canonical.AccountKindPortfolio,
	} {
		if kinds[want] != 1 {
			t.Errorf("missing one account of kind %q", want)
		}
	}

	if len(batch.Instruments) != 3 {
		t.Fatalf("instruments = %d, want 3", len(batch.Instruments))
	}
	classes := map[string]canonical.AssetClass{}
	for _, i := range batch.Instruments {
		classes[i.InstrumentExternalID] = i.AssetClass
	}
	if classes["CH0000000001"] != canonical.AssetClassEquity {
		t.Errorf("ESVTFR → %q, want equity", classes["CH0000000001"])
	}
	if classes["XX0000000002"] != canonical.AssetClassFund {
		t.Errorf("CECIMX → %q, want fund", classes["XX0000000002"])
	}
	if classes["XX0000000003"] != canonical.AssetClassOther {
		t.Errorf("empty CFI → %q, want other", classes["XX0000000003"])
	}
}

func TestSnapshotsHoldingsJoinInstruments(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO instruments(snapshot_at, relationship_id, isin, payload) VALUES
            (1000, 'SFTPCHxx', 'CH0000000001',
             '{"InstrCtgyCFI":"ESVTFR","InstrNm":"Acme","GacInstrRskCcyIsoCd":"CHF"}');
        INSERT INTO holdings(snapshot_at, relationship_id, safekeeping_external_id, isin, payload) VALUES
            (1000, 'SFTPCHxx', 'CH00SAFE', 'CH0000000001', '{"fields":{}}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, err := conn.Snapshots(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	batch, _, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Positions) != 1 {
		t.Fatalf("positions = %d, want 1", len(batch.Positions))
	}
	p := batch.Positions[0]
	if p.AssetClass != canonical.AssetClassEquity {
		t.Errorf("AssetClass = %q, want equity (resolved via instrument)", p.AssetClass)
	}
	if p.Currency != "CHF" {
		t.Errorf("Currency = %q, want CHF (resolved via instrument)", p.Currency)
	}
	if p.AccountExternalID != "CH00SAFE" {
		t.Errorf("AccountExternalID = %q, want CH00SAFE", p.AccountExternalID)
	}
	if p.Quantity != nil || p.MarketValue != nil {
		t.Errorf("Quantity/MarketValue should be NULL (MT535 parsing deferred): %+v", p)
	}
}

func TestSnapshotsCashBalanceSigning(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO cash_balances(snapshot_at, relationship_id, account_external_id, balance_kind, currency_iso, payload) VALUES
            (1000, 'SFTPCHxx', 'CH00CASH', 'closing', 'CHF', '{"amount":1234.56,"credit_debit":"C","currency_iso":"CHF"}'),
            (1000, 'SFTPCHxx', 'CH00CASH', 'available', 'CHF', '{"amount":50.00,"credit_debit":"D","currency_iso":"CHF"}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Snapshots(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	if len(batch.CashBalances) != 2 {
		t.Fatalf("cash_balances = %d, want 2", len(batch.CashBalances))
	}
	byKind := map[canonical.BalanceKind]string{}
	for _, b := range batch.CashBalances {
		byKind[b.BalanceKind] = b.Amount.String()
	}
	if byKind[canonical.BalanceKindClosing] != "1234.56" {
		t.Errorf("closing amount = %q, want 1234.56", byKind[canonical.BalanceKindClosing])
	}
	if byKind[canonical.BalanceKindAvailable] != "-50" {
		t.Errorf("available amount = %q, want -50 (debit sign)", byKind[canonical.BalanceKindAvailable])
	}
}

func TestSnapshotsFxRate(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO fx_rates(snapshot_at, base_currency_iso, quote_currency_iso, payload) VALUES
            (1000, 'CHF', 'USD',
             '{"_base":{"BaseCcyIsoCd":"CHF","RateDt":"2026-05-17"},"CcyIsoCd":"USD","ForeignExchangeRatePeriodData":[{"MiddleRate":1.0987,"RatePeriodCd":"D","RatePeriodDt":"2026-05-17"}]}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Snapshots(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	if len(batch.FxRates) != 1 {
		t.Fatalf("fx_rates = %d, want 1", len(batch.FxRates))
	}
	r := batch.FxRates[0]
	if r.BaseCurrency != "CHF" || r.QuoteCurrency != "USD" || r.MidRate.String() != "1.0987" {
		t.Errorf("fx_rate = %+v, want CHF→USD@1.0987", r)
	}
}

func TestSnapshotsForwardContract(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO forward_contracts(snapshot_at, relationship_id, contract_external_id, payload) VALUES
            (1000, 'SFTPCHxx', 'FWD-001',
             '{"PrtflId":"P1","MrktValueAmt":1500.00,"MrktValueCcyIsoCd":"USD"}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Snapshots(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	if len(batch.Positions) != 1 {
		t.Fatalf("positions = %d, want 1", len(batch.Positions))
	}
	p := batch.Positions[0]
	if p.AssetClass != canonical.AssetClassFxForward {
		t.Errorf("AssetClass = %q, want fx_forward", p.AssetClass)
	}
	if p.AccountExternalID != "P1" {
		t.Errorf("AccountExternalID = %q, want P1 (portfolio)", p.AccountExternalID)
	}
	if p.MarketValue == nil || p.MarketValue.String() != "1500" {
		t.Errorf("MarketValue = %v, want 1500", p.MarketValue)
	}
}

func TestKindMapping(t *testing.T) {
	cases := []struct {
		silverKind, narrative, creditDebit string
		want                               canonical.TxKind
	}{
		{"trade_confirmation", "", "", canonical.TxKindBuy},
		{"corporate_action_confirmation", "", "", canonical.TxKindCorporateAction},
		{"fx_confirmation", "", "", canonical.TxKindFxSpot},
		{"charges_advice", "", "", canonical.TxKindFee},
		{"cash_movement", "Salary deposit", "C", canonical.TxKindDeposit},
		{"cash_movement", "Wire", "D", canonical.TxKindWithdrawal},
		{"cash_movement", "INTERETS Q1", "C", canonical.TxKindInterest},
		{"cash_movement", "ZINSEN BANK", "C", canonical.TxKindInterest},
		{"cash_movement", "FRAIS BANCAIRES", "D", canonical.TxKindFee},
		{"cash_movement", "IMPOT ANTICIPE", "D", canonical.TxKindTax},
		{"cash_movement", "DIVIDENDE ACME", "C", canonical.TxKindDividend},
		{"securities_movement", "", "C", canonical.TxKindTransferIn},
		{"securities_movement", "", "D", canonical.TxKindTransferOut},
		{"debit_credit_confirmation", "", "DBIT", canonical.TxKindWithdrawal},
		{"debit_credit_confirmation", "", "CRDT", canonical.TxKindDeposit},
		{"unknown-bank-kind", "", "", canonical.TxKindOther},
	}
	for _, c := range cases {
		got := kindFor(c.silverKind, c.narrative, c.creditDebit)
		if got != c.want {
			t.Errorf("kindFor(%q, %q, %q) = %q, want %q",
				c.silverKind, c.narrative, c.creditDebit, got, c.want)
		}
	}
}

func TestTransactionsTradeConfirmation(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO events(event_external_id, timestamp, relationship_id, account_external_id, kind, currency_iso, payload) VALUES
            ('TRD1', 1500, 'SFTPCHxx', 'CH00SAFE', 'trade_confirmation', NULL,
             '{"side":"B","isin":"CH0000000001","gross_amount":1500.00,"net_amount":1505.00,
               "net_currency":"CHF","price":15.00,"quantity":100,
               "cash_account_external_id":"CH00CASH"}'),
            ('TRD2', 1700, 'SFTPCHxx', 'CH00SAFE', 'trade_confirmation', NULL,
             '{"side":"S","isin":"CH0000000001","net_amount":2000.00,"net_currency":"CHF",
               "price":20.00,"quantity":100,"cash_account_external_id":"CH00CASH"}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Transactions(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	if len(batch.Transactions) != 2 {
		t.Fatalf("transactions = %d, want 2", len(batch.Transactions))
	}
	if batch.Transactions[0].Kind != canonical.TxKindBuy {
		t.Errorf("TRD1 kind = %q, want buy", batch.Transactions[0].Kind)
	}
	if batch.Transactions[1].Kind != canonical.TxKindSell {
		t.Errorf("TRD2 kind = %q, want sell", batch.Transactions[1].Kind)
	}
	if batch.Transactions[0].InstrumentExternalID == nil || *batch.Transactions[0].InstrumentExternalID != "CH0000000001" {
		t.Errorf("TRD1 isin = %v", batch.Transactions[0].InstrumentExternalID)
	}
	if batch.Transactions[0].AccountExternalID != "CH00CASH" {
		t.Errorf("TRD1 account = %q, want CH00CASH (from cash_account_external_id)", batch.Transactions[0].AccountExternalID)
	}
	if batch.Transactions[0].Currency != "CHF" {
		t.Errorf("TRD1 currency = %q, want CHF", batch.Transactions[0].Currency)
	}
}

func TestTransactionsCashMovementNarrative(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO events(event_external_id, timestamp, relationship_id, account_external_id, kind, currency_iso, payload) VALUES
            ('CASH1', 1100, 'SFTPCHxx', 'CH00CASH', 'cash_movement', 'CHF',
             '{"amount":50.0,"credit_debit":"C","narrative":"INTERETS T1","account":"CH00CASH","funds":"CHF"}'),
            ('CASH2', 1200, 'SFTPCHxx', 'CH00CASH', 'cash_movement', 'CHF',
             '{"amount":25.0,"credit_debit":"D","narrative":"FRAIS BANCAIRES","account":"CH00CASH","funds":"CHF"}'),
            ('CASH3', 1300, 'SFTPCHxx', 'CH00CASH', 'cash_movement', 'CHF',
             '{"amount":1000.0,"credit_debit":"C","narrative":"Salary","account":"CH00CASH","funds":"CHF"}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Transactions(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	byID := map[string]canonical.TxKind{}
	for _, tx := range batch.Transactions {
		byID[tx.TransactionExternalID] = tx.Kind
	}
	if byID["CASH1"] != canonical.TxKindInterest {
		t.Errorf("CASH1 kind = %q, want interest", byID["CASH1"])
	}
	if byID["CASH2"] != canonical.TxKindFee {
		t.Errorf("CASH2 kind = %q, want fee", byID["CASH2"])
	}
	if byID["CASH3"] != canonical.TxKindDeposit {
		t.Errorf("CASH3 kind = %q, want deposit (no narrative prefix → sign-driven)", byID["CASH3"])
	}
}
