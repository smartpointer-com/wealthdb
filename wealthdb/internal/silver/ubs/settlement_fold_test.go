package ubs

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// The settlement fold. A securities trade reaches PSN on two rails that
// share no id — the MT515 confirmation and the MT940 `:61:` line for the
// cash leg settling it — so one trade is two rows unless something
// matches them on the booking. These tests pin that match: what folds,
// what survives, and which account the survivor names.
//
// Every id, account, ISIN, figure and date below is invented, and the
// dates sit in a decade the source cannot have booked in.

const (
	foldCashAcctID = "00000000000000000000A" // the bank's internal form
	foldCashIBAN   = "CH00CASHFOLD0000000A"  // what the registry is keyed by
	foldSafeAcct   = "00000000000000S9"
	foldISIN       = "XX0000000001"
)

// Two days in 2099, far outside any era the adapter reads. The fold keys
// on the settlement DAY, so the gap between them is what the fixture is
// for; both are UTC midnight.
const (
	foldTradeDay  int64 = 4070908800 // 2099-01-01
	foldSettleDay int64 = 4071081600 // 2099-01-03
)

func seedFoldCashAccount(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`
        INSERT INTO cash_accounts (snapshot_at, relationship_id,
            account_external_id, portfolio_external_id, payload)
        VALUES (1, 'R1', ?, 'P1', json_object('AcctId', ?))`,
		foldCashIBAN, foldCashAcctID); err != nil {
		t.Fatalf("seed cash account: %v", err)
	}
}

func seedTradeConfirmation(t *testing.T, db *sql.DB, eventID, side, amount string, settleUnix int64) {
	t.Helper()
	if _, err := db.Exec(`
        INSERT INTO events (event_external_id, timestamp, relationship_id,
            account_external_id, kind, currency_iso, payload)
        VALUES (?, ?, 'R1', ?, 'trade_confirmation', 'CHF',
                json_object('side', ?, 'isin', ?, 'net_amount', ?,
                            'net_currency', 'CHF', 'quantity', 10,
                            'price', 1, 'security_name', 'Fake Equity No1',
                            'cash_account_external_id', ?,
                            'safekeeping_external_id', ?,
                            'settlement_date_unix', ?))`,
		eventID, foldTradeDay, foldSafeAcct, side, foldISIN, amount,
		foldCashAcctID, foldSafeAcct, settleUnix); err != nil {
		t.Fatalf("seed trade confirmation %s: %v", eventID, err)
	}
}

func seedSettlementLeg(t *testing.T, db *sql.DB, eventID, amount, creditDebit string, day int64) {
	t.Helper()
	if _, err := db.Exec(`
        INSERT INTO events (event_external_id, timestamp, relationship_id,
            account_external_id, kind, currency_iso, payload)
        VALUES (?, ?, 'R1', ?, 'cash_movement', 'CHF',
                json_object('amount', ?, 'credit_debit', ?, 'narrative', 'B00?',
                            'account', ?, 'funds', 'CHF', 'txn_type', 'NSEC'))`,
		eventID, day, foldCashIBAN, amount, creditDebit, foldCashIBAN); err != nil {
		t.Fatalf("seed settlement leg %s: %v", eventID, err)
	}
}

// psnRows drains the PSN transaction stream, keyed by transaction id.
func psnRows(t *testing.T, db *sql.DB) map[string]canonical.TransactionChange {
	t.Helper()
	stream, err := (&psnReader{db: db}).Transactions(context.Background(),
		canonical.Window{Start: 0, End: 1 << 40, HasChanges: true}, nil)
	if err != nil {
		t.Fatalf("Transactions: %v", err)
	}
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
			break
		}
	}
	return out
}

// TestOneTradeIsOneRowWhicheverRailPrintedIt: the confirmation and the
// cash line it settles are one booking. The confirmation survives — it
// names the instrument, the quantity and the price where the cash line
// carries a booking code and no security at all.
func TestOneTradeIsOneRowWhicheverRailPrintedIt(t *testing.T) {
	_, db := newFixtureSilver(t)
	seedFoldCashAccount(t, db)
	seedTradeConfirmation(t, db, "mt515:T1", "BUY", "1000.00", foldSettleDay)
	seedSettlementLeg(t, db, "mt940:C1", "1000.00", "D", foldSettleDay)

	got := psnRows(t, db)
	if len(got) != 1 {
		t.Fatalf("emitted %d row(s), want 1: %v", len(got), got)
	}
	tx, ok := got["mt515:T1"]
	if !ok {
		t.Fatalf("the confirmation was dropped and the cash line kept: %v", got)
	}
	if tx.InstrumentExternalID == nil || *tx.InstrumentExternalID != foldISIN {
		t.Errorf("surviving row names no instrument")
	}
}

