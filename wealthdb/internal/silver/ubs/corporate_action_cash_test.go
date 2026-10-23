package ubs

import (
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// Every id, name and figure below is invented.
const (
	caEvent   = "mt566:0000KE0000001"
	caISIN    = "LU0000000000"
	caPayDay  = int64(1781654400) // 2026-06-17
	caCashDay = "20260617"
)

// caFields is an MT566 confirmation paying cash, in the shape the
// collector stores: the tag sequence verbatim.
func caFields(caev, option, crdb, cashAcct, posted string) string {
	fields := [][2]string{
		{"16R", "GENL"}, {"20C", ":SEME//0000KE0000001"}, {"22F", ":CAEV//" + caev}, {"16S", "GENL"},
		{"16R", "USECU"}, {"97A", ":SAFE//00000000000000S1"},
		{"35B", "ISIN " + caISIN + "\nSOME ETF JPY D\n?UBS:+EP:0000000"}, {"16S", "USECU"},
		{"16R", "CACONF"}, {"22H", ":CAOP//" + option},
		{"16R", "CASHMOVE"}, {"22H", ":CRDB//" + crdb}, {"97A", ":CASH//" + cashAcct},
		{"19B", ":PSTA//" + posted}, {"19B", ":GRSS//JPY1300000,"}, {"19B", ":TAXR//JPY65440,"},
		{"98A", ":POST//" + caCashDay}, {"98A", ":VALU//" + caCashDay}, {"98A", ":PAYD//" + caCashDay},
		{"16S", "CASHMOVE"}, {"16S", "CACONF"},
	}
	b, _ := json.Marshal(map[string]any{
		"caev": caev, "isin": caISIN, "safekeeping": "00000000000000S1", "seme": "0000KE0000001", "fields": fields,
	})
	return string(b)
}

func seedCorporateAction(t *testing.T, db *sql.DB, eventID, payload string, day int64) {
	t.Helper()
	if _, err := db.Exec(`
        INSERT INTO events (event_external_id, timestamp, relationship_id,
            account_external_id, kind, currency_iso, payload)
        VALUES (?, ?, 'R1', '00000000000000S1', 'corporate_action_confirmation', NULL, ?)`,
		eventID, day, payload); err != nil {
		t.Fatalf("seed corporate action: %v", err)
	}
}

func newCorporateActionSilver(t *testing.T, payload string) *sql.DB {
	t.Helper()
	_, db := newFixtureSilver(t)
	seedMirrorCashAccount(t, db, mirrorJPYIBAN, mirrorJPYAcctID, "P1", "JPY")
	seedCorporateAction(t, db, caEvent, payload, caPayDay+86400)
	return db
}

// TestADividendPaidIntoAnUncoveredAccountIsBookedFromItsConfirmation: the
// feed never speaks for the JPY account, so the confirmation's cash
// movement is the record of the dividend — on the payment day, at the
// gross, with the tax withheld as its own row and the posted figure in
// the payload.
func TestADividendPaidIntoAnUncoveredAccountIsBookedFromItsConfirmation(t *testing.T) {
	got := psnRowsWith(t, newCorporateActionSilver(t, caFields("DVCA", "CASH", "CRED", mirrorJPYAcctID, "JPY1234560,")), psnHints{})
	if _, ok := got[caEvent]; !ok {
		t.Error("the corporate-action marker itself is no longer emitted")
	}
	leg, ok := got[caEvent+":cash"]
	if !ok {
		t.Fatal("no cash leg was booked")
	}
	if leg.AccountExternalID != mirrorJPYIBAN {
		t.Errorf("cash leg account = %q, want the cash account's IBAN", leg.AccountExternalID)
	}
	if leg.Kind != canonical.TxKindDividend || leg.Currency != "JPY" ||
		leg.NetAmount == nil || leg.NetAmount.String() != "1300000" {
		t.Errorf("cash leg = %s %s %v, want dividend JPY 1300000, the gross", leg.Kind, leg.Currency, leg.NetAmount)
	}
	if leg.GrossAmount == nil || leg.GrossAmount.String() != "1300000" {
		t.Errorf("gross = %v, want 1300000", leg.GrossAmount)
	}
	if leg.OccurredAt != caPayDay {
		t.Errorf("cash leg day = %d, want the payment day %d", leg.OccurredAt, caPayDay)
	}
	if leg.InstrumentExternalID == nil || *leg.InstrumentExternalID != caISIN {
		t.Errorf("instrument = %v, want %s", leg.InstrumentExternalID, caISIN)
	}
	if leg.Description == nil || *leg.Description != "SOME ETF JPY D" {
		t.Errorf("description = %v, want the security's name", leg.Description)
	}
	for key, want := range map[string]string{
		"corporate_action": caEvent, "caev": "DVCA", "posted_amount": "1234560", "posted_currency": "JPY",
		"gross_amount": "1300000", "gross_currency": "JPY", "tax_amount": "65440", "tax_currency": "JPY",
	} {
		if got := payloadField(t, leg, key); got != want {
			t.Errorf("cash leg payload %s = %q, want %q", key, got, want)
		}
	}
	tax, ok := got[caEvent+":cash"+withholdingIDSuffix]
	if !ok {
		t.Fatal("no withholding row was booked beside the cash leg")
	}
	wantWithholding(t, leg, tax, "-65440")
	if tax.Description == nil || *tax.Description != "Withholding tax; SOME ETF JPY D" {
		t.Errorf("withholding description = %v, want it to name the withholding and the security", tax.Description)
	}
	if sum := leg.NetAmount.Add(*tax.NetAmount); sum.String() != "1234560" {
		t.Errorf("dividend + withholding = %s, want the 1234560 posted", sum)
	}
}

// TestAConfirmationWithNoTaxBooksNoWithholding: a cash leg whose
// confirmation states the tax as zero reads the posted amount, which is
// the gross, and books no tax row.
func TestAConfirmationWithNoTaxBooksNoWithholding(t *testing.T) {
	payload := strings.ReplaceAll(caFields("DVCA", "CASH", "CRED", mirrorJPYAcctID, "JPY1300000,"),
		":TAXR//JPY65440,", ":TAXR//JPY0,")
	got := psnRowsWith(t, newCorporateActionSilver(t, payload), psnHints{})
	leg, ok := got[caEvent+":cash"]
	if !ok {
		t.Fatal("no cash leg was booked")
	}
	if leg.NetAmount.String() != "1300000" || leg.GrossAmount == nil || leg.GrossAmount.String() != "1300000" {
		t.Errorf("cash leg = %v gross %v, want 1300000 both", leg.NetAmount, leg.GrossAmount)
	}
	if _, ok := got[caEvent+":cash"+withholdingIDSuffix]; ok {
		t.Error("a withholding row was booked for a confirmation that withheld nothing")
	}
}

// TestNoCashLegWhereTheFeedSpeaksForTheAccount: the statement line is the
// row, and a second one would double the income.
func TestNoCashLegWhereTheFeedSpeaksForTheAccount(t *testing.T) {
	db := newCorporateActionSilver(t, caFields("DVCA", "CASH", "CRED", mirrorJPYAcctID, "JPY1234560,"))
	seedConversionRow(t, db, "mt940:"+mirrorJPYIBAN+":REF0000009", mirrorJPYIBAN, "1", "D", "JPY", "FEE", "REF0000009", caPayDay-86400)
	got := psnRowsWith(t, db, psnHints{})
	if _, ok := got[caEvent+":cash"]; ok {
		t.Error("a cash leg was booked beside the feed's own line")
	}
}

// TestOnlyCashPayingEventsBookALeg: an event settled in securities, a
// debit, or an indicator that does not settle the kind books nothing.
func TestOnlyCashPayingEventsBookALeg(t *testing.T) {
	for name, payload := range map[string]string{
		"securities option": caFields("DVOP", "SECU", "CRED", mirrorJPYAcctID, "JPY1234560,"),
		"a debit":           caFields("DVCA", "CASH", "DEBT", mirrorJPYAcctID, "JPY1234560,"),
		"another indicator": caFields("OTHR", "CASH", "CRED", mirrorJPYAcctID, "JPY1234560,"),
	} {
		got := psnRowsWith(t, newCorporateActionSilver(t, payload), psnHints{})
		if _, ok := got[caEvent+":cash"]; ok {
			t.Errorf("%s: a cash leg was booked", name)
		}
	}
}

// TestInterestPaidIntoAnUncoveredAccountIsInterest pins the other settled
// indicator.
func TestInterestPaidIntoAnUncoveredAccountIsInterest(t *testing.T) {
	got := psnRowsWith(t, newCorporateActionSilver(t, caFields("INTR", "CASH", "CRED", mirrorJPYAcctID, "JPY1000,5")), psnHints{})
	leg, ok := got[caEvent+":cash"]
	if !ok {
		t.Fatal("no cash leg was booked")
	}
	if leg.Kind != canonical.TxKindInterest || leg.NetAmount.String() != "1000.5" {
		t.Errorf("cash leg = %s %v, want interest 1000.5, the posted amount", leg.Kind, leg.NetAmount)
	}
	if _, ok := got[caEvent+":cash"+withholdingIDSuffix]; ok {
		t.Error("interest booked a withholding row; it keeps the posted amount")
	}
}

// The bank withdraws a confirmation by a cancellation (CANC) or a
// reversal (REVR). Every reference below is invented.
const (
	caSEME    = "0000KE0000001"
	caCorp    = "0000CA0000001"
	caCancel  = "mt566:0000KE0000002"
	caCorrect = "mt566:0000KE0000003"
)

// confirmationAs is a confirmation's payload under its own reference
// seme, with its general block stating the event reference, the message
// function and the confirmation it names in its linkage block, each where
// given, in the order MT566 places them. Any it stated before are left
// out.
func confirmationAs(t *testing.T, payload, seme, corp, function, prev string) string {
	t.Helper()
	var c struct {
		CAEV        string      `json:"caev"`
		ISIN        string      `json:"isin"`
		Safekeeping string      `json:"safekeeping"`
		SEME        string      `json:"seme"`
		Fields      [][2]string `json:"fields"`
	}
	if err := json.Unmarshal([]byte(payload), &c); err != nil {
		t.Fatal(err)
	}
	var (
		fields [][2]string
		inLink bool
	)
	for _, f := range c.Fields {
		switch {
		case f == [2]string{"16R", "LINK"}, f == [2]string{"16S", "LINK"}:
			inLink = f[0] == "16R"
			continue
		case inLink, f[0] == "23G", f[0] == "20C" && strings.HasPrefix(f[1], ":CORP//"):
			continue
		}
		switch f {
		case [2]string{"20C", ":SEME//" + c.SEME}:
			if corp != "" {
				fields = append(fields, [2]string{"20C", ":CORP//" + corp})
			}
			fields = append(fields, [2]string{"20C", ":SEME//" + seme})
			if function != "" {
				fields = append(fields, [2]string{"23G", function})
			}
		case [2]string{"16S", "GENL"}:
			if prev != "" {
				fields = append(fields, [2]string{"16R", "LINK"}, [2]string{"20C", ":PREV//" + prev}, [2]string{"16S", "LINK"})
			}
			fields = append(fields, f)
		default:
			fields = append(fields, f)
		}
	}
	c.SEME, c.Fields = seme, fields
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestAWithdrawnConfirmationBooksNoCashLeg: a cancellation or a reversal
// withdraws the confirmation it names, else the one of its event, custody
// account, security and amount posted. Neither books a cash leg or a tax
// row, nor does the one withdrawn. A corrected confirmation books its own.
// Every confirmation keeps its marker.
func TestAWithdrawnConfirmationBooksNoCashLeg(t *testing.T) {
	paid := caFields("DVCA", "CASH", "CRED", mirrorJPYAcctID, "JPY1234560,")
	for _, tc := range []struct {
		name, function, prev string
		corrected            bool
	}{
		{"a cancellation naming it", "CANC", caSEME, false},
		{"a reversal naming it", "REVR", caSEME, false},
		{"a cancellation naming none", "CANC", "", false},
		{"a cancellation and the corrected confirmation", "CANC", caSEME, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newCorporateActionSilver(t, confirmationAs(t, paid, caSEME, caCorp, "NEWM", ""))
			seedCorporateAction(t, db, caCancel, confirmationAs(t, paid, "0000KE0000002", caCorp, tc.function, tc.prev), caPayDay+86400)
			if tc.corrected {
				seedCorporateAction(t, db, caCorrect, confirmationAs(t, paid, "0000KE0000003", caCorp, "NEWM", ""), caPayDay+86400)
			}
			got := psnRowsWith(t, db, psnHints{})
			for _, id := range []string{caEvent, caCancel} {
				if _, ok := got[id]; !ok {
					t.Errorf("%s: the marker is no longer emitted", id)
				}
				if leg, ok := got[id+":cash"]; ok {
					t.Errorf("%s: a cash leg of %s was booked", id, derefDecimal(leg.NetAmount))
				}
				if _, ok := got[id+":cash"+withholdingIDSuffix]; ok {
					t.Errorf("%s: a withholding row was booked", id)
				}
			}
			leg, ok := got[caCorrect+":cash"]
			if ok != tc.corrected {
				t.Fatalf("the corrected confirmation booked a leg: %v, want %v", ok, tc.corrected)
			}
			if ok && derefDecimal(leg.NetAmount) != "1300000" {
				t.Errorf("corrected leg = %s, want its gross 1300000", derefDecimal(leg.NetAmount))
			}
		})
	}
}

// TestAWithdrawalWithdrawsOnlyWhatItNames: a cancellation naming a
// reference silver does not hold, or naming none and stating another
// event or amount, leaves the confirmation standing.
func TestAWithdrawalWithdrawsOnlyWhatItNames(t *testing.T) {
	paid := caFields("DVCA", "CASH", "CRED", mirrorJPYAcctID, "JPY1234560,")
	for name, cancellation := range map[string]string{
		"another reference": confirmationAs(t, paid, "0000KE0000002", caCorp, "CANC", "0000KE0000099"),
		"another event":     confirmationAs(t, paid, "0000KE0000002", "0000CA0000002", "CANC", ""),
		"another amount": confirmationAs(t, caFields("DVCA", "CASH", "CRED", mirrorJPYAcctID, "JPY1234561,"),
			"0000KE0000002", caCorp, "CANC", ""),
	} {
		t.Run(name, func(t *testing.T) {
			db := newCorporateActionSilver(t, confirmationAs(t, paid, caSEME, caCorp, "", ""))
			seedCorporateAction(t, db, caCancel, cancellation, caPayDay+86400)
			got := psnRowsWith(t, db, psnHints{})
			if leg := got[caEvent+":cash"]; derefDecimal(leg.NetAmount) != "1300000" {
				t.Errorf("cash leg = %s, want the standing confirmation's 1300000", derefDecimal(leg.NetAmount))
			}
			if _, ok := got[caCancel+":cash"]; ok {
				t.Error("the cancellation booked a cash leg")
			}
		})
	}
}

// TestTheMessageFunction pins how a confirmation states its function and
// the confirmation it names: a subfunction is no other function, and a
// reference names a confirmation only in the linkage block. A function
// neither new nor a withdrawal makes no confirmation that stands, and
// withdraws nothing.
func TestTheMessageFunction(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		fields                 [][2]string
		function, prev         string
		isNew, withdrawsOthers bool
	}{
		{"none stated", nil, "", "", true, false},
		{"a new confirmation", [][2]string{{"23G", "NEWM"}}, "NEWM", "", true, false},
		{"a duplicate", [][2]string{{"23G", "NEWM/DUPL"}}, "NEWM", "", true, false},
		{"a cancellation", [][2]string{{"23G", "CANC"}, {"16R", "LINK"}, {"20C", ":PREV//0000KE0000001"}, {"16S", "LINK"}},
			"CANC", "0000KE0000001", false, true},
		{"a reference outside the linkage block", [][2]string{{"23G", "REVR"}, {"20C", ":PREV//0000KE0000001"}},
			"REVR", "", false, true},
		{"an unknown function", [][2]string{{"23G", "XXXX"}}, "XXXX", "", false, false},
	} {
		m := parseCorporateAction(corporateActionFields{Fields: tc.fields})
		if m.function != tc.function || m.prev != tc.prev {
			t.Errorf("%s: function %q, prev %q; want %q, %q", tc.name, m.function, m.prev, tc.function, tc.prev)
		}
		if isNewConfirmation(m.function) != tc.isNew || isWithdrawal(m.function) != tc.withdrawsOthers {
			t.Errorf("%s: new %v, a withdrawal %v; want %v, %v", tc.name,
				isNewConfirmation(m.function), isWithdrawal(m.function), tc.isNew, tc.withdrawsOthers)
		}
	}
}

