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

// TestSnapshotsAccountCategoryMapping covers AcctTpDesc →
// AccountCategory for cash accounts (passthrough) and the
// AcctTpDesc + " / " + AcctSubTypeDesc concatenation for
// safekeeping accounts (when the sub-type is present).
func TestSnapshotsAccountCategoryMapping(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO cash_accounts(snapshot_at, relationship_id, account_external_id, payload) VALUES
            (1000, 'SFTPCHxx', 'CH00CASH', '{"AcctCcyIsoCd":"CHF","AcctTpDesc":"Private"}');
        INSERT INTO safekeeping_accounts(snapshot_at, relationship_id, account_external_id, payload) VALUES
            (1000, 'SFTPCHxx', 'CH00SAFE1', '{"InvstmtCcyIsoCd":"CHF","AcctTpDesc":"Custody","AcctSubTypeDesc":"Cash-Custody"}'),
            (1000, 'SFTPCHxx', 'CH00SAFE2', '{"InvstmtCcyIsoCd":"CHF","AcctTpDesc":"Custody"}');
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

	byID := map[string]*string{}
	for i := range batch.Accounts {
		byID[batch.Accounts[i].AccountExternalID] = batch.Accounts[i].AccountCategory
	}
	if got := byID["CH00CASH"]; got == nil || *got != "Private" {
		t.Errorf("CH00CASH category = %v, want 'Private'", got)
	}
	if got := byID["CH00SAFE1"]; got == nil || *got != "Custody / Cash-Custody" {
		t.Errorf("CH00SAFE1 category = %v, want 'Custody / Cash-Custody'", got)
	}
	if got := byID["CH00SAFE2"]; got == nil || *got != "Custody" {
		t.Errorf("CH00SAFE2 category = %v, want 'Custody'", got)
	}
}

// TestSnapshotsHoldingFallbackCurrency exercises the
// position-currency fallback: when instrument metadata carries no
// currency (GacInstrRskCcyIsoCd absent, common for funds), the
// position's currency comes from the chosen 19A:HOLD entry rather
// than defaulting to the "XXX" sentinel.
func TestSnapshotsHoldingFallbackCurrency(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO instruments(snapshot_at, relationship_id, isin, payload) VALUES
            (1000, 'SFTPCHxx', 'CH9999999999',
             '{"InstrCtgyCFI":"CEOI","InstrNm":"Fund w/o currency","GacInstrRskCcyIsoCd":""}');
        INSERT INTO holdings(snapshot_at, relationship_id, safekeeping_external_id, isin, payload) VALUES
            (1000, 'SFTPCHxx', 'CH00SAFE', 'CH9999999999',
             '{"fields":{"19A":[":HOLD//USD1100000,",":BOOK//USD400000,"],"93B":[":AGGR//UNIT/1200,"]}}');
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
	if p.Currency != "USD" {
		t.Errorf("Currency = %q, want USD (rescued from 19A:HOLD)", p.Currency)
	}
	if p.MarketValue == nil || p.MarketValue.String() != "1100000" {
		t.Errorf("MarketValue = %v, want 1100000", p.MarketValue)
	}
}

// TestSnapshotsHoldingsCanonicalSafekeepingID covers the
// safekeeping ID normalisation: holdings use the MT535-flavoured
// format ("023000xxxxxxxxS1") while safekeeping_accounts use the
// canonical ("0230-xxxxxxxx.S1"). The adapter must translate the
// holdings ID so position.account_external_id matches the
// corresponding accounts row.
func TestSnapshotsHoldingsCanonicalSafekeepingID(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO safekeeping_accounts(snapshot_at, relationship_id, account_external_id, payload) VALUES
            (1000, 'SFTPCHxx', '0230-xxxxxxxx.S1', '{"InvstmtCcyIsoCd":"CHF","AcctTpDesc":"Custody"}');
        INSERT INTO instruments(snapshot_at, relationship_id, isin, payload) VALUES
            (1000, 'SFTPCHxx', 'CH0000000001',
             '{"InstrCtgyCFI":"ESVTFR","InstrNm":"Acme","GacInstrRskCcyIsoCd":"CHF"}');
        INSERT INTO holdings(snapshot_at, relationship_id, safekeeping_external_id, isin, payload) VALUES
            (1000, 'SFTPCHxx', '023000xxxxxxxxS1', 'CH0000000001',
             '{"fields":{"19A":[":HOLD//CHF1000,"],"93B":[":AGGR//UNIT/10,"]}}');
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
	if got := batch.Positions[0].AccountExternalID; got != "0230-xxxxxxxx.S1" {
		t.Errorf("position account_external_id = %q, want canonical safekeeping form '0230-xxxxxxxx.S1'", got)
	}
}

