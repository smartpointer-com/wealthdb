package ubs

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
	"github.com/ptu-gh/wealthdb/wealthdb/internal/silver"
)

// newWebTxFixture builds an in-memory ubs-web silver with the accounts and
// transactions tables the transaction stream reads. All ids synthetic /
// IBAN-spec placeholder letters (CLAUDE.md §4).
func newWebTxFixture(t *testing.T) *webReader {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/ubs-web.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	const schema = `
CREATE TABLE accounts (
    snapshot_at INTEGER NOT NULL, account_external_id TEXT NOT NULL,
    kind TEXT NOT NULL, banking_relationship_id TEXT, payload TEXT,
    PRIMARY KEY (snapshot_at, account_external_id));
CREATE TABLE transactions (
    transaction_external_id TEXT NOT NULL, account_external_id TEXT NOT NULL,
    snapshot_at INTEGER NOT NULL, value_date INTEGER NOT NULL,
    currency_iso TEXT NOT NULL, amount_debit REAL, amount_credit REAL,
    counterparty TEXT, description_kind TEXT, payload TEXT NOT NULL,
    PRIMARY KEY (transaction_external_id, account_external_id));`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return &webReader{db: db}
}

const (
	vetoAcctA = "CH0000000000000000AAA"
	vetoAcctB = "CH0000000000000000BBB"
	vetoDay1  = int64(100 * 86400)
	vetoDay2  = int64(101 * 86400)
)

// seedRailEraAnchor inserts a tiny MT940-era row well before the test rows so
// mt940FeedStart opens the rail-promotion era for them (a fixture with only
// PDF rows is otherwise the deep backfill era, where the promotion is off by
// design — TestDeepEraWireStaysInternal pins that). The 1-unit amount can
// never amount-match a veto pair.
func seedRailEraAnchor(t *testing.T, r *webReader) {
	t.Helper()
	seedWebTx(t, r, "ANCHOR", vetoAcctA, 90*86400, "CHF", 1, false)
}

func seedWebAccount(t *testing.T, r *webReader, acct string) {
	t.Helper()
	if _, err := r.db.Exec(`INSERT INTO accounts VALUES (1000, ?, 'cash', 'REL1', '{}')`, acct); err != nil {
		t.Fatalf("seed account: %v", err)
	}
}

// seedWebTx inserts one cash row. Positive amt = credit, negative = debit.
// pdfRail marks the row as a PDF-backfill payment-order booking; otherwise the
// payload has no source marker (MT940-era shape).
func seedWebTx(t *testing.T, r *webReader, txID, acct string, day int64, ccy string, amt float64, pdfRail bool) {
	t.Helper()
	payload, kind := `{}`, ""
	if pdfRail {
		payload = `{"source":"account_statement_pdf","booking_type":"E-BANKING PAYMENT ORDER","internal_transfer":false,"counter_account":null}`
		kind = "E-BANKING PAYMENT ORDER"
	}
	seedWebTxRaw(t, r, txID, acct, day, ccy, amt, kind, payload)
}

// seedWebTxRaw inserts one cash row with the booking type and payload given
// verbatim, for a case seedWebTx's two shapes do not cover.
func seedWebTxRaw(t *testing.T, r *webReader, txID, acct string, day int64, ccy string, amt float64, bookingType, payload string) {
	t.Helper()
	var debit, credit any
	if amt < 0 {
		debit = -amt
	} else {
		credit = amt
	}
	if _, err := r.db.Exec(`
        INSERT INTO transactions (transaction_external_id, account_external_id,
            snapshot_at, value_date, currency_iso, amount_debit, amount_credit,
            description_kind, payload)
        VALUES (?, ?, 1000, ?, ?, ?, ?, ?, ?)`,
		txID, acct, day, ccy, debit, credit, bookingType, payload); err != nil {
		t.Fatalf("seed tx: %v", err)
	}
}

// emittedKinds runs the web transaction stream over the full window and maps
// TransactionExternalID → kind.
func emittedKinds(t *testing.T, r *webReader, psn *psnReader) map[string]canonical.TxKind {
	t.Helper()
	stream, _, err := r.transactionsBeforePSNStart(context.Background(),
		canonical.Window{Start: 0, End: 1 << 40, HasChanges: true}, psn, nil)
	if err != nil {
		t.Fatalf("transactionsBeforePSNStart: %v", err)
	}
	out := map[string]canonical.TxKind{}
	for {
		batch, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		for _, tx := range batch.Transactions {
			out[tx.TransactionExternalID] = tx.Kind
		}
		if !more {
			break
		}
	}
	return out
}

