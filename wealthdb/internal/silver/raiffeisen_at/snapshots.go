package raiffeisenat

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// Snapshots emits, in one batch: one cash ACCOUNT per deposit account, a
// CURRENT cash balance from each account's roster balance, and a CLOSING cash
// balance per day the daily-balance series carries a saldo. Cash is modelled
// as cash_balances, never a position/instrument; gold's report_cash macro
// synthesises the read-time cash position.
func (c *Connection) Snapshots(ctx context.Context, w canonical.Window) (silver.SnapshotStream, error) {
	if !w.HasChanges {
		return silver.NewSnapshotStream(nil), nil
	}
	ccy, err := silver.ReadAccountCurrencies(ctx, c.db, "EUR")
	if err != nil {
		return nil, err
	}
	var batch canonical.SnapshotBatch
	if err := c.appendAccounts(ctx, w, ccy, &batch); err != nil {
		return nil, err
	}
	if err := c.appendDailyBalances(ctx, w, ccy, &batch); err != nil {
		return nil, err
	}
	return silver.NewSnapshotStream([]canonical.SnapshotBatch{batch}), nil
}

// appendAccounts emits one AccountChange (AccountKind cash) per deposit account
// from its latest roster snapshot, plus a CURRENT cash balance when the roster
// captured a live balance.
//
// The CURRENT balance is stamped at the SOURCE's latest roster snapshot, not
// at the account's own. Silver content-dedups an unchanged roster row, so a
// quiet account's MAX(snapshot_at) falls behind the source's; gold's
// cash_chosen keeps only the rows carrying the source's single
// MAX(snapshot_at), so an account stamped at its own would drop out of the
// latest net-worth view once another account changed. The figure is still
// that account's latest known balance, as of this load.
func (c *Connection) appendAccounts(ctx context.Context, w canonical.Window,
	ccy silver.AccountCurrencies, batch *canonical.SnapshotBatch) error {
	const q = `
SELECT a.account_external_id,
       COALESCE(a.account_type, ''), COALESCE(a.nickname, ''), COALESCE(a.mask, ''),
       a.balance,
       (SELECT MIN(snapshot_at) FROM accounts a2 WHERE a2.account_external_id = a.account_external_id),
       a.snapshot_at,
       (SELECT MAX(snapshot_at) FROM accounts),
       a.payload
  FROM accounts a
 WHERE a.snapshot_at = (SELECT MAX(snapshot_at) FROM accounts a3
                         WHERE a3.account_external_id = a.account_external_id)
 ORDER BY a.account_external_id`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("raiffeisen_at appendAccounts: %w", err)
	}
	defer rows.Close()

	wrapper := canonical.TaxWrapperTaxablePersonal
	style := canonical.ManagementStyleSelfDirected
	for rows.Next() {
		var (
			id, accType, nickname, mask, payload string
			bal                                  sql.NullFloat64
			firstSeen, lastSeen, sourceLatest    int64
		)
		if err := rows.Scan(&id, &accType, &nickname, &mask, &bal, &firstSeen, &lastSeen, &sourceLatest, &payload); err != nil {
			return err
		}
		currency := ccy.Of(id)
		acc := canonical.AccountChange{
			AccountExternalID: id,
			AccountKind:       canonical.AccountKindCash,
			DisplayName:       silver.StrPtrIfNonEmpty(displayName(nickname, accType, mask)),
			Nickname:          silver.StrPtrIfNonEmpty(nickname),
			BaseCurrency:      &currency,
			TaxWrapper:        &wrapper,
			ManagementStyle:   &style,
			FirstSeenAt:       firstSeen,
			LastSeenAt:        lastSeen,
			Payload:           json.RawMessage(payload),
		}
		batch.Accounts = append(batch.Accounts, acc)

		if bal.Valid && sourceLatest >= w.Start && sourceLatest <= w.End {
			batch.CashBalances = append(batch.CashBalances, silver.CashBalance(
				sourceLatest, id, currency, canonical.BalanceKindCurrent,
				canonical.NewDecimalFromFloat(bal.Float64), `{"basis":"roster"}`))
		}
	}
	return rows.Err()
}

// displayName prefers a user nickname, then the account type paired with the
// last-4 mask (e.g. "Gehaltekonto …1234"), then either alone.
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
func (c *Connection) appendDailyBalances(ctx context.Context, w canonical.Window,
	ccy silver.AccountCurrencies, batch *canonical.SnapshotBatch) error {
	const q = `
SELECT account_external_id, balance_date, balance
  FROM daily_balances
 WHERE balance_date BETWEEN ? AND ?
 ORDER BY account_external_id, balance_date`
	rows, err := c.db.QueryContext(ctx, q, w.Start, w.End)
	if err != nil {
		return fmt.Errorf("raiffeisen_at appendDailyBalances: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id      string
			day     int64
			balance float64
		)
		if err := rows.Scan(&id, &day, &balance); err != nil {
			return err
		}
		batch.CashBalances = append(batch.CashBalances, silver.CashBalance(
			day, id, ccy.Of(id), canonical.BalanceKindClosing,
			canonical.NewDecimalFromFloat(balance), `{"basis":"daily_balance"}`))
	}
	return rows.Err()
}
