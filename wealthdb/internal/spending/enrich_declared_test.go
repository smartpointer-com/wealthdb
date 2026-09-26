package spending

import (
	"regexp"
	"testing"

	"github.com/ptu-gh/wealthdb/wealthdb/internal/canonical"
)

// TestPassRuleNamesADeclaredAccount: a rule's `far` writes the declared
// account onto the row's far columns in both directions. The outbound
// leg is the spending family's own row; the inbound leg is the income
// family's verdict, and the spending pass carries its far account onto
// the one overlay that has a column for it. A pin on such an inbound
// row outranks the rule, and the boundary block counts the
// declarations off the dimension.
func TestPassRuleNamesADeclaredAccount(t *testing.T) {
	db, ctx := openGold(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              tax_wrapper, display_name, first_seen_at, last_seen_at)
             VALUES (?, 'savings-bank', 'cash', 'taxable_personal', 'Example Savings', 1, 1),
                    (?, 'unused',       'cash', 'taxable_personal', 'Never named',     1, 1)`,
		canonical.DeclaredSourceID, canonical.DeclaredSourceID); err != nil {
		t.Fatalf("declare accounts: %v", err)
	}
	seedTxns(t, db, ctx,
		txn{"bank", "T-OUT", "CASH1", "withdrawal", day(70), -3000, "", "Transfer to Example Savings Bank", ""},
		txn{"bank", "T-IN", "CASH1", "deposit", day(75), 2000, "", "Transfer from Example Savings Bank", ""},
		txn{"bank", "T-IN-PINNED", "CASH1", "deposit", day(76), 500, "", "Transfer from Example Savings Bank", ""},
		txn{"bank", "T-SPEND", "CASH1", "withdrawal", day(77), -60, "", "Corner Market", ""},
		txn{"bank", "T-OTHER-IN", "CASH1", "deposit", day(78), 900, "", "Payroll Example Employer", ""},
	)
	far := Rule{Match: regexp.MustCompile(`(?i)EXAMPLE SAVINGS BANK`),
		Category: canonical.SpendDetailedInternalTransfer, Far: "savings-bank"}
	res := runPass(t, db, ctx, Options{
		Rules: []Rule{far},
		Income: IncomeOptions{
			Rules: []Rule{far},
			Pins: []Pin{{Source: "bank", Account: "CASH1", Day: day(76), Amount: 500,
				Currency: "USD", Detailed: "INCOME_OTHER_INCOME"}},
		},
	})

	if s, a, _ := farOf(t, db, ctx, "bank", "T-OUT"); s != canonical.DeclaredSourceID || a != "savings-bank" {
		t.Errorf("T-OUT far = (%q, %q), want the declared account", s, a)
	}
	if d, p := verdictOf(t, db, ctx, "bank", "T-OUT"); d != canonical.SpendDetailedInternalTransfer || p != ProvenanceRule {
		t.Errorf("T-OUT = (%q, %q)", d, p)
	}
	// The inbound leg: the income overlay carries the verdict, the
	// spending overlay the far account.
	if s, a, _ := farOf(t, db, ctx, "bank", "T-IN"); s != canonical.DeclaredSourceID || a != "savings-bank" {
		t.Errorf("T-IN far = (%q, %q), want the declared account on the spending overlay", s, a)
	}
	var incomeDetailed, incomeProv string
	if err := db.QueryRowContext(ctx, `
        SELECT income_detailed, provenance FROM income_txn_enrichment
         WHERE silver_source_id = 'bank' AND transaction_external_id = 'T-IN'`).
		Scan(&incomeDetailed, &incomeProv); err != nil {
		t.Fatalf("read T-IN income verdict: %v", err)
	}
	if incomeDetailed != canonical.SpendDetailedInternalTransfer || incomeProv != ProvenanceRule {
		t.Errorf("T-IN income = (%q, %q)", incomeDetailed, incomeProv)
	}
	// A pinned inbound row is the pin's, not the far rule's.
	var n int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM spend_txn_enrichment
         WHERE transaction_external_id = 'T-IN-PINNED' AND far_account_external_id IS NOT NULL`).
		Scan(&n); err != nil || n != 0 {
		t.Errorf("the pinned inbound row took the rule's far account (n=%d err=%v)", n, err)
	}
	if _, ok := spendingBaseIDs(t, db, ctx)["T-SPEND"]; !ok {
		t.Error("the purchase left the spending base")
	}
	// A deposit no far rule reaches is nobody's to put on the spending
	// overlay; the far loop emits the rows it reaches, not the pool.
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM spend_txn_enrichment WHERE transaction_external_id = 'T-OTHER-IN'`).
		Scan(&n); err != nil || n != 0 {
		t.Errorf("a deposit no far rule reached is on the spending overlay (n=%d err=%v)", n, err)
	}
	if res.Cashflow.DeclaredAccounts != 2 || res.Cashflow.DeclaredPooled != 2 || res.Cashflow.UnusedDeclarations != 1 {
		t.Errorf("declared counters = %d / %d pooled / %d unused, want 2 / 2 / 1",
			res.Cashflow.DeclaredAccounts, res.Cashflow.DeclaredPooled, res.Cashflow.UnusedDeclarations)
	}

	// Without the far, the same rules leave the far columns empty.
	bare := Rule{Match: far.Match, Category: far.Category}
	runPass(t, db, ctx, Options{Rules: []Rule{bare}, Income: IncomeOptions{Rules: []Rule{bare}}})
	if s, a, _ := farOf(t, db, ctx, "bank", "T-OUT"); s != "" || a != "" {
		t.Errorf("a rule without far still wrote (%q, %q)", s, a)
	}
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM spend_txn_enrichment WHERE transaction_external_id = 'T-IN'`).
		Scan(&n); err != nil || n != 0 {
		t.Errorf("an inbound leg no far rule reached is on the spending overlay (n=%d err=%v)", n, err)
	}
}

