package ubs

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/spending"
)

// The text-column contract (description / counterparty / provider_category)
// per UBS transaction era, pinned on synthetic silver rows. Every string here
// is a placeholder; IBAN-shaped ids use spec placeholder letters (CLAUDE.md §4).

const textAcct = vetoAcctA

// seedWebTextRow inserts one ubs-web transactions row with every column the
// text projection reads. nil for debit / credit / counterparty / descKind
// lands as SQL NULL.
func seedWebTextRow(t *testing.T, r *webReader, id string, day int64, debit, credit, counterparty, descKind any, payload string) {
	t.Helper()
	if _, err := r.db.Exec(`
        INSERT INTO transactions (transaction_external_id, account_external_id,
            snapshot_at, value_date, currency_iso, amount_debit, amount_credit,
            counterparty, description_kind, payload)
        VALUES (?, ?, 1000, ?, 'CHF', ?, ?, ?, ?, ?)`,
		id, textAcct, day, debit, credit, counterparty, descKind, payload); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// Web silver: the CSV-feed rows (Description1/2/3 in the payload, no source
// marker) come first so the PDF-backfill rows fall in the MT940 rail era and
// classify as they do in production.
func seedWebTextRows(t *testing.T, r *webReader) {
	t.Helper()
	seedWebAccount(t, r, textAcct)
	// CSV feed, instrument row: Description1 = "<caption>; <ISIN>".
	seedWebTextRow(t, r, "W1", 100*86400, nil, 12.5, "Example Fund Caption", "Dividend",
		`{"Description1":"Example Fund Caption; XS0000000001","Description2":"Dividend","Description3":"Coupon detail","Transaction no.":"W1"}`)
	// CSV feed, payment: Description1 = "<payee>; <address lines>".
	seedWebTextRow(t, r, "W2", 101*86400, 80.0, nil, "EXAMPLE PAYEE", "e-banking payment order",
		`{"Description1":"EXAMPLE PAYEE; EXAMPLE STREET 1; 9999 EXAMPLE CITY","Description2":"e-banking payment order","Description3":"Invoice 42"}`)
	// CSV feed, code-only: no caption, a bare booking code.
	seedWebTextRow(t, r, "W3", 102*86400, 5.0, nil, nil, "KH",
		`{"Description1":"","Description2":"KH","Description3":""}`)
	// CSV feed, all empty.
	seedWebTextRow(t, r, "W4", 103*86400, nil, 7.0, nil, nil,
		`{"Description1":"","Description2":"","Description3":""}`)
	// CSV feed, no caption but a kind label and a detail line.
	seedWebTextRow(t, r, "W5", 104*86400, nil, 9.0, nil, "credit",
		`{"Description1":"","Description2":"credit","Description3":"Ref 7"}`)
	// CSV feed, payment with a message typed on the order: Description2 =
	// "<message>; <booking type>".
	seedWebTextRow(t, r, "W6", 105*86400, 65.0, nil, "EXAMPLE PAYEE", "THANKS; e-banking payment order",
		`{"Description1":"EXAMPLE PAYEE; EXAMPLE STREET 1; 9999 EXAMPLETOWN","Description2":"THANKS; e-banking payment order","Description3":""}`)
	// CSV feed, no caption, a message on a credit plus a detail line.
	seedWebTextRow(t, r, "W7", 106*86400, nil, 11.0, nil, "see you soon; credit",
		`{"Description1":"","Description2":"see you soon; credit","Description3":"Ref 9"}`)
	// CSV feed, no caption, a reference-led order.
	seedWebTextRow(t, r, "W8", 107*86400, 42.0, nil, nil, "UCCDDEXAMPLE1; order",
		`{"Description1":"","Description2":"UCCDDEXAMPLE1; order","Description3":""}`)
	// PDF backfill, full: booking type + continuation lines (one blank).
	seedWebTextRow(t, r, "P1", 200*86400, 150.0, nil, "EXAMPLE GROCER", "E-BANKING PAYMENT ORDER",
		`{"source":"account_statement_pdf","booking_type":"E-BANKING PAYMENT ORDER","internal_transfer":false,"counter_account":null,"continuation":["EXAMPLE GROCER","  ","EXAMPLE CITY","INVOICE 42"],"running_balance":1000.0,"value_date":"01.02.2025","post_closing":false}`)
	// PDF backfill, kind-only: booking type, no continuation.
	seedWebTextRow(t, r, "P2", 201*86400, 3.0, nil, nil, "FEES",
		`{"source":"account_statement_pdf","booking_type":"FEES","internal_transfer":false,"counter_account":null,"continuation":[]}`)
	// PDF backfill, all empty.
	seedWebTextRow(t, r, "P3", 202*86400, nil, 40.0, nil, nil,
		`{"source":"account_statement_pdf","booking_type":null,"internal_transfer":false,"counter_account":null,"continuation":[]}`)
}

func emitWebText(t *testing.T) map[string]canonical.TransactionChange {
	t.Helper()
	r := newWebTxFixture(t)
	seedWebTextRows(t, r)
	return drainTx(t, emitWebStream(t, r))
}

// PSN silver: MT940 cash movements (multi-line narrative, code-only narrative,
// empty narrative, a classified interest line), a trade and a corporate action.
func emitPSNText(t *testing.T) map[string]canonical.TransactionChange {
	t.Helper()
	path, seed := newFixtureSilver(t)
	if _, err := seed.Exec(`
        INSERT INTO dump_runs(snapshot_at, silver_schema_version, run_dir) VALUES (1000, 1, '/x/1');
        INSERT INTO events(event_external_id, timestamp, relationship_id, account_external_id, kind, currency_iso, payload) VALUES
            ('C1', 1100, 'SFTPCHxx', 'CH00CASH', 'cash_movement', 'CHF',
             '{"amount":100.0,"credit_debit":"D","narrative":"E-BANKING ORDER\n EXAMPLE PAYEE \nREF 12345","account":"CH00CASH","funds":"CHF","txn_type":"NTRF","customer_ref":"NONREF"}'),
            ('C2', 1200, 'SFTPCHxx', 'CH00CASH', 'cash_movement', 'CHF',
             '{"amount":20.0,"credit_debit":"D","narrative":"KH","account":"CH00CASH","funds":"CHF","txn_type":"NMSC","customer_ref":"NONREF"}'),
            ('C3', 1300, 'SFTPCHxx', 'CH00CASH', 'cash_movement', 'CHF',
             '{"amount":5.0,"credit_debit":"C","narrative":"","account":"CH00CASH","funds":"CHF","txn_type":"","customer_ref":""}'),
            ('C4', 1400, 'SFTPCHxx', 'CH00CASH', 'cash_movement', 'CHF',
             '{"amount":2.5,"credit_debit":"C","narrative":"INTERETS T1","account":"CH00CASH","funds":"CHF","txn_type":"NINT","customer_ref":"NONREF"}'),
            ('T1', 1500, 'SFTPCHxx', 'CH00SAFE', 'trade_confirmation', NULL,
             '{"side":"B","isin":"CH0000000001","gross_amount":1500.00,"net_amount":1505.00,"net_currency":"CHF","price":15.00,"quantity":100,"cash_account_external_id":"CH00CASH","security_name":"Example Share"}'),
            ('A1', 1600, 'SFTPCHxx', 'CH00SAFE', 'corporate_action_confirmation', NULL,
             '{"isin":"CH0000000001","safekeeping":"SK1","caev":"DVCA"}');
    `); err != nil {
		t.Fatal(err)
	}
	conn := openAdapter(t, path)
	w, err := conn.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatalf("ChangeWindow: %v", err)
	}
	stream, err := conn.Transactions(context.Background(), w)
	if err != nil {
		t.Fatalf("Transactions: %v", err)
	}
	defer stream.Close()
	return drainTx(t, stream)
}

