package ubs

import (
	"database/sql"
	"encoding/json"
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
// movement is the record of the dividend — the posted figure, on the
// payment day, with the gross and the tax beside it.
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
		leg.NetAmount == nil || leg.NetAmount.String() != "1234560" {
		t.Errorf("cash leg = %s %s %v, want dividend JPY 1234560", leg.Kind, leg.Currency, leg.NetAmount)
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
		"corporate_action": caEvent, "caev": "DVCA",
		"gross_amount": "1300000", "gross_currency": "JPY", "tax_amount": "65440", "tax_currency": "JPY",
	} {
		if got := payloadField(t, leg, key); got != want {
			t.Errorf("cash leg payload %s = %q, want %q", key, got, want)
		}
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
		t.Errorf("cash leg = %s %v, want interest 1000.5", leg.Kind, leg.NetAmount)
	}
}
