package ubs

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/yield"
)

// A dividend line names the security it was paid on by its confirmation,
// its valor or its ticker, and reads the gross where its confirmation
// states it, the tax withheld beside it. Every id, name and figure below
// is invented.
const (
	divIBAN      = "CH0000000000000000DV1"
	divAcctID    = "00000000000000DV00001"
	divPortfolio = "0000000000000009"
	divOther     = "0000000000000008"
	divSafe      = "00000000000000S9"
	divSafeOther = "00000000000000S8"
	divISIN      = "XX0000000011"
	divISIN2     = "XX0000000012"
	divValor     = "1234567"
	divLine      = "mt940:" + divIBAN + ":REF0000011"
	divConf      = "mt566:0000KE0000011"
	divDay       = int64(600 * 86400)
	divNarrative = "Reg.shs Example AG (EXMP)"
)

// swiftDate spells a day as a SWIFT `:98A:` date.
func swiftDate(day int64) string { return time.Unix(day, 0).UTC().Format("20060102") }

// dividendConfirmation is an MT566 cash dividend in the shape the
// collector stores. Each figure is SWIFT's `CCYfigure,` spelling; an
// empty gross or tax leaves that qualifier out. Each further field given
// joins the block MT566 places it in: a balance (93B) the underlying
// securities' account, a rate the cash option's confirmation block,
// before its cash movement.
func dividendConfirmation(seme, cashAcct, posted, gross, tax string, day int64, further ...[2]string) string {
	var balances, rates [][2]string
	for _, f := range further {
		if f[0] == "93B" {
			balances = append(balances, f)
		} else {
			rates = append(rates, f)
		}
	}
	fields := [][2]string{
		{"16R", "GENL"}, {"20C", ":SEME//" + seme}, {"22F", ":CAEV//DVCA"}, {"16S", "GENL"},
		{"16R", "USECU"}, {"35B", "ISIN " + divISIN + "\nEXAMPLE AG REG"},
		{"16R", "ACCTINFO"}, {"97A", ":SAFE//" + divSafe},
	}
	fields = append(fields, balances...)
	fields = append(fields, [][2]string{
		{"16S", "ACCTINFO"}, {"16S", "USECU"},
		{"16R", "CACONF"}, {"22H", ":CAOP//CASH"},
	}...)
	fields = append(fields, rates...)
	fields = append(fields, [][2]string{
		{"16R", "CASHMOVE"}, {"22H", ":CRDB//CRED"}, {"97A", ":CASH//" + cashAcct},
		{"19B", ":PSTA//" + posted},
	}...)
	if gross != "" {
		fields = append(fields, [2]string{"19B", ":GRSS//" + gross})
	}
	if tax != "" {
		fields = append(fields, [2]string{"19B", ":TAXR//" + tax})
	}
	fields = append(fields, [2]string{"98A", ":PAYD//" + swiftDate(day)},
		[2]string{"16S", "CASHMOVE"}, [2]string{"16S", "CACONF"})
	b, _ := json.Marshal(map[string]any{
		"caev": "DVCA", "isin": divISIN, "safekeeping": divSafe, "seme": seme, "fields": fields,
	})
	return string(b)
}

// seedMT940Line inserts one MT940 line on the account, in the currency
// and under the credit/debit mark given. Its bank reference is the last
// part of its id.
func seedMT940Line(t *testing.T, db *sql.DB, eventID, account, ccy, mark, amount, txnType, narrative, customerRef string, day int64) {
	t.Helper()
	if _, err := db.Exec(`
        INSERT INTO events (event_external_id, timestamp, relationship_id,
            account_external_id, kind, currency_iso, payload)
        VALUES (?, ?, 'R1', ?, 'cash_movement', ?,
                json_object('amount', ?, 'credit_debit', ?, 'narrative', ?,
                            'account', ?, 'funds', ?, 'txn_type', ?,
                            'customer_ref', ?, 'bank_ref', ?))`,
		eventID, day, account, ccy, amount, mark, narrative, account, ccy, txnType, customerRef,
		eventID[strings.LastIndexByte(eventID, ':')+1:]); err != nil {
		t.Fatalf("seed %s line %s: %v", mark, eventID, err)
	}
}

// seedDividendLine inserts one MT940 dividend credit (NDIV) on the
// dividends' cash account.
func seedDividendLine(t *testing.T, db *sql.DB, eventID, amount, ccy, narrative, customerRef string, day int64) {
	t.Helper()
	seedMT940Line(t, db, eventID, divIBAN, ccy, "C", amount, "NDIV", narrative, customerRef, day)
}

// newDividendSilver is a feed silver holding the cash account the
// dividends below are credited to, in its portfolio.
func newDividendSilver(t *testing.T) *sql.DB {
	t.Helper()
	_, db := newFixtureSilver(t)
	seedMirrorCashAccount(t, db, divIBAN, divAcctID, divPortfolio, "CHF")
	return db
}

// wantWithholding checks a withholding row against the dividend it was
// withheld from: the same account, day, currency and instrument, and the
// amount given.
func wantWithholding(t *testing.T, div, tax canonical.TransactionChange, amount string) {
	t.Helper()
	if tax.Kind != canonical.TxKindTax || tax.NetAmount == nil || tax.NetAmount.String() != amount {
		t.Errorf("withholding = %s %v, want tax %s", tax.Kind, tax.NetAmount, amount)
	}
	if tax.AccountExternalID != div.AccountExternalID || tax.OccurredAt != div.OccurredAt ||
		tax.Currency != div.Currency || derefText(tax.InstrumentExternalID) != derefText(div.InstrumentExternalID) {
		t.Errorf("withholding on %s %d %s %v, want the dividend's %s %d %s %v",
			tax.AccountExternalID, tax.OccurredAt, tax.Currency, derefText(tax.InstrumentExternalID),
			div.AccountExternalID, div.OccurredAt, div.Currency, derefText(div.InstrumentExternalID))
	}
	if got := payloadField(t, tax, "withheld_from"); got != div.TransactionExternalID {
		t.Errorf("withholding names %q as its dividend, want %q", got, div.TransactionExternalID)
	}
}

// derefDecimal spells an optional amount, "" for none.
func derefDecimal(d *canonical.Decimal) string {
	if d == nil {
		return ""
	}
	return d.String()
}

// TestADividendLineReadsTheGrossItsConfirmationStates: the confirmation
// pairs with the line on the cash account, the payment day, the amount
// and the currency. The line takes its ISIN and reads the gross; the tax
// row is the credited amount less the gross, so the bank's 0.05 between
// GRSS − TAXR and the amount it credited moves no cash.
func TestADividendLineReadsTheGrossItsConfirmationStates(t *testing.T) {
	db := newDividendSilver(t)
	seedDividendLine(t, db, divLine, "849.95", "CHF", divNarrative, "", divDay)
	seedCorporateAction(t, db, divConf, dividendConfirmation("0000KE0000011", divAcctID,
		"CHF849,95", "CHF1000,", "CHF150,", divDay), divDay+86400)
	got := psnRowsWith(t, db, psnHints{})

	div := got[divLine]
	if derefText(div.InstrumentExternalID) != divISIN {
		t.Errorf("instrument = %v, want the confirmation's %s", derefText(div.InstrumentExternalID), divISIN)
	}
	if div.NetAmount.String() != "1000" || div.GrossAmount == nil || div.GrossAmount.String() != "1000" {
		t.Errorf("dividend = %v gross %v, want 1000 both", div.NetAmount, div.GrossAmount)
	}
	if got := payloadField(t, div, "corporate_action"); got != divConf {
		t.Errorf("line payload names %q as its confirmation, want %q", got, divConf)
	}
	var p cashMovementPayload
	if err := json.Unmarshal(div.Payload, &p); err != nil || p.Amount == nil || p.Amount.String() != "849.95" {
		t.Errorf("line payload amount = %v, want the 849.95 credited", p.Amount)
	}
	tax, ok := got[divLine+withholdingIDSuffix]
	if !ok {
		t.Fatal("no withholding row was booked beside the line")
	}
	wantWithholding(t, div, tax, "-150.05")
	if tax.Description == nil || *tax.Description != "Withholding tax; EXAMPLE AG REG" {
		t.Errorf("withholding description = %v, want it to name the withholding and the security", tax.Description)
	}
	if sum := div.NetAmount.Add(*tax.NetAmount); sum.String() != "849.95" {
		t.Errorf("dividend + withholding = %s, want the 849.95 credited", sum)
	}
	if _, ok := got[divConf+":cash"]; ok {
		t.Error("the confirmation also booked a cash leg beside its paired line")
	}
	if n := countOnAccountDay(got, divIBAN, divDay); n != 2 {
		t.Errorf("%d rows on the account that day, want the line and its withholding", n)
	}
}

// countOnAccountDay is how many emitted rows sit on the account on the day.
func countOnAccountDay(got map[string]canonical.TransactionChange, account string, day int64) int {
	n := 0
	for _, tx := range got {
		if tx.AccountExternalID == account && tx.OccurredAt == day {
			n++
		}
	}
	return n
}

// TestTheGrossFromWhatAConfirmationStates covers the other figures a
// confirmation can state: a tax and no gross reads the credited amount
// plus the tax; neither reads the net; a gross and a tax in another
// currency than the line's, with no rate to convert them, read the net
// too. A tax of zero books no tax
// row, and figures that cannot be withholding keep the net. Each line
// still names the security.
func TestTheGrossFromWhatAConfirmationStates(t *testing.T) {
	for _, tc := range []struct {
		name, gross, tax          string
		want, wantGross, withheld string
	}{
		{"tax and no gross", "", "CHF150,", "1000", "1000", "-150"},
		{"neither", "", "", "850", "", ""},
		{"another currency", "USD1100,", "USD165,", "850", "", ""},
		{"a tax of zero", "CHF850,", "CHF0,", "850", "850", ""},
		{"a tax of zero beside another gross", "CHF851,", "CHF0,", "850", "", ""},
		{"a gross below the credit", "CHF800,", "", "850", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newDividendSilver(t)
			seedDividendLine(t, db, divLine, "850", "CHF", divNarrative, "", divDay)
			seedCorporateAction(t, db, divConf, dividendConfirmation("0000KE0000011", divAcctID,
				"CHF850,", tc.gross, tc.tax, divDay), divDay)
			got := psnRowsWith(t, db, psnHints{})
			div := got[divLine]
			if derefText(div.InstrumentExternalID) != divISIN {
				t.Errorf("instrument = %v, want %s", derefText(div.InstrumentExternalID), divISIN)
			}
			if div.NetAmount.String() != tc.want {
				t.Errorf("dividend = %v, want %s", div.NetAmount, tc.want)
			}
			if gross := derefDecimal(div.GrossAmount); gross != tc.wantGross {
				t.Errorf("gross = %q, want %q", gross, tc.wantGross)
			}
			tax, ok := got[divLine+withholdingIDSuffix]
			if ok != (tc.withheld != "") {
				t.Fatalf("withholding row booked = %v, want %v", ok, tc.withheld != "")
			}
			if ok {
				wantWithholding(t, div, tax, tc.withheld)
			}
		})
	}
}