func drainTx(t *testing.T, stream interface {
	Next(context.Context) (canonical.TransactionBatch, bool, error)
}) map[string]canonical.TransactionChange {
	t.Helper()
	out := map[string]canonical.TransactionChange{}
	for {
		batch, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		for _, tx := range batch.Transactions {
			out[tx.TransactionExternalID] = tx
		}
		if !more {
			return out
		}
	}
}

func textOrNil(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

type textCase struct{ desc, cp, cat string }

func checkText(t *testing.T, got map[string]canonical.TransactionChange, cases map[string]textCase) {
	t.Helper()
	for id, want := range cases {
		tx, ok := got[id]
		if !ok {
			t.Errorf("missing tx %s", id)
			continue
		}
		if d := textOrNil(tx.Description); d != want.desc {
			t.Errorf("%s Description = %q, want %q", id, d, want.desc)
		}
		if c := textOrNil(tx.Counterparty); c != want.cp {
			t.Errorf("%s Counterparty = %q, want %q", id, c, want.cp)
		}
		if c := textOrNil(tx.ProviderCategory); c != want.cat {
			t.Errorf("%s ProviderCategory = %q, want %q", id, c, want.cat)
		}
	}
}

// checkMemo pins the memo column: the rows listed carry exactly that
// memo, and every other emitted row carries none — the message is the
// only text that ever travels as a memo.
func checkMemo(t *testing.T, got map[string]canonical.TransactionChange, memos map[string]string) {
	t.Helper()
	for id, tx := range got {
		want, ok := memos[id]
		if !ok {
			want = "<nil>"
		}
		if m := textOrNil(tx.Memo); m != want {
			t.Errorf("%s Memo = %q, want %q", id, m, want)
		}
	}
	for id := range memos {
		if _, ok := got[id]; !ok {
			t.Errorf("missing tx %s", id)
		}
	}
}

// TestWebTextColumns pins the ubs-web contract for both feeds sharing the
// table: counterparty is silver's promoted column verbatim, provider_category
// is the booking type — description_kind verbatim, less the payer's message a
// CSV row may carry before its last "; " — and description is the
// Description1 caption when there is one (unchanged — gold's name lookups key
// on it), else the booking type followed by the narrative lines, "; "-joined.
// The message never enters either: it travels as the row's memo, which the
// gold writer stores at the description's end behind the memo separator, so
// the payee keeps leading and the merchant signature is the same with or
// without it. A row without a message emits byte for byte what it did before
// the split.
func TestWebTextColumns(t *testing.T) {
	got := emitWebText(t)
	checkMemo(t, got, map[string]string{
		"W6@" + textAcct: "THANKS",
		"W7@" + textAcct: "see you soon",
		"W8@" + textAcct: "UCCDDEXAMPLE1",
	})
	checkText(t, got, map[string]textCase{
		"W1@" + textAcct: {"Example Fund Caption", "Example Fund Caption", "Dividend"},
		"W2@" + textAcct: {"EXAMPLE PAYEE; EXAMPLE STREET 1; 9999 EXAMPLE CITY", "EXAMPLE PAYEE", "e-banking payment order"},
		"W3@" + textAcct: {"KH", "<nil>", "KH"},
		"W4@" + textAcct: {"<nil>", "<nil>", "<nil>"},
		"W5@" + textAcct: {"credit; Ref 7", "<nil>", "credit"},
		"W6@" + textAcct: {"EXAMPLE PAYEE; EXAMPLE STREET 1; 9999 EXAMPLETOWN", "EXAMPLE PAYEE", "e-banking payment order"},
		"W7@" + textAcct: {"credit; Ref 9", "<nil>", "credit"},
		"W8@" + textAcct: {"order", "<nil>", "order"},
		"P1@" + textAcct: {"E-BANKING PAYMENT ORDER; EXAMPLE GROCER; EXAMPLE CITY; INVOICE 42", "EXAMPLE GROCER", "E-BANKING PAYMENT ORDER"},
		"P2@" + textAcct: {"FEES", "<nil>", "FEES"},
		"P3@" + textAcct: {"<nil>", "<nil>", "<nil>"},
	})
}

// TestPSNTextColumns pins the ubs-psn contract: a cash movement's description
// is its MT940 :86: narrative with the lines trimmed and "; "-joined (a bare
// code passes through as is), provider_category is the :61: transaction type
// code, and no counterparty is ever derived from the free text. Trades carry
// the security name; corporate actions their CAEV code.
func TestPSNTextColumns(t *testing.T) {
	got := emitPSNText(t)
	checkText(t, got, map[string]textCase{
		"C1": {"E-BANKING ORDER; EXAMPLE PAYEE; REF 12345", "<nil>", "NTRF"},
		"C2": {"KH", "<nil>", "NMSC"},
		"C3": {"<nil>", "<nil>", "<nil>"},
		"C4": {"INTERETS T1", "<nil>", "NINT"},
		"T1": {"Example Share", "<nil>", "<nil>"},
		"A1": {"DVCA", "<nil>", "<nil>"},
	})
}

// txShape is a TransactionChange minus its three text columns — everything
// the returns engine and the id/amount/kind consumers see.
type txShape struct {
	ID      string
	At      int64
	Acct    string
	Instr   string
	Kind    canonical.TxKind
	Ccy     string
	Gross   string
	Net     string
	Qty     string
	Price   string
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
		Instr: textOrNil(tx.InstrumentExternalID), Kind: tx.Kind, Ccy: tx.Currency,
		Gross: dec(tx.GrossAmount), Net: dec(tx.NetAmount), Qty: dec(tx.Quantity),
		Price: dec(tx.Price), Payload: string(tx.Payload),
	}
}

