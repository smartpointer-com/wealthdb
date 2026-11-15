package ubs

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// TestCanonicalGolden holds every record the adapter yields to a golden
// file per case: a PSN silver (testdata/canonical.jsonl), a web silver
// alone (webFixture), and both merged. The PSN silver joins the rows of
// the adapter tests: the roster and the instruments of every class, a
// holding with and without its instrument, signed cash balances, a book
// balance whose account states tiered interest, a rate,
// a forward, and trade confirmations and cash movements.
func TestCanonicalGolden(t *testing.T) {
	t.Run("psn", func(t *testing.T) {
		path, db := newFixtureSilver(t)
		silvertest.Exec(t, db, psnGoldenFixture)
		silvertest.Golden(t, openAdapter(t, path), "testdata/canonical.jsonl")
	})
	t.Run("web", func(t *testing.T) {
		silvertest.Golden(t, openSubsources(t, "", newWebFixtureSilver(t)), "testdata/canonical_web.jsonl")
	})
	t.Run("merged", func(t *testing.T) {
		path, db := newFixtureSilver(t)
		silvertest.Exec(t, db, psnGoldenFixture)
		silvertest.Golden(t, openSubsources(t, path, newWebFixtureSilver(t)), "testdata/canonical_merged.jsonl")
	})
}

