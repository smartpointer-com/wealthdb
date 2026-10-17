package firstcitizens

import (
	"context"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Snapshots emits, in one batch: one cash ACCOUNT per deposit account, a
// CURRENT cash balance from each account's roster balance, and a CLOSING cash
// balance per day the balance moved, valued at the transaction ledger's
// end-of-day running balance. Cash is modelled as cash_balances, never a
// position/instrument; gold's report_cash macro synthesises the read-time cash
// position.
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
	if err := c.appendRunningBalances(ctx, w, ccy, &batch); err != nil {
		return nil, err
	}
	return silver.NewSnapshotStream([]canonical.SnapshotBatch{batch}), nil
}

// appendAccounts emits one AccountChange (AccountKind cash) per deposit account
// from its latest roster snapshot, plus a CURRENT cash balance when the roster
// captured a live balance, stamped at the source's latest roster snapshot
// (silver.BankRosterRow.CurrentBalance).
func (c *Connection) appendAccounts(ctx context.Context, w canonical.Window,
	ccy silver.AccountCurrencies, batch *canonical.SnapshotBatch) error {
	roster, err := silver.ReadBankRoster(ctx, c.db, "firstcitizens appendAccounts", "nickname", "")
	if err != nil {
		return err
	}
	for _, r := range roster {
		currency := ccy.Of(r.ID)
		acc := r.Account(canonical.AccountKindCash, r.DisplayName(), currency)
		acc.Nickname = silver.StrPtrIfNonEmpty(r.Name)
		batch.Accounts = append(batch.Accounts, acc)
		if cb, ok := r.CurrentBalance(w, currency, canonical.NewDecimalFromFloat(r.Balance.Float64)); ok {
			batch.CashBalances = append(batch.CashBalances, cb)
		}
	}
	return nil
}

// appendRunningBalances emits one CLOSING cash balance per day the balance
// moved, valued at that day's end-of-day running balance from the transaction
// ledger — the exact cash time series, back to the ledger's start. Every
// history row carries a runningBalance, so the series has no gaps.
//
// The ledger has no intra-day order, so the fitid-highest row of the day
// stands for its end-of-day balance (silver.RunningBalanceQuery); the
// imprecision is bounded by same-day activity and resolves by the next day.
func (c *Connection) appendRunningBalances(ctx context.Context, w canonical.Window,
	ccy silver.AccountCurrencies, batch *canonical.SnapshotBatch) error {
	return silver.AppendClosingBalances(ctx, c.db, "firstcitizens appendRunningBalances",
		silver.RunningBalanceQuery, []any{w.Start, w.End}, `{"basis":"running_balance"}`,
		ccy.Of, nil, batch)
}