func checkShapes(t *testing.T, got map[string]canonical.TransactionChange, want map[string]txShape) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("emitted %d rows, want %d", len(got), len(want))
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
	}
}

// TestTextProjectionLeavesNonTextColumnsUnchanged is the byte-identity guard:
// the goldens are what each emitter produced for these rows BEFORE it
// projected any text column. Ids, dates, accounts, instruments, kinds, signs,
// amounts and payloads must keep coming out exactly like this, and no row may
// be added or dropped — the text columns are the only thing the projection
// changed.
func TestTextProjectionLeavesNonTextColumnsUnchanged(t *testing.T) {
	const (
		wd = canonical.TxKindWithdrawal
		dp = canonical.TxKindDeposit
		no = "<nil>"
	)
	t.Run("web", func(t *testing.T) {
		got := emitWebText(t)
		want := map[string]txShape{}
		add := func(id string, day int64, instr string, kind canonical.TxKind, net, payload string) {
			want[id+"@"+textAcct] = txShape{id + "@" + textAcct, day * 86400, textAcct, instr, kind, "CHF", no, net, no, no, payload}
		}
		add("W1", 100, "XS0000000001", canonical.TxKindDividend, "12.5",
			`{"Description1":"Example Fund Caption; XS0000000001","Description2":"Dividend","Description3":"Coupon detail","Transaction no.":"W1"}`)
		add("W2", 101, no, wd, "-80",
			`{"Description1":"EXAMPLE PAYEE; EXAMPLE STREET 1; 9999 EXAMPLE CITY","Description2":"e-banking payment order","Description3":"Invoice 42"}`)
		add("W3", 102, no, wd, "-5", `{"Description1":"","Description2":"KH","Description3":""}`)
		add("W4", 103, no, dp, "7", `{"Description1":"","Description2":"","Description3":""}`)
		add("W5", 104, no, dp, "9", `{"Description1":"","Description2":"credit","Description3":"Ref 7"}`)
		// Message-bearing rows: the kind is classified from the raw
		// column, and a message-led column falls to the direction.
		add("W6", 105, no, wd, "-65",
			`{"Description1":"EXAMPLE PAYEE; EXAMPLE STREET 1; 9999 EXAMPLETOWN","Description2":"THANKS; e-banking payment order","Description3":""}`)
		add("W7", 106, no, dp, "11", `{"Description1":"","Description2":"see you soon; credit","Description3":"Ref 9"}`)
		add("W8", 107, no, wd, "-42", `{"Description1":"","Description2":"UCCDDEXAMPLE1; order","Description3":""}`)
		// PDF rail-era payment order: external, stays a withdrawal.
		add("P1", 200, no, wd, "-150",
			`{"source":"account_statement_pdf","booking_type":"E-BANKING PAYMENT ORDER","internal_transfer":false,"counter_account":null,"continuation":["EXAMPLE GROCER","  ","EXAMPLE CITY","INVOICE 42"],"running_balance":1000.0,"value_date":"01.02.2025","post_closing":false}`)
		add("P2", 201, no, canonical.TxKindFee, "-3",
			`{"source":"account_statement_pdf","booking_type":"FEES","internal_transfer":false,"counter_account":null,"continuation":[]}`)
		// PDF credit with no counter IBAN and no rail booking: conservative
		// internal, demoted to `other`, source sign kept.
		add("P3", 202, no, canonical.TxKindOther, "40",
			`{"source":"account_statement_pdf","booking_type":null,"internal_transfer":false,"counter_account":null,"continuation":[]}`)
		checkShapes(t, got, want)
	})
	t.Run("psn", func(t *testing.T) {
		got := emitPSNText(t)
		want := map[string]txShape{
			"C1": {"C1", 1100, "CH00CASH", no, wd, "CHF", no, "-100", no, no,
				`{"amount":100.0,"credit_debit":"D","narrative":"E-BANKING ORDER\n EXAMPLE PAYEE \nREF 12345","account":"CH00CASH","funds":"CHF","txn_type":"NTRF","customer_ref":"NONREF"}`},
			"C2": {"C2", 1200, "CH00CASH", no, wd, "CHF", no, "-20", no, no,
				`{"amount":20.0,"credit_debit":"D","narrative":"KH","account":"CH00CASH","funds":"CHF","txn_type":"NMSC","customer_ref":"NONREF"}`},
			"C3": {"C3", 1300, "CH00CASH", no, dp, "CHF", no, "5", no, no,
				`{"amount":5.0,"credit_debit":"C","narrative":"","account":"CH00CASH","funds":"CHF","txn_type":"","customer_ref":""}`},
			"C4": {"C4", 1400, "CH00CASH", no, canonical.TxKindInterest, "CHF", no, "2.5", no, no,
				`{"amount":2.5,"credit_debit":"C","narrative":"INTERETS T1","account":"CH00CASH","funds":"CHF","txn_type":"NINT","customer_ref":"NONREF"}`},
			"T1": {"T1", 1500, "CH00CASH", "CH0000000001", canonical.TxKindBuy, "CHF", "-1500", "-1505", "100", "15",
				`{"side":"B","isin":"CH0000000001","gross_amount":1500.00,"net_amount":1505.00,"net_currency":"CHF","price":15.00,"quantity":100,"cash_account_external_id":"CH00CASH","security_name":"Example Share"}`},
			"A1": {"A1", 1600, "SK1", "CH0000000001", canonical.TxKindCorporateAction, "XXX", no, no, no, no,
				`{"isin":"CH0000000001","safekeeping":"SK1","caev":"DVCA"}`},
		}
		checkShapes(t, got, want)
	})
}