// TestTheSurvivingTradeNamesAnAccountTheRegistryKnows: the confirmation
// states the cash account in the bank's internal form, which is not the
// IBAN every other rail — and gold's account registry — uses. Left as
// given, the trade reaches the ledger attached to an account nothing
// else records.
func TestTheSurvivingTradeNamesAnAccountTheRegistryKnows(t *testing.T) {
	_, db := newFixtureSilver(t)
	seedFoldCashAccount(t, db)
	seedTradeConfirmation(t, db, "mt515:T1", "BUY", "1000.00", foldSettleDay)

	tx := psnRows(t, db)["mt515:T1"]
	if tx.AccountExternalID != foldCashIBAN {
		t.Errorf("account = %q, want the IBAN %q", tx.AccountExternalID, foldCashIBAN)
	}
}

// TestATradeTheCashFeedNeverCarriedKeepsItsConfirmation: the MT940 feed
// does not deliver every cash account, so a confirmation with no cash
// line to pair is the only record of that trade and must stand.
func TestATradeTheCashFeedNeverCarriedKeepsItsConfirmation(t *testing.T) {
	_, db := newFixtureSilver(t)
	seedFoldCashAccount(t, db)
	seedTradeConfirmation(t, db, "mt515:T1", "BUY", "1000.00", foldSettleDay)
	// A cash line for a DIFFERENT figure: same account, same day, not
	// this trade.
	seedSettlementLeg(t, db, "mt940:C9", "4321.00", "D", foldSettleDay)

	got := psnRows(t, db)
	if len(got) != 2 {
		t.Fatalf("emitted %d row(s), want both: %v", len(got), got)
	}
}

// TestAFundRedemptionIsASale: ISO 15022 states a fund order's direction
// in the business function (`:22H::BUSE//`) rather than the party the
// holder was, so a redemption says REDM where a market sale says SELL.
// Read for the market vocabulary alone it fell to the buy default, and
// a disposal was booked as an acquisition.
func TestAFundRedemptionIsASale(t *testing.T) {
	for _, tc := range []struct {
		side string
		want canonical.TxKind
	}{
		{"BUY", canonical.TxKindBuy},
		{"SUBS", canonical.TxKindBuy},
		{"SELL", canonical.TxKindSell},
		{"REDM", canonical.TxKindSell},
	} {
		t.Run(tc.side, func(t *testing.T) {
			_, db := newFixtureSilver(t)
			seedFoldCashAccount(t, db)
			seedTradeConfirmation(t, db, "mt515:T1", tc.side, "1000.00", foldSettleDay)
			if got := psnRows(t, db)["mt515:T1"].Kind; got != tc.want {
				t.Errorf("side %q = %q, want %q", tc.side, got, tc.want)
			}
		})
	}
}

// TestOnlyASecuritiesSettlementFolds: the fold reads the bank's own
// `:61:` type, not the figure. An ordinary payment that happens to match
// a trade's account, day and amount is a different booking and stays.
func TestOnlyASecuritiesSettlementFolds(t *testing.T) {
	_, db := newFixtureSilver(t)
	seedFoldCashAccount(t, db)
	seedTradeConfirmation(t, db, "mt515:T1", "BUY", "1000.00", foldSettleDay)
	if _, err := db.Exec(`
        INSERT INTO events (event_external_id, timestamp, relationship_id,
            account_external_id, kind, currency_iso, payload)
        VALUES ('mt940:P1', ?, 'R1', ?, 'cash_movement', 'CHF',
                json_object('amount', '1000.00', 'credit_debit', 'D',
                            'narrative', 'Z44?Blue Harbour Cafe',
                            'account', ?, 'funds', 'CHF', 'txn_type', 'NTRF'))`,
		foldSettleDay, foldCashIBAN, foldCashIBAN); err != nil {
		t.Fatalf("seed payment: %v", err)
	}

	got := psnRows(t, db)
	if len(got) != 2 {
		t.Fatalf("emitted %d row(s), want both: %v", len(got), got)
	}
}