// TestAnUnknownMessageFunctionBooksNothing: a confirmation whose function
// is neither new nor a withdrawal does not stand. It books no cash leg and
// keeps its marker.
func TestAnUnknownMessageFunctionBooksNothing(t *testing.T) {
	paid := caFields("DVCA", "CASH", "CRED", mirrorJPYAcctID, "JPY1234560,")
	got := psnRowsWith(t, newCorporateActionSilver(t, confirmationAs(t, paid, caSEME, caCorp, "XXXX", "")), psnHints{})
	if _, ok := got[caEvent]; !ok {
		t.Error("the marker is no longer emitted")
	}
	if _, ok := got[caEvent+":cash"]; ok {
		t.Error("a confirmation of an unknown function booked a cash leg")
	}
}

// TestAnUnnamedWithdrawalTakesTheNewestBeforeIt: a cancellation that names
// no confirmation withdraws the newest of its posting at or before its own
// time, whichever way the references sort. A confirmation sent again
// after it stands.
func TestAnUnnamedWithdrawalTakesTheNewestBeforeIt(t *testing.T) {
	const (
		original = "mt566:0000KE0000005"
		resent   = "mt566:0000KE0000003"
	)
	paid := caFields("DVCA", "CASH", "CRED", mirrorJPYAcctID, "JPY1234560,")
	_, db := newFixtureSilver(t)
	seedMirrorCashAccount(t, db, mirrorJPYIBAN, mirrorJPYAcctID, "P1", "JPY")
	seedCorporateAction(t, db, original, confirmationAs(t, paid, "0000KE0000005", caCorp, "NEWM", ""), caPayDay)
	seedCorporateAction(t, db, resent, confirmationAs(t, strings.ReplaceAll(paid, caCashDay, swiftDate(caPayDay+86400)),
		"0000KE0000003", caCorp, "NEWM", ""), caPayDay+86400)
	seedCorporateAction(t, db, caCancel, confirmationAs(t, paid, "0000KE0000002", caCorp, "CANC", ""), caPayDay)
	got := psnRowsWith(t, db, psnHints{})
	if _, ok := got[original+":cash"]; ok {
		t.Error("the confirmation the cancellation followed still booked its leg")
	}
	if leg, ok := got[resent+":cash"]; !ok || leg.OccurredAt != caPayDay+86400 {
		t.Errorf("the confirmation sent again booked %v on %d, want its leg on its own payment day", ok, leg.OccurredAt)
	}
}