// emittedInternal is what the veto now writes: the conduit verdict rides the
// payload so the row keeps a truthful kind, which the SPENDING population
// reads. Before this the verdict was a rewritten kind, and a vetoed
// supermarket payment vanished from spending along with the flow.
func emittedInternal(t *testing.T, r *webReader, psn *psnReader) map[string]bool {
	t.Helper()
	stream, _, err := r.transactionsBeforePSNStart(context.Background(),
		canonical.Window{Start: 0, End: 1 << 40, HasChanges: true}, psn, nil)
	if err != nil {
		t.Fatalf("transactionsBeforePSNStart: %v", err)
	}
	out := map[string]bool{}
	for {
		batch, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		for _, tx := range batch.Transactions {
			out[tx.TransactionExternalID] = strings.Contains(
				string(tx.Payload), `"returns_flow":"internal"`)
		}
		if !more {
			break
		}
	}
	return out
}

// TestOffsetVetoDemotesMirroredLegs pins the same-day offset veto: an
// intra-relationship move recorded as a PDF payment order (no counter IBAN)
// plus its mirror credit on another own account must demote BOTH legs to a
// non-flow kind — including the MT940-era credit leg that no per-row
// classifier touches.
func TestOffsetVetoDemotesMirroredLegs(t *testing.T) {
	r := newWebTxFixture(t)
	seedRailEraAnchor(t, r)
	seedWebAccount(t, r, vetoAcctA)
	seedWebAccount(t, r, vetoAcctB)
	seedWebTx(t, r, "T1", vetoAcctA, vetoDay1, "CHF", -250000, true) // PDF payment order out
	seedWebTx(t, r, "T2", vetoAcctB, vetoDay1, "CHF", 250000, false) // MT940-era credit in

	internal := emittedInternal(t, r, nil)
	if !internal["T1@"+vetoAcctA] {
		t.Errorf("outbound mirrored leg: want the conduit verdict on its payload")
	}
	if !internal["T2@"+vetoAcctB] {
		t.Errorf("inbound mirrored leg: want the conduit verdict on its payload")
	}
}

// TestOffsetVetoLeavesUnmirroredWireExternal is the item's regression target:
// an outbound PDF payment order with NO mirror on any own account (the wire to
// an own account at another bank) must stay a counted withdrawal — before the
// rail promotion its null counter demoted it, understating outflows while the
// receiving bank counted the arrival.
func TestOffsetVetoLeavesUnmirroredWireExternal(t *testing.T) {
	r := newWebTxFixture(t)
	seedRailEraAnchor(t, r)
	seedWebAccount(t, r, vetoAcctA)
	seedWebAccount(t, r, vetoAcctB)
	seedWebTx(t, r, "T1", vetoAcctA, vetoDay1, "CHF", -123400, true)

	kinds := emittedKinds(t, r, nil)
	if got := kinds["T1@"+vetoAcctA]; got != canonical.TxKindWithdrawal {
		t.Errorf("unmirrored outbound payment order = %v, want withdrawal", got)
	}
}

// TestOffsetVetoRequiresSameDayAndOtherAccount pins the veto's guards: a
// next-day mirror or a same-account debit/credit pair must not veto.
func TestOffsetVetoRequiresSameDayAndOtherAccount(t *testing.T) {
	r := newWebTxFixture(t)
	seedRailEraAnchor(t, r)
	seedWebAccount(t, r, vetoAcctA)
	seedWebAccount(t, r, vetoAcctB)
	// Next-day mirror: no veto — the wire stays external, the credit stays a
	// deposit (a real round-trip through another bank looks exactly like this).
	seedWebTx(t, r, "T1", vetoAcctA, vetoDay1, "CHF", -50000, true)
	seedWebTx(t, r, "T2", vetoAcctB, vetoDay2, "CHF", 50000, false)
	// Same-account pair: no veto (nothing moved between accounts).
	seedWebTx(t, r, "T3", vetoAcctA, vetoDay2, "USD", -7000, true)
	seedWebTx(t, r, "T4", vetoAcctA, vetoDay2, "USD", 7000, false)

	kinds := emittedKinds(t, r, nil)
	if got := kinds["T1@"+vetoAcctA]; got != canonical.TxKindWithdrawal {
		t.Errorf("next-day outbound = %v, want withdrawal", got)
	}
	if got := kinds["T2@"+vetoAcctB]; got != canonical.TxKindDeposit {
		t.Errorf("next-day inbound = %v, want deposit", got)
	}
	if got := kinds["T3@"+vetoAcctA]; got != canonical.TxKindWithdrawal {
		t.Errorf("same-account outbound = %v, want withdrawal", got)
	}
	if got := kinds["T4@"+vetoAcctA]; got != canonical.TxKindDeposit {
		t.Errorf("same-account inbound = %v, want deposit", got)
	}
}