// TestAConfirmationPairsOnlyWhatItStates: a confirmation in another
// currency, on another day or for another amount is not the line's, and
// two lines answering one confirmation pair neither. None of those lines
// takes the confirmation's security or its gross.
func TestAConfirmationPairsOnlyWhatItStates(t *testing.T) {
	for name, seed := range map[string]func(*testing.T, *sql.DB){
		"another currency": func(t *testing.T, db *sql.DB) {
			seedDividendLine(t, db, divLine, "850", "CHF", divNarrative, "", divDay)
			seedCorporateAction(t, db, divConf, dividendConfirmation("0000KE0000011", divAcctID,
				"USD850,", "USD1000,", "USD150,", divDay), divDay)
		},
		"another day": func(t *testing.T, db *sql.DB) {
			seedDividendLine(t, db, divLine, "850", "CHF", divNarrative, "", divDay)
			seedCorporateAction(t, db, divConf, dividendConfirmation("0000KE0000011", divAcctID,
				"CHF850,", "CHF1000,", "CHF150,", divDay+86400), divDay)
		},
		"another amount": func(t *testing.T, db *sql.DB) {
			seedDividendLine(t, db, divLine, "850", "CHF", divNarrative, "", divDay)
			seedCorporateAction(t, db, divConf, dividendConfirmation("0000KE0000011", divAcctID,
				"CHF851,", "CHF1000,", "CHF150,", divDay), divDay)
		},
		"two lines": func(t *testing.T, db *sql.DB) {
			seedDividendLine(t, db, divLine, "850", "CHF", divNarrative, "", divDay)
			seedDividendLine(t, db, divLine+"#1", "850", "CHF", "Shs Other Example AG (EXMQ)", "", divDay)
			seedCorporateAction(t, db, divConf, dividendConfirmation("0000KE0000011", divAcctID,
				"CHF850,", "CHF1000,", "CHF150,", divDay), divDay)
		},
	} {
		t.Run(name, func(t *testing.T) {
			db := newDividendSilver(t)
			seed(t, db)
			got := psnRowsWith(t, db, psnHints{})
			for _, id := range []string{divLine, divLine + "#1"} {
				div, ok := got[id]
				if !ok {
					continue
				}
				if div.InstrumentExternalID != nil || div.NetAmount.String() != "850" {
					t.Errorf("%s = %v on %v, want 850 on no instrument", id, div.NetAmount, derefText(div.InstrumentExternalID))
				}
				if _, ok := got[id+withholdingIDSuffix]; ok {
					t.Errorf("%s: a withholding row was booked", id)
				}
			}
		})
	}
}

// TestAConfirmationSentTwiceIsOneStatement: two confirmations stating
// the same security and figures pair as one; two that differ pair
// neither.
func TestAConfirmationSentTwiceIsOneStatement(t *testing.T) {
	for _, tc := range []struct {
		name, secondGross string
		paired            bool
	}{
		{"the same figures", "CHF1000,", true},
		{"other figures", "CHF1001,", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newDividendSilver(t)
			seedDividendLine(t, db, divLine, "850", "CHF", divNarrative, "", divDay)
			seedCorporateAction(t, db, divConf, dividendConfirmation("0000KE0000011", divAcctID,
				"CHF850,", "CHF1000,", "CHF150,", divDay), divDay)
			seedCorporateAction(t, db, divConf+"2", dividendConfirmation("0000KE00000112", divAcctID,
				"CHF850,", tc.secondGross, "CHF150,", divDay), divDay)
			div := psnRowsWith(t, db, psnHints{})[divLine]
			if paired := div.InstrumentExternalID != nil; paired != tc.paired {
				t.Errorf("paired = %v, want %v", paired, tc.paired)
			}
			if tc.paired && payloadField(t, div, "corporate_action") != divConf {
				t.Errorf("line names %q, want the first confirmation by id", payloadField(t, div, "corporate_action"))
			}
		})
	}
}

// TestADividendLineLinksByTheValorItStates: with no confirmation, the
// `VN <valor>` the line's customer reference states resolves through the
// valor index. A valor nothing resolves stays the hint.
func TestADividendLineLinksByTheValorItStates(t *testing.T) {
	db := newDividendSilver(t)
	seedValor(t, db, divISIN, divValor)
	seedDividendLine(t, db, divLine, "850", "CHF", "K00?", "VN"+divValor, divDay)
	seedDividendLine(t, db, divLine+"#1", "850", "CHF", "K00?", "VN  7654321", divDay+86400)
	got := psnRowsWith(t, db, psnHints{})
	if div := got[divLine]; derefText(div.InstrumentExternalID) != divISIN || div.InstrumentHint != "" {
		t.Errorf("instrument = %v hint %q, want %s by the valor", derefText(div.InstrumentExternalID), div.InstrumentHint, divISIN)
	}
	if div := got[divLine+"#1"]; div.InstrumentExternalID != nil || div.InstrumentHint != "7654321" {
		t.Errorf("instrument = %v hint %q, want none and the valor as the hint",
			derefText(div.InstrumentExternalID), div.InstrumentHint)
	}
}

// custodyPosition is one custody position a statement of assets
// prints: its portfolio, ISIN, valor and description.
type custodyPosition struct{ portfolio, isin, valor, description string }