// TestPassScopedFarRuleReachesThePool: a far rule narrowed by account,
// portfolio and date reaches an inbound leg through the matcher pool
// exactly as it reaches a population row. The pool row carries the same
// scope facts, so a row outside the scope is left alone and one inside
// it takes the far account.
func TestPassScopedFarRuleReachesThePool(t *testing.T) {
	db, ctx := openGold(t)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO accounts (silver_source_id, account_external_id, account_kind,
                              tax_wrapper, display_name, first_seen_at, last_seen_at)
             VALUES (?, 'savings-bank', 'cash', 'taxable_personal', 'Example Savings', 1, 1)`,
		canonical.DeclaredSourceID); err != nil {
		t.Fatalf("declare account: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
        UPDATE accounts SET portfolio_external_id = 'PF1'
         WHERE silver_source_id = 'bank' AND account_external_id = 'CASH1'`); err != nil {
		t.Fatalf("place the account in a portfolio: %v", err)
	}
	seedTxns(t, db, ctx,
		txn{"bank", "T-IN-EARLY", "CASH1", "deposit", day(60), 1500, "", "Transfer from Example Savings Bank", ""},
		txn{"bank", "T-IN", "CASH1", "deposit", day(75), 2000, "", "Transfer from Example Savings Bank", ""},
	)
	far := Rule{Match: regexp.MustCompile(`(?i)EXAMPLE SAVINGS BANK`),
		Category: canonical.SpendDetailedInternalTransfer, Far: "savings-bank",
		Scope: RuleScope{Source: "bank", Account: "CASH1", Portfolio: "PF1", From: day(70)}}
	runPass(t, db, ctx, Options{Rules: []Rule{far}, Income: IncomeOptions{Rules: []Rule{far}}})

	if s, a, _ := farOf(t, db, ctx, "bank", "T-IN"); s != canonical.DeclaredSourceID || a != "savings-bank" {
		t.Errorf("T-IN far = (%q, %q), want the declared account through the scoped rule", s, a)
	}
	var n int
	if err := db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM spend_txn_enrichment WHERE transaction_external_id = 'T-IN-EARLY'`).
		Scan(&n); err != nil || n != 0 {
		t.Errorf("a row before the scope's start took the far rule (n=%d err=%v)", n, err)
	}
}
