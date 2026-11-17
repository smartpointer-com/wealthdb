package ubs

import (
	"testing"
)

// The export states a debit as the sheet's own signed cell, so a
// booking whose kind pins no direction keeps the outflow its cell
// states: the interest the bank charges, and a currency sold. Every
// value is synthetic.
func TestAnExportDebitOfAnUnsignedKindStaysAnOutflow(t *testing.T) {
	r := newWebTxFixture(t)
	seedWebAccount(t, r, vetoAcctA)
	for _, row := range []struct{ id, kind string }{
		{"EXPINT", "Interest"},
		{"EXPFX", "Sale FX spot"},
	} {
		if _, err := r.db.Exec(`
        INSERT INTO transactions (transaction_external_id, account_external_id,
            snapshot_at, value_date, currency_iso, amount_debit, description_kind, payload)
        VALUES (?, ?, 1000, ?, 'CHF', -12.5, ?, '{}')`, row.id, vetoAcctA, vetoDay1, row.kind); err != nil {
			t.Fatal(err)
		}
	}
	got := drainTx(t, connTransactions(t, r, nil, nil))
	for _, id := range []string{"EXPINT", "EXPFX"} {
		tx := got[id+"@"+vetoAcctA]
		if tx.NetAmount == nil || tx.NetAmount.String() != "-12.5" {
			t.Errorf("%s net = %v, want -12.5", id, tx.NetAmount)
		}
	}
}

// An export row whose payload does not decode still states its cells
// signed: the conservative backfill reading belongs to the external gate,
// not to the sign. The payload names no PDF source, so the debit cell is
// added as the sheet signs it.
func TestAnUndecodableExportRowKeepsItsSignedCell(t *testing.T) {
	r := newWebTxFixture(t)
	seedWebAccount(t, r, vetoAcctA)
	if _, err := r.db.Exec(`
        INSERT INTO transactions (transaction_external_id, account_external_id,
            snapshot_at, value_date, currency_iso, amount_debit, description_kind, payload)
        VALUES ('EXPUNDEC', ?, 1000, ?, 'CHF', -12.5, 'Interest', '{"continuation":"not-a-list"}')`,
		vetoAcctA, vetoDay1); err != nil {
		t.Fatal(err)
	}
	tx := drainTx(t, connTransactions(t, r, nil, nil))["EXPUNDEC@"+vetoAcctA]
	if tx.NetAmount == nil || tx.NetAmount.String() != "-12.5" {
		t.Errorf("net = %v, want -12.5", tx.NetAmount)
	}
}

// A payment advice prints its debit as a positive figure, so its cell is
// subtracted like a statement's, though its id is not a statement's.
func TestAPaymentAdviceDebitIsAPrintedFigure(t *testing.T) {
	r := newWebTxFixture(t)
	seedWebAccount(t, r, vetoAcctA)
	if _, err := r.db.Exec(`
        INSERT INTO transactions (transaction_external_id, account_external_id,
            snapshot_at, value_date, currency_iso, amount_debit, payload)
        VALUES ('ADVDEBIT', ?, 1000, ?, 'CHF', 200, '{"source": "payment_advice_pdf", "continuation": []}')`,
		vetoAcctA, vetoDay1); err != nil {
		t.Fatal(err)
	}
	tx := drainTx(t, connTransactions(t, r, nil, nil))["ADVDEBIT@"+vetoAcctA]
	if tx.NetAmount == nil || tx.NetAmount.String() != "-200" {
		t.Errorf("net = %v, want -200", tx.NetAmount)
	}
}