// TestOffsetVetoIsOneToOne pins 1:1 greedy pairing: two equal outbound orders
// against ONE mirror credit demote exactly one debit (plus the credit); the
// unpaired twin keeps its external classification via the rail promotion.
func TestOffsetVetoIsOneToOne(t *testing.T) {
	r := newWebTxFixture(t)
	seedRailEraAnchor(t, r)
	seedWebAccount(t, r, vetoAcctA)
	seedWebAccount(t, r, vetoAcctB)
	seedWebTx(t, r, "T1", vetoAcctA, vetoDay1, "CHF", -10000, true)
	seedWebTx(t, r, "T2", vetoAcctA, vetoDay1, "CHF", -10000, true)
	seedWebTx(t, r, "T3", vetoAcctB, vetoDay1, "CHF", 10000, false)

	kinds := emittedKinds(t, r, nil)
	internal := emittedInternal(t, r, nil)
	if !internal["T3@"+vetoAcctB] {
		t.Errorf("mirror credit: want the conduit verdict on its payload")
	}
	vetoed := 0
	for _, id := range []string{"T1@" + vetoAcctA, "T2@" + vetoAcctA} {
		if got := kinds[id]; got != canonical.TxKindWithdrawal {
			t.Errorf("%s = %v, want withdrawal — the veto no longer rewrites the kind", id, got)
		}
		if internal[id] {
			vetoed++
		}
	}
	if vetoed != 1 {
		t.Errorf("vetoed %d of the twin debits, want exactly 1", vetoed)
	}
}

// TestOffsetVetoPrefersBankLinkedTwin pins the shared-Transaction-no.
// preference: UBS stamps both sides of an inter-account transfer with the same
// number, so among equal-amount candidates the bank-linked leg pairs first and
// the coincidental other credit keeps its kind.
func TestOffsetVetoPrefersBankLinkedTwin(t *testing.T) {
	r := newWebTxFixture(t)
	seedRailEraAnchor(t, r)
	const acctC = "CH0000000000000000CCC"
	seedWebAccount(t, r, vetoAcctA)
	seedWebAccount(t, r, vetoAcctB)
	seedWebAccount(t, r, acctC)
	// The debit's twin shares its Transaction no. on account C; account B holds
	// an unrelated same-amount credit that sorts FIRST (B < C) and would win
	// under naive ordering.
	seedWebTx(t, r, "T9", vetoAcctA, vetoDay1, "CHF", -25000, true)
	seedWebTx(t, r, "T2", vetoAcctB, vetoDay1, "CHF", 25000, false)
	seedWebTx(t, r, "T9", acctC, vetoDay1, "CHF", 25000, false)

	kinds := emittedKinds(t, r, nil)
	internal := emittedInternal(t, r, nil)
	if !internal["T9@"+acctC] {
		t.Errorf("bank-linked twin: want the conduit verdict on its payload")
	}
	if got := kinds["T2@"+vetoAcctB]; got != canonical.TxKindDeposit {
		t.Errorf("coincidental credit = %v, want deposit", got)
	}
}

