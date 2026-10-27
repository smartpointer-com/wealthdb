package spending

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/gold"
)

// Reimbursements: a repayment nets against the outflow it answers (gold
// migration 0160, docs/SPENDING.md §3). Two of its three roads need no pass:
// the signature road and the merchant-store road are SQL, read at query time,
// so a verdict the model places later links too. The ledger road is the
// holder naming the pair, and resolving a natural key to a transaction is the
// pass's work, so it writes `spend_reimbursement_pairs` here.

// resolveReimbursements rewrites `spend_reimbursement_pairs` from the
// ledger's `reimburse` lines and returns how many pairs it wrote and how many
// lines named no pair. A line names a pair when each of its legs describes
// exactly one transaction: the leg with the positive amount is the repayment,
// the other the outflow. The repayment's income category must resolve to
// `reimbursement`, and the outflow must be a line spending counts, or the
// pair would net nothing. A leg that describes none or several, a repayment
// of another category, or an outflow spending does not count (an own-account
// move, an investment) is reported, not guessed at, for the reason an
// unmatched transfer override is.
func resolveReimbursements(ctx context.Context, tx *sql.Tx, rules []gold.TransferOverrideRule) (written, unresolved int, err error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM spend_reimbursement_pairs`); err != nil {
		return 0, 0, fmt.Errorf("spending: reimbursement pairs: %w", err)
	}
	seen := map[txKey]bool{}
	for _, r := range rules {
		if r.Verb != gold.VerbReimburse || r.B == nil {
			continue
		}
		in, out := r.A, *r.B
		if in.Amount < 0 {
			in, out = out, in
		}
		repay, ok, err := describedTransaction(ctx, tx, in)
		if err != nil {
			return 0, 0, err
		}
		spent, ok2, err := describedTransaction(ctx, tx, out)
		if err != nil {
			return 0, 0, err
		}
		if !ok || !ok2 || in.Amount <= 0 || out.Amount >= 0 || seen[repay] {
			unresolved++
			continue
		}
		var category sql.NullString
		if err := tx.QueryRowContext(ctx, `
SELECT income_detailed FROM income_txn_categories()
 WHERE silver_source_id = ? AND transaction_external_id = ?`, repay.source, repay.txID).Scan(&category); err != nil && err != sql.ErrNoRows {
			return 0, 0, fmt.Errorf("spending: reimbursement leg: %w", err)
		}
		if category.String != canonical.IncomeDetailedReimbursement {
			unresolved++
			continue
		}
		counted, err := spendingCounts(ctx, tx, spent)
		if err != nil {
			return 0, 0, err
		}
		if !counted {
			unresolved++
			continue
		}
		seen[repay] = true
		if _, err := tx.ExecContext(ctx, `
INSERT INTO spend_reimbursement_pairs (silver_source_id, transaction_external_id,
    out_silver_source_id, out_transaction_external_id) VALUES (?, ?, ?, ?)`,
			repay.source, repay.txID, spent.source, spent.txID); err != nil {
			return 0, 0, fmt.Errorf("spending: reimbursement pairs: %w", err)
		}
		written++
	}
	return written, unresolved, nil
}

// describedTransaction finds the one transaction a ledger leg describes: its
// source, account, currency, UTC day and amount to the cent, the natural key
// a pin resolves by (pinMatches). ok is false where none or several answer.
func describedTransaction(ctx context.Context, tx *sql.Tx, s gold.TransferOverrideSelector) (txKey, bool, error) {
	from := gold.EpochDay(s.Day) * gold.SecondsPerDay
	rows, err := tx.QueryContext(ctx, `
SELECT transaction_external_id
  FROM transactions
 WHERE silver_source_id = ? AND account_external_id = ? AND currency = ?
   AND occurred_at >= ? AND occurred_at < ?
   AND abs(CAST(net_amount AS DOUBLE) - ?) <= ?
 LIMIT 2`, s.Source, s.Account, s.Currency, from, from+gold.SecondsPerDay, s.Amount, pinAmountEps)
	if err != nil {
		return txKey{}, false, fmt.Errorf("spending: reimbursement leg: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return txKey{}, false, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil || len(ids) != 1 {
		return txKey{}, false, err
	}
	return txKey{s.Source, ids[0]}, true, nil
}

// spendingCounts reports whether spending counts the outflow: a line of
// spending_lines_own with a category, the set 0160's pair road joins to.
func spendingCounts(ctx context.Context, tx *sql.Tx, k txKey) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `
SELECT 1 FROM spending_lines_own(0, ?)
 WHERE silver_source_id = ? AND transaction_external_id = ? AND spend_detailed IS NOT NULL`,
		gold.MaxEpoch, k.source, k.txID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("spending: reimbursement outflow: %w", err)
	}
	return true, nil
}

// countUnlinkedReimbursements counts the repayments no road links: an inflow
// placed `reimbursement` that nets against nothing and stays out of both
// bases. The ledger's `reimburse` verb is the remedy, so the load says how
// many are left.
func countUnlinkedReimbursements(ctx context.Context, tx *sql.Tx) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `
SELECT count(*)
  FROM income_txn_categories() ic
  JOIN transactions t
    ON t.silver_source_id = ic.silver_source_id
   AND t.transaction_external_id = ic.transaction_external_id
 WHERE ic.income_detailed = ? AND t.net_amount > 0
   AND NOT EXISTS (SELECT 1 FROM spend_reimbursements() rb
                    WHERE rb.silver_source_id = t.silver_source_id
                      AND rb.transaction_external_id = t.transaction_external_id)`,
		canonical.IncomeDetailedReimbursement).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("spending: unlinked reimbursements: %w", err)
	}
	return n, nil
}