// seedPSNCashMovement inserts one MT940 cash movement with the bank
// reference the era text fold matches on. All ids synthetic.
func seedPSNCashMovement(t *testing.T, db *sql.DB, acct, bankRef, narrative, txnType string, day int64) {
	t.Helper()
	if _, err := db.Exec(`
        INSERT INTO events (event_external_id, timestamp, relationship_id,
            account_external_id, kind, currency_iso, payload)
        VALUES (?, ?, 'R1', ?, 'cash_movement', 'CHF',
                json_object('amount', '200', 'credit_debit', 'D',
                            'narrative', ?, 'account', '',
                            'funds', 'CHF', 'txn_type', ?, 'bank_ref', ?))`,
		"mt940:"+acct+":"+bankRef, day, acct, narrative, txnType, bankRef); err != nil {
		t.Fatalf("seed psn cash movement %s: %v", bankRef, err)
	}
}

// mergedText runs the merged transaction stream over both subsources
// and maps TransactionExternalID → the emitted row.
func mergedText(t *testing.T, r *webReader, psnDB *sql.DB) map[string]canonical.TransactionChange {
	t.Helper()
	conn := &Connection{web: r, psn: &psnReader{db: psnDB}}
	stream, err := conn.Transactions(context.Background(),
		canonical.Window{Start: 0, End: 1 << 40, HasChanges: true})
	if err != nil {
		t.Fatalf("merged Transactions: %v", err)
	}
	defer stream.Close()
	return drainTx(t, stream)
}

