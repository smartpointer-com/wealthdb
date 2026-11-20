package ubs

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// A cash row that no other road links to an instrument takes the
// private-equity fund its text names, where it names exactly one fund of
// its account's portfolios, and books as what it is. Every id, name and
// figure below is invented.
const (
	pfIBAN      = "CH0000000000000000PF1"
	pfAcctID    = "00000000000000PF00001"
	pfPortfolio = "0000000000000007"
	pfOther     = "0000000000000006"
	pfSafe      = "00000000000000S7"
	pfSafeOther = "00000000000000S6"
	pfFund      = "XX0000000071"
	pfFund2     = "XX0000000072"
	pfFundAway  = "XX0000000073"
	pfStock     = "XX0000000074"
	pfDay       = int64(700 * 86400)
)

// newPrivateFundSilver is a feed silver whose cash account sits in a
// portfolio that holds two private-equity funds and a share. A third fund
// sits in another portfolio.
func newPrivateFundSilver(t *testing.T) *sql.DB {
	t.Helper()
	_, db := newFixtureSilver(t)
	seedMirrorCashAccount(t, db, pfIBAN, pfAcctID, pfPortfolio, "CHF")
	for _, s := range []struct{ id, portfolio string }{{pfSafe, pfPortfolio}, {pfSafeOther, pfOther}} {
		if _, err := db.Exec(`
            INSERT INTO safekeeping_accounts (snapshot_at, relationship_id, account_external_id,
                portfolio_external_id, payload)
            VALUES (1, 'R1', ?, ?, json_object('PrtflId', ?))`, s.id, s.portfolio, s.portfolio); err != nil {
			t.Fatal(err)
		}
	}
	for _, in := range []struct{ isin, cfi, uac, name, safe string }{
		{pfFund, "", "0400", "Northwind Private Markets II", pfSafe},
		{pfFund2, "", "0400", "Bluefield Growth Partners", pfSafe},
		{pfStock, "ESVUFR", "0300", "Example Holdings AG", pfSafe},
		{pfFundAway, "", "0400", "Harbor Lane Secondaries", pfSafeOther},
	} {
		if _, err := db.Exec(`
            INSERT INTO instruments (snapshot_at, relationship_id, isin, payload)
            VALUES (1, 'R1', ?, json_object('InstrCtgyCFI', ?, 'UacAsstClsCd', ?,
                    'InstrNm', json_object('LngNmEnglish', ?)))`,
			in.isin, in.cfi, in.uac, in.name); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`
            INSERT INTO holdings (snapshot_at, relationship_id, safekeeping_external_id, isin, payload)
            VALUES (1, 'R1', ?, ?, '{}')`, in.safe, in.isin); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// TestACashRowNamingOneFundTakesTheFund: on both rails, a credit naming
// one fund of the account's portfolios is that fund's distribution, and a
// payment naming one is a call, a buy of its units; a debit taken back is
// a call taken back. A row naming two funds, a fund another portfolio
// holds, a share, or nothing stays as it is, as does a move between own
// accounts: one the offset veto pairs, one whose narrative names an
// internal transfer, or one to an own counter account. The amounts stay
// the rows'.
func TestACashRowNamingOneFundTakesTheFund(t *testing.T) {
	db := newPrivateFundSilver(t)
	seedMT940Line(t, db, "mt940:"+pfIBAN+":REF71", pfIBAN, "CHF", "C", "500", "NTRF",
		"DISTRIBUTION NORTHWIND PRIVATE MARKETS II", "", pfDay)
	seedMT940Line(t, db, "mt940:"+pfIBAN+":REF72", pfIBAN, "CHF", "D", "300", "NTRF",
		"CAPITAL CALL BLUEFIELD GROWTH PARTNERS", "", pfDay+86400)
	seedMT940Line(t, db, "mt940:"+pfIBAN+":REF73", pfIBAN, "CHF", "RC", "500", "NTRF",
		"DISTRIBUTION NORTHWIND PRIVATE MARKETS II", "", pfDay+2*86400)
	seedMT940Line(t, db, "mt940:"+pfIBAN+":REF74", pfIBAN, "CHF", "C", "200", "NTRF",
		"NORTHWIND PRIVATE MARKETS II AND BLUEFIELD GROWTH PARTNERS", "", pfDay+3*86400)
	seedMT940Line(t, db, "mt940:"+pfIBAN+":REF75", pfIBAN, "CHF", "C", "150", "NTRF",
		"DISTRIBUTION HARBOR LANE SECONDARIES", "", pfDay+4*86400)
	seedMT940Line(t, db, "mt940:"+pfIBAN+":REF76", pfIBAN, "CHF", "C", "120", "NTRF",
		"DISTRIBUTION", "", pfDay+5*86400)
	seedMT940Line(t, db, "mt940:"+pfIBAN+":REF77", pfIBAN, "CHF", "C", "110", "NTRF",
		"CREDIT EXAMPLE HOLDINGS AG", "", pfDay+6*86400)
	seedMT940Line(t, db, "mt940:"+pfIBAN+":REF78", pfIBAN, "CHF", "RD", "300", "NTRF",
		"CAPITAL CALL BLUEFIELD GROWTH PARTNERS", "", pfDay+7*86400)
	seedMT940Line(t, db, "mt940:"+pfIBAN+":REF79", pfIBAN, "CHF", "C", "140", "NTRF",
		"UEBERTRAG NORTHWIND PRIVATE MARKETS II", "", pfDay+8*86400)
	seedMT940Line(t, db, "mt940:"+pfIBAN+":REF70", pfIBAN, "CHF", "C", "333", "NTRF",
		"DISTRIBUTION NORTHWIND PRIVATE MARKETS II", "", pfDay+9*86400)

	r := newWebTxFixture(t)
	seedWebAccount(t, r, pfIBAN)
	seedWebTxRaw(t, r, "stmt:00000000000000p1", pfIBAN, pfDay+10*86400, "CHF", -400,
		"E-BANKING PAYMENT ORDER", statementPayload("E-BANKING PAYMENT ORDER", "CAPITAL CALL",
			"NORTHWIND PRIVATE", "MARKETS II"))
	seedWebTxRaw(t, r, "WX0000000071", pfIBAN, pfDay+11*86400, "CHF", 250, "Credit",
		exportPayload("Return of capital", "Northwind Private Markets II", ""))
	seedWebTxRaw(t, r, "WX0000000072", pfIBAN, pfDay+12*86400, "CHF", -90, "e-banking payment order",
		exportPayload("Example Shop", "", ""))
	// A payment between own accounts whose text names the fund: its
	// counter account is an own one.
	seedWebAccount(t, r, vetoAcctB)
	seedWebTxRaw(t, r, "stmt:00000000000000p2", pfIBAN, pfDay+13*86400, "CHF", -60,
		"E-BANKING PAYMENT ORDER", `{"source":"account_statement_pdf","booking_type":"E-BANKING PAYMENT ORDER",`+
			`"counter_account":"`+vetoAcctB+`","continuation":["CAPITAL CALL NORTHWIND PRIVATE MARKETS II"]}`)
	// A statement payment whose narrative names an internal transfer, and
	// the mirror on another own account that pairs REF70 by the offset
	// veto.
	seedWebTxRaw(t, r, "stmt:00000000000000p4", pfIBAN, pfDay+14*86400, "CHF", -70,
		"E-BANKING PAYMENT ORDER", statementPayload("E-BANKING PAYMENT ORDER",
			"UEBERTRAG NORTHWIND PRIVATE MARKETS II"))
	seedWebAccount(t, r, vetoAcctA)
	seedWebTx(t, r, "VT70", vetoAcctA, pfDay+9*86400, "CHF", -333, true)

	got := drainTx(t, connTransactions(t, r, &psnReader{db: db}, nil))
	for _, tc := range []struct {
		id, isin string
		kind     canonical.TxKind
		amount   string
	}{
		{"mt940:" + pfIBAN + ":REF71", pfFund, canonical.TxKindDistribution, "500"},
		{"mt940:" + pfIBAN + ":REF72", pfFund2, canonical.TxKindBuy, "-300"},
		{"mt940:" + pfIBAN + ":REF73", pfFund, canonical.TxKindDistribution, "-500"},
		{"mt940:" + pfIBAN + ":REF74", "", canonical.TxKindDeposit, "200"},
		{"mt940:" + pfIBAN + ":REF75", "", canonical.TxKindDeposit, "150"},
		{"mt940:" + pfIBAN + ":REF76", "", canonical.TxKindDeposit, "120"},
		{"mt940:" + pfIBAN + ":REF77", "", canonical.TxKindDeposit, "110"},
		{"mt940:" + pfIBAN + ":REF78", pfFund2, canonical.TxKindBuy, "300"},
		{"mt940:" + pfIBAN + ":REF79", "", canonical.TxKindDeposit, "140"},
		{"mt940:" + pfIBAN + ":REF70", "", canonical.TxKindDeposit, "333"},
		{"stmt:00000000000000p1@" + pfIBAN, pfFund, canonical.TxKindBuy, "-400"},
		{"WX0000000071@" + pfIBAN, pfFund, canonical.TxKindDistribution, "250"},
		{"WX0000000072@" + pfIBAN, "", canonical.TxKindWithdrawal, "-90"},
		{"stmt:00000000000000p2@" + pfIBAN, "", canonical.TxKindWithdrawal, "-60"},
		{"stmt:00000000000000p4@" + pfIBAN, "", canonical.TxKindWithdrawal, "-70"},
	} {
		tx, ok := got[tc.id]
		if !ok {
			t.Errorf("%s is missing from the stream", tc.id)
			continue
		}
		if derefText(tx.InstrumentExternalID) != tc.isin || tx.Kind != tc.kind || derefDecimal(tx.NetAmount) != tc.amount {
			t.Errorf("%s = %s %q %s, want %s %q %s", tc.id, tx.Kind, derefText(tx.InstrumentExternalID),
				derefDecimal(tx.NetAmount), tc.kind, tc.isin, tc.amount)
		}
		if tc.isin != "" && (tx.AssetClass != "" || tx.InstrumentHint != "") {
			t.Errorf("%s states %q %q beside its fund", tc.id, tx.AssetClass, tx.InstrumentHint)
		}
	}
}

// TestTheNameTestReadsWholeNames: a fund is named by its ISIN or by a
// name of two words or more, whole, in any case and spacing; a part of a
// name, or a name inside a longer word, names nothing.
func TestTheNameTestReadsWholeNames(t *testing.T) {
	f := &privateFunds{
		portfolios: accountPortfolios{"A": {"P"}},
		held:       map[string]map[string]bool{"P": {pfFund: true, pfFund2: true}},
		withFunds:  map[string]bool{"A": true},
		names:      map[string][]string{},
	}
	f.addName(pfFund, "Northwind Private Markets II")
	f.addName(pfFund2, "Bluefield")
	for _, tc := range []struct {
		text, want string
	}{
		{"Dist. northwind   PRIVATE-markets ii / Q3", pfFund},
		{"Payment re " + pfFund2, pfFund2},
		{"NORTHWIND PRIVATE MARKETS", ""},
		{"NORTHWIND PRIVATE MARKETS III", ""},
		{"BLUEFIELD", ""},
		{"Northwind Private Markets II", ""},
	} {
		account := "A"
		if tc.text == "Northwind Private Markets II" {
			account = "B" // an account in no portfolio that holds it
		}
		got, _ := f.named(account, tc.text)
		if got != tc.want {
			t.Errorf("named(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

// TestALongerNameNamesItsFund: of two funds whose names one contains the
// other, a text that holds the shorter only inside the longer names the
// longer one's fund. A text that also holds the shorter on its own names
// both, so neither; an ISIN names its fund as ever.
func TestALongerNameNamesItsFund(t *testing.T) {
	f := &privateFunds{
		portfolios: accountPortfolios{"A": {"P"}},
		held:       map[string]map[string]bool{"P": {pfFund: true, pfFund2: true}},
		withFunds:  map[string]bool{"A": true},
		names:      map[string][]string{},
	}
	f.addName(pfFund, "Northwind Partners")
	f.addName(pfFund2, "Northwind Partners II")
	for _, tc := range []struct{ text, want string }{
		{"Capital call Northwind Partners II", pfFund2},
		{"Distribution Northwind Partners", pfFund},
		{"Northwind Partners and Northwind Partners II", ""},
		{"Northwind Partners II " + pfFund, ""},
		{"Northwind Partners " + pfFund2, ""},
	} {
		if got, _ := f.named("A", tc.text); got != tc.want {
			t.Errorf("named(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

// fundAction is a corporate-action confirmation on the fund, in the shape
// the collector stores, whose cash option credits (CRED) or debits (DEBT)
// the posted amount to the cash account on the day.
func fundAction(seme, caev, isin, crdb, posted string, day int64) string {
	fields := [][2]string{
		{"16R", "GENL"}, {"20C", ":SEME//" + seme}, {"23G", "NEWM"}, {"22F", ":CAEV//" + caev}, {"16S", "GENL"},
		{"16R", "USECU"}, {"35B", "ISIN " + isin + "\nEXAMPLE FUND"},
		{"16R", "ACCTINFO"}, {"97A", ":SAFE//" + pfSafe}, {"16S", "ACCTINFO"}, {"16S", "USECU"},
		{"16R", "CACONF"}, {"22H", ":CAOP//CASH"},
		{"16R", "CASHMOVE"}, {"22H", ":CRDB//" + crdb}, {"97A", ":CASH//" + pfAcctID},
		{"19B", ":PSTA//" + posted}, {"19B", ":GRSS//" + posted}, {"98A", ":PAYD//" + swiftDate(day)},
		{"16S", "CASHMOVE"}, {"16S", "CACONF"},
	}
	b, _ := json.Marshal(map[string]any{"caev": caev, "isin": isin, "safekeeping": pfSafe, "seme": seme, "fields": fields})
	return string(b)
}

// TestAFundCorporateActionPairsWithItsCashLine: a fund's corporate action
// that moves cash pairs with the one line on its cash account, payment
// day, signed posted amount and currency, whatever the line's text says.
// A credit is the fund's distribution, a debit a call. A line already on
// the fund keeps what it is; two lines that answer one action both stay
// as they are. The action keeps its marker.
func TestAFundCorporateActionPairsWithItsCashLine(t *testing.T) {
	db := newPrivateFundSilver(t)
	if _, err := db.Exec(`UPDATE instruments SET payload = json_set(payload, '$.InstrIdtfr',
            json_object('Valor', '7200072')) WHERE isin = ?`, pfFund2); err != nil {
		t.Fatal(err)
	}
	day := pfDay + 20*86400
	seedMT940Line(t, db, "mt940:"+pfIBAN+":REF81", pfIBAN, "CHF", "C", "800", "NTRF", "CREDIT", "", day)
	seedCorporateAction(t, db, "mt566:FUND81", fundAction("FUND81", "CAPD", pfFund, "CRED", "CHF800,", day), day)
	seedMT940Line(t, db, "mt940:"+pfIBAN+":REF82", pfIBAN, "CHF", "D", "650", "NTRF", "PAYMENT", "", day)
	seedCorporateAction(t, db, "mt566:FUND82", fundAction("FUND82", "OTHR", pfFund, "DEBT", "CHF650,", day), day)
	seedMT940Line(t, db, "mt940:"+pfIBAN+":REF83", pfIBAN, "CHF", "C", "430", "NTRF", "CREDIT", "", day)
	seedMT940Line(t, db, "mt940:"+pfIBAN+":REF84", pfIBAN, "CHF", "C", "430", "NTRF", "CREDIT", "", day)
	seedCorporateAction(t, db, "mt566:FUND83", fundAction("FUND83", "OTHR", pfFund, "CRED", "CHF430,", day), day)

	r := newWebTxFixture(t)
	seedWebAccount(t, r, pfIBAN)
	webDay := day + 30*86400
	seedWebTxRaw(t, r, "stmt:00000000000000p3", pfIBAN, webDay, "CHF", -720, "CAPITAL CALL",
		`{"source":"account_statement_pdf","booking_type":"CAPITAL CALL","counter_account":null,`+
			`"security_valor":"7200072","continuation":["CAPITAL CALL"]}`)
	seedCorporateAction(t, db, "mt566:FUND84", fundAction("FUND84", "OTHR", pfFund2, "DEBT", "CHF720,", webDay), webDay)

	got := drainTx(t, connTransactions(t, r, &psnReader{db: db}, nil))
	for _, tc := range []struct {
		id, isin string
		kind     canonical.TxKind
		amount   string
	}{
		{"mt940:" + pfIBAN + ":REF81", pfFund, canonical.TxKindDistribution, "800"},
		{"mt940:" + pfIBAN + ":REF82", pfFund, canonical.TxKindBuy, "-650"},
		{"mt940:" + pfIBAN + ":REF83", "", canonical.TxKindDeposit, "430"},
		{"mt940:" + pfIBAN + ":REF84", "", canonical.TxKindDeposit, "430"},
		{"stmt:00000000000000p3@" + pfIBAN, pfFund2, canonical.TxKindBuy, "-720"},
	} {
		tx, ok := got[tc.id]
		if !ok {
			t.Errorf("%s is missing from the stream", tc.id)
			continue
		}
		if derefText(tx.InstrumentExternalID) != tc.isin || tx.Kind != tc.kind || derefDecimal(tx.NetAmount) != tc.amount {
			t.Errorf("%s = %s %q %s, want %s %q %s", tc.id, tx.Kind, derefText(tx.InstrumentExternalID),
				derefDecimal(tx.NetAmount), tc.kind, tc.isin, tc.amount)
		}
	}
	for _, id := range []string{"mt566:FUND81", "mt566:FUND82", "mt566:FUND83", "mt566:FUND84"} {
		if tx, ok := got[id]; !ok || tx.Kind != canonical.TxKindCorporateAction || tx.NetAmount != nil {
			t.Errorf("%s = %+v, want its marker with no amount", id, tx)
		}
		if _, ok := got[id+cashLegIDSuffix]; ok {
			t.Errorf("%s booked a cash leg of its own", id)
		}
	}
}

// TestAFundCorporateActionPairsOnlyWhereItStands: an action a
// cancellation withdraws pairs with no line, nor does the cancellation;
// two standing actions on one key pair where they name one fund, and not
// where they name two. On the web, a row whose text names nothing pairs
// with the action on its key; a row the valor index links to another
// security keeps it; and a statement copy the reconcile table folds into
// its export row does not count as a second line.
func TestAFundCorporateActionPairsOnlyWhereItStands(t *testing.T) {
	db := newPrivateFundSilver(t)
	if _, err := db.Exec(`UPDATE instruments SET payload = json_set(payload, '$.InstrIdtfr',
            json_object('Valor', '7500075')) WHERE isin = ?`, pfStock); err != nil {
		t.Fatal(err)
	}
	day := pfDay + 60*86400
	// cancel is a cancellation of the confirmation prev, which names it.
	cancel := func(seme, prev, isin, crdb, posted string, at int64) string {
		a := strings.Replace(fundAction(seme, "OTHR", isin, crdb, posted, at), `["23G","NEWM"]`, `["23G","CANC"]`, 1)
		return strings.Replace(a, `["16S","GENL"]`, `["16R","LINK"],["20C",":PREV//`+prev+`"],["16S","LINK"],["16S","GENL"]`, 1)
	}
	seedMT940Line(t, db, "mt940:"+pfIBAN+":REF85", pfIBAN, "CHF", "C", "333", "NTRF", "CREDIT", "", day)
	seedCorporateAction(t, db, "mt566:FUND85", fundAction("FUND85", "OTHR", pfFund, "CRED", "CHF333,", day), day)
	seedCorporateAction(t, db, "mt566:FUND86", cancel("FUND86", "FUND85", pfFund, "CRED", "CHF333,", day), day)
	seedMT940Line(t, db, "mt940:"+pfIBAN+":REF87", pfIBAN, "CHF", "C", "444", "NTRF", "CREDIT", "", day)
	seedCorporateAction(t, db, "mt566:FUND87", fundAction("FUND87", "OTHR", pfFund, "CRED", "CHF444,", day), day)
	seedCorporateAction(t, db, "mt566:FUND88", fundAction("FUND88", "CAPD", pfFund, "CRED", "CHF444,", day), day)
	seedMT940Line(t, db, "mt940:"+pfIBAN+":REF89", pfIBAN, "CHF", "C", "555", "NTRF", "CREDIT", "", day)
	seedCorporateAction(t, db, "mt566:FUND89", fundAction("FUND89", "OTHR", pfFund, "CRED", "CHF555,", day), day)
	seedCorporateAction(t, db, "mt566:FUND90", fundAction("FUND90", "OTHR", pfFund2, "CRED", "CHF555,", day), day)

	r := newWebTxFixture(t)
	seedWebAccount(t, r, pfIBAN)
	seedRailEraAnchor(t, r)
	webDay := day + 30*86400
	seedWebTxRaw(t, r, "WX0000000091", pfIBAN, webDay, "CHF", 910, "Credit", exportPayload("Credit", "", ""))
	seedCorporateAction(t, db, "mt566:FUND91", fundAction("FUND91", "OTHR", pfFund2, "CRED", "CHF910,", webDay), webDay)
	seedWebTxRaw(t, r, "stmt:00000000000000s2", pfIBAN, webDay+86400, "CHF", -920, "CAPITAL CALL",
		`{"source":"account_statement_pdf","booking_type":"CAPITAL CALL","counter_account":null,`+
			`"security_valor":"7500075","continuation":["CAPITAL CALL"]}`)
	seedCorporateAction(t, db, "mt566:FUND92", fundAction("FUND92", "OTHR", pfFund, "DEBT", "CHF920,",
		webDay+86400), webDay+86400)
	seedWebTxRaw(t, r, "stmt:00000000000000s3", pfIBAN, webDay+2*86400, "CHF", -930, "E-BANKING PAYMENT ORDER",
		statementPayload("E-BANKING PAYMENT ORDER", "EXAMPLE PAYMENT"))
	seedWebTxRaw(t, r, "WX0000000093", pfIBAN, webDay+2*86400, "CHF", -930, "e-banking payment order",
		exportPayload("Example payment", "", ""))
	seedCorporateAction(t, db, "mt566:FUND93", fundAction("FUND93", "OTHR", pfFund, "DEBT", "CHF930,",
		webDay+2*86400), webDay+2*86400)

	got := drainTx(t, connTransactions(t, r, &psnReader{db: db}, nil))
	if _, ok := got["stmt:00000000000000s3@"+pfIBAN]; ok {
		t.Errorf("the statement copy s3 was not folded into its export row")
	}
	for _, tc := range []struct {
		id, isin string
		kind     canonical.TxKind
	}{
		{"mt940:" + pfIBAN + ":REF85", "", canonical.TxKindDeposit},
		{"mt940:" + pfIBAN + ":REF87", pfFund, canonical.TxKindDistribution},
		{"mt940:" + pfIBAN + ":REF89", "", canonical.TxKindDeposit},
		{"WX0000000091@" + pfIBAN, pfFund2, canonical.TxKindDistribution},
		{"stmt:00000000000000s2@" + pfIBAN, pfStock, canonical.TxKindBuy},
		{"WX0000000093@" + pfIBAN, pfFund, canonical.TxKindBuy},
	} {
		tx, ok := got[tc.id]
		if !ok {
			t.Errorf("%s is missing from the stream", tc.id)
			continue
		}
		if derefText(tx.InstrumentExternalID) != tc.isin || tx.Kind != tc.kind {
			t.Errorf("%s = %s %q, want %s %q", tc.id, tx.Kind, derefText(tx.InstrumentExternalID), tc.kind, tc.isin)
		}
	}
}

// TestACashRowNamingOneFundByValorTakesTheFund: a fund's valor names it
// in a row's free text, and in a valor the statement promoted that the
// valor index drops because another portfolio's security states it too.
// The search reads only the funds of the row's own portfolios. A payment
// naming the valor of no fund stays as it is.
func TestACashRowNamingOneFundByValorTakesTheFund(t *testing.T) {
	db := newPrivateFundSilver(t)
	for _, isin := range []string{pfFund, pfFundAway} {
		if _, err := db.Exec(`UPDATE instruments SET payload = json_set(payload, '$.InstrIdtfr',
                json_object('Valor', '07400074')) WHERE isin = ?`, isin); err != nil {
			t.Fatal(err)
		}
	}
	r := newWebTxFixture(t)
	seedWebAccount(t, r, pfIBAN)
	day := pfDay + 40*86400
	seedWebTxRaw(t, r, "stmt:00000000000000v1", pfIBAN, day, "CHF", -510,
		"E-BANKING PAYMENT ORDER", statementPayload("E-BANKING PAYMENT ORDER", "CAPITAL CALL VN 7400074"))
	seedWebTxRaw(t, r, "stmt:00000000000000v2", pfIBAN, day+86400, "CHF", -520, "CAPITAL CALL",
		`{"source":"account_statement_pdf","booking_type":"CAPITAL CALL","counter_account":null,`+
			`"security_valor":"7400074","continuation":["CAPITAL CALL"]}`)
	seedWebTxRaw(t, r, "stmt:00000000000000v3", pfIBAN, day+2*86400, "CHF", -530,
		"E-BANKING PAYMENT ORDER", statementPayload("E-BANKING PAYMENT ORDER", "INVOICE 7400075"))
	got := drainTx(t, connTransactions(t, r, &psnReader{db: db}, nil))
	for _, tc := range []struct {
		id, isin string
		kind     canonical.TxKind
	}{
		{"stmt:00000000000000v1@" + pfIBAN, pfFund, canonical.TxKindBuy},
		{"stmt:00000000000000v2@" + pfIBAN, pfFund, canonical.TxKindBuy},
		{"stmt:00000000000000v3@" + pfIBAN, "", canonical.TxKindWithdrawal},
	} {
		tx := got[tc.id]
		if derefText(tx.InstrumentExternalID) != tc.isin || tx.Kind != tc.kind {
			t.Errorf("%s = %s %q, want %s %q", tc.id, tx.Kind, derefText(tx.InstrumentExternalID), tc.kind, tc.isin)
		}
		if tc.isin != "" && tx.InstrumentHint != "" {
			t.Errorf("%s keeps the hint %q beside its fund", tc.id, tx.InstrumentHint)
		}
	}
}

// TestAWebOnlySwissFundIsNamedByItsISINsValor: a fund only the printed
// statements describe, with no valor printed beside it, is named by the
// valor its Swiss ISIN carries.
func TestAWebOnlySwissFundIsNamedByItsISINsValor(t *testing.T) {
	const isin = "CH0007700771"
	db := newPrivateFundSilver(t)
	r := newDividendWeb(t, custodyPosition{pfPortfolio, isin, "", "Example Private Equity Fund"})
	seedWebAccount(t, r, pfIBAN)
	seedWebTxRaw(t, r, "stmt:00000000000000w1", pfIBAN, pfDay+50*86400, "CHF", -540,
		"E-BANKING PAYMENT ORDER", statementPayload("E-BANKING PAYMENT ORDER", "CAPITAL CALL VN 770077"))
	got := drainTx(t, connTransactions(t, r, &psnReader{db: db}, nil))
	if tx := got["stmt:00000000000000w1@"+pfIBAN]; derefText(tx.InstrumentExternalID) != isin ||
		tx.Kind != canonical.TxKindBuy {
		t.Errorf("w1 = %s %q, want a buy of %s", tx.Kind, derefText(tx.InstrumentExternalID), isin)
	}
}
