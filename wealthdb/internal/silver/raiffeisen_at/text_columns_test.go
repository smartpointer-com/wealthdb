package raiffeisenat

import (
	"context"
	"database/sql"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// seedTextRows inserts the ledger rows the text-column projection is pinned
// on. Every string is synthetic. The payload is the raw kontoumsaetze row the
// collector preserves, holding the fields the projection falls back to when
// the promoted columns are empty.
func seedTextRows(t *testing.T, db *sql.DB) {
	t.Helper()
	exec := func(q string, args ...any) {
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	exec(`INSERT INTO dump_runs VALUES (?,1,'/run')`, loadUnix)
	exec(`INSERT INTO accounts VALUES (?,?,?,?,?,?,?,?)`,
		loadUnix, iban, "Gehaltekonto", "", "…1234", "EUR", 100.00, "{}")
	tx := func(id string, d int64, amt float64, category, desc, cp, payload string) {
		exec(`INSERT INTO transactions VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, iban, d, d, amt, "EUR", "", category, desc, cp, "history", payload)
	}
	// Promoted columns all present: everything verbatim.
	tx("r1", day(2026, 3, 1), -25.00, "shopping", "GROCERIES WEEKLY", "EXAMPLE MARKET", `{}`)
	// Card row: no purpose line, no participant line. The merchant lives in
	// the card-network object and the reference is the only narrative.
	tx("r2", day(2026, 3, 2), -12.00, "restaurants", "", "",
		`{"ethocaHaendler":{"id":"m1","name":"EXAMPLE CAFE"},"zahlungsreferenz":"POS 4711 EXAMPLE CAFE","folgenummerKarte":1}`)
	// No purpose line, but a short order purpose and a payment reference.
	tx("r3", day(2026, 3, 3), -30.00, "insurance", "", "EXAMPLE INSURER",
		`{"auftragskurzVerwendungszweck":"POLICY 9","zahlungsreferenz":"REF 2026-01"}`)
	// Nothing at all: every text column must stay NULL.
	tx("r4", day(2026, 3, 4), 15.00, "", "", "", `{}`)
	// Primary purpose present: the fallback reference must NOT be appended,
	// and the fee classification still reads the raw purpose column.
	tx("r5", day(2026, 3, 5), -5.00, "fees_bank", "KONTOENTGELT", "", `{"zahlungsreferenz":"Q1"}`)
}

func emitTextRows(t *testing.T) map[string]canonical.TransactionChange {
	t.Helper()
	path, db := newFixture(t)
	seedTextRows(t, db)
	conn := openConn(t, path)
	ctx := context.Background()
	stream, err := conn.Transactions(ctx, fullWindow(t, conn))
	if err != nil {
		t.Fatalf("Transactions: %v", err)
	}
	defer stream.Close()
	batch, _, err := stream.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	out := map[string]canonical.TransactionChange{}
	for _, tx := range batch.Transactions {
		out[tx.TransactionExternalID] = tx
	}
	return out
}

func strOrNil(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// TestTransactionsTextColumns pins the text-column contract: counterparty is
// the participant line verbatim (the card-network merchant name when that is
// empty), provider_category is the source category slug verbatim, and
// description is the purpose line — else the short order purpose followed by
// the payment reference — never fabricated.
func TestTransactionsTextColumns(t *testing.T) {
	got := emitTextRows(t)
	cases := map[string]struct{ desc, cp, cat string }{
		"r1": {"GROCERIES WEEKLY", "EXAMPLE MARKET", "shopping"},
		"r2": {"POS 4711 EXAMPLE CAFE", "EXAMPLE CAFE", "restaurants"},
		"r3": {"POLICY 9; REF 2026-01", "EXAMPLE INSURER", "insurance"},
		"r4": {"<nil>", "<nil>", "<nil>"},
		"r5": {"KONTOENTGELT", "<nil>", "fees_bank"},
	}
	for id, want := range cases {
		tx, ok := got[id]
		if !ok {
			t.Errorf("missing tx %s", id)
			continue
		}
		if d := strOrNil(tx.Description); d != want.desc {
			t.Errorf("%s Description = %q, want %q", id, d, want.desc)
		}
		if c := strOrNil(tx.Counterparty); c != want.cp {
			t.Errorf("%s Counterparty = %q, want %q", id, c, want.cp)
		}
		if c := strOrNil(tx.ProviderCategory); c != want.cat {
			t.Errorf("%s ProviderCategory = %q, want %q", id, c, want.cat)
		}
	}
}

// txShape is a TransactionChange with its three text columns removed — the
// projection the returns engine and every id/amount/kind consumer sees.
type txShape struct {
	ID      string
	At      int64
	Acct    string
	Kind    canonical.TxKind
	Ccy     string
	Gross   string
	Net     string
	Payload string
}

func shapeOf(tx canonical.TransactionChange) txShape {
	dec := func(d *canonical.Decimal) string {
		if d == nil {
			return "<nil>"
		}
		return d.String()
	}
	return txShape{
		ID: tx.TransactionExternalID, At: tx.OccurredAt, Acct: tx.AccountExternalID,
		Kind: tx.Kind, Ccy: tx.Currency, Gross: dec(tx.GrossAmount), Net: dec(tx.NetAmount),
		Payload: string(tx.Payload),
	}
}

// TestTextProjectionLeavesNonTextColumnsUnchanged is the byte-identity guard
// for the text projection: the golden below is what the adapter emitted for
// these rows BEFORE it projected any text column, and it must keep emitting
// exactly this — ids, dates, kinds, signs, amounts and payloads — with the
// text columns being the only thing that changed. It also pins that no row is
// added or dropped, and that the instrument / quantity / price columns stay
// unset for a deposit ledger.
func TestTextProjectionLeavesNonTextColumnsUnchanged(t *testing.T) {
	got := emitTextRows(t)
	want := map[string]txShape{
		"r1": {"r1", day(2026, 3, 1), iban, canonical.TxKindWithdrawal, "EUR", "-25", "-25", `{}`},
		"r2": {"r2", day(2026, 3, 2), iban, canonical.TxKindWithdrawal, "EUR", "-12", "-12",
			`{"ethocaHaendler":{"id":"m1","name":"EXAMPLE CAFE"},"zahlungsreferenz":"POS 4711 EXAMPLE CAFE","folgenummerKarte":1}`},
		"r3": {"r3", day(2026, 3, 3), iban, canonical.TxKindWithdrawal, "EUR", "-30", "-30",
			`{"auftragskurzVerwendungszweck":"POLICY 9","zahlungsreferenz":"REF 2026-01"}`},
		"r4": {"r4", day(2026, 3, 4), iban, canonical.TxKindDeposit, "EUR", "15", "15", `{}`},
		"r5": {"r5", day(2026, 3, 5), iban, canonical.TxKindFee, "EUR", "-5", "-5", `{"zahlungsreferenz":"Q1"}`},
	}
	if len(got) != len(want) {
		t.Fatalf("emitted %d rows, want %d", len(got), len(want))
	}
	for id, w := range want {
		tx, ok := got[id]
		if !ok {
			t.Errorf("missing tx %s", id)
			continue
		}
		if g := shapeOf(tx); g != w {
			t.Errorf("%s non-text columns changed:\n got %+v\nwant %+v", id, g, w)
		}
		if tx.InstrumentExternalID != nil || tx.Quantity != nil || tx.Price != nil {
			t.Errorf("%s: instrument/quantity/price must stay unset", id)
		}
	}
}