// TestEraTextFoldTakesTheRicherNarrative pins the era text fold. Both
// feeds record the same booking; the hard cut gives the MT940 row to
// gold, and where the bank wrote nothing but its own code into the
// :86: narrative that row reaches gold as that code, with no payee and
// a bare SWIFT type as the whole provider filing — a narrative no tier
// can place. The account-statement export's record of the same entry,
// found by the bank's own transaction number, carries the payee, the
// printed booking type and the detail line, and that is what the row
// keeps.
//
// A row whose own narrative already says something keeps it: the MT940
// row is the row of record and the fold only fills what it left as a
// code. Every value is invented.
func TestEraTextFoldTakesTheRicherNarrative(t *testing.T) {
	r := newWebTxFixture(t)
	seedWebAccount(t, r, textAcct)
	// The export's rows. X1's MT940 twin is a bare code; X2's twin
	// already carries a narrative; X3 has no twin at all.
	seedWebTextRow(t, r, "X1", 300*86400, 200.0, nil, "EXAMPLE PAYEE", "ATM Withdrawal",
		`{"Description1":"EXAMPLE PAYEE; 0000XXXXXXXX0000; ATM Withdrawal","Description2":"ATM Withdrawal","Description3":"Cost 0.00"}`)
	seedWebTextRow(t, r, "X2", 301*86400, 200.0, nil, "EXAMPLE GROCER", "e-banking payment order",
		`{"Description1":"EXAMPLE GROCER; EXAMPLE STREET 1; 9999 EXAMPLETOWN","Description2":"e-banking payment order","Description3":""}`)
	seedWebTextRow(t, r, "X3", 302*86400, 200.0, nil, "EXAMPLE FLORIST", "e-banking payment order",
		`{"Description1":"EXAMPLE FLORIST","Description2":"e-banking payment order","Description3":""}`)

	_, psnDB := newFixtureSilver(t)
	seedPSNCashMovement(t, psnDB, textAcct, "X1", "BAR", "NMSC", 300*86400)
	seedPSNCashMovement(t, psnDB, textAcct, "X2", "E-BANKING ORDER\nEXAMPLE GROCER", "NTRF", 301*86400)
	seedPSNCashMovement(t, psnDB, textAcct, "X9", "BAR", "NMSC", 303*86400)

	got := mergedText(t, r, psnDB)
	checkText(t, got, map[string]textCase{
		// The code-only row takes the export's three columns.
		"mt940:" + textAcct + ":X1": {"EXAMPLE PAYEE; 0000XXXXXXXX0000; ATM Withdrawal", "EXAMPLE PAYEE", "ATM Withdrawal"},
		// A narrative of its own is kept; only the columns that were
		// codes move, so the payee it never had is filled and the
		// description it did have is not.
		"mt940:" + textAcct + ":X2": {"E-BANKING ORDER; EXAMPLE GROCER", "EXAMPLE GROCER", "e-banking payment order"},
		// No export row under that number: untouched.
		"mt940:" + textAcct + ":X9": {"BAR", "<nil>", "NMSC"},
		// The export's own rows project as they always did.
		"X3@" + textAcct: {"EXAMPLE FLORIST", "EXAMPLE FLORIST", "e-banking payment order"},
	})
	// Nothing but the text moves.
	folded, ok := got["mt940:"+textAcct+":X1"]
	if !ok {
		t.Fatal("the folded row is missing from the merged stream")
	}
	if folded.OccurredAt != 300*86400 || folded.AccountExternalID != textAcct ||
		folded.Kind != canonical.TxKindWithdrawal || folded.Currency != "CHF" ||
		folded.NetAmount == nil || folded.NetAmount.String() != "-200" {
		t.Errorf("the fold moved something other than the text: %+v", folded)
	}
}

