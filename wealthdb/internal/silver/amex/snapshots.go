package amex

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Snapshots emits, in one batch: one ACCOUNT per card, a CURRENT balance from
// each card's roster figure, and the CLOSING balance series from the statement
// periods. A card carries no instrument, so nothing here is a position.
//
// The two are told apart by kind rather than by date: a CURRENT mark stamped
// at the source's latest load, and a CLOSING mark at a period's own end date.
// They can fall on the same day — the activity channel's period ends at the
// window a run fetched — and do not compete when they do.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	ccy, err := silver.ReadAccountCurrencies(ctx, c.db, "USD")
	if err != nil {
		return nil, err
	}
	var batch canonical.SnapshotBatch
	if err := c.appendAccounts(ctx, w, ccy, &batch); err != nil {
		return nil, err
	}
	if err := c.appendStatementBalances(ctx, w, ccy, &batch); err != nil {
		return nil, err
	}
	return silver.NewSnapshotStream([]canonical.SnapshotBatch{batch}), nil
}

// appendAccounts emits one AccountChange per card from its latest snapshot,
// plus that card's CURRENT balance when the roster captured one, stamped at
// the source's latest roster snapshot (silver.BankRosterRow.CurrentBalance).
func (c *Connection) appendAccounts(ctx context.Context, w canonical.Window,
	ccy silver.AccountCurrencies, batch *canonical.SnapshotBatch) error {
	roster, err := silver.ReadBankRoster(ctx, c.db, "amex appendAccounts", "display_name", "")
	if err != nil {
		return err
	}
	for _, r := range roster {
		currency := ccy.Of(r.ID)
		batch.Accounts = append(batch.Accounts,
			r.Account(canonical.AccountKindCard, r.DisplayName(), currency))
		if cb, ok := r.CurrentBalance(w, currency, signedBalance(r.Balance.Float64)); ok {
			batch.CashBalances = append(batch.CashBalances, cb)
		}
	}
	return nil
}

// appendStatementBalances emits one CLOSING balance at each statement period's
// end, from the figure the period stated — the only historic balance truth a
// card has, since no Amex channel carries a running balance per row.
//
// Silver records a period from whichever channel stated it (the activity JSON
// for the ~24 months it reaches, the statement PDFs for the deeper archive),
// keyed on (account, period_end) so the two converge on one row per period
// rather than competing. `transactions_covered` is deliberately not consulted:
// it records whether the period's ROWS reached silver, and the closing balance
// is asserted either way — a period whose rows were refused is precisely one
// that needs its anchor.
func (c *Connection) appendStatementBalances(ctx context.Context, w canonical.Window,
	ccy silver.AccountCurrencies, batch *canonical.SnapshotBatch) error {
	const q = `
SELECT account_external_id, period_end, closing
  FROM statement_balances
 WHERE closing IS NOT NULL
   AND period_end BETWEEN ? AND ?
 ORDER BY account_external_id, period_end`
	return silver.AppendClosingBalances(ctx, c.db, "amex appendStatementBalances",
		q, []any{w.Start, w.End}, `{"basis":"statement_closing"}`, ccy.Of,
		func(_ string, v float64) canonical.Decimal { return signedBalance(v) }, batch)
}

// signedBalance converts a silver card balance to the canonical sign. Silver
// holds every card figure the provider's way — the balance is the POSITIVE
// amount owed — and gold carries a revolving-credit liability as negative
// cash. Every account this source emits is a card, so the flip is
// unconditional, and this is the only place it happens.
//
// The collector rounds money to cents before storing, so the REAL→Decimal step
// is exact at display precision.
func signedBalance(amount float64) canonical.Decimal {
	return canonical.NewDecimalFromFloat(amount).Neg()
}
