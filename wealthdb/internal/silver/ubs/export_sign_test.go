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
