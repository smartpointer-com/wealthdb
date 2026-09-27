package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// Every id, name and figure below is invented.
const (
	mirrorHolder    = "A. HOLDER U/O B. HOLDER"
	mirrorUSDIBAN   = "CH0000000000000000USD"
	mirrorUSDAcctID = "00000000000000US0000D"
	mirrorJPYIBAN   = "CH0000000000000000JPY"
	mirrorJPYAcctID = "00000000000000JP0000Y"
	mirrorOtherJPY  = "CH0000000000000000JP2"
	mirrorDay       = int64(500 * 86400)
	mirrorEvent     = "mt940:" + mirrorUSDIBAN + ":REF0000001"
	mirrorNarrative = "Z24?A. HOLDER U/O  B. HOLDER\n0000 SOMEWHERE\n/OCMT/JPY12000000,/\nKURS JPY/USD 160.0000"
)

func seedHolder(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	if _, err := db.Exec(`
        INSERT INTO account_holders (snapshot_at, relationship_id, client_external_id, payload)
        VALUES (1, 'R1', 'C1', json_object('FrstSurNm', ?))`, name); err != nil {
		t.Fatalf("seed holder: %v", err)
	}
}

func seedMirrorCashAccount(t *testing.T, db *sql.DB, iban, acctID, portfolio, ccy string) {
	t.Helper()
	if _, err := db.Exec(`
        INSERT INTO cash_accounts (snapshot_at, relationship_id, account_external_id,
            portfolio_external_id, payload)
        VALUES (1, 'R1', ?, ?, json_object('AcctId', ?, 'AcctCcyIsoCd', ?))`,
		iban, portfolio, acctID, ccy); err != nil {
		t.Fatalf("seed cash account %s: %v", iban, err)
	}
}

func seedConversionRow(t *testing.T, db *sql.DB, eventID, acct, amount, creditDebit, ccy, narrative, bankRef string, day int64) {
	t.Helper()
	if _, err := db.Exec(`
        INSERT INTO events (event_external_id, timestamp, relationship_id,
            account_external_id, kind, currency_iso, payload)
        VALUES (?, ?, 'R1', ?, 'cash_movement', ?,
                json_object('amount', ?, 'credit_debit', ?, 'narrative', ?,
                            'account', ?, 'funds', ?, 'txn_type', 'NTRF', 'bank_ref', ?))`,
		eventID, day, acct, ccy, amount, creditDebit, narrative, acct, ccy, bankRef); err != nil {
		t.Fatalf("seed conversion row %s: %v", eventID, err)
	}
}

// newMirrorSilver is a relationship with a USD and a JPY cash account in
// one managed portfolio, whose USD account the feed describes a
// conversion on.
func newMirrorSilver(t *testing.T) *sql.DB {
	t.Helper()
	_, db := newFixtureSilver(t)
	seedHolder(t, db, mirrorHolder)
	seedMirrorCashAccount(t, db, mirrorUSDIBAN, mirrorUSDAcctID, "P1", "USD")
	seedMirrorCashAccount(t, db, mirrorJPYIBAN, mirrorJPYAcctID, "P1", "JPY")
	seedConversionRow(t, db, mirrorEvent, mirrorUSDIBAN, "75012.34", "D", "USD", mirrorNarrative, "REF0000001", mirrorDay)
	return db
}

func psnRowsWith(t *testing.T, db *sql.DB, hints psnHints) map[string]canonical.TransactionChange {
	t.Helper()
	stream, err := (&psnReader{db: db}).Transactions(context.Background(),
		canonical.Window{Start: 0, End: 1 << 40, HasChanges: true}, hints)
	if err != nil {
		t.Fatalf("Transactions: %v", err)
	}
	defer stream.Close()
	return drainTx(t, stream)
}

func payloadField(t *testing.T, tx canonical.TransactionChange, key string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(tx.Payload, &m); err != nil {
		t.Fatalf("%s: payload does not decode: %v", tx.TransactionExternalID, err)
	}
	v, _ := m[key].(string)
	return v
}