// TestEraTextFoldKeepsASingleEraRowUnchanged pins the other side of
// it: with no second feed to fold from, every row reaches gold exactly
// as its own era projects it — the PSN rows through the merged stream
// are byte-identical to the PSN-only ones.
func TestEraTextFoldKeepsASingleEraRowUnchanged(t *testing.T) {
	r := newWebTxFixture(t)
	seedWebAccount(t, r, textAcct)
	seedWebTextRow(t, r, "Y1", 300*86400, 200.0, nil, "EXAMPLE PAYEE", "ATM Withdrawal",
		`{"Description1":"EXAMPLE PAYEE; ATM Withdrawal","Description2":"ATM Withdrawal","Description3":""}`)

	_, psnDB := newFixtureSilver(t)
	// The same number on ANOTHER account: UBS stamps both legs of an
	// inter-account transfer with one number, so the account is half
	// the key and this must not fold.
	seedPSNCashMovement(t, psnDB, "CH0000000000000000ZZZ", "Y1", "BAR", "NMSC", 300*86400)
	// And an entry the export never carried.
	seedPSNCashMovement(t, psnDB, textAcct, "Y2", "INTERETS", "NINT", 301*86400)

	checkText(t, mergedText(t, r, psnDB), map[string]textCase{
		"mt940:CH0000000000000000ZZZ:Y1": {"BAR", "<nil>", "NMSC"},
		"mt940:" + textAcct + ":Y2":      {"INTERETS", "<nil>", "NINT"},
		"Y1@" + textAcct:                 {"EXAMPLE PAYEE; ATM Withdrawal", "EXAMPLE PAYEE", "ATM Withdrawal"},
	})
}

