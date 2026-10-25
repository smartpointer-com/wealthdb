package raiffeisenat

import (
	"context"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Snapshots emits, in one batch: one cash ACCOUNT per deposit account, a
// CURRENT cash balance per run whose roster listed the account (silver's
// run_accounts), and a CLOSING cash balance per day the daily-balance series
// carries a saldo. Cash is modelled as cash_balances, never a
// position/instrument; gold's report_cash macro synthesises the read-time
// cash position.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	ccy, err := silver.ReadAccountCurrencies(ctx, c.db, "EUR")
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
	if err := c.appendDailyBalances(ctx, w, ccy, &batch); err != nil {
		return nil, err
	}
	return silver.NewSnapshotStream([]canonical.SnapshotBatch{batch}), nil
}

// appendAccounts emits one AccountChange (AccountKind cash) per deposit account
// from its latest roster snapshot.
func (c *Connection) appendAccounts(ctx context.Context, ccy silver.AccountCurrencies,
	batch *canonical.SnapshotBatch) error {
	roster, err := silver.ReadBankRoster(ctx, c.db, "raiffeisen_at appendAccounts", "nickname", "account_type")
	if err != nil {
		return err
	}
	for _, r := range roster {
		acc := r.Account(canonical.AccountKindCash, displayName(r.Name, r.AccountType, r.Mask), ccy.Of(r.ID))
		acc.Nickname = silver.StrPtrIfNonEmpty(r.Name)
		batch.Accounts = append(batch.Accounts, acc)
	}
	return nil
}

// displayName prefers a user nickname, then the account type paired with the
// last-4 mask (e.g. "Testkonto …1234"), then either alone.
func displayName(nickname, accType, mask string) string {
	if nickname != "" {
		return nickname
	}
	switch {
	case accType != "" && mask != "":
		return accType + " " + mask
	case accType != "":
		return accType
	default:
		return mask
	}
}

// appendDailyBalances emits one CLOSING cash balance per day the kontostaende
// series carries a saldo — the cash time series (silver's daily_balances
// table). Unlike the US siblings the transaction history has no per-row
// balance, so this dedicated series is the source of the closing marks.
//
// Each saldo is stamped at its day's last second, the instant it speaks
// for. A run's CURRENT balance earlier that day holds only until the
// day's close, and the series gives the day's value.
func (c *Connection) appendDailyBalances(ctx context.Context, w canonical.Window,
	ccy silver.AccountCurrencies, batch *canonical.SnapshotBatch) error {
	q := fmt.Sprintf(`
SELECT account_external_id, balance_date + %[1]d, balance
  FROM daily_balances
 WHERE balance_date + %[1]d BETWEEN ? AND ?
 ORDER BY account_external_id, balance_date`, silver.DayEndOffset)
	return silver.AppendClosingBalances(ctx, c.db, "raiffeisen_at appendDailyBalances",
		q, []any{w.Start, w.End}, `{"basis":"daily_balance"}`, ccy.Of, nil, batch)
}
