package chase

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Snapshots emits, in one batch: one ACCOUNT per roster account (cash for a
// deposit account, card for a credit card), a CURRENT balance from each
// account's roster figure, and the CLOSING balance series. Cash is modelled as
// cash_balances, never a position/instrument; gold's report_cash macro
// synthesises the read-time cash position.
//
// THREE SOURCES FEED THE BALANCE SERIES, and they are mutually exclusive by
// construction so that no account ever gets two marks of one kind on one day:
//
//   - the transaction ledger's running balance (appendRunningBalances), one
//     mark per day the balance moved. It is the only source for deposits, and
//     for a card it covers the export era, where the loader reconstructs the
//     running balance neither card export carries;
//   - a card statement's printed closing figure (appendStatementBalances),
//     one mark at each period_end — but only for the periods the
//     reconstruction does not reach, which is what keeps the two apart;
//   - the roster, which is a CURRENT mark, not a closing one, and is stamped
//     at the load rather than at a ledger day.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	facts, err := c.accountFactsByID(ctx)
	if err != nil {
		return nil, err
	}
	var batch canonical.SnapshotBatch
	if err := c.appendAccounts(ctx, w, facts, &batch); err != nil {
		return nil, err
	}
	if err := c.appendRunningBalances(ctx, w, facts, &batch); err != nil {
		return nil, err
	}
	if err := c.appendStatementBalances(ctx, w, facts, &batch); err != nil {
		return nil, err
	}
	return silver.NewSnapshotStream([]canonical.SnapshotBatch{batch}), nil
}

// appendAccounts emits one AccountChange per roster account from its latest
// snapshot, plus that account's CURRENT balance when the roster captured one,
// stamped at the source's latest roster snapshot
// (silver.BankRosterRow.CurrentBalance).
func (c *Connection) appendAccounts(ctx context.Context, w canonical.Window,
	facts map[string]accountFacts, batch *canonical.SnapshotBatch) error {
	roster, err := silver.ReadBankRoster(ctx, c.db, "chase appendAccounts", "nickname", "")
	if err != nil {
		return err
	}
	for _, r := range roster {
		card := isCard(facts, r.ID)
		kind := canonical.AccountKindCash
		if card {
			kind = canonical.AccountKindCard
		}
		currency := currencyOf(facts, r.ID)
		acc := r.Account(kind, r.DisplayName(), currency)
		acc.Nickname = silver.StrPtrIfNonEmpty(r.Name)
		batch.Accounts = append(batch.Accounts, acc)
		if cb, ok := r.CurrentBalance(w, currency, signedBalance(r.Balance.Float64, card)); ok {
			batch.CashBalances = append(batch.CashBalances, cb)
		}
	}
	return nil
}

// appendRunningBalances emits one CLOSING balance per day the balance moved,
// valued at that day's end-of-day running balance from the transaction ledger
// — the exact balance time series, back to the ledger's start. On a deposit
// account this, not the statements, is the source of the historic cash marks:
// the running balance carries the balance at every date, whereas the collector
// records statement dates without their parsed balances (and only for recent
// years). The statement dates are a subset of these, so an as-of query at any
// statement date returns that statement's closing balance.
//
// On a card the same column carries the loader's reconstruction, which neither
// card export supplies: it rolls the posted ledger between the statement
// closing anchors, and a span that fails to land on its anchor keeps no
// balance at all. The statement era carries none by design. Those gaps are
// what appendStatementBalances fills, at period granularity.
//
// The fitid-highest row of a day stands for its end-of-day balance
// (silver.RunningBalanceQuery). On a card that is exact — the reconstruction
// rolls forward in (posted_at, fitid) order, so the fitid-highest row of a day
// IS the one carrying its end-of-day balance. On a deposit account the balance
// is the provider's own column and the ledger has no intra-day order, so the
// pick is arbitrary among that day's rows; the imprecision is bounded by
// same-day activity and resolves by the next day.
func (c *Connection) appendRunningBalances(ctx context.Context, w canonical.Window,
	facts map[string]accountFacts, batch *canonical.SnapshotBatch) error {
	return silver.AppendClosingBalances(ctx, c.db, "chase appendRunningBalances",
		silver.RunningBalanceQuery, []any{w.Start, w.End}, `{"basis":"running_balance"}`,
		func(id string) string { return currencyOf(facts, id) },
		func(id string, v float64) canonical.Decimal { return signedBalance(v, isCard(facts, id)) },
		batch)
}

// appendStatementBalances emits one CLOSING balance at a card statement's
// period_end, from the figure the statement printed — the balance truth for
// the era the reconstruction cannot reach.
//
// It is product-guarded to cards: the table is written by the card statement
// pass only, and a deposit account's historic marks come from its export's own
// running-balance column at day density.
//
// The NOT EXISTS clause is what keeps this source and appendRunningBalances
// off the same day. A period contributes its anchor only when NO reconstructed
// balance falls anywhere inside [period_start, period_end] — and period_end is
// inside its own period, so a collision is impossible. Suppressing the whole
// period rather than just the day is also the right reading: the
// reconstruction is anchored TO these closing figures, so where it runs it
// already carries them, at balance-move density instead of monthly.
//
// `transactions_covered` is deliberately not consulted. It records whether the
// period's TRANSACTIONS reached silver; the printed closing balance is
// asserted by the statement either way, and a period whose rows were refused
// is precisely one that needs its anchor.
func (c *Connection) appendStatementBalances(ctx context.Context, w canonical.Window,
	facts map[string]accountFacts, batch *canonical.SnapshotBatch) error {
	const q = `
SELECT sb.account_external_id, sb.period_end, sb.closing
  FROM statement_balances sb
 WHERE sb.closing IS NOT NULL
   AND sb.period_end BETWEEN ? AND ?
   AND sb.account_external_id IN (SELECT account_external_id FROM accounts
                                   WHERE product = ?)
   AND NOT EXISTS (SELECT 1 FROM transactions t
                    WHERE t.account_external_id = sb.account_external_id
                      AND t.balance IS NOT NULL
                      AND t.posted_at BETWEEN sb.period_start AND sb.period_end)
 ORDER BY sb.account_external_id, sb.period_end`
	// Every row here is a card's by the product guard above, so the printed
	// owed-positive figure is unconditionally negated.
	return silver.AppendClosingBalances(ctx, c.db, "chase appendStatementBalances",
		q, []any{w.Start, w.End, productCard}, `{"basis":"statement_closing"}`,
		func(id string) string { return currencyOf(facts, id) },
		func(_ string, v float64) canonical.Decimal { return signedBalance(v, true) },
		batch)
}

// signedBalance converts a silver balance to the canonical sign. Silver stores
// every card figure the provider's way — the balance is the POSITIVE amount
// owed — and gold carries a revolving-credit liability as negative cash, so a
// card figure is negated here and a deposit figure passes through. This is the
// only place the flip happens.
//
// The collector rounds money to cents before storing, so the REAL→Decimal step
// is exact at display precision.
func signedBalance(amount float64, card bool) canonical.Decimal {
	d := canonical.NewDecimalFromFloat(amount)
	if card {
		return d.Neg()
	}
	return d
}