// psnGoldenFixture is the PSN silver of the golden cases.
const psnGoldenFixture = `
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO cash_accounts(snapshot_at, relationship_id, account_external_id, payload) VALUES
            (1000, 'SFTPCHxx', 'CH00CASH', '{"AcctCcyIsoCd":"CHF","AcctTpDesc":"Private"}'),
            (1000, 'SFTPCHxx', 'CH00SAVE', '{"AcctCcyIsoCd":"CHF","AcctTpDesc":"Savings","BookBalAmt":2000.00,
             "BookBalCcyIsoCd":"CHF","CdtIntrstRate1":"0.25","CdtLmtAmt1":"25000.00","CdtIntrstRate2":"0.10",
             "CdtIntrstVldFrmDt":"2026-01-01","DbtIntrstRate1":"5.5","EndOfDurtn":"9999-12-31",
             "CalIntrstBasisDesc":"act/360"}');
        INSERT INTO safekeeping_accounts(snapshot_at, relationship_id, account_external_id, payload) VALUES
            (1000, 'SFTPCHxx', 'CH00SAFE', '{"InvstmtCcyIsoCd":"CHF","AcctTpDesc":"Custody"}');
        INSERT INTO portfolios(snapshot_at, relationship_id, portfolio_external_id, payload) VALUES
            (1000, 'SFTPCHxx', 'P1', '{"PrtflKey":"P1"}');
        INSERT INTO instruments(snapshot_at, relationship_id, isin, payload) VALUES
            (1000, 'SFTPCHxx', 'CH0000000001',
             '{"InstrCtgyCFI":"ESVTFR","InstrNm":"Acme AG","GacInstrRskCcyIsoCd":"CHF"}'),
            (1000, 'SFTPCHxx', 'XX0000000002',
             '{"InstrCtgyCFI":"CECIMX","InstrNm":"Euro Tracker","GacInstrRskCcyIsoCd":"EUR"}'),
            (1000, 'SFTPCHxx', 'XX0000000007',
             '{"InstrCtgyCFI":"CIOGMX","InstrNm":"Euro Fund","GacInstrRskCcyIsoCd":"EUR"}'),
            (1000, 'SFTPCHxx', 'XX0000000008',
             '{"InstrCtgyCFI":"CIOGMX","UacAsstClsCd":"0100","InstrNm":"MM Fund","GacInstrRskCcyIsoCd":"USD"}'),
            (1000, 'SFTPCHxx', 'XX0000000009',
             '{"InstrCtgyCFI":"CIMGMX","UacAsstClsCd":"0400","InstrNm":"PM Feeder","GacInstrRskCcyIsoCd":"CHF"}'),
            (1000, 'SFTPCHxx', 'XX0000000003',
             '{"InstrCtgyCFI":"","InstrNm":"No CFI","GacInstrRskCcyIsoCd":"USD"}'),
            (1000, 'SFTPCHxx', 'XX0000000004',
             '{"InstrCtgyCFI":"","UacAsstClsCd":"0400","InstrNm":"PE Fund LP","GacInstrRskCcyIsoCd":"USD"}'),
            (1000, 'SFTPCHxx', 'XX0000000005',
             '{"InstrCtgyCFI":"","UacAsstClsCd":"0600","InstrNm":"Gold Deposit","GacInstrRskCcyIsoCd":"USD"}'),
            (1000, 'SFTPCHxx', 'XX0000000006',
             '{"InstrCtgyCFI":"","UacAsstClsCd":"0700","InstrNm":"UAC Others","GacInstrRskCcyIsoCd":"USD"}');
        INSERT INTO holdings(snapshot_at, relationship_id, safekeeping_external_id, isin, payload) VALUES
            (1000, 'SFTPCHxx', 'CH00SAFE', 'CH0000000001',
             '{"fields":{"19A":[":HOLD//CHF45000,",":BOOK//CHF40000,"],"93B":[":AGGR//UNIT/100,",":AVAI//UNIT/100,"]}}');
        INSERT INTO holdings(snapshot_at, relationship_id, safekeeping_external_id, isin, payload) VALUES
            (1000, 'SFTPCHxx', 'CH00SAFE', 'CH9999999999',
             '{"fields":{}}');
        INSERT INTO cash_balances(snapshot_at, relationship_id, account_external_id, balance_kind, currency_iso, payload) VALUES
            (1000, 'SFTPCHxx', 'CH00CASH', 'closing', 'CHF', '{"amount":1234.56,"credit_debit":"C","currency_iso":"CHF"}'),
            (1000, 'SFTPCHxx', 'CH00CASH', 'available', 'CHF', '{"amount":50.00,"credit_debit":"D","currency_iso":"CHF"}');
        INSERT INTO fx_rates(snapshot_at, base_currency_iso, quote_currency_iso, payload) VALUES
            (1000, 'CHF', 'USD',
             '{"_base":{"BaseCcyIsoCd":"CHF","RateDt":"2026-05-17"},"CcyIsoCd":"USD","ForeignExchangeRatePeriodData":[{"MiddleRate":1.0987,"RatePeriodCd":"D","RatePeriodDt":"2026-05-17"}]}');
        INSERT INTO forward_contracts(snapshot_at, relationship_id, contract_external_id, payload) VALUES
            (1000, 'SFTPCHxx', 'FWD-001',
             '{"PrtflId":"P1","MrktValueAmt":1500.00,"MrktValueCcyIsoCd":"USD"}');
        INSERT INTO events(event_external_id, timestamp, relationship_id, account_external_id, kind, currency_iso, payload) VALUES
            ('TRD1', 1500, 'SFTPCHxx', 'CH00SAFE', 'trade_confirmation', NULL,
             '{"side":"B","isin":"CH0000000001","gross_amount":1500.00,"net_amount":1505.00,
               "net_currency":"CHF","price":15.00,"quantity":100,
               "cash_account_external_id":"CH00CASH"}'),
            ('TRD2', 1700, 'SFTPCHxx', 'CH00SAFE', 'trade_confirmation', NULL,
             '{"side":"S","isin":"CH0000000001","net_amount":2000.00,"net_currency":"CHF",
               "price":20.00,"quantity":100,"cash_account_external_id":"CH00CASH"}'),
            ('CASH1', 1100, 'SFTPCHxx', 'CH00CASH', 'cash_movement', 'CHF',
             '{"amount":50.0,"credit_debit":"C","narrative":"INTERETS T1","account":"CH00CASH","funds":"CHF"}'),
            ('CASH2', 1200, 'SFTPCHxx', 'CH00CASH', 'cash_movement', 'CHF',
             '{"amount":25.0,"credit_debit":"D","narrative":"FRAIS BANCAIRES","account":"CH00CASH","funds":"CHF"}'),
            ('CASH3', 1300, 'SFTPCHxx', 'CH00CASH', 'cash_movement', 'CHF',
             '{"amount":1000.0,"credit_debit":"C","narrative":"Salary","account":"CH00CASH","funds":"CHF"}');`