// TestOffsetVetoPairsAgainstPSNMovement pins the feed seam through the MERGED
// stream: a web debit whose mirror lives in the PSN feed (a cash_movement
// event) is vetoed AND the PSN mirror is demoted too — a pair must drop on
// both sides or the survivor books a one-sided phantom external flow. An
// unmatched PSN movement keeps its kind.
func TestOffsetVetoPairsAgainstPSNMovement(t *testing.T) {
	r := newWebTxFixture(t)
	seedRailEraAnchor(t, r)
	seedWebAccount(t, r, vetoAcctA)
	seedWebTx(t, r, "T1", vetoAcctA, vetoDay1, "CHF", -40000, true)

	_, psnDB := newFixtureSilver(t)
	seedPSNCash := func(id string, day int64, amt, cd, ccy string) {
		t.Helper()
		if _, err := psnDB.Exec(`
        INSERT INTO events (event_external_id, timestamp, relationship_id,
            account_external_id, kind, currency_iso, payload)
        VALUES (?, ?, 'R1', '02300000000000XX0000X', 'cash_movement', ?,
                '{"amount":"`+amt+`","credit_debit":"`+cd+`","narrative":"TRANSFER","account":"","funds":"`+ccy+`"}')`,
			id, day, ccy); err != nil {
			t.Fatalf("seed psn event: %v", err)
		}
	}
	seedPSNCash("mt940:1", vetoDay1, "40000", "C", "CHF") // T1's mirror
	seedPSNCash("mt940:2", vetoDay1, "12345", "C", "CHF") // unmatched arrival

	conn := &Connection{web: r, psn: &psnReader{db: psnDB}}
	stream, err := conn.Transactions(context.Background(),
		canonical.Window{Start: 0, End: 1 << 40, HasChanges: true})
	if err != nil {
		t.Fatalf("merged Transactions: %v", err)
	}
	kinds := map[string]canonical.TxKind{}
	internal := map[string]bool{}
	for {
		batch, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		for _, tx := range batch.Transactions {
			kinds[tx.TransactionExternalID] = tx.Kind
			internal[tx.TransactionExternalID] = strings.Contains(
				string(tx.Payload), `"returns_flow":"internal"`)
		}
		if !more {
			break
		}
	}
	if !internal["T1@"+vetoAcctA] {
		t.Errorf("web debit mirrored by a PSN movement: want the conduit verdict on its payload")
	}
	if !internal["mt940:1"] {
		t.Errorf("PSN mirror leg: want the conduit verdict on the payload")
	}
	if got := kinds["mt940:2"]; got != canonical.TxKindDeposit {
		t.Errorf("unmatched PSN movement = %v, want deposit", got)
	}
}

// TestOffsetVetoTwinPhaseIsGlobal pins the two-phase matcher: the bank-linked
// twin phase completes for EVERY debit before any loose pairing, so a
// twin-less debit that merely sorts earlier can never steal another debit's
// shared-Transaction-no. twin.
func TestOffsetVetoTwinPhaseIsGlobal(t *testing.T) {
	r := newWebTxFixture(t)
	const acctC = "CH0000000000000000CCC"
	seedRailEraAnchor(t, r)
	seedWebAccount(t, r, vetoAcctA)
	seedWebAccount(t, r, vetoAcctB)
	seedWebAccount(t, r, acctC)
	// Twin-less debit on account A sorts before the twinned debit on B; the
	// only credit is B's bank-linked twin on C.
	seedWebTx(t, r, "T1", vetoAcctA, vetoDay1, "CHF", -15000, true)
	seedWebTx(t, r, "T9", vetoAcctB, vetoDay1, "CHF", -15000, true)
	seedWebTx(t, r, "T9", acctC, vetoDay1, "CHF", 15000, false)

	kinds := emittedKinds(t, r, nil)
	internal := emittedInternal(t, r, nil)
	if !internal["T9@"+vetoAcctB] {
		t.Errorf("twinned debit: want the conduit verdict on its payload")
	}
	if !internal["T9@"+acctC] {
		t.Errorf("twin credit: want the conduit verdict on its payload")
	}
	if got := kinds["T1@"+vetoAcctA]; got != canonical.TxKindWithdrawal {
		t.Errorf("twin-less debit = %v, want withdrawal (must not steal the twin)", got)
	}
}

