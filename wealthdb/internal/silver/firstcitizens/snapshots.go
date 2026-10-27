package firstcitizens

import (
	"context"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Snapshots emits, in one batch: one cash ACCOUNT per deposit account, a
// CURRENT cash balance per run whose roster listed the account, and a CLOSING
// cash balance per day the balance moved, valued at the transaction ledger's
// end-of-day running balance. The rate a statement states rides on a
// closing balance of its cycle (applyStatementRates). Cash is modelled as
// cash_balances, never a position/instrument; gold's report_cash macro
// synthesises the read-time cash position.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	ccy, err := silver.ReadAccountCurrencies(ctx, c.db, "USD")
	if err != nil {
		return nil, err
	}
	var batch canonical.SnapshotBatch
	if err := c.appendAccounts(ctx, ccy, &batch); err != nil {
		return nil, err
	}
	if err := silver.AppendReportedBalances(ctx, c.db, kindName, "run_accounts", w, ccy.Of, &batch); err != nil {
		return nil, err
	}
	if err := c.appendRunningBalances(ctx, w, ccy, &batch); err != nil {
		return nil, err
	}
	if err := c.applyStatementRates(ctx, &batch); err != nil {
		return nil, err
	}
	return silver.NewSnapshotStream([]canonical.SnapshotBatch{batch}), nil
}

// appendAccounts emits one AccountChange (AccountKind cash) per deposit account
// from its latest roster snapshot.
func (c *Connection) appendAccounts(ctx context.Context, ccy silver.AccountCurrencies,
	batch *canonical.SnapshotBatch) error {
	roster, err := silver.ReadBankRoster(ctx, c.db, "firstcitizens appendAccounts", "nickname", "")
	if err != nil {
		return err
	}
	for _, r := range roster {
		acc := r.Account(canonical.AccountKindCash, r.DisplayName(), ccy.Of(r.ID))
		acc.Nickname = silver.StrPtrIfNonEmpty(r.Name)
		batch.Accounts = append(batch.Accounts, acc)
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
//
// Each balance is stamped at its day's last second, the instant it speaks
// for. A run's CURRENT balance earlier that day holds only until the
// day's close, and the ledger gives the day's value. The window bounds
// the stamps, so the shared query reads the days whose stamp falls in it.
func (c *Connection) appendRunningBalances(ctx context.Context, w canonical.Window,
	ccy silver.AccountCurrencies, batch *canonical.SnapshotBatch) error {
	q := fmt.Sprintf(`SELECT account_external_id, posted_at + %d, balance FROM (`+
		silver.RunningBalanceQuery+`) ORDER BY 1, 2`, silver.DayEndOffset)
	return silver.AppendClosingBalances(ctx, c.db, "firstcitizens appendRunningBalances",
		q, []any{w.Start - silver.DayEndOffset, w.End - silver.DayEndOffset},
		`{"basis":"running_balance"}`, ccy.Of, nil, batch)
}
