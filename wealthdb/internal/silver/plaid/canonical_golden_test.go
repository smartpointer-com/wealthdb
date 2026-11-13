package plaid

import (
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver/silvertest"
)

// TestCanonicalGolden holds every record the adapter yields to a golden
// file per case. The item case is the shared seed item with the bank and
// investment ledgers of the ledger tests and a card statement; the lots
// case is the tax-lot fixture with long, short and bare lots.
func TestCanonicalGolden(t *testing.T) {
	t.Run("item", func(t *testing.T) {
		path, db := newFixture(t)
		seedItem(t, db, runAt(10))
		liability(t, db, runAt(10), "acct-card", "380", day(5))
		for _, r := range []bankRow{
			{id: "tx-grocer", account: "acct-checking", amount: "-40", name: "Grocer",
				original: "GROCER 0001", merchant: "Grocer", primary: "FOOD_AND_DRINK",
				detailed: "FOOD_AND_DRINK_GROCERIES", check: "1001", at: day(3)},
			{id: "tx-pay", account: "acct-checking", amount: "2500", name: "Payroll",
				primary: "INCOME", detailed: "INCOME_SALARY", check: "77", at: day(4)},
			{id: "tx-zero", account: "acct-checking", amount: "0", name: "Void",
				check: "1002", at: day(4)},
			{id: "tx-nocur", account: "acct-checking", amount: "-5", name: "Kiosk",
				noCurrency: true, at: day(4)},
			{id: "tx-card", account: "acct-card", amount: "-12.5", name: "Cafe",
				primary: "FOOD_AND_DRINK", detailed: "FOOD_AND_DRINK_COFFEE", pending: true, at: day(5)},
			{id: "tx-bill", account: "acct-card", amount: "300", name: "Payment",
				primary: "LOAN_PAYMENTS", detailed: "LOAN_PAYMENTS_CREDIT_CARD_PAYMENT", at: day(6)},
			{id: "tx-loan", account: "acct-student", amount: "200", name: "Payment",
				primary: "LOAN_PAYMENTS", detailed: "LOAN_PAYMENTS_STUDENT_LOAN_PAYMENT", at: day(6)},
			{id: "tx-home", account: "acct-home", amount: "1500", name: "Payment",
				primary: "LOAN_PAYMENTS", detailed: "LOAN_PAYMENTS_MORTGAGE_PAYMENT", at: day(6)},
		} {
			bank(t, db, r)
		}
		acct(t, db, runAt(10), "acct-coin", "investment", "crypto exchange", "0")
		sec(t, db, "sec-old", "PLACEHOLDER SOLD CORP", "EXB", "equity")
		sec(t, db, "sec-coin-a", "PLACEHOLDER TOKEN A", "EXC", "cryptocurrency")
		sec(t, db, "sec-coin-b", "PLACEHOLDER TOKEN B", "EXD", "cryptocurrency")
		for _, r := range []invRow{
			{id: "itx-buy", account: "acct-ira", security: "sec-eq", typ: "buy", subtype: "buy",
				amount: "-1500", quantity: "10", price: "150", at: day(2)},
			{id: "itx-div", account: "acct-ira", security: "sec-eq", typ: "cash",
				subtype: "qualified dividend", amount: "8.72", quantity: "0", price: "0", at: day(3)},
			{id: "itx-in", account: "acct-ira", security: "sec-cash", typ: "cash",
				subtype: "contribution", amount: "1200", quantity: "-1200", price: "1", at: day(4)},
			{id: "itx-out", account: "acct-ira", security: "sec-cash", typ: "cash",
				subtype: "withdrawal", amount: "-500", quantity: "500", price: "1", at: day(4)},
			{id: "itx-fee", account: "acct-ira", security: "sec-cash", typ: "fee",
				subtype: "account fee", amount: "-3", quantity: "3", price: "1", at: day(4)},
			{id: "itx-hint", account: "acct-ira", security: "sec-unknown", typ: "cash",
				subtype: "contribution", amount: "1200", quantity: "-1200", price: "1", at: day(4)},
			{id: "itx-xfer", account: "acct-ira", security: "sec-old", typ: "transfer",
				subtype: "transfer", amount: "0", quantity: "-5", price: "20", at: day(5)},
			{id: "itx-claw", account: "acct-ira", security: "sec-eq", typ: "cash",
				subtype: "dividend", amount: "-3", at: day(6)},
			{id: "itx-odd", account: "acct-ira", typ: "cash", subtype: "something new",
				amount: "1", at: day(8)},
			{id: "itx-newxfer", account: "acct-ira", security: "sec-old", typ: "transfer",
				subtype: "something new", amount: "0", quantity: "-5", price: "20", at: day(8)},
			{id: "itx-trade-out", account: "acct-coin", security: "sec-coin-a", typ: "transfer",
				subtype: "trade", amount: "0", quantity: "-0.1", price: "30000", at: day(9)},
			{id: "itx-trade-in", account: "acct-coin", security: "sec-coin-b", typ: "transfer",
				subtype: "trade", amount: "0", quantity: "1.5", price: "2000", at: day(9)},
		} {
			inv(t, db, r)
		}
		silvertest.Golden(t, openConn(t, path), "testdata/canonical_item.jsonl")
	})
	t.Run("lots", func(t *testing.T) {
		path, db := lotsFixture(t)
		hold(t, db, runAt(10), "acct-brk", "sec-a", 0, "9", "1200", "900")
		taxLots(t, db, "sec-a", 0, `[
		{"cost_basis":1000,"current_value":1500,"institution_lot_id":"LOT-1",
		 "original_purchase_datetime":"2030-04-15T00:00:00.000Z","position_type":"LONG",
		 "purchase_price":100,"quantity":10},
		{"cost_basis":-100,"current_value":-300,"institution_lot_id":"LOT-2",
		 "original_purchase_datetime":"2029-02-10T00:00:00Z","position_type":"SHORT",
		 "purchase_price":50,"quantity":-2},
		{"cost_basis":null,"current_value":null,"institution_lot_id":null,
		 "original_purchase_datetime":null,"position_type":null,
		 "purchase_price":null,"quantity":1}]`)
		silvertest.Golden(t, openConn(t, path), "testdata/canonical_lots.jsonl")
	})
}