// TestOffsetVetoInternalLegPairsItsMirror pins the parser-internal true-pair
// class: an UEBERTRAG-marked PDF payment order's receiving leg lands in the
// MT940 feed as a bare credit, and the veto must demote that mirror too —
// the parser flag already proves the money stayed inside the relationship,
// so a surviving mirror would book a phantom external inflow.
func TestOffsetVetoInternalLegPairsItsMirror(t *testing.T) {
	r := newWebTxFixture(t)
	seedRailEraAnchor(t, r)
	seedWebAccount(t, r, vetoAcctA)
	seedWebAccount(t, r, vetoAcctB)
	if _, err := r.db.Exec(`
        INSERT INTO transactions (transaction_external_id, account_external_id,
            snapshot_at, value_date, currency_iso, amount_debit, amount_credit,
            description_kind, payload)
        VALUES ('T1', ?, 1000, ?, 'CHF', 20000, NULL, 'SPECIAL PAYMENT ORDER',
                '{"source":"account_statement_pdf","booking_type":"SPECIAL PAYMENT ORDER","internal_transfer":true,"counter_account":null}')`,
		vetoAcctA, vetoDay1); err != nil {
		t.Fatalf("seed internal debit: %v", err)
	}
	seedWebTx(t, r, "T2", vetoAcctB, vetoDay1, "CHF", 20000, false)

	internal := emittedInternal(t, r, nil)
	if !internal["T1@"+vetoAcctA] {
		t.Errorf("parser-internal debit: want the conduit verdict on its payload")
	}
	if !internal["T2@"+vetoAcctB] {
		t.Error("its MT940 mirror: want the conduit verdict too (both sides of the move drop)")
	}
}

// TestOffsetVetoProbeSkipsSuppressedWebRows pins the emitted-universe filter:
// a web row at/after its relationship's PSN cutover is not emitted and must
// not consume a veto match either (its movement is represented by the PSN
// feed).
func TestOffsetVetoProbeSkipsSuppressedWebRows(t *testing.T) {
	r := newWebTxFixture(t)
	seedRailEraAnchor(t, r)
	// The debit's account has no relationship mapping (no cutover applies);
	// the credit's account belongs to REL1, whose rows are suppressed from
	// the cutover onward.
	if _, err := r.db.Exec(`INSERT INTO accounts VALUES (1000, ?, 'cash', NULL, '{}')`, vetoAcctA); err != nil {
		t.Fatalf("seed unmapped account: %v", err)
	}
	seedWebAccount(t, r, vetoAcctB)
	seedWebTx(t, r, "T1", vetoAcctA, vetoDay1, "CHF", -25000, true)
	seedWebTx(t, r, "T2", vetoAcctB, vetoDay1, "CHF", 25000, false)

	// With no PSN reader at all the splice is degenerate and no cutover
	// applies, so one is present (empty PSN silver + explicit override).
	_, psnDB := newFixtureSilver(t)
	stream, _, err := r.transactionsBeforePSNStart(context.Background(),
		canonical.Window{Start: 0, End: 1 << 40, HasChanges: true}, &psnReader{db: psnDB},
		[]silver.RelationshipPair{{WebID: "REL1", PSNStartOverride: vetoDay1 - 86400}})
	if err != nil {
		t.Fatalf("transactionsBeforePSNStart: %v", err)
	}
	kinds := map[string]canonical.TxKind{}
	for {
		batch, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		for _, tx := range batch.Transactions {
			kinds[tx.TransactionExternalID] = tx.Kind
		}
		if !more {
			break
		}
	}
	if _, ok := kinds["T2@"+vetoAcctB]; ok {
		t.Error("suppressed credit must not be emitted")
	}
	// The suppressed credit is also absent from the probe, so the emitted
	// debit keeps its external classification instead of being demoted
	// against a row the stream never carries.
	if got := kinds["T1@"+vetoAcctA]; got != canonical.TxKindWithdrawal {
		t.Errorf("debit paired against a suppressed row = %v, want withdrawal", got)
	}
}

// TestDeepEraWireStaysInternal pins the era gate end-to-end: with NO MT940
// rows in the silver, the rail-promotion era never opens and an outbound PDF
// payment order stays demoted — the deep backfill era is conservative in both
// directions.
func TestDeepEraWireStaysInternal(t *testing.T) {
	r := newWebTxFixture(t)
	seedWebAccount(t, r, vetoAcctA)
	seedWebTx(t, r, "T1", vetoAcctA, vetoDay1, "CHF", -123400, true)

	internal := emittedInternal(t, r, nil)
	if !internal["T1@"+vetoAcctA] {
		t.Errorf("deep-era outbound payment order: want the conduit verdict on its payload")
	}
}