// TestCardReferenceIsNotAMemo pins the one lead that is the bank's own
// reference rather than the payer's words.
//
// A card-booked entry prefixes its type with the card's number and
// expiry. The memo is defined as what the payer typed — it is shown as
// such and a config rule may key on it — so the reference must not land
// there. Everything else about the row is unchanged: the booking type is
// still what follows the separator, so it categorises as before.
//
// Every value here is invented.
func TestCardReferenceIsNotAMemo(t *testing.T) {
	for _, tc := range []struct {
		name, in, wantMemo, wantType string
	}{
		{"card reference before an ATM booking",
			"11112222-0 10/27; ATM Withdrawal", "", "ATM Withdrawal"},
		{"card reference before a debit purchase",
			"11112222-0 10/27; Debit card payment", "", "Debit card payment"},
		{"card reference before a bancomat booking",
			"33334444-9 01/30; UBS Bancomat Withdrawal", "",
			"UBS Bancomat Withdrawal"},

		// A payer's message still travels, including one that opens
		// with digits — the reference is recognised by its whole
		// shape, not by starting with a number.
		{"a typed message", "THANKS; e-banking payment order",
			"THANKS", "e-banking payment order"},
		{"a message that is only digits", "12345678; e-banking payment order",
			"12345678", "e-banking payment order"},
		{"a message that opens like a reference",
			"11112222-0 SUBSCRIPTION; e-banking payment order",
			"11112222-0 SUBSCRIPTION", "e-banking payment order"},
		{"a date-shaped message", "10/27; e-banking payment order",
			"10/27", "e-banking payment order"},

		// No separator at all: the column is the booking type whole.
		{"no lead", "ATM WITHDRAWAL", "", "ATM WITHDRAWAL"},
		{"reversal suffix has no space", "CREDIT;Reversal", "", "CREDIT;Reversal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memo, bookingType := splitBookingType(tc.in)
			if memo != tc.wantMemo || bookingType != tc.wantType {
				t.Errorf("splitBookingType(%q) = (%q, %q), want (%q, %q)",
					tc.in, memo, bookingType, tc.wantMemo, tc.wantType)
			}
		})
	}
}