// TestAConversionTheFeedDescribesGetsItsCounterLeg: the feed's row on the
// USD account states the JPY leg; the JPY account is the portfolio's, and
// the feed does not speak for it. So the JPY leg is booked there — the
// stated figure, the same day, the opposite direction — and each leg
// names the other's account.
func TestAConversionTheFeedDescribesGetsItsCounterLeg(t *testing.T) {
	got := psnRowsWith(t, newMirrorSilver(t), psnHints{})

	mirror, ok := got[mirrorIDPrefix+mirrorEvent]
	if !ok {
		t.Fatal("no counter-leg was booked")
	}
	if mirror.AccountExternalID != mirrorJPYIBAN {
		t.Errorf("counter-leg account = %q, want the portfolio's JPY account", mirror.AccountExternalID)
	}
	if mirror.Kind != canonical.TxKindDeposit || mirror.Currency != "JPY" ||
		mirror.NetAmount == nil || mirror.NetAmount.String() != "12000000" {
		t.Errorf("counter-leg = %s %s %v, want deposit JPY 12000000", mirror.Kind, mirror.Currency, mirror.NetAmount)
	}
	if mirror.OccurredAt != mirrorDay {
		t.Errorf("counter-leg day = %d, want the feed row's %d", mirror.OccurredAt, mirrorDay)
	}
	for key, want := range map[string]string{
		"mirror_of": mirrorEvent, "counter_account": mirrorUSDIBAN, "bank_ref": "REF0000001",
		"counter_currency": "USD", "counter_amount": "75012.34", "rate": "JPY/USD 160.0000",
	} {
		if got := payloadField(t, mirror, key); got != want {
			t.Errorf("counter-leg payload %s = %q, want %q", key, got, want)
		}
	}
	source := got[mirrorEvent]
	for key, want := range map[string]string{
		"counter_account": mirrorJPYIBAN, "counter_currency": "JPY", "counter_amount": "12000000",
	} {
		if got := payloadField(t, source, key); got != want {
			t.Errorf("feed row payload %s = %q, want %q", key, got, want)
		}
	}
	if source.Kind != canonical.TxKindWithdrawal || source.NetAmount.String() != "-75012.34" {
		t.Errorf("the feed row itself moved: %s %v", source.Kind, source.NetAmount)
	}
}

// TestNoCounterLegWhereTheFeedSpeaksForTheOtherAccount: the JPY account's
// own statement carries the leg, so nothing is booked for it and the feed
// row names no counter account it did not state.
func TestNoCounterLegWhereTheFeedSpeaksForTheOtherAccount(t *testing.T) {
	db := newMirrorSilver(t)
	seedConversionRow(t, db, "mt940:"+mirrorJPYIBAN+":REF0000002", mirrorJPYIBAN, "1", "D", "JPY", "FEE", "REF0000002", mirrorDay-86400)
	got := psnRowsWith(t, db, psnHints{})
	if _, ok := got[mirrorIDPrefix+mirrorEvent]; ok {
		t.Error("a counter-leg was booked on an account the feed speaks for")
	}
	if v := payloadField(t, got[mirrorEvent], "counter_account"); v != "" {
		t.Errorf("feed row names a counter account %q it should not", v)
	}
}

// TestNoCounterLegForAPaymentToSomeoneElse: a payment abroad carries the
// same subfield, and names the payee rather than the holder.
func TestNoCounterLegForAPaymentToSomeoneElse(t *testing.T) {
	_, db := newFixtureSilver(t)
	seedHolder(t, db, mirrorHolder)
	seedMirrorCashAccount(t, db, mirrorUSDIBAN, mirrorUSDAcctID, "P1", "USD")
	seedMirrorCashAccount(t, db, mirrorJPYIBAN, mirrorJPYAcctID, "P1", "JPY")
	seedConversionRow(t, db, mirrorEvent, mirrorUSDIBAN, "75012.34", "D", "USD",
		"Z44?SOME VENDOR KK\n0000 ELSEWHERE\n/OCMT/JPY12000000,/\nKURS JPY/USD 160.0000", "REF0000001", mirrorDay)
	got := psnRowsWith(t, db, psnHints{})
	if _, ok := got[mirrorIDPrefix+mirrorEvent]; ok {
		t.Error("a counter-leg was booked for a payment to a third party")
	}
}