// TestSnapshotsCashBalancesCanonicalID covers the cash-side
// counterpart of the safekeeping normalisation. cash_balances
// references accounts by AcctId ("023000xxxxxxxx010000G") which
// also lives in the cash_accounts payload; the
// account_external_id on cash_accounts is the IBAN
// ("CH0000230230xxxxxxxx"). The adapter must translate the
// balance's AcctId to the IBAN so the CashBalanceChange's
// AccountExternalID joins to the AccountChange.
func TestSnapshotsCashBalancesCanonicalID(t *testing.T) {
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO cash_accounts(snapshot_at, relationship_id, account_external_id, payload) VALUES
            (1000, 'SFTPCHxx', 'CH0000230230xxxxxxxx',
             '{"AcctCcyIsoCd":"CHF","AcctTpDesc":"Private","AcctId":"023000xxxxxxxx010000G"}');
        INSERT INTO cash_balances(snapshot_at, relationship_id, account_external_id, balance_kind, currency_iso, payload) VALUES
            (1000, 'SFTPCHxx', '023000xxxxxxxx010000G', 'closing', 'CHF',
             '{"amount":1500.00,"credit_debit":"C","currency_iso":"CHF"}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, _ := conn.ChangeWindow(context.Background(), -1)
	stream, _ := conn.Snapshots(context.Background(), w)
	defer stream.Close()
	batch, _, _ := stream.Next(context.Background())

	if len(batch.CashBalances) != 1 {
		t.Fatalf("cash_balances = %d, want 1", len(batch.CashBalances))
	}
	if got := batch.CashBalances[0].AccountExternalID; got != "CH0000230230xxxxxxxx" {
		t.Errorf("cash balance account_external_id = %q, want canonical IBAN 'CH0000230230xxxxxxxx'", got)
	}
}

// TestTrailingSuffix exercises the suffix extractor used by the
// safekeeping ID normaliser.
func TestTrailingSuffix(t *testing.T) {
	cases := map[string]string{
		"023000xxxxxxxxS1": "S1",
		"023000xxxxxxxxT1": "T1",
		"023000xxxxxxxxS10": "S10",
		"ABC":              "", // no trailing digits
		"123":              "", // no leading letter
		"":                 "",
		"S":                "", // no trailing digits
	}
	for in, want := range cases {
		if got := trailingSuffix(in); got != want {
			t.Errorf("trailingSuffix(%q) = %q, want %q", in, got, want)
		}
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
            (1000, 'SFTPCHxx', 'CH00SAFE', 'CH0000000001',
             '{"fields":{"19A":[":HOLD//CHF45000,",":BOOK//CHF40000,"],"93B":[":AGGR//UNIT/100,",":AVAI//UNIT/100,"]}}');
        INSERT INTO holdings(snapshot_at, relationship_id, safekeeping_external_id, isin, payload) VALUES
            (1000, 'SFTPCHxx', 'CH00SAFE', 'CH9999999999',
             '{"fields":{}}');
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
	if len(batch.Positions) != 2 {
		t.Fatalf("positions = %d, want 2", len(batch.Positions))
	}

	// Find each position by ISIN.
	var withMT, withoutMT *canonical.PositionChange
	for i := range batch.Positions {
		p := &batch.Positions[i]
		if p.PositionKey == "CH0000000001" {
			withMT = p
		} else if p.PositionKey == "CH9999999999" {
			withoutMT = p
		}
	}
	if withMT == nil || withoutMT == nil {
		t.Fatalf("positions misnamed: %+v", batch.Positions)
	}

	// Parsed-from-MT535 position should have quantity + market_value.
	if withMT.AssetClass != canonical.AssetClassEquity {
		t.Errorf("AssetClass = %q, want equity (resolved via instrument)", withMT.AssetClass)
	}
	if withMT.Currency != "CHF" {
		t.Errorf("Currency = %q, want CHF", withMT.Currency)
	}
	if withMT.Quantity == nil || withMT.Quantity.String() != "100" {
		t.Errorf("Quantity = %v, want 100 (parsed from 93B:AGGR//UNIT/100,)", withMT.Quantity)
	}
	if withMT.MarketValue == nil || withMT.MarketValue.String() != "45000" {
		t.Errorf("MarketValue = %v, want 45000 (parsed from 19A:HOLD//CHF45000,)", withMT.MarketValue)
	}

	// Empty-fields holding stays NULL — the parser gracefully
	// degrades when there are no SWIFT subfields to decode.
	if withoutMT.Quantity != nil || withoutMT.MarketValue != nil {
		t.Errorf("empty-fields holding should leave qty/mv NULL: %+v", withoutMT)
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