// TestCardReferenceDropDoesNotMoveTheProviderCategory: the categorisation
// of these rows must be untouched by the memo change — the booking type
// is what the provider tier reads, and it still is.
func TestCardReferenceDropDoesNotMoveTheProviderCategory(t *testing.T) {
	text, _, memo := projectWebTxText(
		"", "11112222-0 10/27; ATM Withdrawal", webTxPayload{}, false)
	if memo != "" {
		t.Errorf("memo = %q, want none", memo)
	}
	if text.providerCategory != "ATM Withdrawal" {
		t.Errorf("provider category = %q, want the booking type",
			text.providerCategory)
	}
	// And the description still leads with the booking type, so the
	// merchant signature this row reduces to is unchanged.
	if text.description != "ATM Withdrawal" {
		t.Errorf("description = %q, want the booking type",
			text.description)
	}
}

// A booking type promoted into the counterparty column is not a payee.
// The export feed writes it into Description1 on a row the bank filed
// without one, and gold's merchant signature prefers the counterparty
// over the description — so left standing it files every such row
// under one merchant named after the booking, and hides the payee the
// MT940 feed carries for the same booking in its narrative.
func TestBookingTypeIsNotPromotedAsThePayee(t *testing.T) {
	cases := []struct {
		name         string
		counterparty string
		wantPayee    string
	}{
		{"a fee booking names no payee", "Third-Party Charges", ""},
		{"nor does any other booking type", "Dividend", ""},
		{"a booking type spelled as the statement prints it", "DIVIDEND", ""},
		{"a real payee is still promoted", "Blue Harbour Cafe", "Blue Harbour Cafe"},
		{"a payee whose name merely contains one is still a payee",
			"Dividend Coffee Roasters", "Dividend Coffee Roasters"},
		{"an absent counterparty stays absent", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, _, _ := projectWebTxText(tc.counterparty, "", webTxPayload{}, false)
			if text.counterparty != tc.wantPayee {
				t.Errorf("counterparty = %q, want %q",
					text.counterparty, tc.wantPayee)
			}
		})
	}
}

// The two changes meet here: with the booking type refused as a payee,
// the MT940 narrative is what the signature reads, and its first
// segment is the payee rather than the bank's filing.
func TestRefusedBookingTypeLetsTheNarrativeNameTheMerchant(t *testing.T) {
	text, _, _ := projectWebTxText("Third-Party Charges", "", webTxPayload{}, false)
	got := spending.Normalize(text.counterparty,
		"Z44?Blue Harbour Cafe;Hafenstrasse 1;CH 8000 Zurich;INVOICE 4471")
	if got != "BLUE HARBOUR CAFE" {
		t.Errorf("signature = %q, want the payee from the narrative", got)
	}
}