// TestAnUnnamedWithdrawalBeforeEveryConfirmationTakesTheFirstByID: a
// cancellation that names no confirmation, dated before every one of its
// posting, withdraws the first by id. The other stands.
func TestAnUnnamedWithdrawalBeforeEveryConfirmationTakesTheFirstByID(t *testing.T) {
	const (
		later = "mt566:0000KE0000005"
		first = "mt566:0000KE0000003"
	)
	paid := caFields("DVCA", "CASH", "CRED", mirrorJPYAcctID, "JPY1234560,")
	_, db := newFixtureSilver(t)
	seedMirrorCashAccount(t, db, mirrorJPYIBAN, mirrorJPYAcctID, "P1", "JPY")
	seedCorporateAction(t, db, later, confirmationAs(t, paid, "0000KE0000005", caCorp, "NEWM", ""), caPayDay)
	seedCorporateAction(t, db, first, confirmationAs(t, paid, "0000KE0000003", caCorp, "NEWM", ""), caPayDay+86400)
	seedCorporateAction(t, db, caCancel, confirmationAs(t, paid, "0000KE0000002", caCorp, "CANC", ""), caPayDay-86400)
	got := psnRowsWith(t, db, psnHints{})
	if _, ok := got[first+":cash"]; ok {
		t.Error("the first confirmation by id still booked its leg")
	}
	if _, ok := got[later+":cash"]; !ok {
		t.Error("the other confirmation booked no leg")
	}
}