// TestNoCounterLegWhereTheCurrencyNamesTwoAccounts: two JPY accounts in
// the portfolio, and the leg is refused rather than guessed at.
func TestNoCounterLegWhereTheCurrencyNamesTwoAccounts(t *testing.T) {
	db := newMirrorSilver(t)
	seedMirrorCashAccount(t, db, mirrorOtherJPY, "00000000000000JP0000Z", "P1", "JPY")
	got := psnRowsWith(t, db, psnHints{})
	if _, ok := got[mirrorIDPrefix+mirrorEvent]; ok {
		t.Error("a counter-leg was booked where two accounts could have received it")
	}
}

// TestAConversionAndItsCounterLegPairForReturns: through the merged
// stream, the veto's conversion phase reads the stated leg off the feed
// row and finds the counter-leg by it, so both carry the conduit verdict.
func TestAConversionAndItsCounterLegPairForReturns(t *testing.T) {
	r := newWebTxFixture(t)
	seedRailEraAnchor(t, r)
	got := mergedText(t, r, newMirrorSilver(t))
	for _, id := range []string{mirrorEvent, mirrorIDPrefix + mirrorEvent} {
		tx, ok := got[id]
		if !ok {
			t.Fatalf("%s not emitted", id)
		}
		if !strings.Contains(string(tx.Payload), `"returns_flow":"internal"`) {
			t.Errorf("%s: not demoted — a one-sided external flow", id)
		}
	}
}

// TestTheExportsRecordOutranksTheCounterLeg: the JPY account is outside the
// MT940 delivery but inside the export's, which carries the credit. The
// export's row is the one gold holds; the counter-leg is withheld, the
// feed row still names the account, and the pair still demotes together.
func TestTheExportsRecordOutranksTheCounterLeg(t *testing.T) {
	r := newWebTxFixture(t)
	seedRailEraAnchor(t, r)
	seedWebAccount(t, r, mirrorJPYIBAN)
	seedWebTx(t, r, "EXPORTED", mirrorJPYIBAN, mirrorDay, "JPY", 12000000, false)
	got := mergedText(t, r, newMirrorSilver(t))
	if _, ok := got[mirrorIDPrefix+mirrorEvent]; ok {
		t.Error("a counter-leg was booked beside the export's record of the booking")
	}
	exported, ok := got["EXPORTED@"+mirrorJPYIBAN]
	if !ok {
		t.Fatal("the export's row was not emitted")
	}
	if v := payloadField(t, got[mirrorEvent], "counter_account"); v != mirrorJPYIBAN {
		t.Errorf("feed row counter account = %q, want the JPY account", v)
	}
	for _, tx := range []canonical.TransactionChange{exported, got[mirrorEvent]} {
		if !strings.Contains(string(tx.Payload), `"returns_flow":"internal"`) {
			t.Errorf("%s: not demoted against the other leg", tx.TransactionExternalID)
		}
	}
}

// TestAStatementCopyFoldsOntoTheCounterLeg: a later annual statement
// reconstructs the JPY credit; it is a second record of the booking the
// counter-leg holds, and the era fold drops it as it drops any statement
// copy of a feed row.
func TestAStatementCopyFoldsOntoTheCounterLeg(t *testing.T) {
	r := newWebTxFixture(t)
	seedRailEraAnchor(t, r)
	seedWebAccount(t, r, mirrorJPYIBAN)
	seedWebTxRaw(t, r, "stmt:0000000000000001", mirrorJPYIBAN, mirrorDay, "JPY", 12000000, "CREDIT",
		`{"source":"account_statement_pdf","booking_type":"CREDIT","internal_transfer":false,"counter_account":null}`)
	got := mergedText(t, r, newMirrorSilver(t))
	if _, ok := got["stmt:0000000000000001@"+mirrorJPYIBAN]; ok {
		t.Error("the statement copy of the booking was emitted beside the counter-leg")
	}
	if _, ok := got[mirrorIDPrefix+mirrorEvent]; !ok {
		t.Error("the counter-leg was not emitted")
	}
}