// TestWebChangeWindowStartIsTrueMinimum pins the window-start regression: the
// start must be the SMALLEST valid minimum across the snapshot and
// transaction ranges, whichever order the two are scanned in. (The old guard
// short-circuited on a flag no prior code set, leaving Start at the LAST
// valid minimum — wrong whenever snapshots begin before transactions.)
func TestWebChangeWindowStartIsTrueMinimum(t *testing.T) {
	r := newWebTxFixture(t)
	if _, err := r.db.Exec(`
        CREATE TABLE dump_runs (snapshot_at INTEGER, silver_schema_version INTEGER, run_dir TEXT);
        INSERT INTO dump_runs VALUES (500, 1, '/x/1');`); err != nil {
		t.Fatalf("seed dump_runs: %v", err)
	}
	seedWebAccount(t, r, vetoAcctA)
	seedWebTx(t, r, "T1", vetoAcctA, 900, "CHF", 100, false) // txMin AFTER snapMin

	w, err := r.ChangeWindow(context.Background(), -1)
	if err != nil {
		t.Fatalf("ChangeWindow: %v", err)
	}
	if !w.HasChanges || w.Start != 500 {
		t.Errorf("Start = %d (HasChanges=%v), want the snapshot minimum 500", w.Start, w.HasChanges)
	}
}

// TestConduitVerdictLeavesTheRowSpendable is the regression this whole
// carrier change exists for. The verdict answers a RETURNS question — is
// this owner capital crossing the boundary? — and used to be recorded by
// rewriting the row's kind. The spending population selects on that same
// kind, so a deep-era card payment, which is not owner capital under any
// reading, was demoted out of spending too — taking the whole pre-2024
// card and cash population with it.
func TestConduitVerdictLeavesTheRowSpendable(t *testing.T) {
	r := newWebTxFixture(t)
	seedWebAccount(t, r, vetoAcctA)
	// A deep-era (pre-rail) debit-card payment: conservative-internal for
	// returns, and unambiguous consumption for spending.
	seedWebTxRaw(t, r, "CARD", vetoAcctA, vetoDay1, "CHF", -111.11,
		"DEBIT CARD PAYMENT",
		`{"source":"account_statement_pdf","booking_type":"DEBIT CARD PAYMENT",`+
			`"internal_transfer":false,"counter_account":null,"continuation":[]}`)

	kinds := emittedKinds(t, r, nil)
	internal := emittedInternal(t, r, nil)
	if got := kinds["CARD@"+vetoAcctA]; got != canonical.TxKindWithdrawal {
		t.Errorf("kind = %v, want withdrawal — spending reads the kind", got)
	}
	if !internal["CARD@"+vetoAcctA] {
		t.Error("want the conduit verdict on the payload — returns must still skip it")
	}
}

// TestTheConduitVerdictNeverGoesMissing: a payload that cannot carry the
// flag falls back to the old carrier rather than losing the verdict.
// Counting conduit churn as owner capital is the worse of the two errors —
// a row demoted to `other` is merely absent from spending, which is where
// every such row already was.
func TestTheConduitVerdictNeverGoesMissing(t *testing.T) {
	for _, tc := range []struct {
		name, payload, want string
		degrades            bool
	}{
		{"object", `{"a":1}`, `{"returns_flow":"internal","a":1}`, false},
		{"empty object", `{}`, `{"returns_flow":"internal"}`, false},
		{"not an object", `"scalar"`, `"scalar"`, true},
		{"empty", ``, ``, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := withReturnsFlow(tc.payload, true)
			if string(got) != tc.want {
				t.Errorf("payload = %s, want %s", got, tc.want)
			}
			if ok == tc.degrades {
				t.Errorf("carried = %v, want %v", ok, !tc.degrades)
			}
			_, kind := markReturnsInternal(json.RawMessage(tc.payload), canonical.TxKindWithdrawal)
			wantKind := canonical.TxKindWithdrawal
			if tc.degrades {
				wantKind = canonical.TxKindOther
			}
			if kind != wantKind {
				t.Errorf("kind = %v, want %v", kind, wantKind)
			}
		})
	}
}