// newDividendWeb is a web silver with the transaction tables and printed
// statements of assets holding the positions given, in CHF.
func newDividendWeb(t *testing.T, positions ...custodyPosition) *webReader {
	t.Helper()
	r := newWebTxFixture(t)
	if _, err := r.db.Exec(`
CREATE TABLE historical_position_snapshots (
    as_of_date INTEGER, portfolio_external_id TEXT, account_external_id TEXT,
    instrument_isin TEXT, currency_iso TEXT, units REAL, market_value REAL,
    market_value_currency TEXT, cost_price REAL, market_price REAL,
    accrued_interest REAL, exchange_rate_to_base REAL, description TEXT,
    sector TEXT, source_doc_token TEXT, payload TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, p := range positions {
		if _, err := r.db.Exec(`
            INSERT INTO historical_position_snapshots
                (as_of_date, portfolio_external_id, account_external_id, instrument_isin,
                 currency_iso, units, market_value_currency, description, source_doc_token, payload)
            VALUES (1000, ?, '', ?, 'CHF', 100, 'CHF', ?, 'tok',
                    json_object('valor', ?, 'isin', ?, 'headline', ?))`,
			p.portfolio, p.isin, p.description, p.valor, p.isin,
			"100 "+p.description+" CHF 80.000000 85.00 6.25% 8 500 1.00"); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// TestADividendLineLinksByItsTicker: the ticker closing the narrative
// names the one custody position of the line's portfolio whose
// description closes with it. A position of another portfolio does not
// answer. The merged stream carries the web pass's roads to the feed.
func TestADividendLineLinksByItsTicker(t *testing.T) {
	db := newDividendSilver(t)
	seedDividendLine(t, db, divLine, "850", "CHF", divNarrative, "", divDay)
	web := newDividendWeb(t,
		custodyPosition{divPortfolio, divISIN, divValor, "Reg.shs Example AG (EXMP)"},
		custodyPosition{divPortfolio, divISIN2, "7777777", "Shs Other Example AG (EXMQ)"},
		custodyPosition{divOther, divISIN2, "7777777", "Reg.shs Example AG (EXMP)"})
	div := mergedText(t, web, db)[divLine]
	if derefText(div.InstrumentExternalID) != divISIN {
		t.Errorf("instrument = %v, want %s by the ticker", derefText(div.InstrumentExternalID), divISIN)
	}
}

// TestATickerTwoPositionsPrintLinksNothing: two securities of the
// portfolio close their description with the ticker, so it names
// neither.
func TestATickerTwoPositionsPrintLinksNothing(t *testing.T) {
	db := newDividendSilver(t)
	seedDividendLine(t, db, divLine, "850", "CHF", divNarrative, "", divDay)
	web := newDividendWeb(t,
		custodyPosition{divPortfolio, divISIN, divValor, "Reg.shs Example AG (EXMP)"},
		custodyPosition{divPortfolio, divISIN2, "7777777", "Reg.shs Example AG (EXMP)"})
	div := mergedText(t, web, db)[divLine]
	if div.InstrumentExternalID != nil {
		t.Errorf("instrument = %v, want none: two positions print the ticker", *div.InstrumentExternalID)
	}
}

// seedCashPosition makes a statement of assets print a cash position of
// the account under the portfolio: a row with no ISIN.
func seedCashPosition(t *testing.T, r *webReader, portfolio, account string) {
	t.Helper()
	if _, err := r.db.Exec(`
        INSERT INTO historical_position_snapshots
            (as_of_date, portfolio_external_id, account_external_id, instrument_isin,
             currency_iso, market_value_currency, description, source_doc_token, payload)
        VALUES (1000, ?, ?, NULL, 'CHF', 'CHF', 'Current account', 'tok', '{}')`,
		portfolio, account); err != nil {
		t.Fatal(err)
	}
}

// TestAPrintedCashPositionPlacesItsAccount: the feed places the line's
// account in no portfolio, but a statement of assets prints the account
// as a cash position of one. The ticker road reads that portfolio's
// custody positions.
func TestAPrintedCashPositionPlacesItsAccount(t *testing.T) {
	_, db := newFixtureSilver(t)
	seedDividendLine(t, db, divLine, "850", "CHF", divNarrative, "", divDay)
	web := newDividendWeb(t, custodyPosition{divPortfolio, divISIN, divValor, "Reg.shs Example AG (EXMP)"})
	seedCashPosition(t, web, divPortfolio, divIBAN)
	div := mergedText(t, web, db)[divLine]
	if derefText(div.InstrumentExternalID) != divISIN {
		t.Errorf("instrument = %v, want %s by the ticker", derefText(div.InstrumentExternalID), divISIN)
	}
}

// TestATickerTwoPortfoliosPrintForTwoSecuritiesLinksNothing: the line's
// account sits in two portfolios, and each prints the ticker for another
// security, so it names neither.
func TestATickerTwoPortfoliosPrintForTwoSecuritiesLinksNothing(t *testing.T) {
	db := newDividendSilver(t)
	seedDividendLine(t, db, divLine, "850", "CHF", divNarrative, "", divDay)
	web := newDividendWeb(t,
		custodyPosition{divPortfolio, divISIN, divValor, "Reg.shs Example AG (EXMP)"},
		custodyPosition{divOther, divISIN2, "7777777", "Reg.shs Example AG (EXMP)"})
	seedCashPosition(t, web, divOther, divIBAN)
	div := mergedText(t, web, db)[divLine]
	if div.InstrumentExternalID != nil {
		t.Errorf("instrument = %v, want none: the account's portfolios print the ticker for two securities",
			*div.InstrumentExternalID)
	}
}

// TestAMarkerIsNoTicker: a share-class currency or a domicile in
// parentheses looks like a ticker. A token in the middle of a name is no
// ticker, whatever its shape; one that is a currency the statements
// print, or shorter than three characters, names no security even where
// it closes both names.
func TestAMarkerIsNoTicker(t *testing.T) {
	db := newDividendSilver(t)
	seedDividendLine(t, db, divLine, "850", "CHF", "Units Third Example Fund (CHF)", "", divDay)
	seedDividendLine(t, db, divLine+"#1", "120", "CHF", "Shs Example Two AG (EX)", "", divDay)
	seedDividendLine(t, db, divLine+"#2", "90", "CHF", "Units Example Fund A (EXMR)", "", divDay)
	web := newDividendWeb(t,
		custodyPosition{divPortfolio, divISIN, divValor, "Units Example ETF (CH) - Index (CHF) A-dis"},
		custodyPosition{divPortfolio, divISIN2, "7777777", "Units Other Example Fund (CHF)"},
		custodyPosition{divPortfolio, "XX0000000013", "8888888", "Shs Example Two AG (EX)"},
		// A token of a ticker's shape inside the name, not closing it.
		custodyPosition{divPortfolio, "XX0000000014", "9999999", "Units Example (EXMR) Fund A"})
	got := mergedText(t, web, db)
	for _, id := range []string{divLine, divLine + "#1", divLine + "#2"} {
		if div := got[id]; div.InstrumentExternalID != nil {
			t.Errorf("%s: instrument = %v, want none: the token is a marker", id, *div.InstrumentExternalID)
		}
	}
}

// TestAStatedValorOutranksACoincidentPair: the line's four facts match a
// confirmation of another security, but the valor it states names its
// own. The pair is refused: the line takes its valor's security and
// keeps the net.
func TestAStatedValorOutranksACoincidentPair(t *testing.T) {
	db := newDividendSilver(t)
	seedValor(t, db, divISIN2, "7777777")
	seedDividendLine(t, db, divLine, "850", "CHF", divNarrative, "VN 7777777", divDay)
	seedCorporateAction(t, db, divConf, dividendConfirmation("0000KE0000011", divAcctID,
		"CHF850,", "CHF1000,", "CHF150,", divDay), divDay)
	got := psnRowsWith(t, db, psnHints{})
	div := got[divLine]
	if derefText(div.InstrumentExternalID) != divISIN2 || div.NetAmount.String() != "850" {
		t.Errorf("line = %v on %q, want 850 on %s", div.NetAmount, derefText(div.InstrumentExternalID), divISIN2)
	}
	if _, ok := got[divLine+withholdingIDSuffix]; ok {
		t.Error("a withholding row was booked from another security's confirmation")
	}
	if payloadField(t, div, "corporate_action") != "" {
		t.Error("the line names a confirmation it was refused")
	}
}

// TestAConfirmationNamingNoSecurityStillPairs: a confirmation that states
// no ISIN contradicts no valor. The line pairs and books the gross, and
// its own valor names the security.
func TestAConfirmationNamingNoSecurityStillPairs(t *testing.T) {
	db := newDividendSilver(t)
	seedValor(t, db, divISIN2, "7777777")
	seedDividendLine(t, db, divLine, "850", "CHF", divNarrative, "VN 7777777", divDay)
	noISIN := strings.Replace(dividendConfirmation("0000KE0000011", divAcctID,
		"CHF850,", "CHF1000,", "CHF150,", divDay), `"isin":"`+divISIN+`"`, `"isin":""`, 1)
	seedCorporateAction(t, db, divConf, noISIN, divDay)
	got := psnRowsWith(t, db, psnHints{})
	div := got[divLine]
	if derefText(div.InstrumentExternalID) != divISIN2 || div.NetAmount.String() != "1000" {
		t.Errorf("line = %v on %q, want the gross 1000 on %s", div.NetAmount, derefText(div.InstrumentExternalID), divISIN2)
	}
	if tax, ok := got[divLine+withholdingIDSuffix]; !ok {
		t.Error("no withholding row was booked beside the paired line")
	} else {
		wantWithholding(t, div, tax, "-150")
	}
}

// The statement era states a dividend's valor in its narrative,
// `DIVIDEND; VN <valor> <portfolio>; <NAME>`, where the parser does not
// promote it.
const (
	divStmt     = "stmt:00000000000000d1"
	divStmtRev  = "stmt:00000000000000d2"
	divStmtNone = "stmt:00000000000000d3"
	divStmtTwo  = "stmt:00000000000000d4"
)

// TestAStatementDividendLinksByItsValor: a dividend and its reversal take
// the valor road; a valor the index does not hold, or holds for two
// instruments, links nothing and stays the hint.
func TestAStatementDividendLinksByItsValor(t *testing.T) {
	_, psnDB := newFixtureSilver(t)
	seedValor(t, psnDB, divISIN, divValor)
	seedValor(t, psnDB, "XX0000000013", "2222222")
	seedValor(t, psnDB, "XX0000000014", "2222222")
	r := newWebTxFixture(t)
	seedWebAccount(t, r, divIBAN)
	seedWebTxRaw(t, r, divStmt, divIBAN, divDay, "CHF", 850, "DIVIDEND",
		statementPayload("DIVIDEND", "VN "+divValor+" XXXXXXS9", "EXAMPLE AG"))
	// A statement prints a reversal as a negative figure in the column the
	// booking it reverses went in.
	if _, err := r.db.Exec(`
        INSERT INTO transactions (transaction_external_id, account_external_id,
            snapshot_at, value_date, currency_iso, amount_credit, description_kind, payload)
        VALUES (?, ?, 1000, ?, 'CHF', -850, 'REVERSAL DIVIDEND', ?)`,
		divStmtRev, divIBAN, divDay+86400,
		statementPayload("REVERSAL DIVIDEND", "VN "+divValor+" XXXXXXS9", "EXAMPLE AG")); err != nil {
		t.Fatal(err)
	}
	seedWebTxRaw(t, r, divStmtNone, divIBAN, divDay+2*86400, "CHF", 120, "DIVIDEND",
		statementPayload("DIVIDEND", "VN 7654321 XXXXXXS9", "UNKNOWN AG"))
	seedWebTxRaw(t, r, divStmtTwo, divIBAN, divDay+3*86400, "CHF", 130, "DIVIDEND",
		statementPayload("DIVIDEND", "VN 2222222 XXXXXXS9", "TWOFOLD AG"))
	stream, _, err := r.transactionsBeforePSNStart(context.Background(),
		canonical.Window{Start: 0, End: 1 << 40, HasChanges: true}, &psnReader{db: psnDB}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	got := drainTx(t, stream)

	for _, tc := range []struct {
		id, isin, hint string
	}{
		{divStmt, divISIN, ""},
		{divStmtRev, divISIN, ""},
		{divStmtNone, "", "7654321"},
		{divStmtTwo, "", "2222222"},
	} {
		tx, ok := got[tc.id+"@"+divIBAN]
		if !ok {
			t.Fatalf("%s is missing from the stream", tc.id)
		}
		if tx.Kind != canonical.TxKindDividend {
			t.Errorf("%s: kind = %s, want dividend", tc.id, tx.Kind)
		}
		if derefText(tx.InstrumentExternalID) != tc.isin || tx.InstrumentHint != tc.hint {
			t.Errorf("%s: instrument = %q hint %q, want %q hint %q",
				tc.id, derefText(tx.InstrumentExternalID), tx.InstrumentHint, tc.isin, tc.hint)
		}
	}
	if rev := got[divStmtRev+"@"+divIBAN]; rev.NetAmount == nil || !rev.NetAmount.IsNegative() {
		t.Errorf("reversal = %v, want the clawback negative", rev.NetAmount)
	}
}

// TestAStatementCopyOfAGrossedLineStillFolds: the era fold keys the feed
// line on the amount it credited, which the statement prints too. The
// line reading the gross does not stop its statement copy folding away.
func TestAStatementCopyOfAGrossedLineStillFolds(t *testing.T) {
	db := newDividendSilver(t)
	seedDividendLine(t, db, divLine, "850", "CHF", divNarrative, "", divDay)
	seedCorporateAction(t, db, divConf, dividendConfirmation("0000KE0000011", divAcctID,
		"CHF850,", "CHF1000,", "CHF150,", divDay), divDay)
	r := newWebTxFixture(t)
	seedWebAccount(t, r, divIBAN)
	seedWebTxRaw(t, r, divStmt, divIBAN, divDay, "CHF", 850, "DIVIDEND",
		statementPayload("DIVIDEND", "VN "+divValor+" XXXXXXS9", "EXAMPLE AG"))
	got := mergedText(t, r, db)
	if _, ok := got[divStmt+"@"+divIBAN]; ok {
		t.Error("the statement copy survived beside the feed's line")
	}
	div, tax := got[divLine], got[divLine+withholdingIDSuffix]
	if div.NetAmount == nil || div.NetAmount.String() != "1000" || tax.NetAmount == nil || tax.NetAmount.String() != "-150" {
		t.Errorf("line %v, withholding %v; want 1000 and -150", div.NetAmount, tax.NetAmount)
	}
}

// TestAnExportDividendReadsTheGrossItStates: an export row states its
// gross in Description3. In the row's own currency, the dividend reads
// it and the tax withheld is its own row, the two summing to what
// moved; a reversal reverses both. A gross equal to what moved, or one
// below it, books no tax row.
func TestAnExportDividendReadsTheGrossItStates(t *testing.T) {
	r := newWebTxFixture(t)
	seedWebAccount(t, r, divIBAN)
	d1 := "Reg.shs Example AG (EXA); " + divISIN
	gross := func(amount string) string {
		return "Number/Amt. " + amount + "; Transaction no. AW00000KE0000000"
	}
	seedWebTxRaw(t, r, "AW00000KE0000001", divIBAN, divDay, "CHF", 650, "Dividend",
		exportPayload(d1, "Dividend", gross("1000.00 CHF")))
	if _, err := r.db.Exec(`
        INSERT INTO transactions (transaction_external_id, account_external_id,
            snapshot_at, value_date, currency_iso, amount_credit, description_kind, payload)
        VALUES ('AW00000KE0000002', ?, 1000, ?, 'CHF', -650, 'Dividend;Reversal', ?)`,
		divIBAN, divDay+86400, exportPayload(d1, "Dividend;Reversal", gross("1000.00 CHF"))); err != nil {
		t.Fatal(err)
	}
	seedWebTxRaw(t, r, "AW00000KE0000004", divIBAN, divDay+3*86400, "CHF", 100, "Dividend",
		exportPayload(d1, "Dividend", gross("100.00 CHF")))
	seedWebTxRaw(t, r, "AW00000KE0000005", divIBAN, divDay+4*86400, "CHF", 100, "Dividend",
		exportPayload(d1, "Dividend", gross("90.00 CHF")))
	seedWebTxRaw(t, r, "AW00000KE0000006", divIBAN, divDay+5*86400, "CHF", 780, "Dividend",
		exportPayload(d1, "Dividend", gross("1'200.00 CHF")))
	if _, err := r.db.Exec(`
        INSERT INTO transactions (transaction_external_id, account_external_id,
            snapshot_at, value_date, currency_iso, amount_credit, description_kind, payload)
        VALUES ('AW00000KE0000007', ?, 1000, ?, 'CHF', -650, 'Dividend;Reversal', ?)`,
		divIBAN, divDay+6*86400, exportPayload(d1, "Dividend;Reversal", gross("-1000.00 CHF"))); err != nil {
		t.Fatal(err)
	}
	// The statement's copy of the first row folds onto it: the era fold
	// keys both on the amount credited.
	seedWebTxRaw(t, r, divStmt, divIBAN, divDay, "CHF", 650, "DIVIDEND",
		statementPayload("DIVIDEND", "VN "+divValor+" XXXXXXS9", "EXAMPLE AG"))
	stream, _, err := r.transactionsBeforePSNStart(context.Background(),
		canonical.Window{Start: 0, End: 1 << 40, HasChanges: true}, &psnReader{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	got := drainTx(t, stream)

	for _, tc := range []struct {
		id, net, tax string
	}{
		{"AW00000KE0000001", "1000", "-350"},
		{"AW00000KE0000002", "-1000", "350"},
		{"AW00000KE0000004", "100", ""},
		{"AW00000KE0000005", "100", ""},
		{"AW00000KE0000006", "1200", "-420"},
		{"AW00000KE0000007", "-1000", "350"},
	} {
		id := tc.id + "@" + divIBAN
		tx := got[id]
		if tx.Kind != canonical.TxKindDividend || tx.NetAmount == nil || tx.NetAmount.String() != tc.net {
			t.Errorf("%s: %s %v, want a dividend of %s", tc.id, tx.Kind, tx.NetAmount, tc.net)
		}
		if derefText(tx.InstrumentExternalID) != divISIN {
			t.Errorf("%s: instrument %q, want %s", tc.id, derefText(tx.InstrumentExternalID), divISIN)
		}
		if tc.tax != "" && derefDecimal(tx.GrossAmount) != tc.net {
			t.Errorf("%s: gross amount %s, want %s", tc.id, derefDecimal(tx.GrossAmount), tc.net)
		}
		tax, ok := got[id+withholdingIDSuffix]
		switch {
		case tc.tax == "" && ok:
			t.Errorf("%s: a withholding row %v, want none", tc.id, tax.NetAmount)
		case tc.tax == "":
		case !ok || tax.Kind != canonical.TxKindTax || tax.NetAmount == nil || tax.NetAmount.String() != tc.tax:
			t.Errorf("%s: withholding %v, want a tax row of %s", tc.id, tax.NetAmount, tc.tax)
		case derefText(tax.InstrumentExternalID) != divISIN || tax.Currency != "CHF" || tax.OccurredAt != tx.OccurredAt:
			t.Errorf("%s: withholding on %q %s at %d, want the dividend's", tc.id,
				derefText(tax.InstrumentExternalID), tax.Currency, tax.OccurredAt)
		case string(tax.Payload) != `{"withheld_from":"`+id+`"}`:
			t.Errorf("%s: withholding payload %s", tc.id, tax.Payload)
		case !strings.HasPrefix(derefText(tax.Description), withholdingLabel):
			t.Errorf("%s: withholding described %q", tc.id, derefText(tax.Description))
		}
	}
	if _, ok := got[divStmt+"@"+divIBAN]; ok {
		t.Error("the statement's copy of a grossed export row survived beside it")
	}
}

// TestAReversalTakesBackOnlyAGrossGoldBooked: the feed credits 500 with
// no confirmation, so gold books it net; the export's copy of the same
// booking states a gross of 600, but the cut gives the booking to the
// feed. The feed's reversal of the credit takes back the 500 gold booked,
// with no tax row.
func TestAReversalTakesBackOnlyAGrossGoldBooked(t *testing.T) {
	db := newDividendSilver(t)
	seedValor(t, db, divISIN, divValor)
	seedCreditLine(t, db, divLine, divIBAN, "CHF", "500", "NDIV", divNarrative, "VN "+divValor, divDay)
	seedReversalLine(t, db, divRev, "500", "NDIV", divNarrative, "VN "+divValor, divDay+86400)
	r := newDividendWeb(t, custodyPosition{divPortfolio, divISIN, divValor, divNarrative})
	seedWebAccount(t, r, divIBAN)
	seedWebTxRaw(t, r, "EXP0000002", divIBAN, divDay, "CHF", 500, "Dividend",
		exportPayload(divNarrative+"; "+divISIN, "Dividend", "Number/Amt. 600.00 CHF; Transaction no. EXP0000002"))
	conn := &Connection{web: r, psn: &psnReader{db: db},
		relationships: []silver.RelationshipPair{{WebID: "REL1", PSNStartOverride: divDay - 86400}}}
	stream, err := conn.Transactions(context.Background(), canonical.Window{Start: 0, End: 1 << 40, HasChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	got := drainTx(t, stream)
	if _, ok := got["EXP0000002@"+divIBAN]; ok {
		t.Fatal("the export's copy of the feed's booking was emitted; the case needs it cut")
	}
	if line := got[divLine]; derefDecimal(line.NetAmount) != "500" {
		t.Errorf("credit = %s, want the 500 booked net", derefDecimal(line.NetAmount))
	}
	if rev := got[divRev]; rev.Kind != canonical.TxKindDividend || derefDecimal(rev.NetAmount) != "-500" {
		t.Errorf("reversal = %s %s, want a dividend reversal of -500", rev.Kind, derefDecimal(rev.NetAmount))
	}
	for id, tx := range got {
		if tx.Kind == canonical.TxKindTax {
			t.Errorf("%s: a tax row of %s, want none", id, derefDecimal(tx.NetAmount))
		}
	}
}

// TestAReversalTakesBackTheCreditItReverses: a reversal reverses the
// newest equal credit on or before its day, and takes back a gross only
// where gold booked that credit at one; two grosses booked that day leave
// the net. The record spans every window, so a reversal loaded apart
// from its credit still takes back the gross.
func TestAReversalTakesBackTheCreditItReverses(t *testing.T) {
	exported := func(id string, day int64, gross string) func(*testing.T, *webReader, *sql.DB) {
		return func(t *testing.T, r *webReader, _ *sql.DB) {
			seedWebTxRaw(t, r, id, divIBAN, day, "CHF", 500, "Dividend",
				exportPayload(divNarrative+"; "+divISIN, "Dividend", "Number/Amt. "+gross+" CHF; Transaction no. "+id))
		}
	}
	for _, tc := range []struct {
		name         string
		credits      []func(*testing.T, *webReader, *sql.DB)
		from         int64
		taken, taxed string
	}{
		{"an export credit at its gross", []func(*testing.T, *webReader, *sql.DB){exported("EXP0000011", divDay-86400, "600.00")},
			0, "-600", "100"},
		{"an export credit at its gross, loaded before the reversal's window",
			[]func(*testing.T, *webReader, *sql.DB){exported("EXP0000011", divDay-86400, "600.00")}, divDay, "-600", "100"},
		{"a credit at its confirmation's gross, loaded before the reversal's window",
			[]func(*testing.T, *webReader, *sql.DB){func(t *testing.T, _ *webReader, db *sql.DB) {
				seedDividendLine(t, db, divLine, "500", "CHF", divNarrative, "", divDay-86400)
				seedCorporateAction(t, db, divConf, dividendConfirmation("0000KE0000011", divAcctID,
					"CHF500,", "CHF600,", "CHF100,", divDay-86400), divDay-86400)
			}}, divDay, "-600", "100"},
		{"a newer credit booked net", []func(*testing.T, *webReader, *sql.DB){
			exported("EXP0000011", divDay-10*86400, "600.00"),
			func(t *testing.T, _ *webReader, db *sql.DB) {
				seedCreditLine(t, db, divLine, divIBAN, "CHF", "500", "NDIV", divNarrative, "VN "+divValor, divDay-86400)
			}}, 0, "-500", ""},
		{"a newer credit booked at its gross", []func(*testing.T, *webReader, *sql.DB){
			func(t *testing.T, _ *webReader, db *sql.DB) {
				seedCreditLine(t, db, divLine, divIBAN, "CHF", "500", "NDIV", divNarrative, "VN "+divValor, divDay-10*86400)
			},
			exported("EXP0000011", divDay-86400, "600.00")}, 0, "-600", "100"},
		{"two grosses booked on the newest day", []func(*testing.T, *webReader, *sql.DB){
			exported("EXP0000011", divDay-86400, "600.00"), exported("EXP0000012", divDay-86400, "650.00")},
			0, "-500", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newDividendSilver(t)
			seedValor(t, db, divISIN, divValor)
			r := newDividendWeb(t, custodyPosition{divPortfolio, divISIN, divValor, divNarrative})
			seedWebAccount(t, r, divIBAN)
			for _, seed := range tc.credits {
				seed(t, r, db)
			}
			seedReversalLine(t, db, divRev, "500", "NDIV", divNarrative, "VN "+divValor, divDay)
			conn := &Connection{web: r, psn: &psnReader{db: db}}
			stream, err := conn.Transactions(context.Background(), canonical.Window{Start: tc.from, End: 1 << 40, HasChanges: true})
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			got := drainTx(t, stream)
			if rev := got[divRev]; rev.Kind != canonical.TxKindDividend || derefDecimal(rev.NetAmount) != tc.taken {
				t.Errorf("reversal = %s %s, want a dividend reversal of %s", rev.Kind, derefDecimal(rev.NetAmount), tc.taken)
			}
			tax, ok := got[divRev+withholdingIDSuffix]
			switch {
			case tc.taxed == "" && ok:
				t.Errorf("a tax row of %s beside the reversal, want none", derefDecimal(tax.NetAmount))
			case tc.taxed != "" && derefDecimal(tax.NetAmount) != tc.taxed:
				t.Errorf("tax taken back = %s, want %s", derefDecimal(tax.NetAmount), tc.taxed)
			}
		})
	}
}

// TestALinkedDividendLandsOnTheCustodyHolding: the dividend is booked to
// the cash account and names the security; the yield report finds the
// holding through the account's portfolio, on the custody account beside
// it.
func TestALinkedDividendLandsOnTheCustodyHolding(t *testing.T) {
	path, db := newFixtureSilver(t)
	seedMirrorCashAccount(t, db, divIBAN, divAcctID, divPortfolio, "CHF")
	if _, err := db.Exec(`
        INSERT INTO dump_runs (snapshot_at, silver_schema_version, run_dir) VALUES (1, 5, '/x/1');
        INSERT INTO instruments (snapshot_at, relationship_id, isin, payload)
        VALUES (1, 'R1', ?, '{"InstrCtgyCFI":"ESVUFR","GacInstrRskCcyIsoCd":"CHF"}');`, divISIN); err != nil {
		t.Fatal(err)
	}
	// Another portfolio holds the same security on an account that sorts
	// first, so only the portfolio road finds the custody account beside
	// the cash account.
	seedSafekeeping(t, db, 1, divOther, divSafeOther)
	seedHolding(t, db, 1, divSafeOther, divISIN)
	seedSafekeeping(t, db, 1, divPortfolio, divSafe)
	seedHolding(t, db, 1, divSafe, divISIN)
	seedDividendLine(t, db, divLine, "850", "CHF", divNarrative, "", divDay)
	seedCorporateAction(t, db, divConf, dividendConfirmation("0000KE0000011", divAcctID,
		"CHF850,", "CHF1000,", "CHF150,", divDay), divDay)

	conn := openAdapter(t, path)
	ctx := context.Background()
	w := canonical.Window{Start: 0, End: 1 << 40, HasChanges: true}
	in := &yield.Inputs{Accounts: map[yield.AccountKey]yield.Account{}}
	for _, batch := range silvertest.Snapshots(t, conn, w) {
		for _, a := range batch.Accounts {
			in.Accounts[yield.AccountKey{Source: kindName, Account: a.AccountExternalID}] =
				yield.Account{Portfolio: derefText(a.PortfolioExternalID)}
		}
		for _, p := range batch.Positions {
			in.Holdings = append(in.Holdings, yield.Holding{
				HoldingKey: yield.HoldingKey{Source: kindName, Account: p.AccountExternalID, PositionKey: p.PositionKey},
				Instrument: derefText(p.InstrumentExternalID),
			})
		}
	}
	sort.Slice(in.Holdings, func(i, j int) bool { return in.Holdings[i].Account < in.Holdings[j].Account })
	if len(in.Holdings) != 2 || in.Holdings[0].Account != divSafeOther {
		t.Fatalf("holdings = %v, want the other portfolio's first", in.Holdings)
	}
	txs, err := conn.Transactions(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	defer txs.Close()
	div := drainTx(t, txs)[divLine]
	got, ok := in.HoldingOf(kindName, div.AccountExternalID, derefText(div.InstrumentExternalID))
	want := yield.HoldingKey{Source: kindName, Account: divSafe, PositionKey: divISIN}
	if !ok || got != want {
		t.Errorf("HoldingOf(%s, %s) = %v %v, want the custody holding %v",
			div.AccountExternalID, derefText(div.InstrumentExternalID), got, ok, want)
	}
}

// TestStatedValorAndTickers pins how a line's valor and the ticker
// closing a narrative are read.
func TestStatedValorAndTickers(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"VN 1234567", "1234567"},
		{"VN1234567", "1234567"},
		{"DIVIDEND; VN  1234567 XXXXXXS9; EXAMPLE AG", "1234567"},
		{"NONREF", ""},
		{"XVN 1234567", ""},
	} {
		if got := statedValor(tc.in); got != tc.want {
			t.Errorf("statedValor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{"Reg.shs Example AG (EXMP)", "EXMP"},
		{"Shs Example Holdings (EXMP.B) ", "EXMP.B"},
		{"Reg.shs Example AG (EXMP)\n", "EXMP"},
		{"Reg.shs Example AG (EXMP); REF 1", ""},
		{"Units Example Fund (CHF) A-dis", ""},
		{"Units Example Fund (Lux)", ""},
		{"K00?", ""},
	} {
		if got := closingTicker(tc.in); got != tc.want {
			t.Errorf("closingTicker(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The bank corrects a dividend by reversing its credit. Every id, name
// and figure below is invented.
const divRev = "mt940:" + divIBAN + ":REF0000012"

// seedReversalLine inserts one MT940 reversal of a credit (`RC`) on the
// dividends' cash account, in CHF.
func seedReversalLine(t *testing.T, db *sql.DB, eventID, amount, txnType, narrative, customerRef string, day int64) {
	t.Helper()
	seedMT940Line(t, db, eventID, divIBAN, "CHF", "RC", amount, txnType, narrative, customerRef, day)
}

// TestADividendCorrectionNetsOnTheHolding: the export books a dividend
// of 500, the feed reverses the credit and books the corrected 467.50,
// whose confirmation states a gross of 550. The reversal names the
// security by the custody ticker, so it is that dividend's reversal: the
// security's dividends sum to the corrected gross, the tax rows to its
// tax, and the cash is 500 - 500 + 467.50. Where the export states the
// first credit's gross, that credit is booked at it, and the reversal
// takes back the gross and its tax.
func TestADividendCorrectionNetsOnTheHolding(t *testing.T) {
	for _, tc := range []struct {
		name, stated, taken string
		taxRows             int
	}{
		{"the export states no gross", "", "-500", 1},
		{"the export states the gross", "Number/Amt. 625.00 CHF; Transaction no. EXP0000001", "-625", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newDividendSilver(t)
			seedReversalLine(t, db, divRev, "500", "NRTI", "Shs Example AG (EXMP)", "", divDay)
			seedDividendLine(t, db, divLine, "467.5", "CHF", "Shs Example AG (EXMP)", "", divDay)
			seedCorporateAction(t, db, divConf, dividendConfirmation("0000KE0000011", divAcctID,
				"CHF467,5", "CHF550,", "CHF82,5", divDay), divDay)
			r := newDividendWeb(t, custodyPosition{divPortfolio, divISIN, divValor, "Shs Example AG (EXMP)"})
			seedWebAccount(t, r, divIBAN)
			seedWebTxRaw(t, r, "EXP0000001", divIBAN, divDay, "CHF", 500, "Dividend",
				exportPayload("Shs Example AG (EXMP); "+divISIN, "Dividend", tc.stated))
			got := mergedText(t, r, db)

			rev := got[divRev]
			if rev.Kind != canonical.TxKindDividend || derefDecimal(rev.NetAmount) != tc.taken ||
				derefText(rev.InstrumentExternalID) != divISIN {
				t.Errorf("reversal = %s %s on %q, want dividend %s on %s",
					rev.Kind, derefDecimal(rev.NetAmount), derefText(rev.InstrumentExternalID), tc.taken, divISIN)
			}
			var dividends, taxes, cash canonical.Decimal
			taxRows := 0
			for id, tx := range got {
				if tx.AccountExternalID != divIBAN || tx.NetAmount == nil {
					continue
				}
				cash = cash.Add(*tx.NetAmount)
				switch tx.Kind {
				case canonical.TxKindDividend:
					if derefText(tx.InstrumentExternalID) != divISIN {
						t.Errorf("%s: dividend on %q, want %s", id, derefText(tx.InstrumentExternalID), divISIN)
					}
					dividends = dividends.Add(*tx.NetAmount)
				case canonical.TxKindTax:
					taxes = taxes.Add(*tx.NetAmount)
					taxRows++
				default:
					t.Errorf("%s: %s %s, want only dividends and tax rows", id, tx.Kind, tx.NetAmount)
				}
			}
			if dividends.String() != "550" || taxes.String() != "-82.5" || taxRows != tc.taxRows {
				t.Errorf("dividends = %s, tax %s in %d row(s); want the corrected gross 550, its tax -82.5, %d rows",
					dividends, taxes, taxRows, tc.taxRows)
			}
			if cash.String() != "467.5" {
				t.Errorf("cash = %s, want 500 - 500 + 467.50", cash)
			}
		})
	}
}

// seedValor makes the feed's instrument dimension state the valor of
// the ISIN.
func seedValor(t *testing.T, db *sql.DB, isin, valor string) {
	t.Helper()
	if _, err := db.Exec(`
        INSERT INTO instruments (snapshot_at, relationship_id, isin, payload)
        VALUES (1, 'R1', ?, json_object('InstrIdtfr', json_object('Valor', ?)))`, isin, valor); err != nil {
		t.Fatal(err)
	}
}

// seedCreditLine inserts one MT940 credit on the account and in the
// currency given.
func seedCreditLine(t *testing.T, db *sql.DB, eventID, account, ccy, amount, txnType, narrative, customerRef string, day int64) {
	t.Helper()
	seedMT940Line(t, db, eventID, account, ccy, "C", amount, txnType, narrative, customerRef, day)
}

// TestAReversalNeedsEvidenceOfTheDividend: a reversal of a credit that
// names a security is a dividend reversal only where something says the
// credit was a dividend: the line itself (its type code or its
// narrative), or a dividend credit of the security, account, currency
// and amount on or before its day. A reversed redemption carries
// neither, and stays a withdrawal on no instrument. The amount is the
// one taken back, negative; where the credit it reverses was booked at a
// gross, the reversal takes back that gross and the tax withheld from it
// is its own positive row.
func TestAReversalNeedsEvidenceOfTheDividend(t *testing.T) {
	const credit = "mt940:" + divIBAN + ":REF0000013"
	// bookedAtGross is a credit gold booked at its confirmation's gross.
	bookedAtGross := func(t *testing.T, db *sql.DB) {
		seedDividendLine(t, db, divLine, "500", "CHF", divNarrative, "", divDay-86400)
		seedCorporateAction(t, db, divConf, dividendConfirmation("0000KE0000011", divAcctID,
			"CHF500,", "CHF600,", "CHF100,", divDay-86400), divDay-86400)
	}
	for _, tc := range []struct {
		name, txnType, narrative string
		before                   func(*testing.T, *sql.DB)
		dividend                 bool
		// taken and tax are the reversal's amount and its tax row's, ""
		// for none; taken defaults to the -500 taken back.
		taken, tax string
		// leg says the confirmation books its own cash leg.
		leg bool
	}{
		{"the type code says dividend", "NDIV", "K00?", nil, true, "", "", false},
		{"the narrative says dividend", "NRTI", "DIVIDENDE EXAMPLE AG", nil, true, "", "", false},
		// The narrative outranks a type code that names another kind, as
		// in the kind map: the line is a dividend reversal, of the gross.
		{"the narrative says dividend, the type code interest", "NINT", "DIVIDENDE EXAMPLE AG",
			bookedAtGross, true, "-600", "100", false},
		{"a dividend credit before it", "NRTI", "K00?", func(t *testing.T, db *sql.DB) {
			seedCreditLine(t, db, credit, divIBAN, "CHF", "500", "NDIV", "K00?", "VN "+divValor, divDay-86400)
		}, true, "", "", false},
		{"a redemption credit before it", "NRTI", "K00?", func(t *testing.T, db *sql.DB) {
			seedCreditLine(t, db, credit, divIBAN, "CHF", "500", "NRED", "K00?", "VN "+divValor, divDay-86400)
		}, false, "", "", false},
		// The type code of a line no narrative places names the booking
		// reversed: a coupon, though a dividend credit stands before it.
		{"an interest reversal after a dividend credit", "NINT", "K00?", func(t *testing.T, db *sql.DB) {
			seedCreditLine(t, db, credit, divIBAN, "CHF", "500", "NDIV", "K00?", "VN "+divValor, divDay-86400)
		}, false, "", "", false},
		{"a dividend credit after it", "NRTI", "K00?", func(t *testing.T, db *sql.DB) {
			seedCreditLine(t, db, credit, divIBAN, "CHF", "500", "NDIV", "K00?", "VN "+divValor, divDay+86400)
		}, false, "", "", false},
		{"a dividend credit of another amount", "NRTI", "K00?", func(t *testing.T, db *sql.DB) {
			seedCreditLine(t, db, credit, divIBAN, "CHF", "501", "NDIV", "K00?", "VN "+divValor, divDay-86400)
		}, false, "", "", false},
		{"a dividend credit on another account", "NRTI", "K00?", func(t *testing.T, db *sql.DB) {
			seedCreditLine(t, db, credit, mirrorUSDIBAN, "CHF", "500", "NDIV", "K00?", "VN "+divValor, divDay-86400)
		}, false, "", "", false},
		{"a dividend credit in another currency", "NRTI", "K00?", func(t *testing.T, db *sql.DB) {
			seedCreditLine(t, db, credit, divIBAN, "EUR", "500", "NDIV", "K00?", "VN "+divValor, divDay-86400)
		}, false, "", "", false},
		// The confirmation's posted cash is a credit too: a line paired by
		// road 1 alone leaves no other trace of it. On a day the feed does
		// not speak for the account, the confirmation books its own cash
		// leg at its gross, and the reversal takes that gross back.
		{"a dividend confirmation before it", "NRTI", "K00?", func(t *testing.T, db *sql.DB) {
			seedCorporateAction(t, db, divConf, dividendConfirmation("0000KE0000011", divAcctID,
				"CHF500,", "CHF600,", "CHF100,", divDay-86400), divDay-86400)
		}, true, "-600", "100", true},
		// On a day it does, the feed's own line would carry the payment:
		// with none to pair, nothing books the gross.
		{"a dividend confirmation on a day the feed speaks for", "NRTI", "K00?", func(t *testing.T, db *sql.DB) {
			seedCreditLine(t, db, "mt940:"+divIBAN+":REF0000019", divIBAN, "CHF", "1", "NINT", "INTEREST", "", divDay-10*86400)
			seedCorporateAction(t, db, divConf, dividendConfirmation("0000KE0000011", divAcctID,
				"CHF500,", "CHF600,", "CHF100,", divDay-86400), divDay-86400)
		}, true, "", "", false},
		// The credit gold booked at its confirmation's gross: the reversal
		// takes the gross back, and the tax withheld from it.
		{"a credit booked at its confirmation's gross before it", "NRTI", "K00?", bookedAtGross, true, "-600", "100", false},
		{"a dividend confirmation in another currency", "NRTI", "K00?", func(t *testing.T, db *sql.DB) {
			seedCorporateAction(t, db, divConf, dividendConfirmation("0000KE0000011", divAcctID,
				"EUR500,", "EUR600,", "EUR100,", divDay-86400), divDay-86400)
		}, false, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newDividendSilver(t)
			seedValor(t, db, divISIN, divValor)
			if tc.before != nil {
				tc.before(t, db)
			}
			seedReversalLine(t, db, divRev, "500", tc.txnType, tc.narrative, "VN "+divValor, divDay)
			got := psnRowsWith(t, db, psnHints{})
			rev := got[divRev]
			taken := cmp.Or(tc.taken, "-500")
			if derefDecimal(rev.NetAmount) != taken {
				t.Errorf("reversal = %s, want %s", derefDecimal(rev.NetAmount), taken)
			}
			if dividend := rev.Kind == canonical.TxKindDividend && derefText(rev.InstrumentExternalID) == divISIN; dividend != tc.dividend {
				t.Errorf("reversal = %s on %q, want a dividend reversal on %s: %v",
					rev.Kind, derefText(rev.InstrumentExternalID), divISIN, tc.dividend)
			}
			if !tc.dividend && (rev.Kind == canonical.TxKindDividend || rev.InstrumentExternalID != nil) {
				t.Errorf("reversal = %s on %q, want it as the kind map reads it, on no instrument",
					rev.Kind, derefText(rev.InstrumentExternalID))
			}
			tax, ok := got[divRev+withholdingIDSuffix]
			switch {
			case tc.tax == "" && ok:
				t.Errorf("a reversal booked a withholding row of %s", derefDecimal(tax.NetAmount))
			case tc.tax != "" && (!ok || tax.Kind != canonical.TxKindTax || derefDecimal(tax.NetAmount) != tc.tax):
				t.Errorf("withholding taken back = %s, want a tax row of %s", derefDecimal(tax.NetAmount), tc.tax)
			}
			// What the reversal takes back is what the confirmation's own
			// cash leg booked, where it books one: on a day the feed does
			// not speak for the account.
			leg, ok := got[divConf+":cash"]
			if !ok && tc.leg {
				t.Error("the confirmation booked no cash leg")
			}
			if ok && tc.tax != "" {
				legTax := got[divConf+":cash"+withholdingIDSuffix]
				if derefDecimal(leg.NetAmount) != strings.TrimPrefix(tc.taken, "-") ||
					derefDecimal(legTax.NetAmount) != "-"+tc.tax {
					t.Errorf("cash leg %s with tax %s, want %s and -%s", derefDecimal(leg.NetAmount),
						derefDecimal(legTax.NetAmount), strings.TrimPrefix(tc.taken, "-"), tc.tax)
				}
			}
		})
	}
}

// TestAReversalPlacedAsAnotherKindStaysAsItIs: a reversal whose
// narrative places an interest, a fee or a tax keeps that kind, though
// its type code says dividend and its valor resolves. Its amount is
// negative.
func TestAReversalPlacedAsAnotherKindStaysAsItIs(t *testing.T) {
	for narrative, kind := range map[string]canonical.TxKind{
		"INTERETS EXAMPLE AG": canonical.TxKindInterest,
		"FRAIS EXAMPLE AG":    canonical.TxKindFee,
		"IMPOT EXAMPLE AG":    canonical.TxKindTax,
	} {
		db := newDividendSilver(t)
		seedValor(t, db, divISIN, divValor)
		seedReversalLine(t, db, divRev, "500", "NDIV", narrative, "VN "+divValor, divDay)
		rev := psnRowsWith(t, db, psnHints{})[divRev]
		if rev.Kind != kind || rev.InstrumentExternalID != nil || derefDecimal(rev.NetAmount) != "-500" {
			t.Errorf("%q = %s %s on %q, want %s -500 on no instrument",
				narrative, rev.Kind, derefDecimal(rev.NetAmount), derefText(rev.InstrumentExternalID), kind)
		}
	}
}

// TestADividendReversalNoRoadAnswersIsNegative: a reversal whose
// narrative or type code says dividend is a dividend by the kind map.
// Where no road names its security it keeps that kind and no instrument,
// so income nets it, and it still takes the money back.
func TestADividendReversalNoRoadAnswersIsNegative(t *testing.T) {
	db := newDividendSilver(t)
	seedReversalLine(t, db, divRev, "120", "NRTI", "DIVIDENDE EXAMPLE AG", "VN 7654321", divDay)
	seedReversalLine(t, db, divRev+"#1", "130", "NDIV", "K00?", "VN 7654321", divDay)
	got := psnRowsWith(t, db, psnHints{})
	for id, want := range map[string]string{divRev: "-120", divRev + "#1": "-130"} {
		rev := got[id]
		if rev.Kind != canonical.TxKindDividend || derefDecimal(rev.NetAmount) != want || rev.InstrumentExternalID != nil {
			t.Errorf("%s = %s %s on %q, want dividend %s on no instrument",
				id, rev.Kind, derefDecimal(rev.NetAmount), derefText(rev.InstrumentExternalID), want)
		}
	}
}

// The reversal of a debit (`RD`) returns money: a payment that came back.
const divReturned = "mt940:" + divIBAN + ":REF0000014"

// TestAReturnedPaymentIsACredit: an `RD` line reads as a withdrawal by
// its direction, and its amount is positive, the money that came back.
func TestAReturnedPaymentIsACredit(t *testing.T) {
	db := newDividendSilver(t)
	seedMT940Line(t, db, divReturned, divIBAN, "CHF", "RD", "500", "NTRF", "K00?", "", divDay)
	tx := psnRowsWith(t, db, psnHints{})[divReturned]
	if tx.Kind != canonical.TxKindWithdrawal || derefDecimal(tx.NetAmount) != "500" {
		t.Errorf("returned payment = %s %s, want withdrawal 500", tx.Kind, derefDecimal(tx.NetAmount))
	}
}

// TestAStatementCopyOfAReturnedPaymentFolds: the statement prints a
// returned payment as a negative debit, which reads as the money coming
// back. The feed's `RD` line projects to the same positive figure, so the
// copy folds onto it.
func TestAStatementCopyOfAReturnedPaymentFolds(t *testing.T) {
	db := newDividendSilver(t)
	seedMT940Line(t, db, divReturned, divIBAN, "CHF", "RD", "500", "NTRF", "K00?", "", divDay)
	r := newWebTxFixture(t)
	seedWebAccount(t, r, divIBAN)
	const copyID = "stmt:00000000000000d5"
	if _, err := r.db.Exec(`
        INSERT INTO transactions (transaction_external_id, account_external_id,
            snapshot_at, value_date, currency_iso, amount_debit, description_kind, payload)
        VALUES (?, ?, 1000, ?, 'CHF', -500, 'E-BANKING PAYMENT ORDER', ?)`,
		copyID, divIBAN, divDay, statementPayload("E-BANKING PAYMENT ORDER", "EXAMPLE PAYEE")); err != nil {
		t.Fatal(err)
	}
	got := mergedText(t, r, db)
	if _, ok := got[copyID+"@"+divIBAN]; ok {
		t.Error("the statement copy of the returned payment survived beside the feed's line")
	}
	if tx := got[divReturned]; derefDecimal(tx.NetAmount) != "500" {
		t.Errorf("returned payment = %s, want 500", derefDecimal(tx.NetAmount))
	}
}

// TestAReturnedPaymentIsACreditLeg: in the offset veto an `RD` line is the
// money coming back, so it mirrors a same-day withdrawal of its figure on
// another own account, and not a deposit.
func TestAReturnedPaymentIsACreditLeg(t *testing.T) {
	for _, tc := range []struct {
		name     string
		amount   float64
		internal bool
	}{
		{"a withdrawal", -500, true},
		{"a deposit", 500, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newDividendSilver(t)
			seedMT940Line(t, db, divReturned, divIBAN, "CHF", "RD", "500", "NTRF", "K00?", "", divDay)
			r := newWebTxFixture(t)
			seedWebAccount(t, r, vetoAcctB)
			seedWebTx(t, r, "B0000001", vetoAcctB, divDay, "CHF", tc.amount, false)
			if got := emittedInternal(t, r, &psnReader{db: db})["B0000001@"+vetoAcctB]; got != tc.internal {
				t.Errorf("%s internal = %v, want %v", tc.name, got, tc.internal)
			}
		})
	}
}

// TestAStatementCopyOfADividendReversalFolds: the statement prints the
// reversal as `REVERSAL DIVIDEND`, a negative credit. The feed's line
// projects to the same negative figure, so the copy folds onto it.
func TestAStatementCopyOfADividendReversalFolds(t *testing.T) {
	db := newDividendSilver(t)
	seedValor(t, db, divISIN, divValor)
	seedReversalLine(t, db, divRev, "120", "NDIV", "DIVIDENDE EXAMPLE AG", "VN "+divValor, divDay)
	r := newWebTxFixture(t)
	seedWebAccount(t, r, divIBAN)
	if _, err := r.db.Exec(`
        INSERT INTO transactions (transaction_external_id, account_external_id,
            snapshot_at, value_date, currency_iso, amount_credit, description_kind, payload)
        VALUES (?, ?, 1000, ?, 'CHF', -120, 'REVERSAL DIVIDEND', ?)`,
		divStmtRev, divIBAN, divDay,
		statementPayload("REVERSAL DIVIDEND", "VN "+divValor+" XXXXXXS9", "EXAMPLE AG")); err != nil {
		t.Fatal(err)
	}
	got := mergedText(t, r, db)
	if _, ok := got[divStmtRev+"@"+divIBAN]; ok {
		t.Error("the statement copy of the reversal survived beside the feed's line")
	}
	if rev := got[divRev]; rev.Kind != canonical.TxKindDividend || derefDecimal(rev.NetAmount) != "-120" ||
		derefText(rev.InstrumentExternalID) != divISIN {
		t.Errorf("reversal = %s %s on %q, want dividend -120 on %s",
			rev.Kind, derefDecimal(rev.NetAmount), derefText(rev.InstrumentExternalID), divISIN)
	}
}

// TestAReversalThatNamesNoSecurityStaysAsItIs: a reversal whose ticker
// no custody position prints, and the reversal of a trade's settlement,
// keep the kind the direction gives them and name no instrument.
func TestAReversalThatNamesNoSecurityStaysAsItIs(t *testing.T) {
	db := newDividendSilver(t)
	seedReversalLine(t, db, divRev, "500", "NRTI", "Shs Unknown AG (ZZZZ)", "", divDay)
	seedReversalLine(t, db, divRev+"#1", "500", "NSEC", "Shs Example AG (EXMP)", "", divDay+86400)
	got := mergedText(t, newDividendWeb(t, custodyPosition{divPortfolio, divISIN, divValor, "Shs Example AG (EXMP)"}), db)
	for _, id := range []string{divRev, divRev + "#1"} {
		if tx := got[id]; tx.Kind != canonical.TxKindWithdrawal || tx.InstrumentExternalID != nil {
			t.Errorf("%s = %s on %q, want a withdrawal on no instrument", id, tx.Kind, derefText(tx.InstrumentExternalID))
		}
	}
}

// TestADividendReversalIsNoOffsetLeg: a reversal of a credit takes money
// out of its account, so a same-day deposit of the same figure on another
// own account is its mirror in the offset veto, and both demote. A
// dividend reversal is no withdrawal in the stream, so it is no leg there
// either, and the deposit keeps its flow.
func TestADividendReversalIsNoOffsetLeg(t *testing.T) {
	for _, tc := range []struct {
		name, txnType, customerRef string
		internal                   bool
	}{
		{"a reversal naming no security", "NRTI", "", true},
		{"a dividend reversal", "NDIV", "VN " + divValor, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newDividendSilver(t)
			seedValor(t, db, divISIN, divValor)
			seedReversalLine(t, db, divRev, "500", tc.txnType, "K00?", tc.customerRef, divDay)
			r := newWebTxFixture(t)
			seedWebAccount(t, r, vetoAcctB)
			seedWebTx(t, r, "D0000001", vetoAcctB, divDay, "CHF", 500, false)
			if got := emittedInternal(t, r, &psnReader{db: db})["D0000001@"+vetoAcctB]; got != tc.internal {
				t.Errorf("deposit internal = %v, want %v", got, tc.internal)
			}
		})
	}
}

// TestAWithdrawnConfirmationNamesTheSecurityAndNothingElse: a line paired
// with a confirmation a cancellation withdraws still takes its security,
// which is not money, and keeps the amount it credited: no gross, no tax
// row, and its payload names no confirmation. A corrected confirmation of
// the same posting is the one the line pairs with.
func TestAWithdrawnConfirmationNamesTheSecurityAndNothingElse(t *testing.T) {
	const corrected = "mt566:0000KE0000013"
	paid := dividendConfirmation("0000KE0000011", divAcctID, "CHF850,", "CHF1000,", "CHF150,", divDay)
	cancelled := confirmationAs(t, paid, "0000KE0000012", "", "CANC", "0000KE0000011")
	// A cancellation need not repeat every figure of what it withdraws.
	bare := confirmationAs(t, dividendConfirmation("0000KE0000011", divAcctID, "CHF850,", "", "", divDay),
		"0000KE0000012", "", "CANC", "0000KE0000011")
	for _, tc := range []struct {
		name, cancellation string
		corrected          bool
	}{
		{"withdrawn", cancelled, false},
		{"withdrawn by a cancellation stating no gross", bare, false},
		{"withdrawn and corrected", cancelled, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newDividendSilver(t)
			seedDividendLine(t, db, divLine, "850", "CHF", divNarrative, "", divDay)
			seedCorporateAction(t, db, divConf, paid, divDay)
			seedCorporateAction(t, db, divConf+"2", tc.cancellation, divDay)
			if tc.corrected {
				seedCorporateAction(t, db, corrected, confirmationAs(t, paid, "0000KE0000013", "", "NEWM", ""), divDay)
			}
			got := psnRowsWith(t, db, psnHints{})
			div := got[divLine]
			if derefText(div.InstrumentExternalID) != divISIN {
				t.Errorf("instrument = %q, want the confirmation's %s", derefText(div.InstrumentExternalID), divISIN)
			}
			_, taxed := got[divLine+withholdingIDSuffix]
			if !tc.corrected {
				if derefDecimal(div.NetAmount) != "850" || div.GrossAmount != nil || taxed {
					t.Errorf("line = %s gross %s, tax row %v; want the 850 credited, no gross, no tax row",
						derefDecimal(div.NetAmount), derefDecimal(div.GrossAmount), taxed)
				}
				if got := payloadField(t, div, "corporate_action"); got != "" {
					t.Errorf("line payload names %q, want no confirmation", got)
				}
				return
			}
			if derefDecimal(div.NetAmount) != "1000" || !taxed {
				t.Errorf("line = %s, tax row %v; want the corrected gross 1000 and a tax row", derefDecimal(div.NetAmount), taxed)
			}
			if got := payloadField(t, div, "corporate_action"); got != corrected {
				t.Errorf("line payload names %q, want the corrected %q", got, corrected)
			}
		})
	}
}

// TestWithdrawnConfirmationsOfTwoSecuritiesNameNone: a line whose only
// confirmations are withdrawn ones that name two securities pairs with
// none, for which security paid is not known. It takes the other roads:
// the valor it states, else no instrument.
func TestWithdrawnConfirmationsOfTwoSecuritiesNameNone(t *testing.T) {
	other := strings.ReplaceAll(dividendConfirmation("0000KE0000011", divAcctID, "CHF850,", "CHF1000,", "CHF150,",
		divDay), divISIN, divISIN2)
	paid := dividendConfirmation("0000KE0000013", divAcctID, "CHF850,", "CHF1000,", "CHF150,", divDay)
	for _, tc := range []struct{ name, customerRef, want string }{
		{"a valor stated", "VN" + divValor, divISIN},
		{"no valor", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newDividendSilver(t)
			seedValor(t, db, divISIN, divValor)
			seedDividendLine(t, db, divLine, "850", "CHF", "K00?", tc.customerRef, divDay)
			seedCorporateAction(t, db, divConf, other, divDay)
			seedCorporateAction(t, db, divConf+"2", confirmationAs(t, other, "0000KE0000012", "", "CANC", "0000KE0000011"),
				divDay)
			seedCorporateAction(t, db, "mt566:0000KE0000013", paid, divDay)
			seedCorporateAction(t, db, "mt566:0000KE0000014", confirmationAs(t, paid, "0000KE0000014", "", "CANC",
				"0000KE0000013"), divDay)
			if got := derefText(psnRowsWith(t, db, psnHints{})[divLine].InstrumentExternalID); got != tc.want {
				t.Errorf("instrument = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTheGrossFromAConfirmationsRates: a confirmation that states no gross
// or tax amount in the line's currency states its gross through a rate.
// Its gross amount at its exchange rate comes first, either way round;
// then the gross rate per share times the confirmed balance; then the
// amount credited over one less the withholding rate. Each holds only
// where it is at least what moved and no more than twice it. A stated
// amount in the line's currency outranks a rate, and with no rate that
// holds, the line keeps the net.
func TestTheGrossFromAConfirmationsRates(t *testing.T) {
	balance := [2]string{"93B", ":CONB//UNIT/100,"}
	for _, tc := range []struct {
		name, gross, tax string
		rates            [][2]string
		want, taxed      string
	}{
		{"a foreign gross at the exchange rate", "USD1250,", "USD187,5", [][2]string{
			{"92B", ":EXCH//USD/CHF/0,8"}, {"92A", ":TAXR//20,"}}, "1000", "-150"},
		{"an exchange rate quoted the other way round", "USD1250,", "USD187,5", [][2]string{
			{"92B", ":EXCH//CHF/USD/1,25"}}, "1000", "-150"},
		{"a foreign gross and the withholding rate", "USD1250,", "USD187,5", [][2]string{
			{"92A", ":TAXR//15,"}}, "1000", "-150"},
		{"the gross rate times the balance", "", "", [][2]string{{"92F", ":GRSS//CHF10,"}, balance}, "1000", "-150"},
		{"a foreign gross rate at the exchange rate", "", "", [][2]string{
			{"92F", ":GRSS//USD12,5"}, {"92B", ":EXCH//USD/CHF/0,8"}, balance}, "1000", "-150"},
		{"the withholding rate", "", "", [][2]string{{"92A", ":TAXR//15,"}}, "1000", "-150"},
		{"a gross rate in another currency", "", "", [][2]string{
			{"92F", ":GRSS//USD11,"}, balance, {"92A", ":TAXR//15,"}}, "1000", "-150"},
		{"a product more than twice the credit", "", "", [][2]string{
			{"92F", ":GRSS//CHF100,"}, balance, {"92A", ":TAXR//15,"}}, "1000", "-150"},
		{"a gross rate and no balance", "", "", [][2]string{{"92F", ":GRSS//CHF10,"}}, "850", ""},
		{"a face amount", "", "", [][2]string{{"92F", ":GRSS//CHF10,"}, {"93B", ":CONB//FAMT/100,"}}, "850", ""},
		{"a withholding rate above half", "", "", [][2]string{{"92A", ":TAXR//60,"}}, "850", ""},
		{"a withholding rate of zero", "", "", [][2]string{{"92A", ":TAXR//0,"}}, "850", ""},
		{"a stated gross outranks a rate", "CHF1000,", "", [][2]string{{"92A", ":TAXR//20,"}}, "1000", "-150"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newDividendSilver(t)
			seedDividendLine(t, db, divLine, "850", "CHF", divNarrative, "", divDay)
			seedCorporateAction(t, db, divConf, dividendConfirmation("0000KE0000011", divAcctID,
				"CHF850,", tc.gross, tc.tax, divDay, tc.rates...), divDay)
			got := psnRowsWith(t, db, psnHints{})
			div := got[divLine]
			if derefDecimal(div.NetAmount) != tc.want {
				t.Errorf("dividend = %s, want %s", derefDecimal(div.NetAmount), tc.want)
			}
			tax, ok := got[divLine+withholdingIDSuffix]
			if ok != (tc.taxed != "") {
				t.Fatalf("withholding row booked = %v, want %v", ok, tc.taxed != "")
			}
			if ok {
				wantWithholding(t, div, tax, tc.taxed)
			}
		})
	}
}

// TestTheCashOptionStatesTheRates: a confirmation of a choice of dividend
// states each option in its own block. The cash option's block states the
// movement and the rates; the shares option's says nothing about the
// cash, and its posting quantity is no balance. The balance the event paid
// on is the confirmed one the underlying securities state.
func TestTheCashOptionStatesTheRates(t *testing.T) {
	m := parseCorporateAction(corporateActionFields{Fields: [][2]string{
		{"16R", "USECU"}, {"16R", "ACCTINFO"}, {"93B", ":CONB//UNIT/100,"}, {"16S", "ACCTINFO"}, {"16S", "USECU"},
		{"16R", "CACONF"}, {"22H", ":CAOP//SECU"}, {"92F", ":GRSS//CHF99,"}, {"92A", ":TAXR//35,"},
		{"16R", "SECMOVE"}, {"36B", ":PSTA//UNIT/5,"}, {"16S", "SECMOVE"}, {"16S", "CACONF"},
		{"16R", "CACONF"}, {"22H", ":CAOP//CASH"}, {"92F", ":GRSS//CHF10,"},
		{"16R", "CASHMOVE"}, {"22H", ":CRDB//CRED"}, {"97A", ":CASH//" + divAcctID},
		{"19B", ":PSTA//CHF850,"}, {"92A", ":TAXR//15,"}, {"92B", ":EXCH//USD/CHF/0,8"},
		{"16S", "CASHMOVE"}, {"16S", "CACONF"},
	}})
	if !m.creditsCash() || derefDecimal(&m.move.posted) != "850" {
		t.Fatalf("cash option = %v, posted %s; want the cash option's 850", m.creditsCash(), derefDecimal(&m.move.posted))
	}
	for name, tc := range map[string]struct {
		got  *canonical.Decimal
		want string
	}{
		"gross rate":       {m.move.grossRate, "10"},
		"withholding rate": {m.move.taxRate, "15"},
		"balance":          {m.move.quantity, "100"},
	} {
		if derefDecimal(tc.got) != tc.want {
			t.Errorf("%s = %s, want %s", name, derefDecimal(tc.got), tc.want)
		}
	}
	if x := m.move.exchange; x == nil || x.from != "USD" || x.to != "CHF" || x.rate.String() != "0.8" {
		t.Errorf("exchange rate = %+v, want USD/CHF 0.8", x)
	}
}

// TestAForeignExportDividendReadsItsConvertedGross: an export row of a
// security quoted in another currency states its gross in that currency,
// beside the rate the bank converted at. The row pins the rate's
// quotation, per unit or per 100 units: the one under which the converted
// gross is at least what moved and no more than twice it. Within the
// rate's rounding of what moved, nothing was withheld: the row books what
// moved, with no tax row. A reversal takes back the converted gross. Where
// no quotation answers, or the row states no rate, it keeps the net.
func TestAForeignExportDividendReadsItsConvertedGross(t *testing.T) {
	r := newWebTxFixture(t)
	seedWebAccount(t, r, divIBAN)
	d1 := "Reg.shs Example Inc (EXI); " + divISIN
	stated := func(gross, rate string) string {
		d3 := "Number/Amt. " + gross
		if rate != "" {
			d3 += "; Exchange rate: " + rate
		}
		return d3 + "; Transaction no. AW00000KE0000000"
	}
	// 15000 JPY at 0.005667 per unit converts to 85.005. The rate's
	// rounding, half a unit of its last digit on 15000, is 0.0075, and
	// half a cent more puts the band at 85.005 plus or minus 0.0125.
	rows := []struct {
		id          string
		credited    float64
		gross, rate string
		net, tax    string
	}{
		{"AW00000KE0000021", 72.25, "100.00 USD", "0.85", "85", "-12.75"},
		{"AW00000KE0000022", 71.4, "15000.00 JPY", "0.5600", "84", "-12.6"},
		{"AW00000KE0000023", 85, "15000.00 JPY", "0.004", "85", ""},
		{"AW00000KE0000024", 72.25, "100.00 USD", "", "72.25", ""},
		{"AW00000KE0000026", 85, "15000.00 JPY", "0.005667", "85", ""},
		{"AW00000KE0000027", 85.01, "15000.00 JPY", "0.005667", "85.01", ""},
		{"AW00000KE0000028", 84.99, "15000.00 JPY", "0.005667", "85.01", "-0.02"},
		{"AW00000KE0000029", 85.02, "15000.00 JPY", "0.005667", "85.02", ""},
	}
	for i, row := range rows {
		seedWebTxRaw(t, r, row.id, divIBAN, divDay+int64(i)*86400, "CHF", row.credited, "Dividend",
			exportPayload(d1, "Dividend", stated(row.gross, row.rate)))
	}
	if _, err := r.db.Exec(`
        INSERT INTO transactions (transaction_external_id, account_external_id,
            snapshot_at, value_date, currency_iso, amount_credit, description_kind, payload)
        VALUES ('AW00000KE0000025', ?, 1000, ?, 'CHF', -72.25, 'Dividend;Reversal', ?)`,
		divIBAN, divDay+20*86400, exportPayload(d1, "Dividend;Reversal", stated("-100.00 USD", "0.85"))); err != nil {
		t.Fatal(err)
	}
	stream, _, err := r.transactionsBeforePSNStart(context.Background(),
		canonical.Window{Start: 0, End: 1 << 40, HasChanges: true}, &psnReader{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	got := drainTx(t, stream)
	want := []struct{ id, net, tax string }{{"AW00000KE0000025", "-85", "12.75"}}
	for _, row := range rows {
		want = append(want, struct{ id, net, tax string }{row.id, row.net, row.tax})
	}
	for _, tc := range want {
		id := tc.id + "@" + divIBAN
		tx := got[id]
		if tx.Kind != canonical.TxKindDividend || derefDecimal(tx.NetAmount) != tc.net {
			t.Errorf("%s: %s %s, want a dividend of %s", tc.id, tx.Kind, derefDecimal(tx.NetAmount), tc.net)
		}
		tax, ok := got[id+withholdingIDSuffix]
		switch {
		case tc.tax == "" && ok:
			t.Errorf("%s: a withholding row of %s, want none", tc.id, derefDecimal(tax.NetAmount))
		case tc.tax != "" && derefDecimal(tax.NetAmount) != tc.tax:
			t.Errorf("%s: withholding %s, want %s", tc.id, derefDecimal(tax.NetAmount), tc.tax)
		}
	}
}

// TestEachReversalTakesBackItsOwnCredit: reversals take back credits one
// to one, in time order, each the newest credit of its amount, security
// and account on or before its day that no earlier reversal took back. A
// second reversal of one credit finds none left: it is a dividend reversal
// of the amount it moves only where its line says dividend. A credit two
// rails record is one credit, and so is a confirmation sent twice; two
// credits one rail records are two. A reversed confirmation's credit is
// still there to take back.
func TestEachReversalTakesBackItsOwnCredit(t *testing.T) {
	// grossed is a credit of 500 gold books at its confirmation's gross of
	// 600 on the day: the line, and the confirmation stating the gross.
	grossed := func(t *testing.T, db *sql.DB, line, seme string, day int64) {
		seedCreditLine(t, db, line, divIBAN, "CHF", "500", "NDIV", divNarrative, "VN "+divValor, day)
		seedCorporateAction(t, db, "mt566:"+seme, dividendConfirmation(seme, divAcctID,
			"CHF500,", "CHF600,", "CHF100,", day), day)
	}
	const second = divRev + "#2"
	for _, tc := range []struct {
		name string
		seed func(*testing.T, *sql.DB)
		// kind, amount and tax are each row's kind, amount and tax row,
		// "" for none.
		want map[string][3]string
	}{
		{"two reversals of one credit", func(t *testing.T, db *sql.DB) {
			grossed(t, db, divLine, "0000KE0000011", divDay-2*86400)
			seedReversalLine(t, db, divRev, "500", "NDIV", divNarrative, "VN "+divValor, divDay-86400)
			seedReversalLine(t, db, second, "500", "NDIV", divNarrative, "VN "+divValor, divDay)
		}, map[string][3]string{
			divRev: {"dividend", "-600", "100"},
			second: {"dividend", "-500", ""},
		}},
		{"an older credit reversed after a newer one", func(t *testing.T, db *sql.DB) {
			grossed(t, db, divLine, "0000KE0000011", divDay-10*86400)
			seedCreditLine(t, db, divLine+"#1", divIBAN, "CHF", "500", "NDIV", divNarrative, "VN "+divValor, divDay-5*86400)
			seedReversalLine(t, db, divRev, "500", "NDIV", divNarrative, "VN "+divValor, divDay-3*86400)
			seedReversalLine(t, db, second, "500", "NDIV", divNarrative, "VN "+divValor, divDay)
		}, map[string][3]string{
			divRev: {"dividend", "-500", ""},
			second: {"dividend", "-600", "100"},
		}},
		{"a credit two rails record", func(t *testing.T, db *sql.DB) {
			grossed(t, db, divLine, "0000KE0000011", divDay-2*86400)
			seedReversalLine(t, db, divRev, "500", "NRTI", "K00?", "VN "+divValor, divDay-86400)
			seedReversalLine(t, db, second, "500", "NRTI", "K00?", "VN "+divValor, divDay)
		}, map[string][3]string{
			divRev: {"dividend", "-600", "100"},
			second: {"withdrawal", "-500", ""},
		}},
		{"two credits one rail records on one day", func(t *testing.T, db *sql.DB) {
			seedCreditLine(t, db, divLine, divIBAN, "CHF", "500", "NDIV", divNarrative, "VN "+divValor, divDay-2*86400)
			seedCreditLine(t, db, divLine+"#1", divIBAN, "CHF", "500", "NDIV", divNarrative, "VN "+divValor, divDay-2*86400)
			seedReversalLine(t, db, divRev, "500", "NRTI", "K00?", "VN "+divValor, divDay-86400)
			seedReversalLine(t, db, second, "500", "NRTI", "K00?", "VN "+divValor, divDay)
		}, map[string][3]string{
			divRev: {"dividend", "-500", ""},
			second: {"dividend", "-500", ""},
		}},
		{"a confirmation the bank sent twice", func(t *testing.T, db *sql.DB) {
			grossed(t, db, divLine, "0000KE0000011", divDay-2*86400)
			seedCorporateAction(t, db, "mt566:0000KE0000012", dividendConfirmation("0000KE0000012", divAcctID,
				"CHF500,", "CHF600,", "CHF100,", divDay-2*86400), divDay-2*86400)
			seedReversalLine(t, db, divRev, "500", "NRTI", "K00?", "VN "+divValor, divDay-86400)
			seedReversalLine(t, db, second, "500", "NRTI", "K00?", "VN "+divValor, divDay)
		}, map[string][3]string{
			divLine: {"dividend", "600", "-100"},
			divRev:  {"dividend", "-600", "100"},
			second:  {"withdrawal", "-500", ""},
		}},
		// The bank reverses the payment a confirmation states, and the
		// line the confirmation alone names takes it back: the credit
		// stands on the confirmation's rail though the reversal withdraws
		// the confirmation.
		{"a reversed confirmation's credit", func(t *testing.T, db *sql.DB) {
			paid := dividendConfirmation("0000KE0000011", divAcctID, "CHF500,", "CHF600,", "CHF100,", divDay-2*86400)
			seedCreditLine(t, db, divLine, divIBAN, "CHF", "500", "NDIV", divNarrative, "", divDay-2*86400)
			seedCorporateAction(t, db, divConf, paid, divDay-2*86400)
			seedCorporateAction(t, db, divConf+"2", confirmationAs(t, paid, "0000KE0000012", "", "REVR", "0000KE0000011"), divDay-2*86400)
			seedReversalLine(t, db, divRev, "500", "NRTI", "K00?", "VN "+divValor, divDay-86400)
		}, map[string][3]string{
			divLine: {"dividend", "500", ""},
			divRev:  {"dividend", "-500", ""},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newDividendSilver(t)
			seedValor(t, db, divISIN, divValor)
			tc.seed(t, db)
			got := psnRowsWith(t, db, psnHints{})
			for id, want := range tc.want {
				rev := got[id]
				if string(rev.Kind) != want[0] || derefDecimal(rev.NetAmount) != want[1] {
					t.Errorf("%s = %s %s, want %s %s", id, rev.Kind, derefDecimal(rev.NetAmount), want[0], want[1])
				}
				tax, ok := got[id+withholdingIDSuffix]
				switch {
				case want[2] == "" && ok:
					t.Errorf("%s: a tax row of %s, want none", id, derefDecimal(tax.NetAmount))
				case want[2] != "" && derefDecimal(tax.NetAmount) != want[2]:
					t.Errorf("%s: tax taken back %s, want %s", id, derefDecimal(tax.NetAmount), want[2])
				}
			}
		})
	}
}