// TestACashLegLoadsWithItsPaymentDay: a load replaces the rows of its
// window by the day they occur, so a leg loads with the window that holds
// its payment day, whatever day its event is dated. A cancellation dated
// outside that window still withdraws it: withdrawals are read over the
// whole silver.
func TestACashLegLoadsWithItsPaymentDay(t *testing.T) {
	paid := confirmationAs(t, caFields("DVCA", "CASH", "CRED", mirrorJPYAcctID, "JPY1234560,"), caSEME, caCorp, "NEWM", "")
	for _, cancelled := range []bool{false, true} {
		db := newCorporateActionSilver(t, paid)
		if cancelled {
			seedCorporateAction(t, db, caCancel, confirmationAs(t, paid, "0000KE0000002", caCorp, "CANC", caSEME), caPayDay+40*86400)
		}
		got := psnRowsIn(t, db, canonical.Window{Start: caPayDay, End: caPayDay, HasChanges: true}, psnHints{})
		if _, ok := got[caEvent+":cash"]; ok == cancelled {
			t.Errorf("cancelled %v: a leg booked %v, want %v", cancelled, ok, !cancelled)
		}
	}
}

// TestACashLegReadsTheGrossItsRatesState: a confirmation that states no
// gross or tax amount books its leg at the gross its rates state: the
// gross rate per share times the confirmed balance, else the amount
// posted over one less the withholding rate.
func TestACashLegReadsTheGrossItsRatesState(t *testing.T) {
	for name, rates := range map[string][][2]string{
		"the gross rate times the balance": {{"92F", ":GRSS//JPY13,"}, {"93B", ":CONB//UNIT/100000,"}},
		"the withholding rate":             {{"92A", ":TAXR//15,"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := psnRowsWith(t, newCorporateActionSilver(t,
				dividendConfirmation(caSEME, mirrorJPYAcctID, "JPY1105000,", "", "", caPayDay, rates...)), psnHints{})
			leg, tax := got[caEvent+":cash"], got[caEvent+":cash"+withholdingIDSuffix]
			if derefDecimal(leg.NetAmount) != "1300000" || derefDecimal(leg.GrossAmount) != "1300000" {
				t.Errorf("cash leg = %s gross %s, want 1300000 both", derefDecimal(leg.NetAmount), derefDecimal(leg.GrossAmount))
			}
			wantWithholding(t, leg, tax, "-195000")
		})
	}
}