// TestASeamDroppedRowDoesNotConsumeAVetoMatch pins the wiring between the
// two suppressions and the veto. The seam drops a web row because the MT940
// feed already holds that booking; if the veto still counts that row in its
// emitted universe, the row it no longer emits can win the mirror its PSN
// counterpart needed. One leg is then demoted while the other keeps its
// flow kind — the one-sided phantom flow the veto exists to prevent.
//
// Shape: two own accounts, one day. A's debit is held by BOTH feeds (the
// seam window), so the seam drops the web copy and the MT940 row is what
// gold keeps. B's credit is the mirror. Every id and figure is invented.
func TestASeamDroppedRowDoesNotConsumeAVetoMatch(t *testing.T) {
	const sharedRef = "AW00000XX0000000"

	r := newWebTxFixture(t)
	seedRailEraAnchor(t, r)
	seedWebAccount(t, r, vetoAcctA)
	seedWebAccount(t, r, vetoAcctB)
	// The duplicated leg, under the bank's own number for the entry...
	seedWebTx(t, r, sharedRef, vetoAcctA, vetoDay1, "CHF", -40000, true)
	// ...and its same-day mirror on another own account, web-only.
	seedWebTx(t, r, "T2", vetoAcctB, vetoDay1, "CHF", 40000, true)

	_, psnDB := newFixtureSilver(t)
	if _, err := psnDB.Exec(`
        INSERT INTO events (event_external_id, timestamp, relationship_id,
            account_external_id, kind, currency_iso, payload)
        VALUES ('mt940:`+vetoAcctA+`:`+sharedRef+`', ?, 'R1', ?, 'cash_movement', 'CHF',
                json_object('amount','40000','credit_debit','D','narrative','TRANSFER',
                            'account', ?, 'funds','CHF','bank_ref', ?))`,
		vetoDay1, vetoAcctA, vetoAcctA, sharedRef); err != nil {
		t.Fatalf("seed psn event: %v", err)
	}

	conn := &Connection{web: r, psn: &psnReader{db: psnDB}}
	stream, err := conn.Transactions(context.Background(),
		canonical.Window{Start: 0, End: 1 << 40, HasChanges: true})
	if err != nil {
		t.Fatalf("merged Transactions: %v", err)
	}
	kinds := map[string]canonical.TxKind{}
	internal := map[string]bool{}
	for {
		batch, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		for _, tx := range batch.Transactions {
			kinds[tx.TransactionExternalID] = tx.Kind
			internal[tx.TransactionExternalID] = strings.Contains(
				string(tx.Payload), `"returns_flow":"internal"`)
		}
		if !more {
			break
		}
	}

	// The seam did its job: the web copy is gone, the MT940 row kept it.
	if _, ok := kinds[sharedRef+"@"+vetoAcctA]; ok {
		t.Fatal("the seam did not drop the web copy — this test no longer tests what it claims")
	}
	psnID := "mt940:" + vetoAcctA + ":" + sharedRef
	if _, ok := kinds[psnID]; !ok {
		t.Fatal("the PSN row is not emitted — this test no longer tests what it claims")
	}
	// Both legs of the pair move together, or neither does.
	if internal[psnID] != internal["T2@"+vetoAcctB] {
		t.Errorf("one-sided demotion: PSN leg internal=%v, mirror internal=%v — a phantom external flow",
			internal[psnID], internal["T2@"+vetoAcctB])
	}
}

// The cut belongs to a relationship: a row on its account is PSN's from
// the relationship's PSN start on, and a relationship with no start — or
// an account no relationship claims — cuts nothing.
func TestPSNCutExcludesFromTheRelationshipsStart(t *testing.T) {
	cut := psnCut{
		startByRel:   map[string]int64{"rel-a": 1000},
		relOfAccount: map[string]string{"acct-a": "rel-a", "acct-b": "rel-b"},
	}
	for _, tc := range []struct {
		account string
		at      int64
		want    bool
	}{
		{"acct-a", 999, false},
		{"acct-a", 1000, true},
		{"acct-b", 5000, false},
		{"acct-x", 5000, false},
	} {
		if got := cut.excludes(tc.account, tc.at); got != tc.want {
			t.Errorf("excludes(%q, %d) = %v, want %v", tc.account, tc.at, got, tc.want)
		}
	}
}
