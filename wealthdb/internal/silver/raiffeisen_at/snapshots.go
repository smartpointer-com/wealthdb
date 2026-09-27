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
	ccy, err := c.accountCurrencies(ctx)
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
func (c *Connection) appendAccounts(ctx context.Context, w canonical.Window,
	ccy map[string]string, batch *canonical.SnapshotBatch) error {
	const q = `
SELECT a.account_external_id,
       COALESCE(a.account_type, ''), COALESCE(a.nickname, ''), COALESCE(a.mask, ''),
       a.balance,
       a.snapshot_at,
       (SELECT MIN(snapshot_at) FROM accounts a2 WHERE a2.account_external_id = a.account_external_id),
       (SELECT MAX(snapshot_at) FROM accounts a3 WHERE a3.account_external_id = a.account_external_id),
       a.payload
  FROM accounts a
 WHERE a.snapshot_at = (SELECT MAX(snapshot_at) FROM accounts a4
                         WHERE a4.account_external_id = a.account_external_id)
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
			snap, firstSeen, lastSeen            int64
		)
		if err := rows.Scan(&id, &accType, &nickname, &mask, &bal, &snap, &firstSeen, &lastSeen, &payload); err != nil {
			return err
		}
		currency := currencyOf(ccy, id)
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

		if bal.Valid && snap >= w.Start && snap <= w.End {
			batch.CashBalances = append(batch.CashBalances, cashBalance(
				snap, id, currency, canonical.BalanceKindCurrent,
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
	ccy map[string]string, batch *canonical.SnapshotBatch) error {
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
		batch.CashBalances = append(batch.CashBalances, cashBalance(
			day, id, currencyOf(ccy, id), canonical.BalanceKindClosing,
			canonical.NewDecimalFromFloat(balance), `{"basis":"daily_balance"}`))
	}
	return rows.Err()
}

// cashBalance builds a CashBalanceChange. The collector rounds money to cents
// before storing, so the REAL→Decimal step is exact at display precision.
func cashBalance(snap int64, accountID, currency string, kind canonical.BalanceKind,
	amount canonical.Decimal, payload string) canonical.CashBalanceChange {
	return canonical.CashBalanceChange{
		SnapshotAt:        snap,
		AccountExternalID: accountID,
		Currency:          currency,
		BalanceKind:       kind,
		Amount:            amount,
		Payload:           json.RawMessage(payload),
	}
}