// TestACopyOfACashLegFoldsIntoIt: on an account the feed does not speak
// for, the export is the record of every day, and a confirmation books its
// own cash leg. An export or a statement copy of that dividend moved the
// amount posted, not the gross the leg reads, and folds into the leg. The
// leg keeps the booking, its gross and its tax, and takes what the copies
// say where it is bare.
func TestACopyOfACashLegFoldsIntoIt(t *testing.T) {
	const (
		export    = "EXP0000021"
		statement = "stmt:00000000000000c1"
	)
	exported := func(t *testing.T, r *webReader) {
		seedWebTxRaw(t, r, export, mirrorJPYIBAN, caPayDay, "JPY", 1234560, "Dividend",
			exportPayload("Units Some ETF JPY D; "+caISIN, "Dividend", "Number/Amt. 1300000 JPY; Transaction no. "+export))
	}
	printed := func(t *testing.T, r *webReader) {
		seedWebTxRaw(t, r, statement, mirrorJPYIBAN, caPayDay, "JPY", 1234560, "DIVIDEND",
			statementPayload("DIVIDEND", "VN 1234567 XXXXXXS1", "SOME ETF JPY D"))
	}
	for _, tc := range []struct {
		name   string
		copies []func(*testing.T, *webReader)
	}{
		{"an export copy", []func(*testing.T, *webReader){exported}},
		{"a statement copy", []func(*testing.T, *webReader){printed}},
		{"both", []func(*testing.T, *webReader){exported, printed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newCorporateActionSilver(t, caFields("DVCA", "CASH", "CRED", mirrorJPYAcctID, "JPY1234560,"))
			r := newWebTxFixture(t)
			seedWebAccount(t, r, mirrorJPYIBAN)
			for _, seed := range tc.copies {
				seed(t, r)
			}
			got := mergedText(t, r, db)
			for _, id := range []string{export + "@" + mirrorJPYIBAN, statement + "@" + mirrorJPYIBAN} {
				if _, ok := got[id]; ok {
					t.Errorf("%s: the copy survived beside the cash leg", id)
				}
			}
			leg, tax := got[caEvent+":cash"], got[caEvent+":cash"+withholdingIDSuffix]
			if derefDecimal(leg.NetAmount) != "1300000" {
				t.Errorf("cash leg = %s, want the gross 1300000", derefDecimal(leg.NetAmount))
			}
			wantWithholding(t, leg, tax, "-65440")
			if n := countOnAccountDay(got, mirrorJPYIBAN, caPayDay); n != 2 {
				t.Errorf("%d rows on the account that day, want the leg and its withholding", n)
			}
			if category := derefText(leg.ProviderCategory); !strings.EqualFold(category, "Dividend") {
				t.Errorf("cash leg category = %q, want the copy's booking type", category)
			}
		})
	}
}

