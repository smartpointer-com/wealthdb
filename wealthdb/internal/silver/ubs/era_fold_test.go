package ubs

import (
	"database/sql"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// The era fold. Three eras record the UBS cash ledger — statement
// reconstructions, the account-statement export and the MT940 feed — over
// overlapping periods and with disjoint id schemes, so an entry recorded by
// two of them reaches gold twice unless something matches them on the booking
// itself. These tests pin that match: what folds, what survives, whose
// identity the survivor keeps, and what the dropped copy leaves behind.
//
// Every id, amount, payee and date below is invented.

const (
	foldStmtA = "stmt:00000000000000a1"
	foldStmtB = "stmt:00000000000000a2"
	foldStmtC = "stmt:00000000000000a3"
	foldStmtD = "stmt:00000000000000a4"
)

// statementPayload is one Account-Statement reconstruction's payload: the
// printed booking type plus the entry's continuation lines.
func statementPayload(bookingType string, continuation ...string) string {
	lines := ""
	for i, c := range continuation {
		if i > 0 {
			lines += ","
		}
		lines += `"` + c + `"`
	}
	return `{"source":"account_statement_pdf","booking_type":"` + bookingType +
		`","internal_transfer":false,"counter_account":null,"continuation":[` + lines + `]}`
}

// exportPayload is one account-statement-export row's payload: the CSV feed's
// three description columns, with no source marker.
func exportPayload(d1, d2, d3 string) string {
	return `{"Description1":"` + d1 + `","Description2":"` + d2 + `","Description3":"` + d3 + `"}`
}

// seedPSNCashAmount inserts one MT940 cash movement with the amount and
// direction given, so a booking key can be matched against it.
func seedPSNCashAmount(t *testing.T, db *sql.DB, eventID, acct, amount, creditDebit, ccy, narrative string, day int64) {
	t.Helper()
	if _, err := db.Exec(`
        INSERT INTO events (event_external_id, timestamp, relationship_id,
            account_external_id, kind, currency_iso, payload)
        VALUES (?, ?, 'R1', ?, 'cash_movement', ?,
                json_object('amount', ?, 'credit_debit', ?, 'narrative', ?,
                            'account', ?, 'funds', ?, 'txn_type', 'NDDT'))`,
		eventID, day, acct, ccy, amount, creditDebit, narrative, acct, ccy); err != nil {
		t.Fatalf("seed psn cash movement %s: %v", eventID, err)
	}
}

// countOnDay reports how many emitted rows fall on one value day.
func countOnDay(got map[string]canonical.TransactionChange, day int64) int {
	n := 0
	for _, tx := range got {
		if tx.OccurredAt == day {
			n++
		}
	}
	return n
}

// TestEraFoldExportKeepsTheBooking: a statement reconstruction and the
// export's record of the same booking — one account, one value day, one
// signed amount, one currency — project as ONE row, and it is the export's:
// its id, its amount and its own narrative all stand.
func TestEraFoldExportKeepsTheBooking(t *testing.T) {
	const day = 400 * 86400
	r := newWebTxFixture(t)
	seedWebAccount(t, r, textAcct)
	seedRailEraAnchor(t, r)
	seedWebTextRow(t, r, foldStmtA, day, 250.0, nil, "EXAMPLE PAYEE", "DIRECT DEBIT",
		statementPayload("DIRECT DEBIT", "EXAMPLE PAYEE", "EXAMPLE CITY"))
	seedWebTextRow(t, r, "E1", day, 250.0, nil, "EXAMPLE UTILITY AG", "direct debit",
		exportPayload("EXAMPLE UTILITY AG; EXAMPLE STREET 1", "direct debit", ""))

	got := drainTx(t, emitWebStream(t, r))
	if _, ok := got[foldStmtA+"@"+textAcct]; ok {
		t.Error("statement reconstruction emitted alongside the export's record of the same booking")
	}
	survivor, ok := got["E1@"+textAcct]
	if !ok {
		t.Fatal("the export's row must keep the booking")
	}
	if n := countOnDay(got, day); n != 1 {
		t.Errorf("rows on the booking's value day = %d, want 1", n)
	}
	if survivor.NetAmount == nil || survivor.NetAmount.String() != "-250" {
		t.Errorf("survivor NetAmount = %v, want -250", survivor.NetAmount)
	}
	// The survivor's own narrative says something, so nothing moves onto it.
	checkText(t, got, map[string]textCase{
		"E1@" + textAcct: {"EXAMPLE UTILITY AG; EXAMPLE STREET 1", "EXAMPLE UTILITY AG", "direct debit"},
	})
}

// TestEraFoldFeedKeepsTheBooking: the same across the silver seam — a
// statement reconstruction whose booking the MT940 feed also carries projects
// once, as the feed's event.
func TestEraFoldFeedKeepsTheBooking(t *testing.T) {
	const day = 410 * 86400
	r := newWebTxFixture(t)
	seedWebAccount(t, r, textAcct)
	seedRailEraAnchor(t, r)
	seedWebTextRow(t, r, foldStmtB, day, 175.5, nil, "EXAMPLE PAYEE", "DIRECT DEBIT",
		statementPayload("DIRECT DEBIT", "EXAMPLE PAYEE", "EXAMPLE CITY"))

	_, psnDB := newFixtureSilver(t)
	seedPSNCashAmount(t, psnDB, "mt940:feed:1", textAcct, "175.50", "D", "CHF", "LSV", day)

	got := mergedText(t, r, psnDB)
	if _, ok := got[foldStmtB+"@"+textAcct]; ok {
		t.Error("statement reconstruction emitted alongside the feed's record of the same booking")
	}
	if _, ok := got["mt940:feed:1"]; !ok {
		t.Fatal("the feed's event must keep the booking")
	}
	if n := countOnDay(got, day); n != 1 {
		t.Errorf("rows on the booking's value day = %d, want 1", n)
	}
	// The feed left every narrative column a bare code; the dropped
	// statement's reading fills them.
	checkText(t, got, map[string]textCase{
		"mt940:feed:1": {"DIRECT DEBIT; EXAMPLE PAYEE; EXAMPLE CITY", "EXAMPLE PAYEE", "DIRECT DEBIT"},
	})
}

// TestEraFoldLeavesSameEraRepeatsAlone: two identical payments on one day are
// an ordinary thing for a ledger to hold. Within one era they carry distinct
// ids because they are distinct bookings, so neither pair folds — not two
// export rows, not two statement rows. Pairing is 1:1 for the same reason: a
// day holding two statement copies and one export row folds exactly one of
// them and leaves the other standing.
func TestEraFoldLeavesSameEraRepeatsAlone(t *testing.T) {
	const exportDay, statementDay, mixedDay = 420 * 86400, 421 * 86400, 422 * 86400
	r := newWebTxFixture(t)
	seedWebAccount(t, r, textAcct)
	seedRailEraAnchor(t, r)
	seedWebTextRow(t, r, "E1", exportDay, 90.0, nil, "EXAMPLE GROCER", "direct debit",
		exportPayload("EXAMPLE GROCER", "direct debit", ""))
	seedWebTextRow(t, r, "E2", exportDay, 90.0, nil, "EXAMPLE GROCER", "direct debit",
		exportPayload("EXAMPLE GROCER", "direct debit", ""))
	seedWebTextRow(t, r, foldStmtC, statementDay, 60.0, nil, "EXAMPLE FLORIST", "DIRECT DEBIT",
		statementPayload("DIRECT DEBIT", "EXAMPLE FLORIST"))
	seedWebTextRow(t, r, foldStmtD, statementDay, 60.0, nil, "EXAMPLE FLORIST", "DIRECT DEBIT",
		statementPayload("DIRECT DEBIT", "EXAMPLE FLORIST"))
	// Two statement copies, one export row: one fold, one survivor.
	seedWebTextRow(t, r, foldStmtA, mixedDay, 55.0, nil, "EXAMPLE BAKER", "DIRECT DEBIT",
		statementPayload("DIRECT DEBIT", "EXAMPLE BAKER"))
	seedWebTextRow(t, r, foldStmtB, mixedDay, 55.0, nil, "EXAMPLE BAKER", "DIRECT DEBIT",
		statementPayload("DIRECT DEBIT", "EXAMPLE BAKER"))
	seedWebTextRow(t, r, "E3", mixedDay, 55.0, nil, "EXAMPLE BAKER AG", "direct debit",
		exportPayload("EXAMPLE BAKER AG", "direct debit", ""))

	got := drainTx(t, emitWebStream(t, r))
	for _, id := range []string{"E1@" + textAcct, "E2@" + textAcct,
		foldStmtC + "@" + textAcct, foldStmtD + "@" + textAcct} {
		if _, ok := got[id]; !ok {
			t.Errorf("%s: a same-era repeat must survive", id)
		}
	}
	if n := countOnDay(got, exportDay); n != 2 {
		t.Errorf("export-era repeats emitted = %d, want 2", n)
	}
	if n := countOnDay(got, statementDay); n != 2 {
		t.Errorf("statement-era repeats emitted = %d, want 2", n)
	}
	// The export row plus exactly one of the two statement copies.
	if n := countOnDay(got, mixedDay); n != 2 {
		t.Errorf("rows emitted where two statement copies meet one export row = %d, want 2", n)
	}
	if _, ok := got["E3@"+textAcct]; !ok {
		t.Error("the export's row must keep its booking")
	}
	_, keptA := got[foldStmtA+"@"+textAcct]
	_, keptB := got[foldStmtB+"@"+textAcct]
	if keptA == keptB {
		t.Errorf("statement copies kept = (%v, %v), want exactly one (pairing is 1:1)", keptA, keptB)
	}
}

// TestEraFoldLeavesUnmatchedStatementAlone: a statement reconstruction no
// other era records is the only account of its booking and reaches gold
// exactly as it did — same id, amount, day and narrative. Only an exact
// booking signature folds: same day and amount on ANOTHER account, the same
// amount on another day, and the same figure in another currency are each a
// different booking, and a row alongside them that does match still folds.
func TestEraFoldLeavesUnmatchedStatementAlone(t *testing.T) {
	const day = 430 * 86400
	r := newWebTxFixture(t)
	seedWebAccount(t, r, textAcct)
	seedWebAccount(t, r, vetoAcctB)
	seedRailEraAnchor(t, r)
	seedWebTextRow(t, r, foldStmtA, day, 42.25, nil, "EXAMPLE FLORIST", "DIRECT DEBIT",
		statementPayload("DIRECT DEBIT", "EXAMPLE FLORIST", "EXAMPLE CITY"))
	// Same day and amount, other account.
	seedWebTxRaw(t, r, "E1", vetoAcctB, day, "CHF", -42.25, "direct debit",
		exportPayload("EXAMPLE GROCER", "direct debit", ""))
	// Same account and amount, next day.
	seedWebTxRaw(t, r, "E2", textAcct, day+86400, "CHF", -42.25, "direct debit",
		exportPayload("EXAMPLE GROCER", "direct debit", ""))
	// Same account, day and figure, other currency.
	seedWebTxRaw(t, r, "E3", textAcct, day, "USD", -42.25, "direct debit",
		exportPayload("EXAMPLE GROCER", "direct debit", ""))
	// A second statement row on the same day whose signature the export DOES
	// match: it folds, so the near misses above are read as near misses and
	// not as a fold that never runs.
	seedWebTextRow(t, r, foldStmtB, day, 88.0, nil, "EXAMPLE BAKER", "DIRECT DEBIT",
		statementPayload("DIRECT DEBIT", "EXAMPLE BAKER"))
	seedWebTxRaw(t, r, "E4", textAcct, day, "CHF", -88.0, "direct debit",
		exportPayload("EXAMPLE BAKER AG", "direct debit", ""))

	got := drainTx(t, emitWebStream(t, r))
	survivor, ok := got[foldStmtA+"@"+textAcct]
	if !ok {
		t.Fatal("an unmatched statement reconstruction must survive")
	}
	if survivor.NetAmount == nil || survivor.NetAmount.String() != "-42.25" {
		t.Errorf("NetAmount = %v, want -42.25", survivor.NetAmount)
	}
	if survivor.OccurredAt != day {
		t.Errorf("OccurredAt = %d, want %d", survivor.OccurredAt, day)
	}
	checkText(t, got, map[string]textCase{
		foldStmtA + "@" + textAcct: {"DIRECT DEBIT; EXAMPLE FLORIST; EXAMPLE CITY", "EXAMPLE FLORIST", "DIRECT DEBIT"},
	})
	for _, id := range []string{"E1@" + vetoAcctB, "E2@" + textAcct, "E3@" + textAcct, "E4@" + textAcct} {
		if _, ok := got[id]; !ok {
			t.Errorf("%s: a near miss must survive too", id)
		}
	}
	if _, ok := got[foldStmtB+"@"+textAcct]; ok {
		t.Error("a statement row the export matches exactly must fold")
	}
}

// TestEraFoldCarriesWhatTheSurvivorLacks: the fold drops a row, never what it
// said. Where the export's record left a column empty or as a bare code, the
// dropped statement's reading of the same booking fills it — per column and
// only downward, so nothing the survivor already said is overwritten.
func TestEraFoldCarriesWhatTheSurvivorLacks(t *testing.T) {
	const day = 440 * 86400
	r := newWebTxFixture(t)
	seedWebAccount(t, r, textAcct)
	seedRailEraAnchor(t, r)
	seedWebTextRow(t, r, foldStmtA, day, 310.0, nil, "EXAMPLE PAYEE", "DIRECT DEBIT",
		statementPayload("DIRECT DEBIT", "EXAMPLE PAYEE", "EXAMPLE CITY"))
	// The export's row for the same booking: no payee at all and a bare
	// booking code as its whole narrative.
	seedWebTextRow(t, r, "E1", day, 310.0, nil, nil, "KH", exportPayload("", "KH", ""))

	got := drainTx(t, emitWebStream(t, r))
	survivor, ok := got["E1@"+textAcct]
	if !ok {
		t.Fatal("the export's row must keep the booking")
	}
	checkText(t, got, map[string]textCase{
		"E1@" + textAcct: {"DIRECT DEBIT; EXAMPLE PAYEE; EXAMPLE CITY", "EXAMPLE PAYEE", "DIRECT DEBIT"},
	})
	// Only the narrative travels: the row of record does not move.
	if survivor.TransactionExternalID != "E1@"+textAcct {
		t.Errorf("id = %q, want the export row's", survivor.TransactionExternalID)
	}
	if survivor.NetAmount == nil || survivor.NetAmount.String() != "-310" {
		t.Errorf("NetAmount = %v, want -310", survivor.NetAmount)
	}
	if survivor.OccurredAt != day {
		t.Errorf("OccurredAt = %d, want %d", survivor.OccurredAt, day)
	}
}

// TestEraFoldPairsAcrossTheSignConventions is the fold's central shape and
// the reason its key is built on the adapter's projection rather than on the
// silver columns. The two web eras write the amount columns to different
// conventions: a statement reconstruction carries the figure a statement
// PRINTS, and a statement prints a debit as a positive figure in its debit
// column, while the export carries the sheet's own cell, which already states
// the direction in its sign. The raw column net therefore comes out with
// opposite signs for one booking. Both eras nonetheless say "debit" by
// putting the figure in the debit column, which is what decides the kind and
// so the projected sign — so both project the same negative amount, and the
// fold pairs them.
func TestEraFoldPairsAcrossTheSignConventions(t *testing.T) {
	const day = 450 * 86400
	r := newWebTxFixture(t)
	seedWebAccount(t, r, textAcct)
	seedRailEraAnchor(t, r)
	// The statement's debit: printed, and so stored, positive.
	seedWebTextRow(t, r, foldStmtA, day, 8317.40, nil, "EXAMPLE PAYEE", "DIRECT DEBIT",
		statementPayload("DIRECT DEBIT", "EXAMPLE PAYEE", "EXAMPLE CITY"))
	// The export's cell for the same booking: already signed, and negative.
	seedWebTextRow(t, r, "E1", day, -8317.40, nil, "EXAMPLE UTILITY AG", "direct debit",
		exportPayload("EXAMPLE UTILITY AG; EXAMPLE STREET 1", "direct debit", ""))

	got := drainTx(t, emitWebStream(t, r))
	if _, ok := got[foldStmtA+"@"+textAcct]; ok {
		t.Error("opposite silver sign conventions must not stop one booking folding to one row")
	}
	survivor, ok := got["E1@"+textAcct]
	if !ok {
		t.Fatal("the export's row must keep the booking")
	}
	if n := countOnDay(got, day); n != 1 {
		t.Errorf("rows on the booking's value day = %d, want 1", n)
	}
	// Both eras project the same negative amount; that is the key.
	if survivor.NetAmount == nil || survivor.NetAmount.String() != "-8317.4" {
		t.Errorf("survivor NetAmount = %v, want -8317.4", survivor.NetAmount)
	}
	if survivor.Kind != canonical.TxKindWithdrawal {
		t.Errorf("survivor Kind = %q, want withdrawal", survivor.Kind)
	}
}

// TestEraFoldKeepsABookingAndItsReversal: equal magnitude, opposite
// direction, one day, one account is a booking and the bank's correction of
// it — two real entries, not one recorded twice. The key is signed for
// exactly this reason: a magnitude-only match would fold them and delete a
// real booking. Both here are the export's own rows, so the same-era rule
// forbids the fold too; the control pair on the same day proves the fold is
// live in this fixture.
func TestEraFoldKeepsABookingAndItsReversal(t *testing.T) {
	const day = 460 * 86400
	r := newWebTxFixture(t)
	seedWebAccount(t, r, textAcct)
	seedRailEraAnchor(t, r)
	// The dividend, and the bank clawing it back: the reversal states the
	// correction in the sign of its own cell, and keeps it.
	seedWebTextRow(t, r, "E1", day, nil, 12.50, "EXAMPLE FUND", "Dividend",
		exportPayload("EXAMPLE FUND", "Dividend", ""))
	seedWebTextRow(t, r, "E2", day, nil, -12.50, "EXAMPLE FUND", "Dividend;Reversal",
		exportPayload("EXAMPLE FUND", "Dividend;Reversal", ""))
	// Control: a genuine cross-era duplicate on the same day still folds.
	seedWebTextRow(t, r, foldStmtA, day, 640.0, nil, "EXAMPLE PAYEE", "DIRECT DEBIT",
		statementPayload("DIRECT DEBIT", "EXAMPLE PAYEE"))
	seedWebTextRow(t, r, "E3", day, -640.0, nil, "EXAMPLE UTILITY AG", "direct debit",
		exportPayload("EXAMPLE UTILITY AG", "direct debit", ""))

	got := drainTx(t, emitWebStream(t, r))
	for _, id := range []string{"E1@" + textAcct, "E2@" + textAcct} {
		if _, ok := got[id]; !ok {
			t.Errorf("%s: a booking and its reversal are two entries and must both survive", id)
		}
	}
	if _, ok := got[foldStmtA+"@"+textAcct]; ok {
		t.Error("control: a genuine cross-era duplicate must still fold")
	}
	if n := countOnDay(got, day); n != 3 {
		t.Errorf("rows on the day = %d, want 3 (the pair, plus the folded booking)", n)
	}
}

// TestEraFoldKeepsACrossEraReversalApart: the same guard where the booking
// and its correction reach gold from DIFFERENT eras. The reversal keeps the
// sign its own record carries — a clawback is money out however the era
// spells it — so it never keys onto the entry it corrects, and the fold has
// no way to collapse the two.
func TestEraFoldKeepsACrossEraReversalApart(t *testing.T) {
	const day = 470 * 86400
	r := newWebTxFixture(t)
	seedWebAccount(t, r, textAcct)
	seedRailEraAnchor(t, r)
	// The statement's record of the dividend: printed, and so stored,
	// positive in the credit column.
	seedWebTextRow(t, r, foldStmtA, day, nil, 12.50, "EXAMPLE FUND", "DIVIDEND",
		statementPayload("DIVIDEND", "EXAMPLE FUND"))
	// The export's record of the clawback: a negative credit cell.
	seedWebTextRow(t, r, "E1", day, nil, -12.50, "EXAMPLE FUND", "Dividend;Reversal",
		exportPayload("EXAMPLE FUND", "Dividend;Reversal", ""))
	// Control: a genuine cross-era duplicate on the same day still folds.
	seedWebTextRow(t, r, foldStmtB, day, 640.0, nil, "EXAMPLE PAYEE", "DIRECT DEBIT",
		statementPayload("DIRECT DEBIT", "EXAMPLE PAYEE"))
	seedWebTextRow(t, r, "E2", day, -640.0, nil, "EXAMPLE UTILITY AG", "direct debit",
		exportPayload("EXAMPLE UTILITY AG", "direct debit", ""))

	got := drainTx(t, emitWebStream(t, r))
	if _, ok := got[foldStmtA+"@"+textAcct]; !ok {
		t.Error("the statement's dividend must survive its cross-era reversal")
	}
	if _, ok := got["E1@"+textAcct]; !ok {
		t.Error("the export's reversal must survive the entry it corrects")
	}
	if _, ok := got[foldStmtB+"@"+textAcct]; ok {
		t.Error("control: a genuine cross-era duplicate must still fold")
	}
	if n := countOnDay(got, day); n != 3 {
		t.Errorf("rows on the day = %d, want 3 (the pair, plus the folded booking)", n)
	}
}

// TestStatementReversalKeepsItsSignEndToEnd is the same guard as
// TestWebProjectedNetKeepsAStatementReversal, but taken through the emit
// path — because the sign is pinned in TWO places and the projection helper
// is only the first. The second runs after the returns verdict is stamped,
// re-signing the row from the kind it ENDS with, and for a while it asked a
// narrower question than the first: it exempted the export's `;Reversal`
// booking type and nothing else, so a statement's negative figure was
// signed correctly by the projection and then flipped straight back. Both
// now ask webReversal, and this test fails if either stops.
//
// A cancelled withdrawal is money coming BACK, so the emitted amount is
// positive while the kind stays the base kind — that is what lets the pair
// net to zero when summed. Every value is synthetic.
func TestStatementReversalKeepsItsSignEndToEnd(t *testing.T) {
	const day = 480 * 86400
	r := newWebTxFixture(t)
	seedWebAccount(t, r, textAcct)
	seedRailEraAnchor(t, r)
	// The statement's booking, printed positive in the debit column.
	seedWebTextRow(t, r, foldStmtA, day, 100.0, nil, "EXAMPLE PAYEE", "MATURITY",
		statementPayload("MATURITY", "EXAMPLE PAYEE"))
	// The statement's cancellation of it: a negative in that same column,
	// which is the only way a statement can say so.
	seedWebTextRow(t, r, foldStmtB, day, -100.0, nil, "EXAMPLE REFERENCE", "CANC.MAT.",
		statementPayload("CANC.MAT.", "EXAMPLE REFERENCE"))

	got := drainTx(t, emitWebStream(t, r))
	booking, ok := got[foldStmtA+"@"+textAcct]
	if !ok {
		t.Fatal("the booking must survive")
	}
	cancellation, ok := got[foldStmtB+"@"+textAcct]
	if !ok {
		t.Fatal("the cancellation must survive: it is a second entry, not a duplicate")
	}
	if booking.NetAmount == nil || cancellation.NetAmount == nil {
		t.Fatal("both rows must carry a net amount")
	}
	if !booking.NetAmount.IsNegative() {
		t.Errorf("booking NetAmount = %s, want negative (money out)", booking.NetAmount)
	}
	if !cancellation.NetAmount.IsPositive() {
		t.Errorf("cancellation NetAmount = %s, want positive — a cancelled withdrawal is money coming back, "+
			"and forcing it back to the kind's direction makes it a second copy of the booking it cancels",
			cancellation.NetAmount)
	}
	if sum := booking.NetAmount.Add(*cancellation.NetAmount); !sum.IsZero() {
		t.Errorf("booking + cancellation = %s, want 0", sum)
	}
}