// TestTheSeamKeysOnTheReferenceAndTheAccount pins the seam set's KEY,
// which is the half that decides what gets dropped: the bank stamps one
// Transaction no. on both legs of an inter-account transfer, so a key
// built on the reference alone would drop a leg the feed never held.
//
// That the drop then happens, and that a booking PSN never carried
// survives it, is pinned end to end against the merged stream by
// TestASeamDroppedRowDoesNotConsumeAVetoMatch (offset_veto_test.go) —
// this test would pass with the seam wired to nothing.
func TestTheSeamKeysOnTheReferenceAndTheAccount(t *testing.T) {
	const (
		sharedRef = "AW00000XX0000000"
		webOnly   = "AW00000XX0000001"
		acct      = "CH00SEAM000000000000"
		day       = foldTradeDay
	)
	_, psnDB := newFixtureSilver(t)
	if _, err := psnDB.Exec(`
        INSERT INTO cash_accounts (snapshot_at, relationship_id,
            account_external_id, portfolio_external_id, payload)
        VALUES (?, 'R1', ?, 'P1', json_object('AcctId', ?))`,
		day, acct, acct); err != nil {
		t.Fatalf("seed cash account: %v", err)
	}
	if _, err := psnDB.Exec(`
        INSERT INTO events (event_external_id, timestamp, relationship_id,
            account_external_id, kind, currency_iso, payload)
        VALUES ('mt940:S1', ?, 'R1', ?, 'cash_movement', 'CHF',
                json_object('amount', '250.00', 'credit_debit', 'D',
                            'narrative', 'Z44?Blue Harbour Cafe',
                            'account', ?, 'funds', 'CHF',
                            'txn_type', 'NTRF', 'bank_ref', ?))`,
		day, acct, acct, sharedRef); err != nil {
		t.Fatalf("seed psn movement: %v", err)
	}

	refs, err := (&webReader{}).buildSeamBankRefs(context.Background(), &psnReader{db: psnDB})
	if err != nil {
		t.Fatalf("buildSeamBankRefs: %v", err)
	}
	if !refs[webTxTextKey{account: acct, txnNo: sharedRef}] {
		t.Error("the booking PSN holds is not in the seam set")
	}
	if refs[webTxTextKey{account: acct, txnNo: webOnly}] {
		t.Error("a booking PSN never held is in the seam set — the web copy would be lost")
	}
	// The reference alone is not the identity: an inter-account
	// transfer's two legs share it and are two bookings.
	if refs[webTxTextKey{account: "CH00OTHER00000000000", txnNo: sharedRef}] {
		t.Error("the seam set matched on the reference without the account")
	}
}

// TestOneConfirmationFoldsOneCashLeg pins the arity. Two NSEC lines can
// land on one account, day, currency and magnitude — one settling a
// trade, one not — and a fold keyed on a set would drop both against the
// single confirmation, taking the second booking's money out of the cash
// ledger with it.
func TestOneConfirmationFoldsOneCashLeg(t *testing.T) {
	_, db := newFixtureSilver(t)
	seedFoldCashAccount(t, db)
	seedTradeConfirmation(t, db, "mt515:T1", "BUY", "1000.00", foldSettleDay)
	seedSettlementLeg(t, db, "mt940:C1", "1000.00", "D", foldSettleDay)
	seedSettlementLeg(t, db, "mt940:C2", "1000.00", "D", foldSettleDay)

	got := psnRows(t, db)
	if len(got) != 2 {
		t.Fatalf("emitted %d row(s), want the confirmation and the unpaired leg: %v", len(got), got)
	}
	if _, ok := got["mt515:T1"]; !ok {
		t.Error("the confirmation was dropped")
	}
	_, c1 := got["mt940:C1"]
	_, c2 := got["mt940:C2"]
	if c1 == c2 {
		t.Errorf("want exactly one cash leg folded, got C1=%v C2=%v", c1, c2)
	}
}
