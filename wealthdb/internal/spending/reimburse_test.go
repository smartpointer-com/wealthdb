package spending

import (
	"regexp"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// TestReimbursementNetsAgainstItsOutflow covers the three roads a
// repayment links by (gold migration 0160) and the one it is left on
// when none answers. Every name and figure is invented.
//
//   - signature: a sports shop refunds part of a purchase; the
//     repayment's payer signature is the purchase's merchant signature.
//   - store: a utility's credit with no outflow in the window takes the
//     category the merchant store files the utility under.
//   - pair: a friend repays a dinner; the ledger names the pair.
//   - a refund larger than the purchase answers no outflow, and nets by
//     the store road; a repayment no road reaches stays out of spending
//     and counts as unlinked.
func TestReimbursementNetsAgainstItsOutflow(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "SHOP", "CARD1", "purchase", day(10), -80, "Example Sports", "", ""},
		txn{"bank", "SHOP-BACK", "CASH1", "deposit", day(20), 30, "Example Sports", "", ""},
		txn{"bank", "POWER-BACK", "CASH1", "deposit", day(30), 25, "Example Power", "", ""},
		txn{"bank", "DINNER", "CARD1", "purchase", day(40), -120, "Example Bistro", "", ""},
		txn{"bank", "FRIEND-BACK", "CASH1", "deposit", day(45), 40, "Example Friend", "", ""},
		txn{"bank", "TOO-MUCH", "CASH1", "deposit", day(46), 500, "Example Sports", "", ""},
		txn{"bank", "STRANGER", "CASH1", "deposit", day(47), 15, "Example Stranger", "", ""},
	)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name) VALUES
            ('EXAMPLE SPORTS', 'Example Sports', 'GENERAL_MERCHANDISE_SPORTING_GOODS', ?, 100, 'test-model'),
            ('EXAMPLE POWER', 'Example Power', 'RENT_AND_UTILITIES_GAS_AND_ELECTRICITY', ?, 100, 'test-model'),
            ('EXAMPLE BISTRO', 'Example Bistro', 'FOOD_AND_DRINK_RESTAURANT', ?, 100, 'test-model')`,
		SignatureVersion, SignatureVersion, SignatureVersion); err != nil {
		t.Fatalf("seed the merchant store: %v", err)
	}
	repaid := Rule{Match: regexp.MustCompile(`(?i)example (sports|power|friend|stranger)`), Category: canonical.IncomeDetailedReimbursement}
	res := runPass(t, db, ctx, Options{
		Income: IncomeOptions{Rules: []Rule{repaid}},
		TransferOverrides: []gold.TransferOverrideRule{{
			Verb: gold.VerbReimburse,
			A:    gold.TransferOverrideSelector{Source: "bank", Account: "CASH1", Day: day(45), Amount: 40, Currency: "USD"},
			B:    &gold.TransferOverrideSelector{Source: "bank", Account: "CARD1", Day: day(40), Amount: -120, Currency: "USD"},
		}, {
			Verb: gold.VerbReimburse,
			A:    gold.TransferOverrideSelector{Source: "bank", Account: "CASH1", Day: day(99), Amount: 1, Currency: "USD"},
			B:    &gold.TransferOverrideSelector{Source: "bank", Account: "CARD1", Day: day(40), Amount: -120, Currency: "USD"},
		}},
	})
	if res.ReimbursementPairs != 1 || res.UnmatchedReimbursements != 1 || res.UnlinkedReimbursements != 1 {
		t.Errorf("pairs %d, unmatched %d, unlinked %d; want 1, 1, 1",
			res.ReimbursementPairs, res.UnmatchedReimbursements, res.UnlinkedReimbursements)
	}

	type link struct{ category, road, out string }
	got := map[string]link{}
	rows, err := db.QueryContext(ctx, `SELECT transaction_external_id, spend_detailed, road,
	        COALESCE(out_transaction_external_id, '') FROM spend_reimbursements()`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		var l link
		if err := rows.Scan(&id, &l.category, &l.road, &l.out); err != nil {
			t.Fatal(err)
		}
		got[id] = l
	}
	rows.Close()
	want := map[string]link{
		"SHOP-BACK":   {"GENERAL_MERCHANDISE_SPORTING_GOODS", "signature", "SHOP"},
		"POWER-BACK":  {"RENT_AND_UTILITIES_GAS_AND_ELECTRICITY", "store", ""},
		"FRIEND-BACK": {"FOOD_AND_DRINK_RESTAURANT", "pair", "DINNER"},
		// Larger than the purchase: no outflow answers, but the store
		// files the payee, so it nets in the payee's category.
		"TOO-MUCH": {"GENERAL_MERCHANDISE_SPORTING_GOODS", "store", ""},
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s links as %+v, want %+v", id, got[id], w)
		}
	}
	if _, ok := got["STRANGER"]; ok {
		t.Error("a repayment no road reaches must stay unlinked")
	}

	// The linked repayments are spending refunds in their category; income
	// still leaves them out; cash flow draws them on the outflow side.
	var refunds, base int
	var net float64
	if err := db.QueryRowContext(ctx, `
        SELECT count(*), sum(CAST(net_amount AS DOUBLE)) FROM spending_lines_base(0, 9999999999)
         WHERE provenance = 'reimbursement'`).Scan(&refunds, &net); err != nil {
		t.Fatal(err)
	}
	if refunds != 4 || net != 595 {
		t.Errorf("refund lines %d summing %v, want 4 summing 595", refunds, net)
	}
	if err := db.QueryRowContext(ctx, `
        SELECT count(*) FROM income_lines_base(0, 9999999999)
         WHERE transaction_external_id IN ('SHOP-BACK','POWER-BACK','FRIEND-BACK','TOO-MUCH','STRANGER')`).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if base != 0 {
		t.Errorf("%d repayments in the income base, want none", base)
	}
	places := map[string]string{}
	rows, err = db.QueryContext(ctx, `SELECT transaction_external_id, section FROM cashflow_txn_nodes(0, 9999999999)
	        WHERE transaction_external_id IN ('SHOP-BACK', 'STRANGER')`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, section string
		if err := rows.Scan(&id, &section); err != nil {
			t.Fatal(err)
		}
		places[id] = section
	}
	rows.Close()
	if places["SHOP-BACK"] != "operating_out" || places["STRANGER"] != "operating_in" {
		t.Errorf("cash flow sections = %v, want the linked one out, the unlinked one in", places)
	}
}

// TestReimbursementRoadsHoldTheirGuards pins what each road refuses:
// an outflow past the 365-day window, one in another currency, one
// spending does not count (an own-account move), a duplicate or a
// mismatched ledger line; and that the latest of two candidates wins
// and an insurance payout stays income. Every name and figure is
// invented.
func TestReimbursementRoadsHoldTheirGuards(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		// window: 366 days back reaches nothing, 365 does
		txn{"bank", "OLD", "CARD1", "purchase", day(100), -50, "Example Old Shop", "", ""},
		txn{"bank", "OLD-BACK", "CASH1", "deposit", day(466), 10, "Example Old Shop", "", ""},
		txn{"bank", "EDGE", "CARD1", "purchase", day(100), -50, "Example Edge Shop", "", ""},
		txn{"bank", "EDGE-BACK", "CASH1", "deposit", day(465), 10, "Example Edge Shop", "", ""},
		// the latest of two outflows
		txn{"bank", "FIRST", "CARD1", "purchase", day(200), -60, "Example Twice", "", ""},
		txn{"bank", "SECOND", "CARD1", "purchase", day(210), -60, "Example Twice", "", ""},
		txn{"bank", "TWICE-BACK", "CASH1", "deposit", day(220), 20, "Example Twice", "", ""},
		// an own-account move the ledger points at
		txn{"bank", "MOVE", "CASH1", "withdrawal", day(300), -70, "Own Transfer", "", ""},
		txn{"other-bank", "MOVE-IN", "CASH2", "deposit", day(300), 70, "Own Transfer", "", ""},
		txn{"bank", "MOVE-BACK", "CASH1", "deposit", day(305), 30, "Example Mover", "", ""},
		// an insurance payout from a payee spending was paid to
		txn{"bank", "PREMIUM", "CARD1", "purchase", day(400), -90, "Example Insurer", "", ""},
		txn{"bank", "PAYOUT", "CASH1", "deposit", day(401), 40, "Example Insurer", "", ""},
	)
	seedTxnsIn(t, db, ctx, "EUR",
		txn{"bank", "EURO", "CARD1", "purchase", day(500), -80, "Example Euro Shop", "", ""})
	seedTxns(t, db, ctx,
		txn{"bank", "EURO-BACK", "CASH1", "deposit", day(505), 30, "Example Euro Shop", "", ""})
	// The outflows carry a category: an uncategorized outflow answers no
	// road until the model files it.
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name) VALUES
            ('EXAMPLE EDGE SHOP', 'Example Edge Shop', 'GENERAL_MERCHANDISE_SPORTING_GOODS', ?, 100, 'test-model'),
            ('EXAMPLE TWICE', 'Example Twice', 'GENERAL_MERCHANDISE_SPORTING_GOODS', ?, 100, 'test-model'),
            ('EXAMPLE INSURER', 'Example Insurer', 'GENERAL_SERVICES_INSURANCE', ?, 100, 'test-model')`,
		SignatureVersion, SignatureVersion, SignatureVersion); err != nil {
		t.Fatalf("seed the merchant store: %v", err)
	}
	repaid := Rule{Match: regexp.MustCompile(`(?i)example (old shop|edge shop|twice|mover|euro shop)`),
		Category: canonical.IncomeDetailedReimbursement}
	insured := Rule{Match: regexp.MustCompile(`(?i)example insurer`), Category: "INCOME_INSURANCE_PAYOUT"}
	leg := func(acct string, d int64, amt float64) gold.TransferOverrideSelector {
		return gold.TransferOverrideSelector{Source: "bank", Account: acct, Day: day(d), Amount: amt, Currency: "USD"}
	}
	move, payout := leg("CARD1", 300, -70), leg("CARD1", 400, -90)
	move.Account = "CASH1"
	// A spending rule files the old shop and the euro shop, which the
	// store does not hold: only the signature road could link them.
	filed := Rule{Match: regexp.MustCompile(`(?i)example (old|euro) shop`), Category: "GENERAL_MERCHANDISE_SPORTING_GOODS"}
	res := runPass(t, db, ctx, Options{
		Rules:  []Rule{filed},
		Income: IncomeOptions{Rules: []Rule{repaid, insured}},
		TransferOverrides: []gold.TransferOverrideRule{
			{Verb: gold.VerbReimburse, A: leg("CASH1", 305, 30), B: &move},   // to an own-account move
			{Verb: gold.VerbReimburse, A: leg("CASH1", 401, 40), B: &payout}, // a payout is no repayment
			{Verb: gold.VerbReimburse, A: leg("CASH1", 220, 20), B: &payout},
			{Verb: gold.VerbReimburse, A: leg("CASH1", 220, 20), B: &payout}, // the same repayment twice
		},
	})
	if res.ReimbursementPairs != 2 || res.UnmatchedReimbursements != 2 {
		t.Errorf("pairs %d, unmatched %d; want 2 written (the move and the first twice-line), 2 refused",
			res.ReimbursementPairs, res.UnmatchedReimbursements)
	}
	roads := map[string]string{}
	outs := map[string]string{}
	rows, err := db.QueryContext(ctx, `SELECT transaction_external_id, road, COALESCE(out_transaction_external_id, '')
	        FROM spend_reimbursements()`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, road, out string
		if err := rows.Scan(&id, &road, &out); err != nil {
			t.Fatal(err)
		}
		roads[id], outs[id] = road, out
	}
	rows.Close()
	var filedOld string
	if err := db.QueryRowContext(ctx, `SELECT spend_detailed FROM spend_txn_categories()
	        WHERE transaction_external_id = 'OLD'`).Scan(&filedOld); err != nil || filedOld == "" {
		t.Fatalf("the old purchase must carry a category for the window test to mean anything: %q, %v", filedOld, err)
	}
	if _, ok := roads["OLD-BACK"]; ok {
		t.Error("an outflow 366 days back must not answer")
	}
	if roads["EDGE-BACK"] != "signature" {
		t.Errorf("an outflow 365 days back answers: %q", roads["EDGE-BACK"])
	}
	// The ledger names the premium for TWICE-BACK, and the premium is a
	// counted purchase, so the pair answers ahead of the signature road.
	if roads["TWICE-BACK"] != "pair" || outs["TWICE-BACK"] != "PREMIUM" {
		t.Errorf("TWICE-BACK = %q to %q, want the ledger's pair", roads["TWICE-BACK"], outs["TWICE-BACK"])
	}
	if _, ok := roads["MOVE-BACK"]; ok {
		t.Error("a pair to an own-account move must not net: spending does not count the move")
	}
	if _, ok := roads["EURO-BACK"]; ok {
		t.Error("an outflow in another currency must not answer")
	}
	if _, ok := roads["PAYOUT"]; ok {
		t.Error("an insurance payout is income, never a repayment")
	}
	var income int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM income_lines_base(0, 9999999999)
	        WHERE transaction_external_id = 'PAYOUT'`).Scan(&income); err != nil || income != 1 {
		t.Errorf("the payout in the income base: %d, %v", income, err)
	}
}

// TestReimbursementLatestOutflowWins: of two outflows the signature road
// could take, the later one answers.
func TestReimbursementLatestOutflowWins(t *testing.T) {
	db, ctx := openGold(t)
	seedTxns(t, db, ctx,
		txn{"bank", "FIRST", "CARD1", "purchase", day(200), -60, "Example Twice", "", ""},
		txn{"bank", "SECOND", "CARD1", "purchase", day(210), -60, "Example Twice", "", ""},
		txn{"bank", "BACK", "CASH1", "deposit", day(220), 20, "Example Twice", "", ""},
	)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO spend_merchant_categories (merchant_signature, merchant_name, spend_detailed,
                                               signature_version, assigned_at, model_name) VALUES
            ('EXAMPLE TWICE', 'Example Twice', 'GENERAL_MERCHANDISE_SPORTING_GOODS', ?, 100, 'test-model')`,
		SignatureVersion); err != nil {
		t.Fatalf("seed the merchant store: %v", err)
	}
	runPass(t, db, ctx, Options{Income: IncomeOptions{Rules: []Rule{{
		Match: regexp.MustCompile(`(?i)example twice`), Category: canonical.IncomeDetailedReimbursement}}}})
	var out, class string
	if err := db.QueryRowContext(ctx, `SELECT out_transaction_external_id FROM spend_reimbursements()`).Scan(&out); err != nil || out != "SECOND" {
		t.Errorf("the repayment links to %q (%v), want the later outflow", out, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT class FROM cashflow_txn_nodes(0, 9999999999)
	        WHERE transaction_external_id = 'BACK'`).Scan(&class); err != nil || class != "consumption" {
		t.Errorf("the repayment's cash flow class = %q (%v), want its outflow's, consumption", class, err)
	}
}