// TestWhichRecordOfACashLegStands: an export copy of a cash leg folds into
// it one to one, and only on the leg's own day; an interest leg's copy
// folds too. Where the leg reads no gross and its copy states one, the
// copy is the record, at its gross, and the leg is withheld. A withdrawn
// confirmation books no leg, so its export copy stands.
func TestWhichRecordOfACashLegStands(t *testing.T) {
	const (
		export = "EXP0000031@" + mirrorJPYIBAN
		other  = "mt566:0000KE0000004"
	)
	paid := caFields("DVCA", "CASH", "CRED", mirrorJPYAcctID, "JPY1234560,")
	exportedAt := func(day int64, amount float64, bookingType, d3 string) func(*testing.T, *webReader) {
		return func(t *testing.T, r *webReader) {
			seedWebTxRaw(t, r, "EXP0000031", mirrorJPYIBAN, day, "JPY", amount, bookingType,
				exportPayload("Units Some ETF JPY D; "+caISIN, bookingType, d3))
		}
	}
	dividendCopy := exportedAt(caPayDay, 1234560, "Dividend", "Transaction no. EXP0000031")
	grossCopy := exportedAt(caPayDay, 1234560, "Dividend", "Number/Amt. 1300000 JPY; Transaction no. EXP0000031")
	for _, tc := range []struct {
		name          string
		confirmations map[string]string
		copy          func(*testing.T, *webReader)
		stand, gone   []string
	}{
		{"two legs of one amount, one copy", map[string]string{
			caEvent: paid,
			other:   confirmationAs(t, strings.ReplaceAll(paid, caISIN, "LU0000000001"), "0000KE0000004", "", "", ""),
		}, dividendCopy, []string{caEvent + ":cash", other + ":cash"}, []string{export}},
		{"a copy on another day", map[string]string{caEvent: paid},
			exportedAt(caPayDay+86400, 1234560, "Dividend", "Transaction no. EXP0000031"),
			[]string{caEvent + ":cash", export}, nil},
		{"an interest leg's copy", map[string]string{caEvent: caFields("INTR", "CASH", "CRED", mirrorJPYAcctID, "JPY1000,5")},
			exportedAt(caPayDay, 1000.5, "Credit interest", "Transaction no. EXP0000031"),
			[]string{caEvent + ":cash"}, []string{export}},
		{"a leg reading no gross, a copy stating one", map[string]string{
			caEvent: dividendConfirmation(caSEME, mirrorJPYAcctID, "JPY1234560,", "", "", caPayDay),
		}, grossCopy, []string{export, export + withholdingIDSuffix}, []string{caEvent + ":cash"}},
		{"a withdrawn confirmation's copy", map[string]string{
			caEvent:  confirmationAs(t, paid, caSEME, caCorp, "NEWM", ""),
			caCancel: confirmationAs(t, paid, "0000KE0000002", caCorp, "CANC", caSEME),
		}, grossCopy, []string{export, export + withholdingIDSuffix}, []string{caEvent + ":cash", caCancel + ":cash"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, db := newFixtureSilver(t)
			seedMirrorCashAccount(t, db, mirrorJPYIBAN, mirrorJPYAcctID, "P1", "JPY")
			for id, payload := range tc.confirmations {
				seedCorporateAction(t, db, id, payload, caPayDay)
			}
			r := newWebTxFixture(t)
			seedWebAccount(t, r, mirrorJPYIBAN)
			tc.copy(t, r)
			got := mergedText(t, r, db)
			for _, id := range tc.stand {
				if _, ok := got[id]; !ok {
					t.Errorf("%s: not booked, want it to stand", id)
				}
			}
			for _, id := range tc.gone {
				if _, ok := got[id]; ok {
					t.Errorf("%s: booked, want it folded or withheld", id)
				}
			}
			// The copy folds into the first leg by id, which takes its text.
			if leg := got[caEvent+":cash"]; slices.Contains(tc.gone, export) && isCodeOnly(derefText(leg.ProviderCategory)) {
				t.Errorf("leg category %q, want the copy's booking type", derefText(leg.ProviderCategory))
			}
		})
	}
}
